package commands

import (
	"encoding/base64"
	"testing"
)

func TestOIDCConfigured(t *testing.T) {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	cases := map[string]bool{
		"":                                 false,
		enc("unconfigured"):                false,
		enc("https://accounts.google.com"): true,
		"not base64!":                      false,
	}
	for in, want := range cases {
		if got := oidcConfigured(in); got != want {
			t.Errorf("oidcConfigured(%q) = %v, want %v", in, got, want)
		}
	}
}
