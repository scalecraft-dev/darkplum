package auth

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
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

// loginBodyPeekLimit bounds how much of a login body the limiter reads to
// find the email. A real login body is a few hundred bytes.
const loginBodyPeekLimit = 64 * 1024

// Per-account bucket: five failed guesses, then one every five seconds,
// whatever addresses they come from.
const (
	accountRate  = 0.2
	accountBurst = 5
)

// LoginRateLimit returns middleware that limits login attempts per client IP
// AND per target account (the lower-cased email in the JSON body). rate is
// tokens added per second; burst is the max tokens (concurrent attempts
// allowed). Two buckets, because neither alone is enough: the IP is whatever
// the first proxy saw and can be shared by a whole office (or, with a
// spoofable X-Forwarded-For, chosen by the attacker), while the account
// bucket is what actually stops an online brute force against one user no
// matter where the attempts come from. A limited attempt gets a Retry-After.
func LoginRateLimit(rate float64, burst int) gin.HandlerFunc {
	byIP := newRateLimiter(rate, burst)
	byAccount := newRateLimiter(accountRate, accountBurst)

	return func(c *gin.Context) {
		if ok, retry := byIP.allow(c.ClientIP()); !ok {
			c.Header("Retry-After", strconv.Itoa(retry))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "too many login attempts, try again later",
			})
			return
		}
		email, tooLarge := peekLoginEmail(c)
		if tooLarge {
			// A body too big to inspect must not become a body that skips
			// the account bucket (padding past the peek limit would do
			// exactly that, since the JSON decoder stops at the first
			// complete object). A real login body is a few hundred bytes.
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": "login request body too large",
			})
			return
		}
		if email != "" {
			key := "email:" + email
			ok, retry := byAccount.peek(key)
			if !ok {
				c.Header("Retry-After", strconv.Itoa(retry))
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
					"error": "too many login attempts, try again later",
				})
				return
			}
			// The account bucket is charged only for a FAILED attempt
			// (the handler calls DebitLoginAccount on 401), so a stranger
			// sending the admin's email every few seconds cannot keep the
			// admin's own successful logins out.
			c.Set(loginDebitKey, func() { byAccount.consume(key) })
		}
		c.Next()
	}
}

// loginDebitKey is the gin context key under which LoginRateLimit leaves the
// closure that charges the per-account bucket.
const loginDebitKey = "login_account_debit"

// DebitLoginAccount charges the per-account login bucket for a failed
// attempt. Called by the login handler on a 401; a no-op when the request
// did not pass through LoginRateLimit or carried no email.
func DebitLoginAccount(c *gin.Context) {
	if f, ok := c.Get(loginDebitKey); ok {
		if debit, ok := f.(func()); ok {
			debit()
		}
	}
}

// peek reports whether key has a token available, without taking one.
func (rl *rateLimiter) peek(key string) (ok bool, retryAfterSeconds int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, found := rl.buckets[key]
	if !found {
		return true, 0
	}
	now := time.Now()
	tokens := b.tokens + now.Sub(b.last).Seconds()*rl.rate
	if tokens > float64(rl.burst) {
		tokens = float64(rl.burst)
	}
	if tokens < 1 {
		return false, int((1-tokens)/rl.rate) + 1
	}
	return true, 0
}

// consume takes one token from key (creating the bucket if needed); the
// bucket may go to zero but never below.
func (rl *rateLimiter) consume(key string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, found := rl.buckets[key]
	if !found {
		b = &bucket{tokens: float64(rl.burst), last: time.Now()}
		rl.buckets[key] = b
	}
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * rl.rate
	if b.tokens > float64(rl.burst) {
		b.tokens = float64(rl.burst)
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
	} else {
		b.tokens = 0
	}
}

// peekLoginEmail reads the request body to find a JSON "email" member,
// lower-cased and trimmed, and puts the body back for the handler. email is
// "" when there is no body, it is not JSON, or it has no email -- the
// handler then rejects it on its own terms. tooLarge is true when the body
// exceeds loginBodyPeekLimit; the caller refuses such a request outright.
func peekLoginEmail(c *gin.Context) (email string, tooLarge bool) {
	if c.Request.Body == nil || c.Request.Method != http.MethodPost {
		return "", false
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, loginBodyPeekLimit+1))
	_ = c.Request.Body.Close()
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if len(raw) > loginBodyPeekLimit {
		return "", true
	}
	if err != nil {
		return "", false
	}
	var body struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(body.Email)), false
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
