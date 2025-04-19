package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"sync"
	"time"
)

// CacheEntry represents a cached HTTP response
type CacheEntry struct {
	Response    []byte
	ContentType string
	StatusCode  int
	Timestamp   time.Time
	Expiry      time.Time
}

// Cache is a simple in-memory cache for HTTP responses
type Cache struct {
	entries map[string]CacheEntry
	mutex   sync.RWMutex
}

// NewCache creates a new cache
func NewCache() *Cache {
	return &Cache{
		entries: make(map[string]CacheEntry),
	}
}

// Get retrieves a cached response
func (c *Cache) Get(key string) (CacheEntry, bool) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	entry, found := c.entries[key]
	if !found {
		return CacheEntry{}, false
	}

	// Check if entry has expired
	if time.Now().After(entry.Expiry) {
		return CacheEntry{}, false
	}

	return entry, true
}

// Set adds a response to the cache
func (c *Cache) Set(key string, entry CacheEntry, proxy *ReverseProxy) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// Check if we would exceed the max cache size
	newSize := proxy.currentCacheSize + int64(len(entry.Response))
	if proxy.maxCacheSize > 0 && newSize > proxy.maxCacheSize {
		// Simple eviction policy: remove oldest entries
		var keysToRemove []string
		var sizeToFree int64

		// Calculate how much we need to free up
		sizeToFree = newSize - proxy.maxCacheSize + 1024*1024 // Free an extra MB for headroom

		// Find oldest entries to remove
		var entries []struct {
			key       string
			timestamp time.Time
			size      int64
		}

		for k, v := range c.entries {
			entries = append(entries, struct {
				key       string
				timestamp time.Time
				size      int64
			}{k, v.Timestamp, int64(len(v.Response))})
		}

		// Sort by timestamp (oldest first)
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].timestamp.Before(entries[j].timestamp)
		})

		// Remove oldest entries until we have enough space
		var freedSize int64
		for _, entry := range entries {
			if freedSize >= sizeToFree {
				break
			}
			keysToRemove = append(keysToRemove, entry.key)
			freedSize += entry.size
		}

		// Remove entries
		for _, k := range keysToRemove {
			oldSize := int64(len(c.entries[k].Response))
			delete(c.entries, k)
			proxy.currentCacheSize -= oldSize
		}

		log.Printf("Cache eviction: removed %d entries, freed %d bytes", len(keysToRemove), freedSize)
	}

	// Add the new entry
	c.entries[key] = entry
	proxy.currentCacheSize += int64(len(entry.Response))

	return true
}

// CleanExpired removes expired entries from the cache
func (c *Cache) CleanExpired(proxy *ReverseProxy) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	now := time.Now()
	var freedSize int64
	count := 0

	for k, v := range c.entries {
		if now.After(v.Expiry) {
			freedSize += int64(len(v.Response))
			delete(c.entries, k)
			count++
		}
	}

	proxy.currentCacheSize -= freedSize

	if count > 0 {
		log.Printf("Cache cleanup: removed %d expired entries, freed %d bytes", count, freedSize)
	}
}

// GetStats returns cache statistics
func (c *Cache) GetStats() map[string]interface{} {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	stats := make(map[string]interface{})
	stats["entries"] = len(c.entries)

	// Count expired entries
	now := time.Now()
	expired := 0
	for _, v := range c.entries {
		if now.After(v.Expiry) {
			expired++
		}
	}

	stats["expired"] = expired
	return stats
}

// ReverseProxy represents our reverse proxy with caching capabilities
type ReverseProxy struct {
	target           *url.URL
	proxy            *httputil.ReverseProxy
	cache            *Cache
	cacheTTL         time.Duration
	cacheableStatus  map[int]bool
	maxCacheSize     int64
	currentCacheSize int64
}

// NewReverseProxy creates a new reverse proxy with caching
func NewReverseProxy(targetURL string, cacheTTL time.Duration, maxCacheSize int64) (*ReverseProxy, error) {
	url, err := url.Parse(targetURL)
	if err != nil {
		return nil, err
	}

	proxy := httputil.NewSingleHostReverseProxy(url)

	// Initialize cacheable status codes (200, 301, 302, etc.)
	cacheableStatus := map[int]bool{
		http.StatusOK:               true,
		http.StatusMovedPermanently: true,
		http.StatusFound:            true,
		http.StatusNotModified:      true,
	}

	return &ReverseProxy{
		target:           url,
		proxy:            proxy,
		cache:            NewCache(),
		cacheTTL:         cacheTTL,
		cacheableStatus:  cacheableStatus,
		maxCacheSize:     maxCacheSize,
		currentCacheSize: 0,
	}, nil
}

// generateCacheKey creates a unique key for a request
func generateCacheKey(r *http.Request) string {
	// Start with method and URL
	key := r.Method + r.URL.String()

	// Add relevant headers that might affect response content
	// For example, Accept-Encoding, Accept-Language
	if acceptEncoding := r.Header.Get("Accept-Encoding"); acceptEncoding != "" {
		key += "Accept-Encoding:" + acceptEncoding
	}

	if acceptLanguage := r.Header.Get("Accept-Language"); acceptLanguage != "" {
		key += "Accept-Language:" + acceptLanguage
	}

	// Create MD5 hash of the key
	hasher := md5.New()
	hasher.Write([]byte(key))
	return hex.EncodeToString(hasher.Sum(nil))
}

