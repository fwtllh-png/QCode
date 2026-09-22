package goproxy

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// Public cache contract for one bound GOPROXY upstream. Module artifacts are
// immutable per version, so a successful response replays for CacheEntryTTL.
// A permanently negated response (404/410) replays without a body for the
// shorter CacheNegativeTTL: retries stop hammering the upstream, and the
// negation still clears when upstream state changes. CacheBudgetBytes bounds
// the total in-memory body footprint of one service.
const (
	CacheBudgetBytes = 32 << 20
	CacheEntryTTL    = 30 * time.Minute
	CacheNegativeTTL = 5 * time.Minute
)

type cacheEntry struct {
	status    int
	header    http.Header
	body      []byte
	expiresAt time.Time
	sequence  uint64
}

type responseCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	budget  int
	used    int
	next    uint64
}

func newResponseCache(budget int) *responseCache {
	if budget <= 0 {
		budget = CacheBudgetBytes
	}
	return &responseCache{entries: make(map[string]cacheEntry), budget: budget}
}

func (c *responseCache) get(key string, now time.Time) (cacheEntry, bool) {
	if c == nil {
		return cacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return cacheEntry{}, false
	}
	if !now.Before(entry.expiresAt) {
		delete(c.entries, key)
		c.used -= len(entry.body)
		return cacheEntry{}, false
	}
	return entry, true
}

func (c *responseCache) put(key string, entry cacheEntry) {
	if c == nil || len(entry.body) > c.budget {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if previous, ok := c.entries[key]; ok {
		c.used -= len(previous.body)
		delete(c.entries, key)
	}
	for c.used+len(entry.body) > c.budget {
		var oldestKey string
		var oldestSequence uint64
		for key, candidate := range c.entries {
			if oldestKey == "" || candidate.sequence < oldestSequence {
				oldestKey, oldestSequence = key, candidate.sequence
			}
		}
		if oldestKey == "" {
			break
		}
		c.used -= len(c.entries[oldestKey].body)
		delete(c.entries, oldestKey)
	}
	c.next++
	entry.sequence = c.next
	c.entries[key] = entry
	c.used += len(entry.body)
}

// rememberUpstreamBody buffers the upstream response body up to the cache
// budget and stores a cache entry when the status is cacheable and the whole
// body fit. It returns the buffered prefix either way so the caller can
// serve it and stream only the remainder.
func (s *Service) rememberUpstreamBody(
	key string,
	method string,
	response *http.Response,
	now time.Time,
) []byte {
	if s == nil || s.cache == nil || method != http.MethodGet ||
		!cacheableStatus(response.StatusCode) {
		return nil
	}
	buffer, err := io.ReadAll(io.LimitReader(response.Body, CacheBudgetBytes+1))
	if err != nil || len(buffer) > CacheBudgetBytes {
		return buffer
	}
	entry := cacheEntry{
		status: response.StatusCode,
		header: cloneHeader(response.Header),
		body:   buffer,
	}
	if negationStatus(response.StatusCode) {
		entry.body = nil
		entry.expiresAt = now.Add(CacheNegativeTTL)
	} else {
		entry.expiresAt = now.Add(CacheEntryTTL)
	}
	s.cache.put(key, entry)
	return buffer
}

func cacheableStatus(status int) bool {
	return status == http.StatusOK || negationStatus(status)
}

func negationStatus(status int) bool {
	return status == http.StatusNotFound || status == http.StatusGone
}

func cloneHeader(header http.Header) http.Header {
	if len(header) == 0 {
		return nil
	}
	cloned := make(http.Header, len(header))
	for name, values := range header {
		if hopHeader(name) {
			continue
		}
		cloned[name] = append([]string(nil), values...)
	}
	return cloned
}
