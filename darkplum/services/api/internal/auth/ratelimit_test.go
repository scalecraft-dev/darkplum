package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func newRLRouter(mw gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/x", mw, func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

// TestLoginRateLimit_Unchanged proves LoginRateLimit's externally-visible
// behavior survived being rebuilt on the shared rateLimiter: burst requests
// succeed, the next is refused with the original plain error body, and no
// Retry-After header is added (login behavior is explicitly not to change).
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
	if rec.Header().Get("Retry-After") != "" {
		t.Fatalf("expected no Retry-After header on the login limiter, got %q", rec.Header().Get("Retry-After"))
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
