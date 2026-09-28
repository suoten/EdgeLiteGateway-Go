package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// InferenceCache provides TTL-based caching for AI inference results.
// This prevents redundant inference calls for the same input data.
type InferenceCache struct {
	mu       sync.Mutex
	items    map[string]*cacheItem
	ttl      time.Duration
	maxSize  int
	hits     int64
	misses   int64
	evictions int64
}

type cacheItem struct {
	value     interface{}
	timestamp time.Time
}

// NewInferenceCache creates a new InferenceCache.
func NewInferenceCache(ttlSeconds float64, maxSize int) *InferenceCache {
	if maxSize <= 0 {
		maxSize = 1024
	}
	return &InferenceCache{
		items:   make(map[string]*cacheItem),
		ttl:     time.Duration(ttlSeconds * float64(time.Second)),
		maxSize: maxSize,
	}
}

// makeKey creates a cache key from model ID and input data.
func makeKey(modelID string, inputData []float64) string {
	// Hash the input data for a compact key
	h := sha256.New()
	h.Write([]byte(modelID))
	for _, v := range inputData {
		// Use fmt to handle float formatting consistently
		h.Write([]byte(fmt.Sprintf("%g", v)))
	}
	return modelID + ":" + hex.EncodeToString(h.Sum(nil))
}

// Get retrieves a cached result.
func (c *InferenceCache) Get(modelID string, inputData []float64) (interface{}, bool) {
	key := makeKey(modelID, inputData)
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, false
	}
	// Check TTL
	if time.Since(item.timestamp) > c.ttl {
		delete(c.items, key)
		c.evictions++
		c.misses++
		return nil, false
	}
	c.hits++
	return item.value, true
}

// Put stores a result in the cache.
func (c *InferenceCache) Put(modelID string, inputData []float64, outputData interface{}) {
	key := makeKey(modelID, inputData)
	c.mu.Lock()
	defer c.mu.Unlock()
	// Evict expired items if at capacity
	if len(c.items) >= c.maxSize {
		c.evictExpired()
		// If still at capacity, evict oldest
		if len(c.items) >= c.maxSize {
			c.evictOldest()
		}
	}
	c.items[key] = &cacheItem{
		value:     outputData,
		timestamp: time.Now(),
	}
}

// Clear clears all cached items.
func (c *InferenceCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*cacheItem)
}

// InvalidateModel removes all cached items for a specific model.
func (c *InferenceCache) InvalidateModel(modelID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for key := range c.items {
		if len(key) > len(modelID) && key[:len(modelID)] == modelID {
			delete(c.items, key)
			count++
		}
	}
	return count
}

// GetStats returns cache statistics.
func (c *InferenceCache) GetStats() map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := c.hits + c.misses
	var hitRate float64
	if total > 0 {
		hitRate = float64(c.hits) / float64(total)
	}
	return map[string]interface{}{
		"size":      len(c.items),
		"max_size":  c.maxSize,
		"hits":      c.hits,
		"misses":    c.misses,
		"evictions": c.evictions,
		"hit_rate":  hitRate,
		"ttl_seconds": c.ttl.Seconds(),
	}
}

// evictExpired removes all expired items.
func (c *InferenceCache) evictExpired() {
	now := time.Now()
	for key, item := range c.items {
		if now.Sub(item.timestamp) > c.ttl {
			delete(c.items, key)
			c.evictions++
		}
	}
}

// evictOldest removes the oldest item from the cache.
func (c *InferenceCache) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for key, item := range c.items {
		if first || item.timestamp.Before(oldestTime) {
			oldestKey = key
			oldestTime = item.timestamp
			first = false
		}
	}
	if oldestKey != "" {
		delete(c.items, oldestKey)
		c.evictions++
	}
}
