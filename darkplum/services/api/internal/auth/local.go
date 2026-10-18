package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
)

const bcryptCost = 12

type LocalProvider struct {
	SessionManager
	Activity *activity.Recorder
	pepper   []byte
}

func NewLocalProvider(cfg *Config, db *pgxpool.Pool, rec *activity.Recorder) *LocalProvider {
	return &LocalProvider{
		SessionManager: SessionManager{
			DB:         db,
			Secret:     cfg.Secret,
			SecureCook: cfg.SecureCookie,
		},
		Activity: rec,
		pepper:   cfg.Pepper,
	}
}

func (p *LocalProvider) Logout(c *gin.Context) {
	p.SessionManager.HandleLogout(c)
	c.JSON(http.StatusOK, gin.H{"redirect": "/login"})
}

type loginRequest struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

func (p *LocalProvider) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "email and password are required"})
		return
	}

	var userID, email, name, role, hash string
	err := p.DB.QueryRow(c.Request.Context(), `
		SELECT id, email, name, role, password_hash
		FROM users WHERE lower(email) = lower($1) AND password_hash IS NOT NULL
	`, strings.TrimSpace(req.Email)).Scan(&userID, &email, &name, &role, &hash)
	if err != nil {
		// User not found — record failed login before returning identical 401.
		failEvt := activity.Event{
			Type:   activity.UserLoginFailed,
			IP:     c.ClientIP(),
			Detail: map[string]any{"email": req.Email},
		}
		if rid, ok := c.Get("request_id"); ok {
			if s, ok := rid.(string); ok {
				failEvt.RequestID = s
			}
		}
		p.Activity.Record(c.Request.Context(), failEvt)
		DebitLoginAccount(c)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), PepperPassword([]byte(req.Password), p.pepper)); err != nil {
		// Wrong password — record failed login before returning identical 401.
		failEvt := activity.Event{
			Type:   activity.UserLoginFailed,
			IP:     c.ClientIP(),
			Detail: map[string]any{"email": req.Email},
		}
		if rid, ok := c.Get("request_id"); ok {
			if s, ok := rid.(string); ok {
				failEvt.RequestID = s
			}
		}
		p.Activity.Record(c.Request.Context(), failEvt)
		DebitLoginAccount(c)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	if err := p.CreateSession(c, userID, email, name, role); err != nil {
		slog.Error("session creation failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "login failed"})
		return
	}

	loginEvt := activity.Event{
		Type:    activity.UserLogin,
		ActorID: &userID,
		IP:      c.ClientIP(),
	}
	if rid, ok := c.Get("request_id"); ok {
		if s, ok := rid.(string); ok {
			loginEvt.RequestID = s
		}
	}
	p.Activity.Record(c.Request.Context(), loginEvt)

	c.JSON(http.StatusOK, gin.H{
		"id":    userID,
		"email": email,
		"name":  name,
		"role":  role,
	})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password" binding:"required"`
	NewPassword     string `json:"new_password" binding:"required,min=8"`
}

func (p *LocalProvider) ChangePassword(c *gin.Context) {
	claims, err := p.ValidateSession(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return
	}

	var req changePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var currentHash string
	err = p.DB.QueryRow(c.Request.Context(),
		`SELECT password_hash FROM users WHERE id = $1 AND password_hash IS NOT NULL`,
		claims.UserID).Scan(&currentHash)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account does not use password authentication"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(currentHash), PepperPassword([]byte(req.CurrentPassword), p.pepper)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "current password is incorrect"})
		return
	}

	newHash, err := bcrypt.GenerateFromPassword(PepperPassword([]byte(req.NewPassword), p.pepper), bcryptCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "password hashing failed"})
		return
	}

	_, err = p.DB.Exec(c.Request.Context(),
		`WITH u AS (UPDATE users SET password_hash = $1 WHERE id = $2 RETURNING id)
		 -- Every OTHER session ends: whoever else holds a cookie for this
		 -- account is logged out by the password change; this one stays.
		 DELETE FROM sessions WHERE user_id = $2 AND id::text <> $3`, string(newHash), claims.UserID, claims.SID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update password"})
		return
	}

	pwEvt := activity.Event{
		Type:    activity.UserPasswordChanged,
		ActorID: &claims.UserID,
		IP:      c.ClientIP(),
	}
	if rid, ok := c.Get("request_id"); ok {
		if s, ok := rid.(string); ok {
			pwEvt.RequestID = s
		}
	}
	p.Activity.Record(c.Request.Context(), pwEvt)

	c.JSON(http.StatusOK, gin.H{"message": "password changed"})
}

type setupRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Name     string `json:"name" binding:"required"`
	Password string `json:"password" binding:"required,min=8"`
}

func (p *LocalProvider) Setup(c *gin.Context) {
	var req setupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	hash, err := bcrypt.GenerateFromPassword(PepperPassword([]byte(req.Password), p.pepper), bcryptCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "password hashing failed"})
		return
	}

	// Atomic check-and-insert: only succeeds if zero local users exist.
	var userID, email, name, role string
	err = p.DB.QueryRow(c.Request.Context(), `
		INSERT INTO users (email, name, role, password_hash, auth_provider)
		SELECT $1, $2, 'admin', $3, 'local'
		WHERE NOT EXISTS (SELECT 1 FROM users WHERE password_hash IS NOT NULL)
		RETURNING id, email, name, role
	`, strings.ToLower(strings.TrimSpace(req.Email)), strings.TrimSpace(req.Name), string(hash)).Scan(&userID, &email, &name, &role)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusConflict, gin.H{"error": "setup already completed"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create admin user"})
		return
	}

	if err := p.CreateSession(c, userID, email, name, role); err != nil {
		slog.Error("session creation failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "setup failed"})
		return
	}

	setupEvt := activity.Event{
		Type:    activity.UserLogin,
		ActorID: &userID,
		IP:      c.ClientIP(),
	}
	if rid, ok := c.Get("request_id"); ok {
		if s, ok := rid.(string); ok {
			setupEvt.RequestID = s
		}
	}
	p.Activity.Record(c.Request.Context(), setupEvt)

	slog.Info("initial admin created", "email", email)
	c.JSON(http.StatusCreated, gin.H{
		"id":    userID,
		"email": email,
		"name":  name,
		"role":  role,
	})
}
