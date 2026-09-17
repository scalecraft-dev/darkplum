package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	ContextKeyID  = "key_id"
	ContextUserID = "user_id"
)

// apiKeyRow holds the columns we care about from the api_keys / service_keys lookup.
type apiKeyRow struct {
	ID      string
	UserID  string
	KeyType string
}

// APIKey is a Gin middleware that validates Bearer tokens against the api_keys table.
// It sets "key_id" and "user_id" on the Gin context for downstream handlers.
// On failure it aborts with 401 and an OpenAI-compatible error envelope.
func APIKey(db *pgxpool.Pool) gin.HandlerFunc {
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
			// Treat any DB error — including pgx.ErrNoRows — as unauthorized.
			// We intentionally don't distinguish "not found" from "db error" to
			// avoid leaking infrastructure state to unauthenticated callers.
			abortUnauthorized(c)
			return
		}

		c.Set(ContextKeyID, row.ID)
		c.Set(ContextUserID, row.UserID)
		c.Next()
	}
}

// hashKey returns the lowercase hex SHA-256 of the raw API key string.
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// lookupKey queries api_keys and service_keys for a non-revoked row matching
// keyHash. A row is valid when revoked_at IS NULL OR revoked_at is in the
// future — the latter supports the service-key rotation grace window
// (BootstrapServiceKey supersedes the previous "_service" row with
// revoked_at = now() + 10 minutes rather than revoking it immediately, so
// overlapping pod rollouts on the old key don't get 401s mid-rotation).
// For service keys, UserID is empty. Returns an error (including
// pgx.ErrNoRows) if no matching row is found.
func lookupKey(ctx context.Context, db *pgxpool.Pool, keyHash string) (*apiKeyRow, error) {
	const q = `
		SELECT id::text, user_id::text, 'user' AS key_type
		  FROM api_keys WHERE key_hash = $1 AND (revoked_at IS NULL OR revoked_at > now())
		UNION ALL
		SELECT id::text, ''::text, 'service' AS key_type
		  FROM service_keys WHERE key_hash = $1 AND (revoked_at IS NULL OR revoked_at > now())
		LIMIT 1`

	row := &apiKeyRow{}
	err := db.QueryRow(ctx, q, keyHash).Scan(&row.ID, &row.UserID, &row.KeyType)
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
