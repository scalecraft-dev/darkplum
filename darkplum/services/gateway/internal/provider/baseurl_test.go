package provider_test

import (
	"testing"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
)

func TestValidateVendorBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		rawURL  string
		devMode bool
		wantErr bool
	}{
		{name: "empty is always valid", rawURL: "", devMode: false, wantErr: false},
		{name: "https allowed in prod", rawURL: "https://api.anthropic.com", devMode: false, wantErr: false},
		{name: "http rejected outside dev mode", rawURL: "http://api.anthropic.com", devMode: false, wantErr: true},
		{name: "http allowed in dev mode", rawURL: "http://127.0.0.1:9999", devMode: true, wantErr: false},
		{name: "https allowed in dev mode too", rawURL: "https://api.anthropic.com", devMode: true, wantErr: false},
		{name: "missing scheme rejected", rawURL: "api.anthropic.com", devMode: false, wantErr: true},
		{name: "missing host rejected", rawURL: "https://", devMode: false, wantErr: true},
		{name: "unparseable URL rejected", rawURL: "://bad", devMode: false, wantErr: true},
		{name: "other scheme rejected even in dev mode", rawURL: "ftp://host", devMode: true, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := provider.ValidateVendorBaseURL("ANTHROPIC_BASE_URL", tc.rawURL, tc.devMode)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error for %q (devMode=%v)", tc.rawURL, tc.devMode)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q (devMode=%v): %v", tc.rawURL, tc.devMode, err)
			}
		})
	}
}
