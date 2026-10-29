package config

import (
	"strings"
	"testing"
	"time"
)

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

// ---------------------------------------------------------------------------
// Vendor provider config (anthropic)
// ---------------------------------------------------------------------------

func TestLoad_ProviderKeysDirDefault(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ProviderKeysDir != "/var/run/darkplum/provider-keys" {
		t.Errorf("ProviderKeysDir = %q, want the default", cfg.ProviderKeysDir)
	}
}

func TestLoad_ProviderKeysDirOverride(t *testing.T) {
	baseEnv(t)
	t.Setenv("PROVIDER_KEYS_DIR", "/tmp/custom-provider-keys")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ProviderKeysDir != "/tmp/custom-provider-keys" {
		t.Errorf("ProviderKeysDir = %q", cfg.ProviderKeysDir)
	}
}

func TestLoad_AnthropicDefaultMaxTokensDefault(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AnthropicDefaultMaxTokens != 8192 {
		t.Errorf("AnthropicDefaultMaxTokens = %d, want default 8192", cfg.AnthropicDefaultMaxTokens)
	}
}

func TestLoad_AnthropicDefaultMaxTokensRejectsNonPositive(t *testing.T) {
	baseEnv(t)
	t.Setenv("ANTHROPIC_DEFAULT_MAX_TOKENS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for ANTHROPIC_DEFAULT_MAX_TOKENS=0")
	}
	t.Setenv("ANTHROPIC_DEFAULT_MAX_TOKENS", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for a non-integer ANTHROPIC_DEFAULT_MAX_TOKENS")
	}
}

// TestLoad_AnthropicModelFilterAcceptsInvalidGlobWithoutFailing is the
// regression guard for the failure-isolation ruling: a typo'd vendor filter
// must never be able to block the whole gateway from starting, unlike
// BEDROCK_MODEL_FILTER (parsed via the validating parseGlobList, which
// fails Load outright on a bad pattern). Validity of each Anthropic pattern
// is instead checked per-tick by routing.ApplyGlobFilter, which fails
// closed (non-fatally) for that one vendor only.
func TestLoad_AnthropicModelFilterAcceptsInvalidGlobWithoutFailing(t *testing.T) {
	baseEnv(t)
	t.Setenv("ANTHROPIC_MODEL_FILTER", "[invalid-glob")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("an invalid ANTHROPIC_MODEL_FILTER glob must not fail Load, got: %v", err)
	}
	if len(cfg.AnthropicModelFilter) != 1 || cfg.AnthropicModelFilter[0] != "[invalid-glob" {
		t.Errorf("expected the raw (unvalidated) pattern to be carried through, got %v", cfg.AnthropicModelFilter)
	}
}

func TestLoad_AnthropicModelFilterParsesCommaList(t *testing.T) {
	baseEnv(t)
	t.Setenv("ANTHROPIC_MODEL_FILTER", "claude-haiku-*, claude-opus-* ,,claude-sonnet-*")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"claude-haiku-*", "claude-opus-*", "claude-sonnet-*"}
	if len(cfg.AnthropicModelFilter) != len(want) {
		t.Fatalf("AnthropicModelFilter = %v, want %v", cfg.AnthropicModelFilter, want)
	}
	for i, p := range want {
		if cfg.AnthropicModelFilter[i] != p {
			t.Errorf("AnthropicModelFilter[%d] = %q, want %q", i, cfg.AnthropicModelFilter[i], p)
		}
	}
}

func TestLoad_AnthropicModelFilterEmptyIsNil(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AnthropicModelFilter != nil {
		t.Errorf("expected a nil filter by default, got %v", cfg.AnthropicModelFilter)
	}
}

func TestLoad_AnthropicModelFilterSetAtParsesRFC3339(t *testing.T) {
	baseEnv(t)
	t.Setenv("ANTHROPIC_MODEL_FILTER_SET_AT", "2026-01-15T12:00:00Z")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-01-15T12:00:00Z")
	if !cfg.AnthropicModelFilterSetAt.Equal(want) {
		t.Errorf("AnthropicModelFilterSetAt = %v, want %v", cfg.AnthropicModelFilterSetAt, want)
	}
}

func TestLoad_AnthropicModelFilterSetAtUnsetIsZeroTime(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.AnthropicModelFilterSetAt.IsZero() {
		t.Errorf("expected the zero time when unset, got %v", cfg.AnthropicModelFilterSetAt)
	}
}

// TestLoad_AnthropicModelFilterSetAtUnparseableDoesNotFailLoad mirrors
// TestLoad_AnthropicModelFilterAcceptsInvalidGlobWithoutFailing: a malformed
// SET_AT must never be able to block the gateway from starting — it simply
// loses every last-writer-wins comparison against a DB row (the zero time).
func TestLoad_AnthropicModelFilterSetAtUnparseableDoesNotFailLoad(t *testing.T) {
	baseEnv(t)
	t.Setenv("ANTHROPIC_MODEL_FILTER_SET_AT", "not-a-timestamp")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("an unparseable ANTHROPIC_MODEL_FILTER_SET_AT must not fail Load, got: %v", err)
	}
	if !cfg.AnthropicModelFilterSetAt.IsZero() {
		t.Errorf("expected the zero time for an unparseable value, got %v", cfg.AnthropicModelFilterSetAt)
	}
}

