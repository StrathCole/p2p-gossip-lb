package cache

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// Entry captures a serialised HTTP response for reuse.
type Entry struct {
	Status    int
	Header    http.Header
	Body      []byte
	StoredAt  time.Time
	ExpiresAt time.Time
	Size      int64
}

// Clone returns a deep copy so callers can mutate headers safely.
func (e *Entry) Clone() *Entry {
	if e == nil {
		return nil
	}
	cp := &Entry{
		Status:    e.Status,
		Body:      append([]byte(nil), e.Body...),
		StoredAt:  e.StoredAt,
		ExpiresAt: e.ExpiresAt,
		Size:      e.Size,
	}
	cp.Header = cloneHeader(e.Header)
	return cp
}

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return http.Header{}
	}
	cp := make(http.Header, len(h))
	for k, values := range h {
		cp[k] = append([]string(nil), values...)
	}
	return cp
}

// ResponseCache stores cacheable responses with an approximate size budget.
type ResponseCache struct {
	mu           sync.RWMutex
	store        *lru.Cache[string, *Entry]
	maxBytes     int64
	currentBytes int64
}

// NewResponseCache builds a cache with the provided capacity and byte budget.
func NewResponseCache(capacity int, maxBytes int64) (*ResponseCache, error) {
	if capacity <= 0 {
		return nil, errors.New("cache: capacity must be positive")
	}
	store, err := lru.New[string, *Entry](capacity)
	if err != nil {
		return nil, err
	}
	return &ResponseCache{store: store, maxBytes: maxBytes}, nil
}

// Store records the entry under key, evicting least recently used entries when budget is exceeded.
func (c *ResponseCache) Store(key string, entry *Entry) {
	if entry == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.store.Peek(key); ok {
		c.currentBytes -= existing.Size
	}

	c.store.Add(key, entry)
	c.currentBytes += entry.Size

	if c.maxBytes > 0 {
		for c.currentBytes > c.maxBytes {
			k, evicted, ok := c.store.RemoveOldest()
			if !ok {
				break
			}
			if evicted != nil {
				c.currentBytes -= evicted.Size
			}
			if k == key {
				break
			}
		}
	}
}

// Lookup retrieves a fresh copy if the entry exists and has not expired.
func (c *ResponseCache) Lookup(key string, now time.Time) (*Entry, bool) {
	c.mu.RLock()
	entry, ok := c.store.Get(key)
	c.mu.RUnlock()
	if !ok || entry == nil {
		return nil, false
	}
	if !entry.ExpiresAt.IsZero() && entry.ExpiresAt.Before(now) {
		c.Delete(key)
		return nil, false
	}
	return entry.Clone(), true
}

// Delete removes the key if present.
func (c *ResponseCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.store.Peek(key); ok {
		c.currentBytes -= existing.Size
	}
	c.store.Remove(key)
}

// Purge clears all entries.
func (c *ResponseCache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.store.Purge()
	c.currentBytes = 0
}

// ByteSize returns the approximate total size currently stored.
func (c *ResponseCache) ByteSize() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentBytes
}

// BuildEntry hydrates an Entry from an HTTP response body, ensuring byte limits are respected.
func BuildEntry(resp *http.Response, body []byte, ttl time.Duration) *Entry {
	if resp == nil {
		return nil
	}
	entry := &Entry{
		Status:   resp.StatusCode,
		Header:   cloneHeader(resp.Header),
		Body:     append([]byte(nil), body...),
		StoredAt: time.Now(),
		Size:     int64(len(body)),
	}
	if ttl > 0 {
		entry.ExpiresAt = entry.StoredAt.Add(ttl)
	}
	return entry
}

func ReadAndBuffer(resp *http.Response, limit int64) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := resp.Body.Close(); err != nil {
		return nil, err
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, errors.New("cache: body exceeds limit")
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	return data, nil
}
