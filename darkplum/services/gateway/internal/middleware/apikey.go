package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	ContextKeyID   = "key_id"
	ContextUserID  = "user_id"
	ContextKeyType = "key_type"
)

// Key types stored under ContextKeyType. A "service" key is the api service's
// bootstrapped credential used to proxy session-authenticated user traffic
// (see BootstrapServiceKey); a "user" key is issued directly to an end user.
const (
	KeyTypeUser    = "user"
	KeyTypeService = "service"
)

// apiKeyRow holds the columns we care about from the api_keys / service_keys lookup.
type apiKeyRow struct {
	ID      string
	UserID  string
	KeyType string
}

// dbQuerier is the subset of *pgxpool.Pool that lookupKey needs. Defined as an
// interface so tests can substitute a fake and exercise the full middleware
// (including the 401-vs-503 error classification below) without a real
// database connection.
type dbQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// APIKey is a Gin middleware that validates Bearer tokens against the api_keys
// table. It sets "key_id", "user_id", and "key_type" on the Gin context for
// downstream handlers. On failure it aborts with 401 (key not found/revoked)
// or 503 (the lookup itself failed, e.g. a database outage — see
// apiKeyWithQuerier) and an OpenAI-compatible error envelope.
func APIKey(db *pgxpool.Pool) gin.HandlerFunc {
	return apiKeyWithQuerier(db)
}

// apiKeyWithQuerier is the testable core of APIKey, parameterised over the
// dbQuerier seam.
func apiKeyWithQuerier(db dbQuerier) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			abortUnauthorized(c)
			return
		}

		// Require exactly "Bearer <token>" — no other schemes accepted.
		const prefix = "Bearer "
		if !strings.HasPrefix(authHeader, prefix) {
			abortUnauthorized(c)
			return
		}

		rawKey := strings.TrimPrefix(authHeader, prefix)
		if rawKey == "" {
			abortUnauthorized(c)
			return
		}

		hash := hashKey(rawKey)

		row, err := lookupKey(c.Request.Context(), db, hash)
		if err != nil {
			// pgx.ErrNoRows is a genuine authentication decision: no
			// non-revoked key matches this hash -> 401. Any other error
			// (connection refused, timeout, pool exhaustion, ...) is an
			// infrastructure failure, not an authentication decision — a
			// Postgres outage must not present to every caller as "Invalid
			// API key", which would look like an org-wide credential
			// incident rather than the DB problem it actually is. Fail
			// closed (the request is still refused) but report it honestly
			// as 503, and log the underlying error server-side only; the
			// response body stays generic so unauthenticated callers learn
			// nothing about the infrastructure beyond "try again".
			if errors.Is(err, pgx.ErrNoRows) {
				abortUnauthorized(c)
				return
			}
			slog.Error("apikey: lookup failed", "err", err)
			abortAuthUnavailable(c)
			return
		}

		c.Set(ContextKeyID, row.ID)
		c.Set(ContextUserID, row.UserID)
		c.Set(ContextKeyType, row.KeyType)
		c.Next()
	}
}

// hashKey returns the lowercase hex SHA-256 of the raw API key string.
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// lookupKeyQuery is the two-branch UNION lookup against api_keys and
// service_keys. The two branches deliberately use DIFFERENT revocation
// conditions:
//
//   - api_keys: revoked_at IS NULL only. A user key's revocation must be
//     immediate — a revoked user credential must stop working on the next
//     request, not up to 10 minutes later. This also keeps the partial index
//     on api_keys (WHERE revoked_at IS NULL) usable for this query.
//   - service_keys: revoked_at IS NULL OR revoked_at > now(). This is the
//     rotation grace window: BootstrapServiceKey supersedes the previous
//     "_service" row with revoked_at = now() + 10 minutes rather than
//     revoking it immediately, so overlapping pod rollouts on the old key
//     don't get 401s mid-rotation.
//
// For service keys, UserID is empty.
const lookupKeyQuery = `
	SELECT id::text, user_id::text, 'user' AS key_type
	  FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL
	UNION ALL
	SELECT id::text, ''::text, 'service' AS key_type
	  FROM service_keys WHERE key_hash = $1 AND (revoked_at IS NULL OR revoked_at > now())
	LIMIT 1`

// lookupKey queries api_keys and service_keys for a non-revoked row matching
// keyHash, using the per-table revocation rules documented on lookupKeyQuery.
// Returns an error (including pgx.ErrNoRows) if no matching row is found.
func lookupKey(ctx context.Context, db dbQuerier, keyHash string) (*apiKeyRow, error) {
	row := &apiKeyRow{}
	err := db.QueryRow(ctx, lookupKeyQuery, keyHash).Scan(&row.ID, &row.UserID, &row.KeyType)
	if err != nil {
		return nil, err
	}
	return row, nil
}

// abortUnauthorized terminates the request with a 401 and OpenAI-compatible error body.
func abortUnauthorized(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error": gin.H{
			"message": "Invalid API key",
			"type":    "invalid_request_error",
		},
	})
}

// abortAuthUnavailable terminates the request with a 503 when the key lookup
// itself failed (not a "this key is invalid" decision). The message is
// generic and carries no infrastructure detail — a 503 with a generic body
// leaks nothing an attacker could use, and is the honest status for "we
// couldn't check" as opposed to "we checked and said no".
func abortAuthUnavailable(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"error": gin.H{
			"message": "authentication is temporarily unavailable; try again shortly",
			"type":    "service_unavailable",
			"code":    "auth_unavailable",
		},
	})
}