// ServeHTTP handles HTTP requests
func (p *ReverseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Check for cache status request
	if r.URL.Path == "/_cache/stats" {
		p.handleCacheStats(w, r)
		return
	}

	// Only cache GET and HEAD requests
	if r.Method != "GET" && r.Method != "HEAD" {
		p.proxy.ServeHTTP(w, r)
		return
	}

	// Don't cache if the client explicitly requests fresh content
	if r.Header.Get("Cache-Control") == "no-cache" {
		w.Header().Set("X-Cache", "BYPASS")
		p.proxy.ServeHTTP(w, r)
		return
	}

	// Generate cache key
	key := generateCacheKey(r)

	// Check if response is in cache
	if entry, found := p.cache.Get(key); found {
		// Serve from cache
		log.Printf("Cache hit: %s %s", r.Method, r.URL.Path)

		// Set response headers
		w.Header().Set("Content-Type", entry.ContentType)
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("X-Cache-Age", time.Since(entry.Timestamp).String())
		w.Header().Set("X-Cache-Expires", time.Until(entry.Expiry).String())

		// Set status code and write response
		w.WriteHeader(entry.StatusCode)
		w.Write(entry.Response)
		return
	}

	log.Printf("Cache miss: %s %s", r.Method, r.URL.Path)

	// Create a custom response writer to capture the response
	responseBuffer := &bytes.Buffer{}
	responseWriter := &ResponseCapturer{
		ResponseWriter: w,
		Buffer:         responseBuffer,
	}

	// Serve the request with our capturing writer
	p.proxy.ServeHTTP(responseWriter, r)

	// Check if we should cache this response
	isCacheable := p.cacheableStatus[responseWriter.StatusCode]

	// Don't cache if the response says not to
	if cacheControl := responseWriter.Header().Get("Cache-Control"); cacheControl != "" {
		if cacheControl == "no-store" || cacheControl == "no-cache" {
			isCacheable = false
		}
	}

	// If status is cacheable, store in cache
	if isCacheable {
		p.cache.Set(key, CacheEntry{
			Response:    responseBuffer.Bytes(),
			ContentType: responseWriter.Header().Get("Content-Type"),
			StatusCode:  responseWriter.StatusCode,
			Timestamp:   time.Now(),
			Expiry:      time.Now().Add(p.cacheTTL),
		}, p)

		// Set cache header
		w.Header().Set("X-Cache", "MISS")
	}
}

// handleCacheStats returns cache statistics
func (p *ReverseProxy) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	stats := p.cache.GetStats()
	stats["current_size_bytes"] = p.currentCacheSize
	stats["max_size_bytes"] = p.maxCacheSize
	stats["ttl_seconds"] = p.cacheTTL.Seconds()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{
  "entries": %d,
  "expired": %d,
  "current_size_bytes": %d,
  "max_size_bytes": %d,
  "ttl_seconds": %.2f
}`, stats["entries"], stats["expired"], stats["current_size_bytes"], stats["max_size_bytes"], stats["ttl_seconds"])
}

// ResponseCapturer captures response data for caching
type ResponseCapturer struct {
	http.ResponseWriter
	Buffer     *bytes.Buffer
	StatusCode int
}

// WriteHeader captures status code before writing it
func (r *ResponseCapturer) WriteHeader(statusCode int) {
	r.StatusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

// Write captures response data before writing it
func (r *ResponseCapturer) Write(b []byte) (int, error) {
	// Write to both the original writer and our buffer
	r.Buffer.Write(b)
	return r.ResponseWriter.Write(b)
}

func main() {
	// Parse command line flags
	port := flag.Int("port", 8080, "Port to serve on")
	target := flag.String("target", "http://example.com", "Target URL to proxy")
	cacheTTL := flag.Duration("cache-ttl", 5*time.Minute, "Cache TTL (e.g., 5m, 1h)")
	maxCacheSize := flag.Int64("max-cache-size", 100*1024*1024, "Maximum cache size in bytes (default 100MB)")
	flag.Parse()

	// Create the reverse proxy
	proxy, err := NewReverseProxy(*target, *cacheTTL, *maxCacheSize)
	if err != nil {
		log.Fatal(err)
	}

	// Start a goroutine for periodic cache cleanup
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		for {
			select {
			case <-ticker.C:
				proxy.cache.CleanExpired(proxy)
			}
		}
	}()

	// Start server
	server := http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: proxy,
		// Set reasonable timeouts
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("Reverse proxy started at :%d -> %s", *port, *target)
	log.Printf("Cache TTL: %s, Max cache size: %d MB", *cacheTTL, *maxCacheSize/1024/1024)
	log.Printf("Cache stats available at http://localhost:%d/_cache/stats", *port)

	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
