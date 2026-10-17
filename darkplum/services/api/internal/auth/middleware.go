package auth

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionRequired authenticates via SessionManager.ValidateSession, so the DB
// (user exists, current role) is authoritative over the JWT claims.
func SessionRequired(db *pgxpool.Pool, secret []byte, secureCook bool) gin.HandlerFunc {
	sm := &SessionManager{DB: db, Secret: secret, SecureCook: secureCook}
	return func(c *gin.Context) {
		claims, err := sm.ValidateSession(c)
		if err != nil {
			msg := "session expired"
			if errors.Is(err, errNoSession) {
				msg = "not authenticated"
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msg})
			return
		}
		c.Set("user_id", claims.UserID)
		c.Set("user_email", claims.Email)
		c.Set("user_name", claims.Name)
		c.Set("user_role", claims.Role)
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
