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

// ---------------------------------------------------------------------------
// Routed-alias classifier config (CLASSIFIER_*, SEMANTIC_ROUTER_*)
// ---------------------------------------------------------------------------

func TestLoad_ClassifierDefaults(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ClassifierTimeoutMS != 500 {
		t.Errorf("ClassifierTimeoutMS = %d, want default 500", cfg.ClassifierTimeoutMS)
	}
	if cfg.ClassifierBreakerFailures != 5 {
		t.Errorf("ClassifierBreakerFailures = %d, want default 5", cfg.ClassifierBreakerFailures)
	}
	if cfg.ClassifierBreakerOpenSeconds != 30 {
		t.Errorf("ClassifierBreakerOpenSeconds = %d, want default 30", cfg.ClassifierBreakerOpenSeconds)
	}
	if cfg.SemanticRouterURL != "" {
		t.Errorf("SemanticRouterURL = %q, want empty (unconfigured) by default", cfg.SemanticRouterURL)
	}
	if cfg.SemanticRouterMinConfidence != 0.6 {
		t.Errorf("SemanticRouterMinConfidence = %v, want default 0.6", cfg.SemanticRouterMinConfidence)
	}
}

// TestLoad_ClassifierTimeoutMSRejectsOutOfBounds matches the pattern of
// every other bounded knob in this file (e.g.
// TestLoad_PolicyMaxStalenessSecondsRejectsBelowMinimum): a value outside
// [50, 2000] fails Load with an error rather than being silently clamped.
func TestLoad_ClassifierTimeoutMSRejectsOutOfBounds(t *testing.T) {
	baseEnv(t)
	t.Setenv("CLASSIFIER_TIMEOUT_MS", "1")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for CLASSIFIER_TIMEOUT_MS below the 50ms minimum")
	}

	t.Setenv("CLASSIFIER_TIMEOUT_MS", "999999")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for CLASSIFIER_TIMEOUT_MS above the 2000ms maximum")
	}
}

func TestLoad_ClassifierTimeoutMSAcceptsBounds(t *testing.T) {
	baseEnv(t)
	t.Setenv("CLASSIFIER_TIMEOUT_MS", "50")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error at the minimum boundary: %v", err)
	}
	if cfg.ClassifierTimeoutMS != 50 {
		t.Errorf("ClassifierTimeoutMS = %d, want 50", cfg.ClassifierTimeoutMS)
	}

	t.Setenv("CLASSIFIER_TIMEOUT_MS", "2000")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error at the maximum boundary: %v", err)
	}
	if cfg.ClassifierTimeoutMS != 2000 {
		t.Errorf("ClassifierTimeoutMS = %d, want 2000", cfg.ClassifierTimeoutMS)
	}
}

func TestLoad_ClassifierBreakerFailuresRejectsBelowMinimum(t *testing.T) {
	baseEnv(t)
	t.Setenv("CLASSIFIER_BREAKER_FAILURES", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for CLASSIFIER_BREAKER_FAILURES below the 1 minimum")
	}
}

func TestLoad_ClassifierBreakerOpenSecondsRejectsBelowMinimum(t *testing.T) {
	baseEnv(t)
	t.Setenv("CLASSIFIER_BREAKER_OPEN_SECONDS", "1")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for CLASSIFIER_BREAKER_OPEN_SECONDS below the 5s minimum")
	}
}

func TestLoad_SemanticRouterURLAndMinConfidence(t *testing.T) {
	baseEnv(t)
	t.Setenv("SEMANTIC_ROUTER_URL", "http://semantic-router.darkplum-system.svc.cluster.local:8080")
	t.Setenv("SEMANTIC_ROUTER_MIN_CONFIDENCE", "0.8")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SemanticRouterURL != "http://semantic-router.darkplum-system.svc.cluster.local:8080" {
		t.Errorf("SemanticRouterURL = %q", cfg.SemanticRouterURL)
	}
	if cfg.SemanticRouterMinConfidence != 0.8 {
		t.Errorf("SemanticRouterMinConfidence = %v, want 0.8", cfg.SemanticRouterMinConfidence)
	}
}

func TestLoad_SemanticRouterMinConfidenceRejectsOutOfUnitInterval(t *testing.T) {
	baseEnv(t)
	t.Setenv("SEMANTIC_ROUTER_MIN_CONFIDENCE", "-1")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for SEMANTIC_ROUTER_MIN_CONFIDENCE below 0")
	}

	t.Setenv("SEMANTIC_ROUTER_MIN_CONFIDENCE", "5")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for SEMANTIC_ROUTER_MIN_CONFIDENCE above 1")
	}
}

func TestLoad_ClassifierTimeoutMSRejectsNonInteger(t *testing.T) {
	baseEnv(t)
	t.Setenv("CLASSIFIER_TIMEOUT_MS", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for a non-integer CLASSIFIER_TIMEOUT_MS")
	}
}
