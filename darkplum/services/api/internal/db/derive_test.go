package db

import (
	"strings"
	"testing"
)

func TestDeriveServiceKey_Deterministic(t *testing.T) {
	secret := []byte("a-session-signing-key-at-least-32-chars")
	a := deriveServiceKey(secret)
	b := deriveServiceKey(secret)
	if a != b {
		t.Fatalf("deriveServiceKey is not deterministic: %q != %q", a, b)
	}
}

func TestDeriveServiceKey_DiffersPerSecret(t *testing.T) {
	k1 := deriveServiceKey([]byte("a-session-signing-key-at-least-32-chars"))
	k2 := deriveServiceKey([]byte("a-totally-different-signing-key-of-len32"))
	if k1 == k2 {
		t.Fatalf("deriveServiceKey produced the same key for two different secrets: %q", k1)
	}
}

func TestDeriveServiceKey_PrefixAndLength(t *testing.T) {
	secret := []byte("a-session-signing-key-at-least-32-chars")
	key := deriveServiceKey(secret)

	if !strings.HasPrefix(key, "dp_") {
		t.Fatalf("expected key to start with dp_, got %q", key)
	}
	wantLen := len("dp_") + serviceKeyBodyLen
	if len(key) != wantLen {
		t.Fatalf("key length = %d, want %d (key=%q)", len(key), wantLen, key)
	}
}

func TestDeriveServiceKey_DoesNotContainSecret(t *testing.T) {
	secret := []byte("super-secret-session-signing-key-value!")
	key := deriveServiceKey(secret)
	if strings.Contains(key, string(secret)) {
		t.Fatalf("derived key leaks the raw secret: %q", key)
	}
	// The derived key must not equal the secret itself either.
	if key == string(secret) {
		t.Fatalf("derived key equals the raw secret")
	}
}

func TestDeriveServiceKey_OnlyBase62BodyCharacters(t *testing.T) {
	key := deriveServiceKey([]byte("a-session-signing-key-at-least-32-chars"))
	body := strings.TrimPrefix(key, "dp_")
	for _, r := range body {
		if !strings.ContainsRune(base62Chars, r) {
			t.Fatalf("derived key body contains non-base62 character %q: %q", r, key)
		}
	}
}

// TestBase62EncodeDigest_PadsShortEncodings exercises the left-pad safety net
// directly: a digest with enough leading zero bytes that math/big's encoding
// alone would be shorter than the requested length must still come out at
// exactly that length, and must be reproducible.
func TestBase62EncodeDigest_PadsShortEncodings(t *testing.T) {
	shortDigest := []byte{0x00, 0x00, 0x00, 0x01} // encodes to a single base62 char without padding
	got := base62EncodeDigest(shortDigest, 43)
	if len(got) != 43 {
		t.Fatalf("len(got) = %d, want 43 (got=%q)", len(got), got)
	}
	if !strings.HasSuffix(got, string(base62Chars[1])) {
		t.Fatalf("expected padded encoding to end in the digit for 1, got %q", got)
	}
	if got != base62EncodeDigest(shortDigest, 43) {
		t.Fatalf("base62EncodeDigest is not deterministic for a fixed input")
	}
}

func TestBase62EncodeDigest_TruncatesLongEncodings(t *testing.T) {
	// All-0xFF is the largest possible digest for its length, guaranteed to
	// base62-encode to more than serviceKeyBodyLen characters.
	longDigest := make([]byte, 32)
	for i := range longDigest {
		longDigest[i] = 0xFF
	}
	got := base62EncodeDigest(longDigest, serviceKeyBodyLen)
	if len(got) != serviceKeyBodyLen {
		t.Fatalf("len(got) = %d, want %d", len(got), serviceKeyBodyLen)
	}
}

func TestBase62EncodeDigest_ZeroDigest(t *testing.T) {
	zero := make([]byte, 32)
	got := base62EncodeDigest(zero, serviceKeyBodyLen)
	want := strings.Repeat(string(base62Chars[0]), serviceKeyBodyLen)
	if got != want {
		t.Fatalf("base62EncodeDigest(zero) = %q, want %q", got, want)
	}
}
