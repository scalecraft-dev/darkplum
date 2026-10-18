package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newRLRouter(mw gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/x", mw, func(c *gin.Context) { c.Status(http.StatusOK) })
	// /fail behaves like a wrong password: the account bucket is charged.
	r.POST("/fail", mw, func(c *gin.Context) { DebitLoginAccount(c); c.Status(http.StatusUnauthorized) })
	return r
}

// TestLoginRateLimit_Unchanged proves LoginRateLimit's externally-visible
// behavior: burst requests succeed, the next is refused with the plain
// error body the login form shows, and a Retry-After tells the form how
// long to disable the button.
func TestLoginRateLimit_Unchanged(t *testing.T) {
	r := newRLRouter(LoginRateLimit(0.2, 2))

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("expected a Retry-After header on the login limiter")
	}
	if got := rec.Body.String(); got != `{"error":"too many login attempts, try again later"}` {
		t.Fatalf("body = %s", got)
	}

	// A different client IP is not affected by the first IP's bucket.
	req2 := httptest.NewRequest(http.MethodPost, "/x", nil)
	req2.RemoteAddr = "10.0.0.2:1234"
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("different IP: status = %d, want 200", rec2.Code)
	}
}

// TestLoginRateLimit_PerAccount proves the second bucket: the same email
// attempted from many different client addresses is throttled once the
// account's burst is spent, so a brute force cannot sidestep the per-IP
// bucket by rotating (or forging) its source address.
func TestLoginRateLimit_PerAccount(t *testing.T) {
	r := newRLRouter(LoginRateLimit(100, 100)) // per-IP effectively off
	attempt := func(path, ip, email string) int {
		body := strings.NewReader(`{"email":"` + email + `","password":"x"}`)
		req := httptest.NewRequest(http.MethodPost, path, body)
		req.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	// accountBurst failed guesses from rotating addresses are allowed...
	for i := 0; i < accountBurst; i++ {
		if code := attempt("/fail", "10.0.0."+strconv.Itoa(i), "Victim@Example.com"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, code)
		}
	}
	// ...the next, from yet another address and with the email in another
	// case, is refused before the handler runs.
	if code := attempt("/fail", "10.0.0.99", "victim@example.com"); code != http.StatusTooManyRequests {
		t.Fatalf("same account from a new IP: status = %d, want 429", code)
	}
	// A different account from that same address is unaffected.
	if code := attempt("/fail", "10.0.0.99", "other@example.com"); code != http.StatusUnauthorized {
		t.Fatalf("other account: status = %d, want 401", code)
	}
	// Successful attempts never charge the account bucket: the rightful
	// owner logging in repeatedly is not throttled.
	for i := 0; i < accountBurst*3; i++ {
		if code := attempt("/x", "10.0.0.50", "owner@example.com"); code != http.StatusOK {
			t.Fatalf("successful login %d: status = %d, want 200", i, code)
		}
	}
	// Padding the body past the peek limit must not un-key the account
	// bucket: it is refused, not waved through.
	padded := strings.NewReader(`{"email":"victim@example.com","password":"x"}` + strings.Repeat(" ", loginBodyPeekLimit))
	req := httptest.NewRequest(http.MethodPost, "/x", padded)
	req.RemoteAddr = "10.0.0.10:1234"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("padded body: status = %d, want 413", rec.Code)
	}
}

// TestKeyedRateLimit_PerKeyIsolationAndRetryAfter covers the two things
// providers.go depends on: independent buckets per key (so one admin
// hammering the vendor-key routes doesn't rate-limit a different admin, or
// the reverse for an org-wide key), and a Retry-After header on refusal.
func TestKeyedRateLimit_PerKeyIsolationAndRetryAfter(t *testing.T) {
	r := newRLRouter(KeyedRateLimit(1.0/60.0, 1, func(c *gin.Context) string {
		return c.GetHeader("X-Actor")
	}))

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("X-Actor", "user-a")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request for user-a: status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("X-Actor", "user-a")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request for user-a: status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header on the keyed limiter")
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error to be an object, got %T", body["error"])
	}
	if errObj["code"] != "rate_limited" {
		t.Fatalf("code = %v, want rate_limited", errObj["code"])
	}

	// A different key is unaffected.
	req = httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("X-Actor", "user-b")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("user-b: status = %d, want 200", rec.Code)
	}
}

// TestKeyedRateLimit_EmptyKeyNeverLimited covers the documented escape hatch:
// a request whose keyFunc returns "" always proceeds.
func TestKeyedRateLimit_EmptyKeyNeverLimited(t *testing.T) {
	r := newRLRouter(KeyedRateLimit(1.0/60.0, 1, func(c *gin.Context) string { return "" }))

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
	}
}
