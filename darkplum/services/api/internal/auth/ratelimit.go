package auth

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type ipBucket struct {
	tokens float64
	last   time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ipBucket
	rate    float64
	burst   int
}

// LoginRateLimit returns middleware that limits login attempts per client IP.
// rate is tokens added per second; burst is the max tokens (concurrent attempts allowed).
func LoginRateLimit(rate float64, burst int) gin.HandlerFunc {
	rl := &rateLimiter{
		buckets: make(map[string]*ipBucket),
		rate:    rate,
		burst:   burst,
	}

	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			rl.mu.Lock()
			cutoff := time.Now().Add(-30 * time.Minute)
			for ip, b := range rl.buckets {
				if b.last.Before(cutoff) {
					delete(rl.buckets, ip)
				}
			}
			rl.mu.Unlock()
		}
	}()

	return func(c *gin.Context) {
		ip := c.ClientIP()

		rl.mu.Lock()
		b, ok := rl.buckets[ip]
		if !ok {
			b = &ipBucket{tokens: float64(burst), last: time.Now()}
			rl.buckets[ip] = b
		}

		now := time.Now()
		b.tokens += now.Sub(b.last).Seconds() * rate
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
		b.last = now

		if b.tokens < 1 {
			rl.mu.Unlock()
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "too many login attempts, try again later",
			})
			return
		}
		b.tokens--
		rl.mu.Unlock()
		c.Next()
	}
}
