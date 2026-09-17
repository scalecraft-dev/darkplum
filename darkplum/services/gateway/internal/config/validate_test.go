package config

import (
	"testing"
)

// TestValidateUpstreamURL covers the startup URL guard. Table-driven to keep
// additions cheap. devMode=false is the production path; devMode=true is the
// explicit override that permits local addresses.
func TestValidateUpstreamURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		devMode bool
		wantErr bool
	}{
		// ------------------------------------------------------------------
		// Loopback — rejected in production, allowed in dev
		// ------------------------------------------------------------------
		{
			name:    "localhost rejected when devMode=false",
			url:     "http://localhost:1234",
			devMode: false,
			wantErr: true,
		},
		{
			name:    "127.0.0.1 rejected when devMode=false",
			url:     "http://127.0.0.1:8000",
			devMode: false,
			wantErr: true,
		},
		{
			name:    "IPv6 loopback [::1] rejected when devMode=false",
			url:     "http://[::1]:1234",
			devMode: false,
			wantErr: true,
		},
		{
			name:    "localhost allowed when devMode=true",
			url:     "http://localhost:1234",
			devMode: true,
			wantErr: false,
		},
		{
			name:    "127.0.0.1 allowed when devMode=true",
			url:     "http://127.0.0.1:8000",
			devMode: true,
			wantErr: false,
		},
		{
			name:    "IPv6 loopback [::1] allowed when devMode=true",
			url:     "http://[::1]:1234",
			devMode: true,
			wantErr: false,
		},

		// ------------------------------------------------------------------
		// Link-local — rejected in production, allowed in dev
		// ------------------------------------------------------------------
		{
			name:    "169.254.169.254 (IMDS) rejected when devMode=false",
			url:     "http://169.254.169.254",
			devMode: false,
			wantErr: true,
		},
		{
			name:    "fe80:: link-local IPv6 rejected when devMode=false",
			url:     "http://[fe80::1]:8000",
			devMode: false,
			wantErr: true,
		},
		{
			name:    "169.254.169.254 allowed when devMode=true",
			url:     "http://169.254.169.254",
			devMode: true,
			wantErr: false,
		},
		{
			name:    "fe80:: link-local IPv6 allowed when devMode=true",
			url:     "http://[fe80::1]:8000",
			devMode: true,
			wantErr: false,
		},

		// ------------------------------------------------------------------
		// Normal in-cluster hostname — always allowed
		// ------------------------------------------------------------------
		{
			name:    "cluster service URL allowed when devMode=false",
			url:     "http://vllm-inference.darkplum-inference.svc.cluster.local:8000",
			devMode: false,
			wantErr: false,
		},
		{
			name:    "cluster service URL allowed when devMode=true",
			url:     "http://vllm-inference.darkplum-inference.svc.cluster.local:8000",
			devMode: true,
			wantErr: false,
		},

		// ------------------------------------------------------------------
		// "mock" — main.go branches before calling ValidateUpstreamURL when
		// VLLM_UPSTREAM=="mock", so this function is never called with "mock".
		// ValidateUpstreamURL treats "mock" as a hostname without a scheme/host
		// (url.Parse succeeds but u.Host is empty), so it returns an error.
		// Document that behavior here rather than asserting something the code
		// doesn't implement.
		// ------------------------------------------------------------------
		{
			name:    "mock string rejected by ValidateUpstreamURL (main.go bypasses this call)",
			url:     "mock",
			devMode: false,
			wantErr: true,
		},
		{
			name:    "mock string rejected by ValidateUpstreamURL even in devMode",
			url:     "mock",
			devMode: true,
			wantErr: true,
		},

		// ------------------------------------------------------------------
		// Malformed inputs
		// ------------------------------------------------------------------
		{
			name:    "URL missing scheme rejected",
			url:     "vllm-inference:8000",
			devMode: false,
			wantErr: true,
		},
		{
			name:    "empty string rejected",
			url:     "",
			devMode: false,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateUpstreamURL(tc.url, tc.devMode)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateUpstreamURL(%q, devMode=%v): expected error, got nil", tc.url, tc.devMode)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateUpstreamURL(%q, devMode=%v): unexpected error: %v", tc.url, tc.devMode, err)
			}
		})
	}
}
