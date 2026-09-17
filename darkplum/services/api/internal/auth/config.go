package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

type Config struct {
	Secret       []byte
	SecureCookie bool
	Pepper       []byte
	// OIDC is nil unless OIDC is fully configured. Local password auth is always
	// available; OIDC is additive.
	OIDC *OIDCConfig
}

type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// LoadConfig resolves authentication config. Local password auth is always
// enabled; OIDC is additionally enabled when all four OIDC vars are set. There
// is intentionally no way to disable authentication.
func LoadConfig() (*Config, error) {
	secret, err := sessionSigningKey()
	if err != nil {
		return nil, err
	}
	pepper := loadPepper()

	issuer := os.Getenv("OIDC_ISSUER")
	clientID := os.Getenv("OIDC_CLIENT_ID")
	clientSecret := os.Getenv("OIDC_CLIENT_SECRET")
	redirectURL := os.Getenv("OIDC_REDIRECT_URL")

	oidcReady := issuer != "" && issuer != "unconfigured" &&
		clientID != "" && clientID != "unconfigured" &&
		clientSecret != "" && clientSecret != "unconfigured" &&
		redirectURL != "" && redirectURL != "unconfigured"

	if oidcReady {
		return &Config{
			Secret:       secret,
			SecureCookie: loadSecureCookies(),
			Pepper:       pepper,
			OIDC: &OIDCConfig{
				Issuer:       issuer,
				ClientID:     clientID,
				ClientSecret: clientSecret,
				RedirectURL:  redirectURL,
			},
		}, nil
	}

	// If some OIDC vars are set but not all, the operator intended OIDC and
	// misconfigured it. Fail closed rather than silently degrading to local
	// auth (which would expose the unauthenticated first-run setup endpoint).
	vars := []struct{ name, val string }{
		{"OIDC_ISSUER", issuer},
		{"OIDC_CLIENT_ID", clientID},
		{"OIDC_CLIENT_SECRET", clientSecret},
		{"OIDC_REDIRECT_URL", redirectURL},
	}
	var setVars, missingVars []string
	for _, v := range vars {
		if v.val != "" && v.val != "unconfigured" {
			setVars = append(setVars, v.name)
		} else {
			missingVars = append(missingVars, v.name)
		}
	}
	if len(setVars) > 0 && len(missingVars) > 0 {
		return nil, fmt.Errorf("partial OIDC configuration: set %v but missing %v — "+
			"set all four OIDC vars to enable OIDC, or unset them all to use local auth",
			setVars, missingVars)
	}

	return &Config{
		Secret:       secret,
		SecureCookie: loadSecureCookies(),
		Pepper:       pepper,
	}, nil
}

// loadSecureCookies controls the Secure flag on session cookies. Defaults to
// true; set SECURE_COOKIES=false (or 0) only for local http development.
func loadSecureCookies() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SECURE_COOKIES"))) {
	case "false", "0":
		return false
	default:
		return true
	}
}

func loadPepper() []byte {
	p := os.Getenv("PASSWORD_PEPPER")
	if p == "" {
		slog.Warn("PASSWORD_PEPPER not set — passwords will be hashed without a pepper")
		return nil
	}
	if len(p) < 32 {
		slog.Warn("PASSWORD_PEPPER is shorter than 32 characters — consider using a longer value")
	}
	return []byte(p)
}

// PepperPassword applies HMAC-SHA256 with the configured pepper.
// If no pepper is set, the raw password is returned unchanged.
func PepperPassword(password []byte, pepper []byte) []byte {
	if len(pepper) == 0 {
		return password
	}
	mac := hmac.New(sha256.New, pepper)
	mac.Write(password)
	return mac.Sum(nil)
}

// sessionSigningKey returns the long-lived HMAC key used to sign and verify
// session JWTs (see SignSessionJWT). It is a required server secret: the same
// key must be present on every API replica or sessions won't verify across
// pods, and it must survive restarts or every session is invalidated. We fail
// closed rather than generate a throwaway key, which would silently break
// multi-replica deployments and log everyone out on each restart.
func sessionSigningKey() ([]byte, error) {
	s := os.Getenv("SESSION_SIGNING_KEY")
	if len(s) < 32 {
		return nil, fmt.Errorf("SESSION_SIGNING_KEY must be set to at least 32 characters")
	}
	return []byte(s), nil
}
