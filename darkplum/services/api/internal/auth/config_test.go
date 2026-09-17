package auth

import "testing"

// clearAuthEnv blanks every env var LoadConfig reads, then sets a valid
// session signing key so a test starts from a known, auth-required baseline
// (local mode, no OIDC) regardless of the ambient environment. Tests that
// exercise signing-key validation override it explicitly.
func clearAuthEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AUTH_DISABLED", "PASSWORD_PEPPER",
		"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_REDIRECT_URL",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("SESSION_SIGNING_KEY", "test-session-signing-key-at-least-32-chars")
}

// With nothing configured, an instance must come up in local auth mode — never
// unauthenticated. This is the core guarantee behind removing AUTH_DISABLED.
func TestLoadConfig_NoEnvDefaultsToLocal(t *testing.T) {
	clearAuthEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OIDC != nil {
		t.Fatal("expected OIDC to be nil for local-only config")
	}
}

// AUTH_DISABLED must no longer have any effect — setting it true still yields
// an authenticated (local) instance.
func TestLoadConfig_AuthDisabledIsIgnored(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("AUTH_DISABLED", "true")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OIDC != nil {
		t.Fatal("AUTH_DISABLED must be ignored: expected OIDC to be nil")
	}
}

// All four OIDC vars set => OIDC mode.
func TestLoadConfig_FullOIDC(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("OIDC_CLIENT_ID", "client")
	t.Setenv("OIDC_CLIENT_SECRET", "secret")
	t.Setenv("OIDC_REDIRECT_URL", "https://app.example.com/auth/callback")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OIDC == nil {
		t.Fatal("expected OIDC config to be set")
	}
}

// Partial OIDC config must fail closed rather than silently degrade to local
// (which would expose the unauthenticated first-run setup endpoint).
func TestLoadConfig_PartialOIDCFailsClosed(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("OIDC_CLIENT_ID", "client")
	// OIDC_CLIENT_SECRET and OIDC_REDIRECT_URL intentionally unset.

	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig: expected error for partial OIDC config, got nil")
	}
}

// A missing session signing key must fail closed — we never fabricate one.
func TestLoadConfig_MissingSigningKeyFailsClosed(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("SESSION_SIGNING_KEY", "")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig: expected error for missing SESSION_SIGNING_KEY, got nil")
	}
}

// A too-short signing key must also fail closed.
func TestLoadConfig_ShortSigningKeyFailsClosed(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("SESSION_SIGNING_KEY", "too-short")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig: expected error for short SESSION_SIGNING_KEY, got nil")
	}
}
