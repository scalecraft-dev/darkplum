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

	AvailabilityOK         = "ok"
	AvailabilityNeedsOptIn = "needs_aws_opt_in"
	AvailabilityUnknown    = "unknown"
	// AvailabilityRetired: the cloud has retired the model (end of life). It
	// stays in the catalogue, shown as retired, and chat is refused before any
	// call to the cloud.
	AvailabilityRetired       = "retired"
	AvailabilityReasonRetired = "end_of_life"

	// DenialModelRetired is the error code and denial_code for a chat to a
	// retired model.
	DenialModelRetired = "model_retired"
	// ModelRetiredMessage is what the caller reads. The web UI says "the Models
	// page"; the CLI words its own hint.
	ModelRetiredMessage       = "This model has been retired by AWS and can no longer be used. An administrator can enable another model on the Models page."
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
	RequiredMode string `json:"required_mode,omitempty"`
}

// AvailabilityReporter is implemented by a provider that can say, per model,
// whether the account has opted in. Providers without it get no availability.
type AvailabilityReporter interface {
	ModelAvailability(model string) Availability
}

// RetiredChecker is implemented by a provider that knows a model is retired.
// The handler asks it before dispatching, so a retired model is refused without
// a call to the cloud.
type RetiredChecker interface {
	ModelRetired(model string) bool
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
