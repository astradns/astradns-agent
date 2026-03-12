package proxy

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const defaultStaleTTL = 5 * time.Minute

type responseCache struct {
	mu         sync.RWMutex
	entries    map[string]cachedResponse
	maxEntries int
	defaultTTL time.Duration
	staleTTL   time.Duration
}

type cachedResponse struct {
	message   *dns.Msg
	storedAt  time.Time
	expiresAt time.Time
}

func newResponseCache(maxEntries int, defaultTTL time.Duration) *responseCache {
	if maxEntries <= 0 {
		return nil
	}
	if defaultTTL <= 0 {
		defaultTTL = 30 * time.Second
	}

	return &responseCache{
		entries:    make(map[string]cachedResponse, maxEntries),
		maxEntries: maxEntries,
		defaultTTL: defaultTTL,
		staleTTL:   defaultStaleTTL,
	}
}

func (c *responseCache) Get(request *dns.Msg, now time.Time) (*dns.Msg, bool) {
	if c == nil {
		return nil, false
	}

	key, ok := cacheKey(request)
	if !ok {
		return nil, false
	}

	c.mu.RLock()
	entry, exists := c.entries[key]
	c.mu.RUnlock()
	if !exists {
		return nil, false
	}

	if !entry.expiresAt.After(now) {
		return nil, false
	}

	return cloneCachedResponse(entry, request, now), true
}

// GetStale returns an expired entry that is still within the stale window.
// Used as fallback when the engine is unreachable (RFC 8767 serve-stale).
func (c *responseCache) GetStale(request *dns.Msg, now time.Time) (*dns.Msg, bool) {
	if c == nil {
		return nil, false
	}

	key, ok := cacheKey(request)
	if !ok {
		return nil, false
	}

	c.mu.RLock()
	entry, exists := c.entries[key]
	c.mu.RUnlock()
	if !exists {
		return nil, false
	}

	// Still fresh — caller should have used Get().
	if entry.expiresAt.After(now) {
		return cloneCachedResponse(entry, request, now), true
	}

	// Expired beyond the stale window — truly gone.
	if now.Sub(entry.expiresAt) > c.staleTTL {
		return nil, false
	}

	// Serve stale with TTL=1 so downstream caches don't hold it long.
	response := entry.message.Copy()
	if request != nil {
		response.Id = request.Id
	}
	for _, records := range [][]dns.RR{response.Answer, response.Ns, response.Extra} {
		for _, record := range records {
			if header := record.Header(); header != nil {
				header.Ttl = 1
			}
		}
	}
	return response, true
}

func (c *responseCache) Store(request, response *dns.Msg, now time.Time) {
	if c == nil {
		return
	}

	key, ok := cacheKey(request)
	if !ok || !isCacheableResponse(response) {
		return
	}

	ttl := cacheEntryTTL(response, c.defaultTTL)
	if ttl <= 0 {
		return
	}

	entry := cachedResponse{
		message:   response.Copy(),
		storedAt:  now,
		expiresAt: now.Add(ttl),
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.evictExpiredLocked(now)
	if len(c.entries) >= c.maxEntries {
		c.evictOldestLocked()
	}

	c.entries[key] = entry
}

func (c *responseCache) evictExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if now.Sub(entry.expiresAt) > c.staleTTL {
			delete(c.entries, key)
		}
	}
}

func (c *responseCache) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	for key, entry := range c.entries {
		if oldestKey == "" || entry.expiresAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.expiresAt
		}
	}

	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

func cacheKey(request *dns.Msg) (string, bool) {
	if request == nil || len(request.Question) == 0 {
		return "", false
	}

	question := request.Question[0]
	return fmt.Sprintf("%s|%d|%d", strings.ToLower(question.Name), question.Qtype, question.Qclass), true
}

func isCacheableResponse(response *dns.Msg) bool {
	if response == nil {
		return false
	}
	if response.Rcode != dns.RcodeSuccess {
		return false
	}
	if response.Truncated {
		return false
	}

	return true
}

func cacheEntryTTL(response *dns.Msg, fallback time.Duration) time.Duration {
	minTTL := minResponseTTL(response)
	if minTTL > 0 {
		return minTTL
	}
	return fallback
}

func minResponseTTL(response *dns.Msg) time.Duration {
	if response == nil {
		return 0
	}

	var minTTL uint32
	seen := false
	for _, records := range [][]dns.RR{response.Answer, response.Ns, response.Extra} {
		for _, record := range records {
			header := record.Header()
			if header == nil || header.Ttl == 0 {
				continue
			}
			if !seen || header.Ttl < minTTL {
				minTTL = header.Ttl
				seen = true
			}
		}
	}

	if !seen {
		return 0
	}

	return time.Duration(minTTL) * time.Second
}

func cloneCachedResponse(entry cachedResponse, request *dns.Msg, now time.Time) *dns.Msg {
	response := entry.message.Copy()
	if request != nil {
		response.Id = request.Id
	}

	elapsedSeconds := uint32(now.Sub(entry.storedAt).Seconds())
	if elapsedSeconds == 0 {
		return response
	}

	for _, records := range [][]dns.RR{response.Answer, response.Ns, response.Extra} {
		for _, record := range records {
			header := record.Header()
			if header == nil || header.Ttl == 0 {
				continue
			}
			if elapsedSeconds >= header.Ttl {
				header.Ttl = 0
				continue
			}
			header.Ttl -= elapsedSeconds
		}
	}

	return response
}
