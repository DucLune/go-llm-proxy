package pipeline

import (
	"sync"
	"time"
)

// maxCacheEntries is the maximum number of entries in each processing cache
// (image descriptions, PDF text). When the cache is full, the least-recently
// accessed entries are evicted to make room for the new one. This bounds memory
// growth while keeping recently-used descriptions warm — a full-table flush
// would force every historical image to be re-processed by the vision model on
// the next turn (Claude Code replays full conversation history on every request).
const maxCacheEntries = 1024

// cacheEntry holds a cached value, an optional expiry, and a recency stamp.
// A zero expiresAt means the entry is permanent (does not expire).
// seq is a monotonically increasing recency counter used for LRU eviction —
// a counter is used instead of wall-clock time because many entries can be
// stored in the same instant, and equal timestamps make eviction order
// nondeterministic.
type cacheEntry struct {
	value     string
	expiresAt time.Time
	seq       uint64
}

// boundedCache is a thread-safe string→string cache with a hard size limit
// and optional per-entry TTL. When the limit is reached, least-recently-used
// entries are evicted before inserting the new entry. This prevents unbounded
// memory growth from long-running proxy processes while preserving the cache
// across turns — a key property for the image pipeline, where Claude Code
// replays full conversation history and a flushed cache forces re-processing
// of every historical image.
//
// Entries stored via Store are permanent; entries stored via StoreWithTTL
// expire after the given duration. Expired entries are removed lazily on
// the next Load for that key.
type boundedCache struct {
	mu    sync.RWMutex
	items map[string]cacheEntry
	// seq is a monotonically increasing recency counter. Every Store and
	// successful Load stamps the entry with the next value, so eviction can
	// pick the least-recently-used entry deterministically even when many
	// entries share a timestamp.
	seq uint64
}

func newBoundedCache() *boundedCache {
	return &boundedCache{items: make(map[string]cacheEntry)}
}

// Load returns the cached value for key. If the entry has expired, it is
// removed and treated as a miss. A successful load refreshes the entry's
// recency stamp so frequently-used descriptions resist eviction.
func (c *boundedCache) Load(key string) (string, bool) {
	c.mu.RLock()
	entry, ok := c.items[key]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
		c.mu.Lock()
		// Re-check under write lock in case another goroutine refreshed it.
		if e2, ok2 := c.items[key]; ok2 && !e2.expiresAt.IsZero() && time.Now().After(e2.expiresAt) {
			delete(c.items, key)
		}
		c.mu.Unlock()
		return "", false
	}
	// Refresh recency. Cheap, and keeps hot entries alive across LRU eviction.
	c.mu.Lock()
	if e2, ok2 := c.items[key]; ok2 && !e2.expiresAt.IsZero() && time.Now().After(e2.expiresAt) {
		// Expired between the RUnlock above and here — remove it.
		delete(c.items, key)
		c.mu.Unlock()
		return "", false
	}
	if e2, ok2 := c.items[key]; ok2 {
		c.seq++
		e2.seq = c.seq
		c.items[key] = e2
	}
	c.mu.Unlock()
	return entry.value, true
}

// evictOne removes the least-recently-accessed entry to make room for a new
// insert. Called with the write lock held.
func (c *boundedCache) evictOne() {
	var oldestKey string
	oldestSeq := uint64(0)
	first := true
	for k, e := range c.items {
		if first || e.seq < oldestSeq {
			oldestKey = k
			oldestSeq = e.seq
			first = false
		}
	}
	if oldestKey != "" {
		delete(c.items, oldestKey)
	}
}

// nextSeq returns the next recency stamp. Callers must hold the write lock.
func (c *boundedCache) nextSeq() uint64 {
	c.seq++
	return c.seq
}

// Store inserts a permanent entry (never expires).
func (c *boundedCache) Store(key, value string) {
	c.mu.Lock()
	if len(c.items) >= maxCacheEntries {
		c.evictOne()
	}
	c.items[key] = cacheEntry{value: value, seq: c.nextSeq()}
	c.mu.Unlock()
}

// StoreWithTTL inserts an entry that expires after ttl. Intended for caching
// transient failures so that repeated failed lookups don't hammer upstream
// services, while still allowing eventual retry.
func (c *boundedCache) StoreWithTTL(key, value string, ttl time.Duration) {
	c.mu.Lock()
	if len(c.items) >= maxCacheEntries {
		c.evictOne()
	}
	c.items[key] = cacheEntry{value: value, expiresAt: time.Now().Add(ttl), seq: c.nextSeq()}
	c.mu.Unlock()
}

func (c *boundedCache) Reset() {
	c.mu.Lock()
	c.items = make(map[string]cacheEntry)
	c.mu.Unlock()
}

// Size returns the current number of cached entries. Used for diagnostics.
func (c *boundedCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
