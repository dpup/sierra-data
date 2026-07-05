package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/dpup/prefab/errors"
	"github.com/dpup/prefab/logging"
)

// Cache provides thread-safe in-memory caching with TTL
// Implementation per data-model.md Cache Entry lines 227-241
type Cache struct {
	entries map[string]*CacheEntry
	mutex   sync.RWMutex
}

// CacheEntry represents a cached item with metadata
// Structure per data-model.md lines 227-241
type CacheEntry struct {
	Key             string        `json:"key"`
	Data            []byte        `json:"data"`
	CreatedAt       time.Time     `json:"created_at"`
	ExpiresAt       time.Time     `json:"expires_at"`
	RefreshInterval time.Duration `json:"refresh_interval"`
	Source          string        `json:"source"`
}

// NewCache creates a new in-memory cache
func NewCache() *Cache {
	return &Cache{
		entries: make(map[string]*CacheEntry),
	}
}

// Set stores data in cache with TTL based on refresh interval
func (c *Cache) Set(key string, data interface{}, refreshInterval time.Duration, source string) error {
	// Serialize data to JSON
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal data for cache: %w", err)
	}

	now := time.Now()
	entry := &CacheEntry{
		Key:             key,
		Data:            jsonData,
		CreatedAt:       now,
		ExpiresAt:       now.Add(refreshInterval),
		RefreshInterval: refreshInterval,
		Source:          source,
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.entries[key] = entry
	return nil
}

// Get retrieves data from cache if not stale
func (c *Cache) Get(key string, result interface{}) (bool, error) {
	c.mutex.RLock()
	entry, exists := c.entries[key]
	c.mutex.RUnlock()

	if !exists {
		return false, nil
	}

	// Check if entry is stale
	if c.IsStale(key) {
		return false, nil
	}

	// Deserialize data
	if err := json.Unmarshal(entry.Data, result); err != nil {
		return false, fmt.Errorf("failed to unmarshal cached data: %w", err)
	}

	return true, nil
}

// IsStale checks if cache entry is stale (past expiration)
func (c *Cache) IsStale(key string) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	entry, exists := c.entries[key]
	if !exists {
		return true
	}

	return time.Now().After(entry.ExpiresAt)
}

// IsVeryStale checks if cache entry is very stale (2x refresh interval)
// Used for stale data detection per research.md default 10 minutes = 2x refresh interval
func (c *Cache) IsVeryStale(key string) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	entry, exists := c.entries[key]
	if !exists {
		return true
	}

	veryStaleThreshold := entry.CreatedAt.Add(entry.RefreshInterval * 2)
	return time.Now().After(veryStaleThreshold)
}

// GetWithMetadata retrieves data and cache metadata
func (c *Cache) GetWithMetadata(key string, result interface{}) (*CacheEntry, bool, error) {
	c.mutex.RLock()
	entry, exists := c.entries[key]
	c.mutex.RUnlock()

	if !exists {
		return nil, false, nil
	}

	// Return metadata even if stale (caller decides how to handle)
	if result != nil {
		if err := json.Unmarshal(entry.Data, result); err != nil {
			return entry, exists, fmt.Errorf("failed to unmarshal cached data: %w", err)
		}
	}

	return entry, exists, nil
}

// Delete removes an entry from cache
func (c *Cache) Delete(key string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	delete(c.entries, key)
}

// Clear removes all entries from cache
func (c *Cache) Clear() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.entries = make(map[string]*CacheEntry)
}

// Keys returns all cache keys
func (c *Cache) Keys() []string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	keys := make([]string, 0, len(c.entries))
	for key := range c.entries {
		keys = append(keys, key)
	}
	return keys
}

// Stats returns cache statistics
func (c *Cache) Stats() CacheStats {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	now := time.Now()
	stats := CacheStats{
		TotalEntries: len(c.entries),
	}

	for _, entry := range c.entries {
		if now.After(entry.ExpiresAt) {
			stats.StaleEntries++
		} else {
			stats.FreshEntries++
		}

		// Update oldest/newest
		if stats.OldestEntry.IsZero() || entry.CreatedAt.Before(stats.OldestEntry) {
			stats.OldestEntry = entry.CreatedAt
		}
		if entry.CreatedAt.After(stats.NewestEntry) {
			stats.NewestEntry = entry.CreatedAt
		}
	}

	return stats
}

// CleanupStale removes entries that are past the very-stale threshold
// (2x refresh interval). Merely-stale entries are kept: services serve them as
// a fallback when an upstream refresh fails, so removing at ExpiresAt would
// silently break that fallback window.
func (c *Cache) CleanupStale() int {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	now := time.Now()
	var removed int

	for key, entry := range c.entries {
		if now.After(entry.CreatedAt.Add(entry.RefreshInterval * 2)) {
			delete(c.entries, key)
			removed++
		}
	}

	return removed
}

// StartPeriodicCleanup starts a goroutine that periodically evicts very-stale
// entries, bounding memory for keys that are never overwritten (e.g. the
// content-hash AI-enhancement entries). Stops when ctx is cancelled.
func (c *Cache) StartPeriodicCleanup(ctx context.Context, interval time.Duration) {
	go func() {
		defer func() {
			// Recover from any panics in the cache cleanup goroutine
			if r := recover(); r != nil {
				err, _ := errors.ParseStack(debug.Stack())
				skipFrames := 3
				numFrames := 5
				logging.Errorw(ctx, "Cache cleanup: recovered from panic",
					"error", r, "error.stack_trace", err.MinimalStack(skipFrames, numFrames))
			}
		}()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if removed := c.CleanupStale(); removed > 0 {
					logging.Infow(ctx, "Cache cleanup removed very-stale entries", "removed", removed)
				}
			}
		}
	}()
}

// Backdate shifts an entry's timestamps into the past by d, as if it had been
// written that long ago. Test helper for exercising stale/very-stale paths
// deterministically; not for production use.
func (c *Cache) Backdate(key string, d time.Duration) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if entry, ok := c.entries[key]; ok {
		entry.CreatedAt = entry.CreatedAt.Add(-d)
		entry.ExpiresAt = entry.ExpiresAt.Add(-d)
	}
}

// CacheStats provides cache usage statistics
type CacheStats struct {
	TotalEntries int
	FreshEntries int
	StaleEntries int
	OldestEntry  time.Time
	NewestEntry  time.Time
}

// Simplified Content-Based Caching Methods
// These replace the complex incident processing infrastructure

// SetEnhancedAlert caches an OpenAI-enhanced alert with content-based key
func (c *Cache) SetEnhancedAlert(contentHash string, enhanced interface{}, ttl time.Duration) error {
	key := fmt.Sprintf("enhanced_alert:%s", contentHash)
	return c.Set(key, enhanced, ttl, "enhanced_alert")
}

// GetEnhancedAlert retrieves a cached enhanced alert by content hash
func (c *Cache) GetEnhancedAlert(contentHash string) (interface{}, bool, error) {
	key := fmt.Sprintf("enhanced_alert:%s", contentHash)

	var enhanced interface{}
	found, err := c.Get(key, &enhanced)
	if err != nil {
		return nil, false, err
	}

	return enhanced, found, nil
}

// IsEnhancedAlertCached checks if an enhanced alert exists without retrieving it
func (c *Cache) IsEnhancedAlertCached(contentHash string) bool {
	key := fmt.Sprintf("enhanced_alert:%s", contentHash)
	return !c.IsStale(key)
}
