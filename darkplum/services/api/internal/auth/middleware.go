package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

func SessionRequired(db *pgxpool.Pool, secret []byte, secureCook bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenStr, err := c.Cookie(sessionCookieName)
		if err == nil && tokenStr != "" {
			claims, err := VerifySessionJWT(tokenStr, secret)
			if err == nil {
				c.Set("user_id", claims.UserID)
				c.Set("user_email", claims.Email)
				c.Set("user_name", claims.Name)
				c.Set("user_role", claims.Role)
				c.Next()
				return
			}
		}

		refreshToken, err := c.Cookie(refreshCookieName)
		if err != nil || refreshToken == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}

		claims, newRefresh, newJWT, err := RotateRefreshToken(c.Request.Context(), db, refreshToken, secret)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired"})
			return
		}

		c.SetSameSite(http.SameSiteLaxMode)
		c.SetCookie(sessionCookieName, newJWT, int(jwtTTL.Seconds()), "/", "", secureCook, true)
		c.SetCookie(refreshCookieName, newRefresh, int(refreshTTL.Seconds()), "/", "", secureCook, true)
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
