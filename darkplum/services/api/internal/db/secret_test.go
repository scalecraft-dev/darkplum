package db

import (
	"strings"
	"testing"
)

func TestSelectServiceKeySecret_UnsetFallsBackToSessionSigningKey(t *testing.T) {
	sessionKey := []byte("a-session-signing-key-at-least-32-chars")
	secret, source, err := SelectServiceKeySecret("", sessionKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(secret) != string(sessionKey) {
		t.Fatalf("secret = %q, want the session signing key %q", secret, sessionKey)
	}
	if source != ServiceKeySecretSourceFallback {
		t.Fatalf("source = %q, want %q", source, ServiceKeySecretSourceFallback)
	}
}

func TestSelectServiceKeySecret_SetUsesDedicatedSecret(t *testing.T) {
	sessionKey := []byte("a-session-signing-key-at-least-32-chars")
	dedicated := "a-completely-different-32-char-secret!!"
	secret, source, err := SelectServiceKeySecret(dedicated, sessionKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(secret) != dedicated {
		t.Fatalf("secret = %q, want %q", secret, dedicated)
	}
	if source != ServiceKeySecretSourceDedicated {
		t.Fatalf("source = %q, want %q", source, ServiceKeySecretSourceDedicated)
	}
}

func TestSelectServiceKeySecret_TooShortFailsClosed(t *testing.T) {
	sessionKey := []byte("a-session-signing-key-at-least-32-chars")
	_, _, err := SelectServiceKeySecret("too-short", sessionKey)
	if err == nil {
		t.Fatal("expected an error for a SERVICE_KEY_SECRET under 32 characters, got nil")
	}
	if !strings.Contains(err.Error(), "SERVICE_KEY_SECRET") {
		t.Fatalf("error %q does not name SERVICE_KEY_SECRET", err)
	}
}

func TestSelectServiceKeySecret_ExactlyThirtyTwoCharsIsAccepted(t *testing.T) {
	sessionKey := []byte("a-session-signing-key-at-least-32-chars")
	exact32 := strings.Repeat("x", 32)
	secret, source, err := SelectServiceKeySecret(exact32, sessionKey)
	if err != nil {
		t.Fatalf("unexpected error for exactly-32-char secret: %v", err)
	}
	if string(secret) != exact32 {
		t.Fatalf("secret = %q, want %q", secret, exact32)
	}
	if source != ServiceKeySecretSourceDedicated {
		t.Fatalf("source = %q, want %q", source, ServiceKeySecretSourceDedicated)
	}
}

// TestSelectServiceKeySecret_DedicatedProducesDifferentKeyThanFallback verifies
// the end-to-end point of this change: deriveServiceKey on the two selected
// secrets must differ, so a dedicated SERVICE_KEY_SECRET actually decouples
// the gateway credential from SESSION_SIGNING_KEY rather than coincidentally
// landing on the same derived key.
func TestSelectServiceKeySecret_DedicatedProducesDifferentKeyThanFallback(t *testing.T) {
	sessionKey := []byte("a-session-signing-key-at-least-32-chars")
	dedicated := "a-completely-different-32-char-secret!!"

	fallbackSecret, _, err := SelectServiceKeySecret("", sessionKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dedicatedSecret, _, err := SelectServiceKeySecret(dedicated, sessionKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if deriveServiceKey(fallbackSecret) == deriveServiceKey(dedicatedSecret) {
		t.Fatal("expected different derived service keys for the fallback vs. dedicated secret")
	}
}
