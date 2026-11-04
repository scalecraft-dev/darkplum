package middleware

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft-dev/darkplum/gateway/internal/recorder"
)

// TestResolveAttributedUserID_ServiceKeyHonoursHeader mirrors the api
// service's proxy path: it authenticates with one shared service key on
// behalf of many session users, so the session user id must come from the
// header it sets.
func TestResolveAttributedUserID_ServiceKeyHonoursHeader(t *testing.T) {
	got := resolveAttributedUserID(KeyTypeService, "", "session-user-42")
	if got != "session-user-42" {
		t.Errorf("resolveAttributedUserID = %q, want header value for a service key", got)
	}
}

// TestResolveAttributedUserID_UserKeyIgnoresHeader is the audit-spoofing
// regression guard: a caller authenticated with their OWN user API key must
// never be able to attribute their traffic to a different user_id by setting
// X-Darkplum-User-Id on the request themselves.
func TestResolveAttributedUserID_UserKeyIgnoresHeader(t *testing.T) {
	got := resolveAttributedUserID(KeyTypeUser, "victim-user-1", "attacker-controlled-header-value")
	if got != "victim-user-1" {
		t.Errorf("resolveAttributedUserID = %q, want the key's own user_id (victim-user-1), header must be ignored for a user key", got)
	}
}

func TestResolveAttributedUserID_ServiceKeyNoHeaderFallsBackToKeyUserID(t *testing.T) {
	got := resolveAttributedUserID(KeyTypeService, "", "")
	if got != "" {
		t.Errorf("resolveAttributedUserID = %q, want empty when a service key request carries no attribution header", got)
	}
}

func TestResolveAttributedUserID_UnknownKeyTypeIgnoresHeader(t *testing.T) {
	// Defensive: an empty/unrecognised key_type (e.g. context key never set)
	// must not be treated as trusted, only the explicit "service" type is.
	got := resolveAttributedUserID("", "user-1", "someone-else")
	if got != "user-1" {
		t.Errorf("resolveAttributedUserID = %q, want the key's own user_id for a non-service key_type", got)
	}
}

func TestCacheTokenPointers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		return c
	}
	one := 1

	// No usage reported: NULL.
	if r, cc := cacheTokenPointers(newCtx(), nil, nil); r != nil || cc != nil {
		t.Fatalf("no usage: got %v/%v, want nil/nil", r, cc)
	}
	// Usage without cache fields: 0, not NULL.
	if r, cc := cacheTokenPointers(newCtx(), &one, &one); r == nil || cc == nil || *r != 0 || *cc != 0 {
		t.Fatalf("usage w/o cache: got %v/%v, want 0/0", r, cc)
	}
	// Reported split passes through.
	c := newCtx()
	c.Set("cache_read_tokens", 60)
	c.Set("cache_creation_tokens", 30)
	if r, cc := cacheTokenPointers(c, &one, nil); r == nil || cc == nil || *r != 60 || *cc != 30 {
		t.Fatalf("split: got %v/%v, want 60/30", r, cc)
	}
}

// Recording buffers the body to read model/stream; it must not buffer an
// unbounded one. A body over the cap reaches the handler as a read that fails
// with *http.MaxBytesError (so the handler answers 413 request_too_large as
// it does for a body it reads itself), never as a silently truncated body; a
// body exactly at the cap arrives whole.
func TestRecording_BodyCap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := recorder.New(nil)
	rec.Drain() // drained: Record drops events, nothing touches the nil pool

	run := func(t *testing.T, n int) (got int, readErr error) {
		t.Helper()
		r := gin.New()
		r.POST("/v1/chat/completions", Recording(rec), func(c *gin.Context) {
			b, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, MaxChatBodyBytes))
			got, readErr = len(b), err
			c.Status(http.StatusOK)
		})
		body := append([]byte(`{"model":"m","stream":true,"pad":"`), bytes.Repeat([]byte("a"), n)...)
		body = body[:n]
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		r.ServeHTTP(w, req)
		return got, readErr
	}

	got, err := run(t, MaxChatBodyBytes)
	if err != nil || got != MaxChatBodyBytes {
		t.Errorf("body at the cap: read %d bytes, err %v; want the whole body", got, err)
	}
	got, err = run(t, MaxChatBodyBytes+1)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Errorf("body over the cap: read %d bytes, err %v; want *http.MaxBytesError", got, err)
	}
	if got != 0 {
		t.Errorf("body over the cap: handler saw %d bytes of a truncated body", got)
	}
}
