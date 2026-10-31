package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft-dev/darkplum/api/internal/kube"
)

func TestValidVendor(t *testing.T) {
	for _, v := range []string{"anthropic", "openai"} {
		if !validVendor(v) {
			t.Errorf("validVendor(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"bedrock", "Anthropic", "", "anthropic "} {
		if validVendor(v) {
			t.Errorf("validVendor(%q) = true, want false", v)
		}
	}
}

func TestSecretFileName(t *testing.T) {
	if got := secretFileName(vendorAnthropic); got != "anthropic-api-key" {
		t.Errorf("secretFileName(anthropic) = %q", got)
	}
	if got := secretFileName(vendorOpenAI); got != "openai-api-key" {
		t.Errorf("secretFileName(openai) = %q", got)
	}
}

func TestConfigFilterKeys(t *testing.T) {
	if got := configFilterKey(vendorAnthropic); got != "ANTHROPIC_MODEL_FILTER" {
		t.Errorf("configFilterKey(anthropic) = %q", got)
	}
	if got := configFilterSetAtKey(vendorAnthropic); got != "ANTHROPIC_MODEL_FILTER_SET_AT" {
		t.Errorf("configFilterSetAtKey(anthropic) = %q", got)
	}
	if got := configFilterKey(vendorOpenAI); got != "OPENAI_MODEL_FILTER" {
		t.Errorf("configFilterKey(openai) = %q", got)
	}
}

func TestValidKeyFormat(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"", false},
		{"tooshort", false},
		{"abcdefghijklmnopqrst", true}, // exactly 20 chars
		{"abcdefghijklmnopqrs", false}, // 19 chars, one short
		{"sk-ant-api03-AbCd_1234567890", true},
		{"has a space in it 1234567890", false},
		{"has\nnewline1234567890", false},
		{"quote\"injection1234567890", false},
	}
	for _, tc := range cases {
		if got := validKeyFormat(tc.key); got != tc.want {
			t.Errorf("validKeyFormat(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}

	// 512 exactly is valid; 513 is not.
	if !validKeyFormat(repeatChar('a', 512)) {
		t.Error("expected exactly 512 chars to be valid")
	}
	if validKeyFormat(repeatChar('a', 513)) {
		t.Error("expected 513 chars to be invalid")
	}
}

func repeatChar(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func TestBareWildcardPattern(t *testing.T) {
	cases := []struct {
		pattern string
		want    bool
	}{
		{"*", true},
		{"?", true},
		{"**??", true},
		{"claude-*", false},
		{"gpt-4*", false},
		{"claude-3-opus", false},
		{"", true}, // empty after stripping is still "admits everything" in the CLI's own test
	}
	for _, tc := range cases {
		if got := bareWildcardPattern(tc.pattern); got != tc.want {
			t.Errorf("bareWildcardPattern(%q) = %v, want %v", tc.pattern, got, tc.want)
		}
	}
}

func TestFirstBareWildcard(t *testing.T) {
	if _, ok := firstBareWildcard([]string{"claude-*", "gpt-4*"}); ok {
		t.Error("expected no bare wildcard among family globs")
	}
	pat, ok := firstBareWildcard([]string{"claude-*", "*", "gpt-4*"})
	if !ok || pat != "*" {
		t.Errorf("firstBareWildcard = %q, %v, want \"*\", true", pat, ok)
	}
}

func TestBareWildcardMessageNamesVendorAndExample(t *testing.T) {
	msg := bareWildcardMessage(vendorAnthropic, "*")
	if !contains(msg, "Anthropic") || !contains(msg, "claude-*") || !contains(msg, "allow_all") {
		t.Errorf("message missing expected content: %s", msg)
	}
	msg = bareWildcardMessage(vendorOpenAI, "?")
	if !contains(msg, "OpenAI") || !contains(msg, "gpt-*") {
		t.Errorf("message missing expected content: %s", msg)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

func TestNextFilterSetAt(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// No env, no previous: just now.
	if got := nextFilterSetAt(now, nil, nil); !got.Equal(now) {
		t.Errorf("got %v, want %v", got, now)
	}

	// env in the future outranks now.
	future := now.Add(time.Hour)
	if got := nextFilterSetAt(now, &future, nil); !got.Equal(future.Add(time.Second)) {
		t.Errorf("got %v, want %v", got, future.Add(time.Second))
	}

	// previous in the future outranks both now and env.
	furtherFuture := now.Add(2 * time.Hour)
	if got := nextFilterSetAt(now, &future, &furtherFuture); !got.Equal(furtherFuture.Add(time.Second)) {
		t.Errorf("got %v, want %v", got, furtherFuture.Add(time.Second))
	}

	// env in the past: now wins.
	past := now.Add(-time.Hour)
	if got := nextFilterSetAt(now, &past, nil); !got.Equal(now) {
		t.Errorf("got %v, want %v (env in the past must not win)", got, now)
	}

	// A laptop clock in the future for env must still be outranked by a
	// fresh UI write's now if now is later still -- GREATEST always wins,
	// regardless of which operand looks "more real".
	labFuture := now.Add(24 * time.Hour)
	laterNow := now.Add(48 * time.Hour)
	if got := nextFilterSetAt(laterNow, &labFuture, nil); !got.Equal(laterNow) {
		t.Errorf("got %v, want %v", got, laterNow)
	}
}

func TestRetryOnce(t *testing.T) {
	calls := 0
	err := retryOnce(func() error {
		calls++
		return nil
	})
	if err != nil || calls != 1 {
		t.Errorf("success case: err=%v calls=%d, want nil,1", err, calls)
	}

	calls = 0
	wantErr := errString("boom")
	err = retryOnce(func() error {
		calls++
		return wantErr
	})
	if err != wantErr || calls != 2 {
		t.Errorf("failure case: err=%v calls=%d, want %v,2", err, calls, wantErr)
	}

	calls = 0
	err = retryOnce(func() error {
		calls++
		if calls == 1 {
			return wantErr
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Errorf("recovers-on-retry case: err=%v calls=%d, want nil,2", err, calls)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestLastN(t *testing.T) {
	if got := lastN("sk-ant-api03-abcdefgh", 4); got != "efgh" {
		t.Errorf("lastN = %q", got)
	}
	if got := lastN("abc", 4); got != "abc" {
		t.Errorf("lastN(short) = %q", got)
	}
}

func TestTranslateVerifyError_UnknownCodeFallsBackToAuthFailed(t *testing.T) {
	got := translateVerifyError("some_new_code_this_api_has_never_seen")
	if got.Code != "auth_failed" {
		t.Errorf("code = %q, want auth_failed", got.Code)
	}
	if contains(got.Message, "some_new_code_this_api_has_never_seen") {
		t.Errorf("message must not echo the unknown code: %s", got.Message)
	}
}

func TestTranslateVerifyError_KnownCodesRoundTrip(t *testing.T) {
	for code := range verifyErrorMessages {
		got := translateVerifyError(code)
		if got.Code != code {
			t.Errorf("code = %q, want %q", got.Code, code)
		}
	}
}

// runKubeErrorJSON invokes kubeErrorJSON against a throwaway gin context and
// decodes the {"error":{"code","message"}} body it wrote.
func runKubeErrorJSON(t *testing.T, err error) (status int, body apiErrorBody) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	kubeErrorJSON(c, err)

	var decoded struct {
		Error apiErrorBody `json:"error"`
	}
	if jsonErr := json.Unmarshal(rec.Body.Bytes(), &decoded); jsonErr != nil {
		t.Fatalf("decode kubeErrorJSON body: %v (body = %s)", jsonErr, rec.Body.String())
	}
	return rec.Code, decoded.Error
}

// TestKubeErrorJSON_ExactRunbookMessages locks in the three 503 message
// strings confirmed with the web engineer and quoted in carla's runbook --
// changing this wording is a cross-team break, not just a copy edit.
func TestKubeErrorJSON_ExactRunbookMessages(t *testing.T) {
	cases := []struct {
		err     error
		code    string
		message string
	}{
		{kube.ErrNotInCluster, "not_in_cluster", "Installing a key from here needs Darkplum to be running in Kubernetes. Use the darkplum keys set command instead."},
		{kube.ErrObjectMissing, "object_missing", "This cluster was deployed before keys could be installed from here. Run darkplum deploy once, then try again."},
		{kube.ErrForbidden, "forbidden", "Darkplum is not permitted to write the vendor-key Secret on this cluster. Run darkplum deploy to restore its role, then try again."},
	}
	for _, tc := range cases {
		status, body := runKubeErrorJSON(t, tc.err)
		if status != http.StatusServiceUnavailable {
			t.Errorf("%v: status = %d, want 503", tc.err, status)
		}
		if body.Code != tc.code {
			t.Errorf("%v: code = %q, want %q", tc.err, body.Code, tc.code)
		}
		if body.Message != tc.message {
			t.Errorf("%v: message = %q, want %q", tc.err, body.Message, tc.message)
		}
	}

	// A wrapped kube error (as internal/kube itself never returns, but a
	// defensive check) still classifies correctly via errors.Is.
	wrapped := errors.New("wrapped: " + kube.ErrForbidden.Error())
	_ = wrapped // not wrapped with %w on purpose -- documents that plain string wrapping does NOT classify, only errors.Is-compatible wrapping does.
	status, body := runKubeErrorJSON(t, wrapped)
	if status != http.StatusServiceUnavailable || body.Code != "unavailable" {
		t.Errorf("an unrecognized error must fall back to the generic unavailable case, got %d/%q", status, body.Code)
	}
}
