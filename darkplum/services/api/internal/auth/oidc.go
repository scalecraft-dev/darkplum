package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
)

type OIDCProvider struct {
	SessionManager
	Activity  *activity.Recorder
	provider  *oidc.Provider
	oauth2Cfg oauth2.Config
	verifier  *oidc.IDTokenVerifier
}

func NewOIDCProvider(ctx context.Context, cfg *Config, db *pgxpool.Pool, rec *activity.Recorder) (*OIDCProvider, error) {
	provider, err := oidc.NewProvider(ctx, cfg.OIDC.Issuer)
	if err != nil {
		return nil, err
	}

	oauth2Cfg := oauth2.Config{
		ClientID:     cfg.OIDC.ClientID,
		ClientSecret: cfg.OIDC.ClientSecret,
		RedirectURL:  cfg.OIDC.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.OIDC.ClientID})

	return &OIDCProvider{
		SessionManager: SessionManager{
			DB:         db,
			Secret:     cfg.Secret,
			SecureCook: cfg.SecureCookie,
		},
		Activity:  rec,
		provider:  provider,
		oauth2Cfg: oauth2Cfg,
		verifier:  verifier,
	}, nil
}

func (p *OIDCProvider) Logout(c *gin.Context) {
	p.SessionManager.HandleLogout(c)

	redirect := "/auth/login"
	var providerClaims struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	if err := p.provider.Claims(&providerClaims); err == nil && providerClaims.EndSessionEndpoint != "" {
		appRoot := strings.TrimSuffix(p.oauth2Cfg.RedirectURL, "/auth/callback")
		redirect = providerClaims.EndSessionEndpoint + "?post_logout_redirect_uri=" + appRoot
	}

	c.JSON(http.StatusOK, gin.H{"redirect": redirect})
}

func (p *OIDCProvider) Login(c *gin.Context) {
	state, err := RandomBase64(32)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state"})
		return
	}

	codeVerifier, err := RandomBase64(32)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE verifier"})
		return
	}

	p.SessionManager.SetCookie(c, stateCookieName, state, 300, "/")
	p.SessionManager.SetCookie(c, pkceCookieName, codeVerifier, 300, "/")

	if returnTo := c.Query("return_to"); returnTo != "" && strings.HasPrefix(returnTo, "/") {
		p.SessionManager.SetCookie(c, returnToCookieName, returnTo, 300, "/")
	}

	codeChallenge := s256Challenge(codeVerifier)
	url := p.oauth2Cfg.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
	c.Redirect(http.StatusFound, url)
}

func (p *OIDCProvider) Callback(c *gin.Context) {
	state, err := c.Cookie(stateCookieName)
	queryState := c.Query("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(queryState)) != 1 {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid state"})
		return
	}

	codeVerifier, err := c.Cookie(pkceCookieName)
	if err != nil || codeVerifier == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "missing PKCE verifier"})
		return
	}

	p.SessionManager.ClearCookie(c, stateCookieName, "/")
	p.SessionManager.ClearCookie(c, pkceCookieName, "/")

	if errMsg := c.Query("error"); errMsg != "" {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsg, "description": c.Query("error_description")})
		return
	}

	code := c.Query("code")
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing code"})
		return
	}

	token, err := p.oauth2Cfg.Exchange(c.Request.Context(), code,
		oauth2.SetAuthURLParam("code_verifier", codeVerifier),
	)
	if err != nil {
		slog.Error("token exchange failed", "err", err)
		c.JSON(http.StatusForbidden, gin.H{"error": "token exchange failed"})
		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "no id_token in response"})
		return
	}

	idToken, err := p.verifier.Verify(c.Request.Context(), rawIDToken)
	if err != nil {
		slog.Error("id_token verification failed", "err", err)
		c.JSON(http.StatusForbidden, gin.H{"error": "id_token verification failed"})
		return
	}

	var claims struct {
		Sub               string `json:"sub"`
		Email             string `json:"email"`
		Name              string `json:"name"`
		GivenName         string `json:"given_name"`
		FamilyName        string `json:"family_name"`
		PreferredUsername string `json:"preferred_username"`
		Picture           string `json:"picture"`
	}
	if err := idToken.Claims(&claims); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to parse claims"})
		return
	}

	name := resolveName(claims.Name, claims.GivenName, claims.FamilyName, claims.PreferredUsername, claims.Email)
	email := claims.Email
	if email == "" {
		email = claims.PreferredUsername
	}

	// Some IdPs (Auth0, others) omit the picture from the id_token but expose it
	// at the userinfo endpoint. Enrich best-effort; never fail login over it.
	picture := claims.Picture
	if picture == "" {
		if userInfo, uerr := p.provider.UserInfo(c.Request.Context(), oauth2.StaticTokenSource(token)); uerr == nil {
			var profile struct {
				Picture string `json:"picture"`
			}
			if userInfo.Claims(&profile) == nil {
				picture = profile.Picture
			}
		} else {
			slog.Warn("userinfo fetch failed; proceeding without picture", "err", uerr)
		}
	}

	userID, userEmail, userName, userRole, err := p.upsertUser(c.Request.Context(), email, name, picture, claims.Sub, idToken.Issuer)
	if err != nil {
		slog.Error("user upsert failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to provision user"})
		return
	}

	if err := p.SessionManager.CreateSession(c, userID, userEmail, userName, userRole); err != nil {
		slog.Error("session creation failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
		return
	}

	oidcEvt := activity.Event{
		Type:    activity.UserLogin,
		ActorID: &userID,
		IP:      c.ClientIP(),
	}
	if rid, ok := c.Get("request_id"); ok {
		if s, ok := rid.(string); ok {
			oidcEvt.RequestID = s
		}
	}
	p.Activity.Record(c.Request.Context(), oidcEvt)

	destination := "/"
	if returnTo, err := c.Cookie(returnToCookieName); err == nil && returnTo != "" && strings.HasPrefix(returnTo, "/") {
		destination = returnTo
		p.SessionManager.ClearCookie(c, returnToCookieName, "/")
	}
	c.Redirect(http.StatusFound, destination)
}

