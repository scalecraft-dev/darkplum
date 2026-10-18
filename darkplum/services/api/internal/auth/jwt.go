package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type SessionClaims struct {
	// SID is the sessions row this access token belongs to. Logout deletes
	// that row, which is what makes a still-unexpired JWT stop working
	// (validateSession refuses a token whose session row is gone). A token
	// without a sid (minted before this field existed) is never accepted
	// as a session on its own; the refresh cookie re-mints one that has it.
	SID    string `json:"sid,omitempty"`
	UserID string `json:"uid"`
	Email  string `json:"email"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	jwt.RegisteredClaims
}

const jwtIssuer = "darkplum-api"

func SignSessionJWT(sid, userID, email, name, role string, secret []byte, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := SessionClaims{
		SID:    sid,
		UserID: userID,
		Email:  email,
		Name:   name,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{jwtIssuer},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(secret)
}

func VerifySessionJWT(tokenStr string, secret []byte) (*SessionClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &SessionClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return secret, nil
	},
		jwt.WithIssuer(jwtIssuer),
		jwt.WithAudience(jwtIssuer),
	)
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*SessionClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}
	return claims, nil
}
