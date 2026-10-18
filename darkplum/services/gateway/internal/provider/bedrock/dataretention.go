package bedrock

import (
	"context"
	"errors"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// Bedrock's data-retention ValidationException has no error code of its own;
// the message text is the only signal.
const dataRetentionMarker = "data retention mode"

// requiredFamilies are the model-id patterns known to need data-retention mode
// aws_review or above. Small and documented on purpose: extend it when AWS
// documents another family. Matched with path.Match against the full id, so a
// regional profile prefix ("us.", "global.") is covered by the leading "*".
var requiredFamilies = []string{
	"*anthropic.claude-fable-*",
}

func modelNeedsOptIn(model string) bool {
	for _, pat := range requiredFamilies {
		if ok, _ := path.Match(pat, model); ok {
			return true
		}
	}
	return false
}

// isDataRetentionValidation reports whether a Bedrock API error is the
// data-retention refusal.
func isDataRetentionValidation(code, message string) bool {
	return code == "ValidationException" && strings.Contains(strings.ToLower(message), dataRetentionMarker)
}

// normalizeMode maps the SDK's mode to the closed vocabulary.
func normalizeMode(m bedrocktypes.DataRetentionMode) string {
	switch m {
	case bedrocktypes.DataRetentionModeNone, bedrocktypes.DataRetentionModeDefault,
		bedrocktypes.DataRetentionModeAwsReview, bedrocktypes.DataRetentionModeProviderDataShare,
		bedrocktypes.DataRetentionModeInherit:
		return string(m)
	}
	return provider.DataRetentionModeUnknown
}

// DataRetentionError is returned by Complete/Stream for the classified refusal.
// It implements provider.APIError (so any path that only knows APIError still
// emits a scrubbed 422) and provider.DataRetentionRefusal (so the handler can
// add the route and set denial_code). Error() never carries Bedrock's text.
type DataRetentionError struct {
	Model        string
	AccountMode  string
	RequiredMode string
	Cause        error
}

func (e *DataRetentionError) Error() string {
	return "bedrock: model " + e.Model + " requires a higher account data-retention mode"
}
func (e *DataRetentionError) Unwrap() error { return e.Cause }
func (e *DataRetentionError) DataRetentionRefusal() (string, string, string) {
	return e.Model, e.AccountMode, e.RequiredMode
}
func (e *DataRetentionError) HTTPStatus() int { return 422 }
func (e *DataRetentionError) ResponseBody() []byte {
	return provider.DataRetentionErrorBody(e.Model, "", e.AccountMode, e.RequiredMode)
}
func (e *DataRetentionError) RetryAfterSeconds() int { return 0 }

var _ provider.APIError = (*DataRetentionError)(nil)
var _ provider.DataRetentionRefusal = (*DataRetentionError)(nil)

// dataRetentionState is the account-mode cache and the per-model refusal flags.
type dataRetentionState struct {
	mu        sync.Mutex
	mode      string // "" until first good read
	checkedAt time.Time
	warned    bool
	refused   map[string]bool
}

func (s *dataRetentionState) accountMode() (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == "" {
		return provider.DataRetentionModeUnknown, time.Time{}
	}
	return s.mode, s.checkedAt
}

func (s *dataRetentionState) setRefused(model string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v {
		if s.refused == nil {
			s.refused = make(map[string]bool)
		}
		s.refused[model] = true
	} else if s.refused != nil {
		delete(s.refused, model)
	}
}

func (s *dataRetentionState) isRefused(model string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused[model]
}

// RefreshAccountDataRetention reads the account's Bedrock data-retention mode
// for this provider's region (same credentials as discovery) and caches it. Any
// error (AccessDenied while the IAM role lacks bedrock:GetAccountDataRetention,
// network, ...) is logged once and swallowed: the last good value, if any, is
// kept, otherwise the mode stays "unknown". Never returns an error because it
// must never affect discovery or readiness.
func (p *BedrockProvider) RefreshAccountDataRetention(ctx context.Context) {
	if p.controlClient == nil {
		return
	}
	out, err := p.controlClient.GetAccountDataRetention(ctx, &bedrock.GetAccountDataRetentionInput{})
	st := &p.retention
	st.mu.Lock()
	defer st.mu.Unlock()
	if err != nil {
		if !st.warned {
			st.warned = true
			// Log the error class only; the raw text can carry account detail.
			slog.Warn("bedrock: could not read account data-retention mode; reporting unknown",
				"region", p.region, "kind", classifyReadError(err))
		}
		return
	}
	st.warned = false
	st.mode = normalizeMode(out.Mode)
	st.checkedAt = time.Now()
}

func classifyReadError(err error) string {
	var ae interface{ ErrorCode() string }
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return "error"
}

// AccountDataRetention implements provider.DataRetentionReporter.
func (p *BedrockProvider) AccountDataRetention() (string, *time.Time) {
	mode, at := p.retention.accountMode()
	if at.IsZero() {
		return mode, nil
	}
	return mode, &at
}

// ModelAvailability implements provider.AvailabilityReporter. Order: (a) a
// classified refusal since the last success, (b) known low mode + known
// required family, (c) known mode, (d) unknown.
func (p *BedrockProvider) ModelAvailability(model string) provider.Availability {
	mode, at := p.retention.accountMode()
	known := !at.IsZero() && mode != provider.DataRetentionModeUnknown
	av := provider.Availability{AccountMode: mode, RequiredMode: provider.RequiredDataRetentionMode}
	low := mode == "none" || mode == "default" || mode == "inherit"
	switch {
	case p.retention.isRefused(model):
		av.State = provider.AvailabilityNeedsOptIn
	case known && low && modelNeedsOptIn(model):
		av.State = provider.AvailabilityNeedsOptIn
	case known:
		av.State = provider.AvailabilityOK
	default:
		av.State = provider.AvailabilityUnknown
	}
	if av.State != provider.AvailabilityOK {
		av.Reason = provider.AvailabilityReasonDataRet
	}
	return av
}

var _ provider.AvailabilityReporter = (*BedrockProvider)(nil)
var _ provider.DataRetentionReporter = (*BedrockProvider)(nil)
