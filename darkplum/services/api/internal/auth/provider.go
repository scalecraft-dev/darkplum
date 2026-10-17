package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	sessionCookieName  = "darkplum_session"
	refreshCookieName  = "darkplum_refresh"
	stateCookieName    = "darkplum_oauth_state"
	pkceCookieName     = "darkplum_pkce"
	returnToCookieName = "darkplum_return_to"

	jwtTTL     = 1 * time.Hour
	refreshTTL = 7 * 24 * time.Hour
)

// SessionManager provides shared cookie and session helpers for all providers.
type SessionManager struct {
	DB         *pgxpool.Pool
	Secret     []byte
	SecureCook bool
}

func (s *SessionManager) SetCookie(c *gin.Context, name, value string, maxAge int, path string) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(name, value, maxAge, path, "", s.SecureCook, true)
}

func (s *SessionManager) ClearCookie(c *gin.Context, name, path string) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(name, "", -1, path, "", s.SecureCook, true)
}

func (s *SessionManager) CreateSession(c *gin.Context, userID, email, name, role string) error {
	// Clear any pre-existing session cookies to prevent session fixation.
	s.ClearCookie(c, sessionCookieName, "/")
	s.ClearCookie(c, refreshCookieName, "/")

	refreshToken, err := RandomBase64(32)
	if err != nil {
		return err
	}
	refreshHash := HashString(refreshToken)
	expiresAt := time.Now().Add(refreshTTL)

	_, err = s.DB.Exec(c.Request.Context(),
		`INSERT INTO sessions (user_id, refresh_token_hash, expires_at) VALUES ($1, $2, $3)`,
		userID, refreshHash, expiresAt,
	)
	if err != nil {
		return err
	}

	jwt, err := SignSessionJWT(userID, email, name, role, s.Secret, jwtTTL)
	if err != nil {
		return err
	}

	s.SetCookie(c, sessionCookieName, jwt, int(jwtTTL.Seconds()), "/")
	s.SetCookie(c, refreshCookieName, refreshToken, int(refreshTTL.Seconds()), "/")
	return nil
}

// ValidateSession returns the caller's claims, with the DB authoritative: the
// user row must still exist, and its role/email/name replace the JWT's. A
// session for a vanished user (wiped DB, deleted account) yields errUserGone
// and the session cookies are cleared so the browser falls back to login or
// first-run setup.
func (s *SessionManager) ValidateSession(c *gin.Context) (*SessionClaims, error) {
	claims, err := s.validateSession(c)
	if errors.Is(err, errUserGone) {
		s.ClearCookie(c, sessionCookieName, "/")
		s.ClearCookie(c, refreshCookieName, "/")
	}
	return claims, err
}

func (s *SessionManager) validateSession(c *gin.Context) (*SessionClaims, error) {
	tokenStr, err := c.Cookie(sessionCookieName)
	if err == nil && tokenStr != "" {
		claims, err := VerifySessionJWT(tokenStr, s.Secret)
		if err == nil {
			return reconcileClaims(c.Request.Context(), s.DB, claims)
		}
	}

	refreshToken, err := c.Cookie(refreshCookieName)
	if err != nil || refreshToken == "" {
		return nil, errNoSession
	}

	claims, newRefresh, newJWT, err := RotateRefreshToken(c.Request.Context(), s.DB, refreshToken, s.Secret)
	if err != nil {
		return nil, err
	}
	s.SetCookie(c, sessionCookieName, newJWT, int(jwtTTL.Seconds()), "/")
	s.SetCookie(c, refreshCookieName, newRefresh, int(refreshTTL.Seconds()), "/")
	return claims, nil
}

func (s *SessionManager) HandleMe(c *gin.Context) {
	claims, err := s.ValidateSession(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return
	}
	var picture *string
	var hasPassword bool
	_ = s.DB.QueryRow(c.Request.Context(),
		`SELECT picture, password_hash IS NOT NULL FROM users WHERE id = $1`,
		claims.UserID).Scan(&picture, &hasPassword)

	c.JSON(http.StatusOK, gin.H{
		"id":           claims.UserID,
		"email":        claims.Email,
		"name":         claims.Name,
		"role":         claims.Role,
		"picture":      picture,
		"has_password": hasPassword,
	})
}

func (s *SessionManager) HandleLogout(c *gin.Context) {
	if refreshToken, err := c.Cookie(refreshCookieName); err == nil && refreshToken != "" {
		hash := HashString(refreshToken)
		_, _ = s.DB.Exec(c.Request.Context(),
			`DELETE FROM sessions WHERE refresh_token_hash = $1`, hash,
		)
	}
	s.ClearCookie(c, sessionCookieName, "/")
	s.ClearCookie(c, refreshCookieName, "/")
}

var errNoSession = errors.New("no valid session")

// RandomBase64 generates a cryptographically random base64url string.
func RandomBase64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashString returns the hex-encoded SHA-256 of s.
func HashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
