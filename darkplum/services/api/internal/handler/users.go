package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/scalecraft/darkplum/api/internal/activity"
	"github.com/scalecraft/darkplum/api/internal/auth"
)

type UserHandler struct {
	db       *pgxpool.Pool
	activity *activity.Recorder
	pepper   []byte
}

func NewUserHandler(db *pgxpool.Pool, rec *activity.Recorder, pepper []byte) *UserHandler {
	return &UserHandler{db: db, activity: rec, pepper: pepper}
}

type User struct {
	ID         string    `json:"id"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	CreatedAt  time.Time `json:"created_at"`
	AuthSource string    `json:"auth_source"`
}

type createUserRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Name     string `json:"name"  binding:"required"`
	Password string `json:"password,omitempty"`
	// Role is optional; empty defaults to 'reader' (the users.role column's
	// own DEFAULT, migration 003_user_roles.sql). Present so an admin can
	// create a fellow admin directly instead of a two-step create-then-
	// PATCH-role dance.
	Role string `json:"role,omitempty"`
}

// CreateUser handles POST /api/v1/users.
func (h *UserHandler) CreateUser(c *gin.Context) {
	var req createUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Role != "" && req.Role != "admin" && req.Role != "reader" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be 'admin' or 'reader'"})
		return
	}

	var passwordHash *string
	if req.Password != "" {
		if len(req.Password) < 8 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "password must be at least 8 characters"})
			return
		}
		raw, err := bcrypt.GenerateFromPassword(auth.PepperPassword([]byte(req.Password), h.pepper), 12)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "password hashing failed"})
			return
		}
		hashStr := string(raw)
		passwordHash = &hashStr
	}

	role := req.Role
	if role == "" {
		role = "reader"
	}

	// $3 is cast to ::text explicitly: with no password (the SSO-invited-user
	// case, req.Password == "" so passwordHash is a nil *string), $3 is used
	// both as a plain value and inside "CASE WHEN $3 IS NOT NULL" -- neither
	// occurrence gives Postgres enough context to infer a type for an
	// untyped NULL, and it fails closed with "could not determine data type
	// of parameter $3" (42P08) rather than silently guessing. This was never
	// exercised by a test before the routing-policy work added
	// TestCreateUser_OptionalRole, which creates a passwordless user.
	var u User
	err := h.db.QueryRow(c.Request.Context(), `
		INSERT INTO users (email, name, password_hash, auth_provider, role)
		VALUES ($1, $2, $3::text, CASE WHEN $3::text IS NOT NULL THEN 'local' ELSE NULL END, $4)
		RETURNING id, email, name, role, created_at
	`, req.Email, req.Name, passwordHash, role).Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "email already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create user failed"})
		return
	}

	evt := activity.FromContext(c, activity.UserCreated)
	evt.TargetType = activity.StrPtr("user")
	evt.TargetID = activity.StrPtr(u.ID)
	evt.Detail = map[string]any{"email": u.Email, "name": u.Name}
	h.activity.Record(c.Request.Context(), evt)

	c.JSON(http.StatusCreated, u)
}

// ListUsers handles GET /api/v1/users.
func (h *UserHandler) ListUsers(c *gin.Context) {
	rows, err := h.db.Query(c.Request.Context(), `
		SELECT id, email, name, role, created_at,
			COALESCE(auth_provider, CASE WHEN oidc_subject IS NOT NULL THEN 'sso' ELSE 'manual' END) AS auth_source
		FROM users
		ORDER BY created_at DESC
	`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list users failed"})
		return
	}
	defer rows.Close()

	users := make([]User, 0)
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.CreatedAt, &u.AuthSource); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan user failed"})
			return
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	c.JSON(http.StatusOK, users)
}

type updateRoleRequest struct {
	Role string `json:"role" binding:"required"`
}

// UpdateRole handles PATCH /api/v1/users/:id/role.
func (h *UserHandler) UpdateRole(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}

	var req updateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Role != "admin" && req.Role != "reader" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be 'admin' or 'reader'"})
		return
	}

	callerID, _ := c.Get("user_id")
	if callerID == id {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "cannot change your own role"})
		return
	}

	tag, err := h.db.Exec(c.Request.Context(),
		"UPDATE users SET role = $1 WHERE id = $2", req.Role, id,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update role failed"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	auth.InvalidateUser(id)

	evt := activity.FromContext(c, activity.UserRoleChanged)
	evt.TargetType = activity.StrPtr("user")
	evt.TargetID = activity.StrPtr(id)
	evt.Detail = map[string]any{"role": req.Role}
	h.activity.Record(c.Request.Context(), evt)

	c.JSON(http.StatusOK, gin.H{"role": req.Role})
}

type resetPasswordRequest struct {
	Password string `json:"password" binding:"required"`
}

// ResetPassword handles PATCH /api/v1/users/:id/password (admin only).
func (h *UserHandler) ResetPassword(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}

	var req resetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.Password) < 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password must be at least 8 characters"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword(auth.PepperPassword([]byte(req.Password), h.pepper), 12)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "password hashing failed"})
		return
	}

	tag, err := h.db.Exec(c.Request.Context(),
		`UPDATE users SET password_hash = $1 WHERE id = $2 AND (auth_provider = 'local' OR auth_provider IS NULL)`,
		string(hash), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "reset password failed"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found or uses SSO authentication"})
		return
	}

	auth.InvalidateUser(id)

	evt := activity.FromContext(c, activity.UserPasswordReset)
	evt.TargetType = activity.StrPtr("user")
	evt.TargetID = activity.StrPtr(id)
	h.activity.Record(c.Request.Context(), evt)

	c.JSON(http.StatusOK, gin.H{"message": "password reset"})
}

// DeleteUser handles DELETE /api/v1/users/:id.
func (h *UserHandler) DeleteUser(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}

	tag, err := h.db.Exec(c.Request.Context(),
		"DELETE FROM users WHERE id = $1", id,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete user failed"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	auth.InvalidateUser(id)

	evt := activity.FromContext(c, activity.UserDeleted)
	evt.TargetType = activity.StrPtr("user")
	evt.TargetID = activity.StrPtr(id)
	h.activity.Record(c.Request.Context(), evt)

	c.Status(http.StatusNoContent)
}
