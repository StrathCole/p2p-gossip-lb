package proxy

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type clientRateLimiter struct {
	mu      sync.Mutex
	limit   rate.Limit
	burst   int
	window  time.Duration
	clients map[string]*clientLimiter
}

type clientLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newClientRateLimiter(perSecond, burst int, window time.Duration) *clientRateLimiter {
	return &clientRateLimiter{
		limit:   rate.Limit(perSecond),
		burst:   burst,
		window:  window,
		clients: make(map[string]*clientLimiter),
	}
}

func (c *clientRateLimiter) Allow(ip string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.clients[ip]
	if !ok {
		entry = &clientLimiter{limiter: rate.NewLimiter(c.limit, c.burst)}
		c.clients[ip] = entry
	}
	entry.lastSeen = time.Now()

	allowed := entry.limiter.Allow()
	if len(c.clients) > 2048 {
		c.cleanup()
	}
	return allowed
}

func (c *clientRateLimiter) cleanup() {
	threshold := time.Now().Add(-c.window)
	for ip, entry := range c.clients {
		if entry.lastSeen.Before(threshold) {
			delete(c.clients, ip)
		}
	}
}
