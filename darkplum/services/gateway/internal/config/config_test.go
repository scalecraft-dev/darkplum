package config

import "testing"

// baseEnv sets the minimum environment Load needs to succeed, without
// touching any of the fields under test in this file.
func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/darkplum")
}

func TestLoad_PolicyMaxStalenessSecondsDefault(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PolicyMaxStalenessSeconds != 900 {
		t.Errorf("PolicyMaxStalenessSeconds = %d, want default 900", cfg.PolicyMaxStalenessSeconds)
	}
}

// TestLoad_PolicyMaxStalenessSecondsRejectsZero is the regression guard for
// "0 = disabled/unbounded is NOT allowed": a policy snapshot loaded once must
// never be enforced forever through an unbounded Postgres outage.
func TestLoad_PolicyMaxStalenessSecondsRejectsZero(t *testing.T) {
	baseEnv(t)
	t.Setenv("POLICY_MAX_STALENESS_SECONDS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for POLICY_MAX_STALENESS_SECONDS=0 (disabling the cap must not be allowed)")
	}
}

func TestLoad_PolicyMaxStalenessSecondsRejectsBelowMinimum(t *testing.T) {
	baseEnv(t)
	t.Setenv("POLICY_MAX_STALENESS_SECONDS", "29")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for POLICY_MAX_STALENESS_SECONDS below the 30s minimum")
	}
}

func TestLoad_PolicyMaxStalenessSecondsAcceptsMinimum(t *testing.T) {
	baseEnv(t)
	t.Setenv("POLICY_MAX_STALENESS_SECONDS", "30")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error at the minimum boundary: %v", err)
	}
	if cfg.PolicyMaxStalenessSeconds != 30 {
		t.Errorf("PolicyMaxStalenessSeconds = %d, want 30", cfg.PolicyMaxStalenessSeconds)
	}
}

func TestLoad_RouteLastGoodTTLSecondsDefault(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RouteLastGoodTTLSeconds != 120 {
		t.Errorf("RouteLastGoodTTLSeconds = %d, want default 120", cfg.RouteLastGoodTTLSeconds)
	}
}

// TestLoad_RouteLastGoodTTLSecondsMustBeAtLeastDiscoveryInterval guards
// against a TTL shorter than the interval that refreshes it, which would
// expire a healthy source's models on every single discovery tick.
func TestLoad_RouteLastGoodTTLSecondsMustBeAtLeastDiscoveryInterval(t *testing.T) {
	baseEnv(t)
	t.Setenv("DISCOVERY_INTERVAL_SECONDS", "300")
	t.Setenv("ROUTE_LAST_GOOD_TTL_SECONDS", "60")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error when ROUTE_LAST_GOOD_TTL_SECONDS < DISCOVERY_INTERVAL_SECONDS")
	}
}

func TestLoad_RouteLastGoodTTLSecondsAcceptsEqualToDiscoveryInterval(t *testing.T) {
	baseEnv(t)
	t.Setenv("DISCOVERY_INTERVAL_SECONDS", "300")
	t.Setenv("ROUTE_LAST_GOOD_TTL_SECONDS", "300")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error when TTL equals the discovery interval: %v", err)
	}
	if cfg.RouteLastGoodTTLSeconds != 300 {
		t.Errorf("RouteLastGoodTTLSeconds = %d, want 300", cfg.RouteLastGoodTTLSeconds)
	}
}
