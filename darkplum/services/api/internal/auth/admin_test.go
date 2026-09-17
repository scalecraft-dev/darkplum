package auth

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestPepperHashRoundTrips locks in the credential-compatibility contract behind
// break-glass admin creation: a hash produced with a given pepper (as
// CreateLocalAdmin does) must verify under the login path's
// PepperPassword+CompareHashAndPassword with the SAME pepper, and must NOT verify
// under a different pepper. This is the property that silently breaks if the
// bootstrap Job and the api deployment ever disagree on PASSWORD_PEPPER.
func TestPepperHashRoundTrips(t *testing.T) {
	const pw = "br3akgl4ss-pw"
	peppers := map[string][]byte{
		"none":    nil,
		"present": []byte("a-pepper-of-at-least-32-characters-xx"),
	}

	for label, pepper := range peppers {
		hash, err := bcrypt.GenerateFromPassword(PepperPassword([]byte(pw), pepper), bcryptCost)
		if err != nil {
			t.Fatalf("%s: hash: %v", label, err)
		}
		// Same pepper => verifies (login succeeds).
		if err := bcrypt.CompareHashAndPassword(hash, PepperPassword([]byte(pw), pepper)); err != nil {
			t.Errorf("%s: hash did not verify under the same pepper: %v", label, err)
		}
		// Different pepper => must NOT verify (proves the drift trap is real).
		other := []byte("a-totally-different-pepper-value-yyyyy")
		if err := bcrypt.CompareHashAndPassword(hash, PepperPassword([]byte(pw), other)); err == nil {
			t.Errorf("%s: hash unexpectedly verified under a different pepper", label)
		}
	}
}
