package main

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// CacheEntry represents a cached HTTP response
type CacheEntry struct {
	Response     []byte
	Headers      http.Header
	StatusCode   int
	Timestamp    time.Time
	Expiry       time.Time
	Uncompressed bool
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
	target             *url.URL
	proxy              *httputil.ReverseProxy
	cache              *Cache
	cacheTTL           time.Duration
	cacheableStatus    map[int]bool
	maxCacheSize       int64
	currentCacheSize   int64
	disableCompression bool
}

// NewReverseProxy creates a new reverse proxy with caching
func NewReverseProxy(targetURL string, cacheTTL time.Duration, maxCacheSize int64, disableCompression bool) (*ReverseProxy, error) {
	url, err := url.Parse(targetURL)
	if err != nil {
		return nil, err
	}

	proxy := httputil.NewSingleHostReverseProxy(url)

	// Customize the director to handle compression
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalDirector(r)

		if disableCompression {
			// Request uncompressed content from the backend if we want to cache uncompressed
			r.Header.Del("Accept-Encoding")
		}
	}

	// Initialize cacheable status codes (200, 301, 302, etc.)
	cacheableStatus := map[int]bool{
		http.StatusOK:               true,
		http.StatusMovedPermanently: true,
		http.StatusFound:            true,
		http.StatusNotModified:      true,
	}

	return &ReverseProxy{
		target:             url,
		proxy:              proxy,
		cache:              NewCache(),
		cacheTTL:           cacheTTL,
		cacheableStatus:    cacheableStatus,
		maxCacheSize:       maxCacheSize,
		currentCacheSize:   0,
		disableCompression: disableCompression,
	}, nil
}

// generateCacheKey creates a unique key for a request
func generateCacheKey(r *http.Request) string {
	// Start with method and URL
	key := r.Method + r.URL.String()

	// Add relevant headers that might affect response content
	if acceptLanguage := r.Header.Get("Accept-Language"); acceptLanguage != "" {
		key += "Accept-Language:" + acceptLanguage
	}

	// Create MD5 hash of the key
	hasher := md5.New()
	hasher.Write([]byte(key))
	return hex.EncodeToString(hasher.Sum(nil))
}

