package s3mapping

import (
	"sync"
	"time"
)

const defaultCacheTTL = 30 * time.Minute

type cacheEntry struct {
	mappingID string
	found     bool // false = negative cache
	expiresAt time.Time
}

type domainCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry
}

func newDomainCache() *domainCache {
	return &domainCache{entries: make(map[string]*cacheEntry)}
}

// get returns the cached mapping for host.
// hit=true when the cache holds an entry (positive or negative).
func (c *domainCache) get(host string) (mappingID string, found bool, hit bool) {
	c.mu.RLock()
	entry, ok := c.entries[host]
	c.mu.RUnlock()

	if !ok {
		return "", false, false
	}
	if time.Now().After(entry.expiresAt) {
		c.mu.Lock()
		if e, ok := c.entries[host]; ok && time.Now().After(e.expiresAt) {
			delete(c.entries, host)
		}
		c.mu.Unlock()
		return "", false, false
	}
	return entry.mappingID, entry.found, true
}

func (c *domainCache) set(host, mappingID string, found bool, ttl time.Duration) {
	c.mu.Lock()
	c.entries[host] = &cacheEntry{
		mappingID: mappingID,
		found:     found,
		expiresAt: time.Now().Add(ttl),
	}
	c.mu.Unlock()
}

// invalidate removes the cache entry for host (if any). Returns whether an entry existed.
func (c *domainCache) invalidate(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[host]; !ok {
		return false
	}
	delete(c.entries, host)
	return true
}

// resolveTTL picks the effective TTL using the precedence:
//  1. Per-row DB value (seconds, non-nil and > 0)
//  2. Handler-level TTL (from Caddyfile or env, already resolved)
//  3. Default (30 minutes)
func resolveTTL(dbTTLSeconds *int64, handlerTTL time.Duration) time.Duration {
	if dbTTLSeconds != nil && *dbTTLSeconds > 0 {
		return time.Duration(*dbTTLSeconds) * time.Second
	}
	if handlerTTL > 0 {
		return handlerTTL
	}
	return defaultCacheTTL
}