func TestLoad_OpenAIModelFilterSetAtParsesRFC3339(t *testing.T) {
	baseEnv(t)
	t.Setenv("OPENAI_MODEL_FILTER_SET_AT", "2026-02-01T09:30:00Z")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-02-01T09:30:00Z")
	if !cfg.OpenAIModelFilterSetAt.Equal(want) {
		t.Errorf("OpenAIModelFilterSetAt = %v, want %v", cfg.OpenAIModelFilterSetAt, want)
	}
}

func TestLoad_OpenAIModelFilterSetAtUnsetIsZeroTime(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.OpenAIModelFilterSetAt.IsZero() {
		t.Errorf("expected the zero time when unset, got %v", cfg.OpenAIModelFilterSetAt)
	}
}

func TestLoad_AnthropicBaseURLRejectsHTTPOutsideDevMode(t *testing.T) {
	baseEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:9999")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for a non-https ANTHROPIC_BASE_URL outside DEV_MODE")
	}
}

func TestLoad_AnthropicBaseURLAllowsHTTPInDevMode(t *testing.T) {
	baseEnv(t)
	t.Setenv("DEV_MODE", "true")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:9999")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AnthropicBaseURL != "http://127.0.0.1:9999" {
		t.Errorf("AnthropicBaseURL = %q", cfg.AnthropicBaseURL)
	}
}

func TestLoad_AnthropicBaseURLEmptyByDefault(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AnthropicBaseURL != "" {
		t.Errorf("AnthropicBaseURL = %q, want empty by default", cfg.AnthropicBaseURL)
	}
}

// ---------------------------------------------------------------------------
// OpenAI knobs — same semantics as the Anthropic ones above, minus a default
// max tokens knob (OpenAI's max_completion_tokens is simply omitted when the
// caller doesn't set one; see internal/provider/openai).
// ---------------------------------------------------------------------------

func TestLoad_OpenAIModelFilterAcceptsInvalidGlobWithoutFailing(t *testing.T) {
	baseEnv(t)
	t.Setenv("OPENAI_MODEL_FILTER", "[invalid-glob")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("an invalid OPENAI_MODEL_FILTER glob must not fail Load, got: %v", err)
	}
	if len(cfg.OpenAIModelFilter) != 1 || cfg.OpenAIModelFilter[0] != "[invalid-glob" {
		t.Errorf("expected the raw (unvalidated) pattern to be carried through, got %v", cfg.OpenAIModelFilter)
	}
}

func TestLoad_OpenAIModelFilterParsesCommaList(t *testing.T) {
	baseEnv(t)
	t.Setenv("OPENAI_MODEL_FILTER", "gpt-4o*, gpt-5* ,,o3*")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"gpt-4o*", "gpt-5*", "o3*"}
	if len(cfg.OpenAIModelFilter) != len(want) {
		t.Fatalf("OpenAIModelFilter = %v, want %v", cfg.OpenAIModelFilter, want)
	}
	for i, p := range want {
		if cfg.OpenAIModelFilter[i] != p {
			t.Errorf("OpenAIModelFilter[%d] = %q, want %q", i, cfg.OpenAIModelFilter[i], p)
		}
	}
}

func TestLoad_OpenAIModelFilterEmptyIsNil(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OpenAIModelFilter != nil {
		t.Errorf("expected a nil filter by default, got %v", cfg.OpenAIModelFilter)
	}
}

func TestLoad_OpenAIBaseURLRejectsHTTPOutsideDevMode(t *testing.T) {
	baseEnv(t)
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:9998")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for a non-https OPENAI_BASE_URL outside DEV_MODE")
	}
}

func TestLoad_OpenAIBaseURLAllowsHTTPInDevMode(t *testing.T) {
	baseEnv(t)
	t.Setenv("DEV_MODE", "true")
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:9998")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OpenAIBaseURL != "http://127.0.0.1:9998" {
		t.Errorf("OpenAIBaseURL = %q", cfg.OpenAIBaseURL)
	}
}

func TestLoad_OpenAIBaseURLEmptyByDefault(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OpenAIBaseURL != "" {
		t.Errorf("OpenAIBaseURL = %q, want empty by default", cfg.OpenAIBaseURL)
	}
}

func TestLoad_AnthropicWorkspaceID(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"unset":     {"", ""},
		"valid":     {"wrkspc_01AbC-d9", "wrkspc_01AbC-d9"},
		"trimmed":   {"  wrkspc_x  ", "wrkspc_x"},
		"malformed": {"bad id!", ""},
		"too_long":  {strings.Repeat("a", 65), ""},
	} {
		t.Run(name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("ANTHROPIC_WORKSPACE_ID", tc.in)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("a bad workspace id must never fail Load: %v", err)
			}
			if cfg.AnthropicWorkspaceID != tc.want {
				t.Errorf("AnthropicWorkspaceID = %q, want %q", cfg.AnthropicWorkspaceID, tc.want)
			}
		})
	}
}

func TestLoad_UserHMACDirDefaultAndOverride(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.UserHMACDir != "/var/run/darkplum/user-hmac" {
		t.Errorf("UserHMACDir = %q, want the default", cfg.UserHMACDir)
	}
	t.Setenv("USER_HMAC_DIR", "/tmp/custom-hmac")
	cfg, err = Load()
	if err != nil || cfg.UserHMACDir != "/tmp/custom-hmac" {
		t.Errorf("UserHMACDir = %q err=%v", cfg.UserHMACDir, err)
	}
}