// copyHeader copies HTTP headers from source to destination
func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
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

	// Generate cache key - we don't include Accept-Encoding since we standardize compression
	key := generateCacheKey(r)

	// Check if response is in cache
	if entry, found := p.cache.Get(key); found {
		// Serve from cache
		log.Printf("Cache hit: %s %s", r.Method, r.URL.Path)

		// Determine if we should compress the response
		acceptEncoding := r.Header.Get("Accept-Encoding")
		supportsGzip := strings.Contains(acceptEncoding, "gzip")

		// Copy cached headers to response
		responseHeader := w.Header()
		copyHeader(responseHeader, entry.Headers)

		// Set cache headers
		responseHeader.Set("X-Cache", "HIT")
		responseHeader.Set("X-Cache-Age", time.Since(entry.Timestamp).String())
		responseHeader.Set("X-Cache-Expires", time.Until(entry.Expiry).String())

		var responseBody []byte

		// If the cache entry is uncompressed and the client accepts gzip, compress it
		if entry.Uncompressed && supportsGzip {
			responseHeader.Set("Content-Encoding", "gzip")

			// Create a buffer to hold the compressed data
			var gzippedBody bytes.Buffer
			gzipWriter := gzip.NewWriter(&gzippedBody)

			// Compress the data
			_, err := gzipWriter.Write(entry.Response)
			if err != nil {
				log.Printf("Error compressing response: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}

			// Close the gzip writer to flush any buffered data
			if err := gzipWriter.Close(); err != nil {
				log.Printf("Error closing gzip writer: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}

			responseBody = gzippedBody.Bytes()
		} else if !entry.Uncompressed && !supportsGzip {
			// If the cache entry is compressed and client doesn't support compression,
			// we should decompress it (but this is a rare case since we usually cache uncompressed)
			log.Printf("Client doesn't support gzip but cache entry is compressed - decompressing")

			// Create a reader for the gzipped data
			gzipReader, err := gzip.NewReader(bytes.NewReader(entry.Response))
			if err != nil {
				log.Printf("Error creating gzip reader: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}

			// Read the decompressed data
			decompressed, err := io.ReadAll(gzipReader)
			if err != nil {
				log.Printf("Error decompressing response: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}

			// Close the reader
			if err := gzipReader.Close(); err != nil {
				log.Printf("Error closing gzip reader: %v", err)
			}

			// Remove the Content-Encoding header
			responseHeader.Del("Content-Encoding")
			responseBody = decompressed
		} else {
			// Use the cached response as-is (already in the right format)
			responseBody = entry.Response
		}

		// Update content length if it changed
		responseHeader.Set("Content-Length", fmt.Sprintf("%d", len(responseBody)))

		// Set status code and write response
		w.WriteHeader(entry.StatusCode)
		w.Write(responseBody)
		return
	}

	log.Printf("Cache miss: %s %s", r.Method, r.URL.Path)

	// Create a custom response writer to capture the response
	responseBuffer := &bytes.Buffer{}
	capturer := &ResponseCapturer{
		ResponseWriter: w,
		Buffer:         responseBuffer,
		headers:        make(http.Header),
	}

	// Serve the request with our capturing writer
	p.proxy.ServeHTTP(capturer, r)

	// Check if we should cache this response
	isCacheable := p.cacheableStatus[capturer.StatusCode]

	// Don't cache if the response says not to
	if cacheControl := capturer.headers.Get("Cache-Control"); cacheControl != "" {
		if strings.Contains(cacheControl, "no-store") || strings.Contains(cacheControl, "no-cache") {
			isCacheable = false
		}
	}

	// If status is cacheable, store in cache
	if isCacheable {
		responseData := capturer.Buffer.Bytes()
		isCompressed := capturer.headers.Get("Content-Encoding") == "gzip"

		// If the response is compressed and we want to cache uncompressed, decompress it
		if isCompressed && p.disableCompression {
			// Create a reader for the gzipped data
			gzipReader, err := gzip.NewReader(bytes.NewReader(responseData))
			if err != nil {
				log.Printf("Error creating gzip reader: %v", err)
			} else {
				// Read the decompressed data
				decompressed, err := io.ReadAll(gzipReader)
				if err != nil {
					log.Printf("Error decompressing response: %v", err)
				} else {
					// Close the reader
					if err := gzipReader.Close(); err != nil {
						log.Printf("Error closing gzip reader: %v", err)
					}

					// Use the decompressed data for caching
					responseData = decompressed

					// Remove the Content-Encoding header for the cached version
					capturer.headers.Del("Content-Encoding")

					// Update Content-Length
					capturer.headers.Set("Content-Length", fmt.Sprintf("%d", len(responseData)))
				}
			}
		}

		p.cache.Set(key, CacheEntry{
			Response:     responseData,
			Headers:      capturer.headers.Clone(),
			StatusCode:   capturer.StatusCode,
			Timestamp:    time.Now(),
			Expiry:       time.Now().Add(p.cacheTTL),
			Uncompressed: p.disableCompression || !isCompressed,
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
	stats["compression_disabled"] = p.disableCompression

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{
  "entries": %d,
  "expired": %d,
  "current_size_bytes": %d,
  "max_size_bytes": %d,
  "ttl_seconds": %.2f,
  "compression_disabled": %t
}`, stats["entries"], stats["expired"], stats["current_size_bytes"], stats["max_size_bytes"], stats["ttl_seconds"], stats["compression_disabled"])
}

// ResponseCapturer captures response data for caching
type ResponseCapturer struct {
	http.ResponseWriter
	Buffer      *bytes.Buffer
	StatusCode  int
	headers     http.Header
	headersSent bool
}

// WriteHeader captures status code before writing it
func (r *ResponseCapturer) WriteHeader(statusCode int) {
	r.StatusCode = statusCode

	// Copy the headers to the real response writer
	copyHeader(r.ResponseWriter.Header(), r.headers)

	r.ResponseWriter.WriteHeader(statusCode)
	r.headersSent = true
}

// Write captures response data before writing it
func (r *ResponseCapturer) Write(b []byte) (int, error) {
	// If headers haven't been sent yet, send them with the default status code
	if !r.headersSent {
		r.WriteHeader(http.StatusOK)
	}

	// Write to both the original writer and our buffer
	r.Buffer.Write(b)
	return r.ResponseWriter.Write(b)
}

// Header returns the header map for this response
func (r *ResponseCapturer) Header() http.Header {
	return r.headers
}

func main() {
	// Parse command line flags
	port := flag.Int("port", 8080, "Port to serve on")
	target := flag.String("target", "http://example.com", "Target URL to proxy")
	cacheTTL := flag.Duration("cache-ttl", 5*time.Minute, "Cache TTL (e.g., 5m, 1h)")
	maxCacheSize := flag.Int64("max-cache-size", 100*1024*1024, "Maximum cache size in bytes (default 100MB)")
	disableCompression := flag.Bool("disable-compression", true, "Disable compression in cached responses (recommended)")
	flag.Parse()

	// Create the reverse proxy
	proxy, err := NewReverseProxy(*target, *cacheTTL, *maxCacheSize, *disableCompression)
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
	log.Printf("Cache TTL: %s, Max cache size: %d MB, Compression disabled: %t",
		*cacheTTL, *maxCacheSize/1024/1024, *disableCompression)
	log.Printf("Cache stats available at http://localhost:%d/_cache/stats", *port)

	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
