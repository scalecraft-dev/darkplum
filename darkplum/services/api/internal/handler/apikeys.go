package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
)

const base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// base62Encode encodes n random bytes into a base62 string of length chars.
// Uses crypto/rand for each character selection — no math/rand.
func base62Encode(length int) (string, error) {
	result := make([]byte, length)
	base := big.NewInt(int64(len(base62Chars)))
	for i := range result {
		n, err := rand.Int(rand.Reader, base)
		if err != nil {
			return "", fmt.Errorf("rand.Int: %w", err)
		}
		result[i] = base62Chars[n.Int64()]
	}
	return string(result), nil
}

// generateAPIKey produces a "dp_" + 43-char base62 key (~256 bits of entropy).
func generateAPIKey() (key, prefix, hash string, err error) {
	body, err := base62Encode(43)
	if err != nil {
		return "", "", "", err
	}
	key = "dp_" + body
	prefix = key[:11] // "dp_" + first 8 chars
	sum := sha256.Sum256([]byte(key))
	hash = hex.EncodeToString(sum[:])
	return key, prefix, hash, nil
}

type APIKeyHandler struct {
	db       *pgxpool.Pool
	activity *activity.Recorder
}

func NewAPIKeyHandler(db *pgxpool.Pool, rec *activity.Recorder) *APIKeyHandler {
	return &APIKeyHandler{db: db, activity: rec}
}

// APIKey is the list/response shape — no full key field.
type APIKey struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	UserName  string     `json:"user_name"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// APIKeyCreated is the creation response — includes the full key, once only.
type APIKeyCreated struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
}

type createAPIKeyRequest struct {
	UserID string `json:"user_id" binding:"required"`
	Name   string `json:"name"    binding:"required"`
}

// CreateAPIKey handles POST /api/v1/api-keys.
func (h *APIKeyHandler) CreateAPIKey(c *gin.Context) {
	var req createAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !isValidUUID(req.UserID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
		return
	}

	key, prefix, hash, err := generateAPIKey()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "key generation failed"})
		return
	}

	var resp APIKeyCreated
	err = h.db.QueryRow(c.Request.Context(), `
		INSERT INTO api_keys (user_id, name, prefix, key_hash)
		VALUES ($1, $2, $3, $4)
		RETURNING id, user_id, name, prefix, created_at
	`, req.UserID, req.Name, prefix, hash).Scan(
		&resp.ID, &resp.UserID, &resp.Name, &resp.Prefix, &resp.CreatedAt,
	)
	if err != nil {
		if isForeignKeyViolation(err) {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create api key failed"})
		return
	}

	// Attach the full key — only time we return it.
	resp.Key = key

	evt := activity.FromContext(c, activity.APIKeyCreated)
	evt.TargetType = activity.StrPtr("apikey")
	evt.TargetID = activity.StrPtr(resp.ID)
	evt.Detail = map[string]any{"name": resp.Name, "prefix": resp.Prefix, "user_id": resp.UserID}
	h.activity.Record(c.Request.Context(), evt)

	c.JSON(http.StatusCreated, resp)
}

// ListAPIKeys handles GET /api/v1/api-keys.
func (h *APIKeyHandler) ListAPIKeys(c *gin.Context) {
	rows, err := h.db.Query(c.Request.Context(), `
		SELECT
			k.id,
			k.user_id,
			u.name  AS user_name,
			k.name,
			k.prefix,
			k.created_at,
			k.revoked_at
		FROM api_keys k
		JOIN users u ON u.id = k.user_id
		ORDER BY k.created_at DESC
	`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list api keys failed"})
		return
	}
	defer rows.Close()

	keys := make([]APIKey, 0)
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(
			&k.ID, &k.UserID, &k.UserName, &k.Name,
			&k.Prefix, &k.CreatedAt, &k.RevokedAt,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan api key failed"})
			return
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	c.JSON(http.StatusOK, keys)
}

// RevokeAPIKey handles DELETE /api/v1/api-keys/:id.
func (h *APIKeyHandler) RevokeAPIKey(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid api key id"})
		return
	}

	tag, err := h.db.Exec(c.Request.Context(), `
		UPDATE api_keys
		SET revoked_at = now()
		WHERE id = $1
		  AND revoked_at IS NULL
	`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "revoke api key failed"})
		return
	}
	if tag.RowsAffected() == 0 {
		// Either not found or already revoked — both are 404 from the caller's view.
		c.JSON(http.StatusNotFound, gin.H{"error": "api key not found or already revoked"})
		return
	}

	evt := activity.FromContext(c, activity.APIKeyRevoked)
	evt.TargetType = activity.StrPtr("apikey")
	evt.TargetID = activity.StrPtr(id)
	h.activity.Record(c.Request.Context(), evt)

	c.Status(http.StatusNoContent)
}
