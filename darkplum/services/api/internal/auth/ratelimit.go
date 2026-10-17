package auth

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type bucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter is a token-bucket limiter keyed by an arbitrary string,
// shared by LoginRateLimit (keyed by client IP, unchanged behavior) and
// KeyedRateLimit (keyed by whatever the caller's keyFunc returns).
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   int
}

// newRateLimiter starts a rateLimiter with rate tokens/second and the given
// burst, and a background goroutine that evicts buckets untouched for 30
// minutes every 10 minutes -- unchanged from LoginRateLimit's original
// cleanup cadence.
func newRateLimiter(rate float64, burst int) *rateLimiter {
	rl := &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
	}

	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			rl.mu.Lock()
			cutoff := time.Now().Add(-30 * time.Minute)
			for key, b := range rl.buckets {
				if b.last.Before(cutoff) {
					delete(rl.buckets, key)
				}
			}
			rl.mu.Unlock()
		}
	}()

	return rl
}

// allow reports whether key may proceed now. When it may not, retryAfter is
// the number of whole seconds (rounded up) until a token will be available,
// for a Retry-After header.
func (rl *rateLimiter) allow(key string) (ok bool, retryAfterSeconds int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(rl.burst), last: time.Now()}
		rl.buckets[key] = b
	}

	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * rl.rate
	if b.tokens > float64(rl.burst) {
		b.tokens = float64(rl.burst)
	}
	b.last = now

	if b.tokens < 1 {
		wait := (1 - b.tokens) / rl.rate
		return false, int(wait) + 1
	}
	b.tokens--
	return true, 0
}

// LoginRateLimit returns middleware that limits login attempts per client IP.
// rate is tokens added per second; burst is the max tokens (concurrent attempts allowed).
func LoginRateLimit(rate float64, burst int) gin.HandlerFunc {
	rl := newRateLimiter(rate, burst)

	return func(c *gin.Context) {
		if ok, _ := rl.allow(c.ClientIP()); !ok {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "too many login attempts, try again later",
			})
			return
		}
		c.Next()
	}
}

// KeyedRateLimit returns middleware limiting requests to rate tokens/second
// (burst concurrent) per key, where key is computed per-request by keyFunc
// -- e.g. the session actor id, or a fixed constant to rate-limit an entire
// route org-wide regardless of which actor calls it. A request whose keyFunc
// returns "" is never limited (used when a route can be reached without the
// identity a keyFunc depends on, though in practice every caller here runs
// behind session auth).
//
// Unlike LoginRateLimit, a limited request gets a Retry-After header and a
// structured {"error":{"code":"rate_limited","message":...}} body, since
// this backs machine-consumed admin write endpoints (PLAN-UI's vendor key
// install routes) rather than a login form.
func KeyedRateLimit(rate float64, burst int, keyFunc func(*gin.Context) string) gin.HandlerFunc {
	rl := newRateLimiter(rate, burst)

	return func(c *gin.Context) {
		key := keyFunc(c)
		if key == "" {
			c.Next()
			return
		}
		ok, retryAfter := rl.allow(key)
		if !ok {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": gin.H{
					"code":    "rate_limited",
					"message": "too many requests, try again later",
				},
			})
			return
		}
		c.Next()
	}
}
