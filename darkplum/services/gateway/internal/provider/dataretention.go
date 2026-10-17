package provider

import (
	"encoding/json"
	"time"
)

// Cross-provider contract for "this model needs an account-level opt-in the
// customer's cloud account has not made" (Bedrock's data-retention mode today).

const (
	// DenialModelRequiresDataRetention is the closed error code, and the
	// inference_events.denial_code value, for a Bedrock model refused because
	// the AWS account's Bedrock data-retention mode is too low.
	DenialModelRequiresDataRetention = "model_requires_data_retention"

	// RequiredDataRetentionMode is the lowest mode that satisfies the models
	// that need an opt-in.
	RequiredDataRetentionMode = "aws_review"

	// DataRetentionModeUnknown is the closed-vocabulary placeholder when the
	// account mode could not be read.
	DataRetentionModeUnknown = "unknown"

	AvailabilityOK            = "ok"
	AvailabilityNeedsOptIn    = "needs_aws_opt_in"
	AvailabilityUnknown       = "unknown"
	AvailabilityReasonDataRet = "data_retention"
)

// DataRetentionRefusal is implemented by a provider error meaning "the model
// was refused because the account's data-retention mode is too low". The
// handler renders the 422 body itself (it alone knows the route name), so the
// provider's raw text never leaves the provider.
type DataRetentionRefusal interface {
	error
	DataRetentionRefusal() (model, accountMode, requiredMode string)
}

// Availability is the per-model availability object on GET /v1/models.
type Availability struct {
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	AccountMode  string `json:"account_mode"`
	RequiredMode string `json:"required_mode"`
}

// AvailabilityReporter is implemented by a provider that can say, per model,
// whether the account has opted in. Providers without it get no availability.
type AvailabilityReporter interface {
	ModelAvailability(model string) Availability
}

// DataRetentionReporter exposes the cached account mode for /health. mode is
// "unknown" until a read has succeeded; checkedAt is nil until then.
type DataRetentionReporter interface {
	AccountDataRetention() (mode string, checkedAt *time.Time)
}

// DataRetentionErrorBody renders the 422 wire body. route may be "".
func DataRetentionErrorBody(model, route, accountMode, requiredMode string) []byte {
	type detail struct {
		Message      string `json:"message"`
		Type         string `json:"type"`
		Code         string `json:"code"`
		Model        string `json:"model"`
		Route        string `json:"route,omitempty"`
		AccountMode  string `json:"account_mode"`
		RequiredMode string `json:"required_mode"`
	}
	b, _ := json.Marshal(struct {
		Error detail `json:"error"`
	}{detail{
		Message:      model + " on Bedrock requires your AWS account's Bedrock data-retention mode to be raised before it can answer. An administrator changes that in AWS; other models still work.",
		Type:         "invalid_request_error",
		Code:         DenialModelRequiresDataRetention,
		Model:        model,
		Route:        route,
		AccountMode:  accountMode,
		RequiredMode: requiredMode,
	}})
	return b
}
