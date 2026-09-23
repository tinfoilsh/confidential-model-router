package main

import (
	"container/list"
	"crypto/sha256"
	"sync"
)

const (
	routeContextCacheMaxBytes = 50 << 20
	// Covers the entry, decoded structs, list node, hash key, and map slack.
	routeContextCacheEntryOverhead = 1024
	// Leave room for string allocation rounding and retained backing storage.
	routeContextCacheStringOverhead = 2
)

type routeContextCacheKey struct {
	credential [sha256.Size]byte
	model      [sha256.Size]byte
}

func routeContextKey(apiKey, model string) routeContextCacheKey {
	return routeContextCacheKey{sha256.Sum256([]byte(apiKey)), sha256.Sum256([]byte(model))}
}

type routeContextCacheEntry struct {
	key      routeContextCacheKey
	resolved routeContext
	err      *routeContextError
	sequence uint64
	bytes    int
}

// Entries are immutable snapshots; readers can use them after releasing mu.
// Stale entries remain usable through outages and are evicted only for space.
type routeContextCache struct {
	mu       sync.Mutex
	entries  map[routeContextCacheKey]*list.Element
	lru      list.List
	bytes    int
	maxBytes int
	// Missing keys reject refreshes older than any evicted result, without
	// retaining an unbounded table of per-key sequence tombstones.
	evictedSequence uint64
}

func (c *routeContextCache) get(key routeContextCacheKey) (routeContext, *routeContextError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[key]; element != nil {
		c.lru.MoveToFront(element)
		entry := element.Value.(routeContextCacheEntry)
		return entry.resolved, entry.err
	}
	return routeContext{}, nil
}

func (c *routeContextCache) put(key routeContextCacheKey, resolved routeContext, err *routeContextError, sequence uint64) {
	size := len(resolved.OrgID)
	if resolved.RateLimit != nil {
		size += len(resolved.RateLimit.Decision) + len(resolved.RateLimit.Reason)
	}
	if err != nil {
		size += len(err.retryAfter) + len(err.apiError.Type) + len(err.apiError.Code) + len(err.apiError.Param) + len(err.apiError.Message)
	}
	size = routeContextCacheEntryOverhead + routeContextCacheStringOverhead*size
	c.mu.Lock()
	defer c.mu.Unlock()
	if size > c.maxBytes {
		return
	}
	if element := c.entries[key]; element != nil {
		entry := element.Value.(routeContextCacheEntry)
		if sequence < entry.sequence {
			return
		}
		c.bytes -= entry.bytes
		c.lru.Remove(element)
		delete(c.entries, key)
	} else if sequence <= c.evictedSequence {
		return
	}
	for c.bytes+size > c.maxBytes {
		oldest := c.lru.Back()
		entry := c.lru.Remove(oldest).(routeContextCacheEntry)
		c.evictedSequence = max(c.evictedSequence, entry.sequence)
		delete(c.entries, entry.key)
		c.bytes -= entry.bytes
	}
	if c.entries == nil {
		c.entries = make(map[routeContextCacheKey]*list.Element)
	}
	c.entries[key] = c.lru.PushFront(routeContextCacheEntry{key, resolved, err, sequence, size})
	c.bytes += size
}
