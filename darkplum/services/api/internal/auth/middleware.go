package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionRequired authenticates via SessionManager.ValidateSession, so the DB
// (user exists, current role) is authoritative over the JWT claims.
func SessionRequired(db *pgxpool.Pool, secret []byte, secureCook bool) gin.HandlerFunc {
	sm := &SessionManager{DB: db, Secret: secret, SecureCook: secureCook}
	return func(c *gin.Context) {
		claims, err := sm.ValidateSession(c)
		if errors.Is(err, errSessionCheck) {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "session could not be verified; retry"})
			return
		}
		if err != nil {
			msg := "session expired"
			if errors.Is(err, errNoSession) {
				msg = "not authenticated"
			}
			if strings.HasPrefix(c.Request.URL.Path, "/v1/") {
				// The OpenAI-compatible surface speaks the OpenAI envelope
				// for every refusal, including this one.
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{
					"message": msg, "type": "authentication_error", "code": "not_authenticated",
				}})
				return
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msg})
			return
		}
		c.Set("user_id", claims.UserID)
		c.Set("user_email", claims.Email)
		c.Set("user_name", claims.Name)
		c.Set("user_role", claims.Role)
		c.Set("session_id", claims.SID)
		c.Next()
	}
}

func AdminRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		role, _ := c.Get("user_role")
		if role != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin access required"})
			return
		}
		c.Next()
	}
}
