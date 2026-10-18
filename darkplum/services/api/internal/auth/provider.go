package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"github.com/jackc/pgx/v5"
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

	sid, refreshToken, err := InsertSession(c.Request.Context(), s.DB, userID)
	if err != nil {
		return err
	}

	jwt, err := SignSessionJWT(sid, userID, email, name, role, s.Secret, jwtTTL)
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
			claims, err = reconcileClaims(c.Request.Context(), s.DB, claims)
			if err != nil {
				return nil, err
			}
			// The signature proves the api minted this token; the session
			// row proves nobody has logged it out since. A token with no
			// sid, or whose row is gone, is not a session: fall through to
			// the refresh cookie, which is deleted at logout too.
			if claims.SID != "" {
				alive, err := sessionAlive(c.Request.Context(), s.DB, claims.SID)
				if err != nil {
					// Cannot tell: say so (503) rather than treat it as a
					// logout and rotate the refresh token, which under a
					// database brownout would log every tab out at once.
					return nil, errSessionCheck
				}
				if alive {
					return claims, nil
				}
			}
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
	// Also by the access token's own session id, so a logout that arrives
	// without the refresh cookie (or after a rotation race) still ends the
	// session the JWT is bound to -- otherwise that JWT replays until it
	// expires.
	if tokenStr, err := c.Cookie(sessionCookieName); err == nil && tokenStr != "" {
		if claims, err := VerifySessionJWT(tokenStr, s.Secret); err == nil && claims.SID != "" {
			_, _ = s.DB.Exec(c.Request.Context(), `DELETE FROM sessions WHERE id = $1`, claims.SID)
		}
	}
	s.ClearCookie(c, sessionCookieName, "/")
	s.ClearCookie(c, refreshCookieName, "/")
}

var errNoSession = errors.New("no valid session")

// InsertSession creates the sessions row a login is bound to and returns its
// id (the JWT's sid claim) and the raw refresh token for the cookie. The
// refresh token is stored hashed; the raw value exists only in the response.
func InsertSession(ctx context.Context, db *pgxpool.Pool, userID string) (sid, refreshToken string, err error) {
	refreshToken, err = RandomBase64(32)
	if err != nil {
		return "", "", err
	}
	err = db.QueryRow(ctx,
		`INSERT INTO sessions (user_id, refresh_token_hash, expires_at) VALUES ($1, $2, $3) RETURNING id::text`,
		userID, HashString(refreshToken), time.Now().Add(refreshTTL),
	).Scan(&sid)
	if err != nil {
		return "", "", err
	}
	return sid, refreshToken, nil
}

// sessionAlive reports whether the sessions row sid still exists and has not
// expired. Only "no such row" is a definite no; any other failure is
// returned so the caller can refuse without deciding either way.
func sessionAlive(ctx context.Context, db *pgxpool.Pool, sid string) (bool, error) {
	var one int
	err := db.QueryRow(ctx, `SELECT 1 FROM sessions WHERE id = $1 AND expires_at > now()`, sid).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// errSessionCheck is "the session could not be verified right now" -- a 503,
// distinct from "there is no session" (401).
var errSessionCheck = errors.New("session check unavailable")

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
