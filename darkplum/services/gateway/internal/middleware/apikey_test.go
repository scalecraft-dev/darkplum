package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ---------------------------------------------------------------------------
// fakeRow / fakeQuerier — dbQuerier seam, no real database.
// ---------------------------------------------------------------------------

// fakeRow implements pgx.Row over a fixed set of string values, or returns a
// canned error from Scan (mirroring how pgx defers row errors to Scan).
type fakeRow struct {
	values []string
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return fmt.Errorf("fakeRow: expected %d scan targets, got %d", len(r.values), len(dest))
	}
	for i, d := range dest {
		p, ok := d.(*string)
		if !ok {
			return fmt.Errorf("fakeRow: unsupported scan target %T", d)
		}
		*p = r.values[i]
	}
	return nil
}

// fakeQuerier implements dbQuerier, returning a fixed row and recording the
// SQL text it was called with so tests can assert on the query shape without
// a database.
type fakeQuerier struct {
	row     fakeRow
	lastSQL string
	lastArg string
}

func (f *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.lastSQL = sql
	if len(args) > 0 {
		if s, ok := args[0].(string); ok {
			f.lastArg = s
		}
	}
	return f.row
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func doAuthedRequest(t *testing.T, h gin.HandlerFunc, authHeader string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	c.Request = req
	h(c)
	return w, c
}

// ---------------------------------------------------------------------------
// Missing/malformed Authorization header
// ---------------------------------------------------------------------------

func TestAPIKey_NoAuthHeader_401(t *testing.T) {
	q := &fakeQuerier{}
	w, _ := doAuthedRequest(t, apiKeyWithQuerier(q), "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAPIKey_WrongScheme_401(t *testing.T) {
	q := &fakeQuerier{}
	w, _ := doAuthedRequest(t, apiKeyWithQuerier(q), "Basic abc123")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Successful lookups — context keys, including key_type (item 3/4 requirement)
// ---------------------------------------------------------------------------

func TestAPIKey_ValidUserKey_SetsContext(t *testing.T) {
	q := &fakeQuerier{row: fakeRow{values: []string{"key-1", "user-1", KeyTypeUser}}}
	w, c := doAuthedRequest(t, apiKeyWithQuerier(q), "Bearer dp_abc")
	if c.IsAborted() {
		t.Fatalf("expected request not to be aborted, body=%s", w.Body.String())
	}
	if got := c.GetString(ContextKeyID); got != "key-1" {
		t.Errorf("key_id = %q, want key-1", got)
	}
	if got := c.GetString(ContextUserID); got != "user-1" {
		t.Errorf("user_id = %q, want user-1", got)
	}
	if got := c.GetString(ContextKeyType); got != KeyTypeUser {
		t.Errorf("key_type = %q, want %q", got, KeyTypeUser)
	}
}

func TestAPIKey_ValidServiceKey_SetsKeyTypeService(t *testing.T) {
	q := &fakeQuerier{row: fakeRow{values: []string{"svc-1", "", KeyTypeService}}}
	_, c := doAuthedRequest(t, apiKeyWithQuerier(q), "Bearer dp_service")
	if c.IsAborted() {
		t.Fatal("expected request not to be aborted")
	}
	if got := c.GetString(ContextKeyType); got != KeyTypeService {
		t.Errorf("key_type = %q, want %q", got, KeyTypeService)
	}
	if got := c.GetString(ContextUserID); got != "" {
		t.Errorf("user_id = %q, want empty for a service key", got)
	}
}

// ---------------------------------------------------------------------------
// Error classification: not-found -> 401, infrastructure failure -> 503.
// This is the item 11 regression guard: previously ANY error (including a
// connection failure) was reported as 401 "Invalid API key".
// ---------------------------------------------------------------------------

func TestAPIKey_KeyNotFound_401(t *testing.T) {
	q := &fakeQuerier{row: fakeRow{err: pgx.ErrNoRows}}
	w, c := doAuthedRequest(t, apiKeyWithQuerier(q), "Bearer dp_unknown")
	if !c.IsAborted() {
		t.Fatal("expected request to be aborted")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for pgx.ErrNoRows, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestAPIKey_DatabaseError_503AuthUnavailable(t *testing.T) {
	q := &fakeQuerier{row: fakeRow{err: errors.New("connection refused")}}
	w, c := doAuthedRequest(t, apiKeyWithQuerier(q), "Bearer dp_whatever")
	if !c.IsAborted() {
		t.Fatal("expected request to be aborted")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for a non-ErrNoRows lookup failure (a DB outage must not look like an invalid key), got %d; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "auth_unavailable") {
		t.Errorf("expected error code auth_unavailable in body, got %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "connection refused") {
		t.Error("the underlying DB error must never be leaked in the response body")
	}
}

// ---------------------------------------------------------------------------
// lookupKeyQuery shape — item 3: the grace window (revoked_at > now()) must
// apply ONLY to the service_keys branch. This is a regression guard against
// reverting to a single shared clause, testable without a database.
// ---------------------------------------------------------------------------

func TestLookupKeyQuery_GraceWindowAppliesOnlyToServiceKeys(t *testing.T) {
	q := &fakeQuerier{row: fakeRow{values: []string{"id", "user", "user"}}}
	if _, err := lookupKey(context.Background(), q, "hash"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	apiKeysClause := extractClause(t, q.lastSQL, "FROM api_keys WHERE key_hash = $1 AND ")
	serviceKeysClause := extractClause(t, q.lastSQL, "FROM service_keys WHERE key_hash = $1 AND ")

	if apiKeysClause != "revoked_at IS NULL" {
		t.Errorf("api_keys revocation clause = %q, want exactly %q (user key revocation must be immediate, no grace window)", apiKeysClause, "revoked_at IS NULL")
	}
	if serviceKeysClause != "(revoked_at IS NULL OR revoked_at > now())" {
		t.Errorf("service_keys revocation clause = %q, want the grace-window clause", serviceKeysClause)
	}
}

// extractClause returns the text between prefix and the next newline in sql,
// trimmed of surrounding whitespace.
func extractClause(t *testing.T, sql, prefix string) string {
	t.Helper()
	_, rest, found := strings.Cut(sql, prefix)
	if !found {
		t.Fatalf("query does not contain expected fragment %q; full query:\n%s", prefix, sql)
	}
	line, _, _ := strings.Cut(rest, "\n")
	return strings.TrimSpace(line)
}