func (p *OIDCProvider) upsertUser(ctx context.Context, email, name, picture, subject, issuer string) (string, string, string, string, error) {
	var userID, userEmail, userName, userRole string
	// COALESCE keeps a previously-stored picture if a later login omits the claim.
	err := p.DB.QueryRow(ctx, `
		INSERT INTO users (email, name, picture, oidc_subject, oidc_issuer)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5)
		ON CONFLICT (oidc_issuer, oidc_subject) WHERE oidc_subject IS NOT NULL DO UPDATE
			SET email = EXCLUDED.email,
			    name = EXCLUDED.name,
			    picture = COALESCE(EXCLUDED.picture, users.picture)
		RETURNING id, email, name, role
	`, email, name, picture, subject, issuer).Scan(&userID, &userEmail, &userName, &userRole)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return "", "", "", "", errors.New("email address already in use by another account")
		}
	}
	return userID, userEmail, userName, userRole, err
}

// RotateRefreshToken atomically validates and rotates a refresh token.
// Returns the session claims, the new raw refresh token, and a new signed JWT.
// If the old token was already consumed (concurrent request), returns an error.
func RotateRefreshToken(ctx context.Context, db *pgxpool.Pool, oldToken string, secret []byte) (*SessionClaims, string, string, error) {
	oldHash := HashString(oldToken)

	newToken, err := RandomBase64(32)
	if err != nil {
		return nil, "", "", err
	}
	newHash := HashString(newToken)
	newExpiry := time.Now().Add(refreshTTL)

	var sessionID, sessionUser string
	err = db.QueryRow(ctx, `
		UPDATE sessions SET refresh_token_hash = $2, expires_at = $3
		WHERE refresh_token_hash = $1 AND expires_at > now()
		RETURNING id::text, user_id::text
	`, oldHash, newHash, newExpiry).Scan(&sessionID, &sessionUser)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", "", errors.New("refresh token expired or already used")
		}
		return nil, "", "", err
	}

	// Same rule as the access-token path: the user row must exist and its
	// current role wins. Uncached so a rotation always sees fresh state.
	InvalidateUser(sessionUser)
	u, err := lookupUser(ctx, db, sessionUser)
	if err != nil {
		if errors.Is(err, errUserGone) {
			_, _ = db.Exec(ctx, `DELETE FROM sessions WHERE refresh_token_hash = $1`, newHash)
		}
		return nil, "", "", err
	}
	userID, email, name, role := sessionUser, u.email, u.name, u.role

	jwt, err := SignSessionJWT(sessionID, userID, email, name, role, secret, jwtTTL)
	if err != nil {
		return nil, "", "", err
	}

	return &SessionClaims{SID: sessionID, UserID: userID, Email: email, Name: name, Role: role}, newToken, jwt, nil
}

func CleanupExpiredSessions(ctx context.Context, db *pgxpool.Pool) {
	_, _ = db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
}

func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func resolveName(name, given, family, preferred, email string) string {
	if name != "" {
		return name
	}
	if given != "" || family != "" {
		return strings.TrimSpace(given + " " + family)
	}
	if preferred != "" {
		return preferred
	}
	if at := strings.Index(email, "@"); at > 0 {
		return email[:at]
	}
	return "User"
}
