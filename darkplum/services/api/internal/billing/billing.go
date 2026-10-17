// Package billing pulls vendor-reported usage and cost (Anthropic and OpenAI
// admin reports, AWS Cost Explorer for Bedrock) so the admin UI can reconcile
// Darkplum's own estimates against what the vendor says it billed.
//
// Hygiene, mirroring the inference-key flow: the billing key is read from a
// mounted file on every use, held only in local variables, sent only in the
// vendor request header, and NEVER logged, echoed, stored in Postgres, or
// wrapped into an error. Vendor response bodies are never put in errors
// either: every failure is reduced to a closed Error{Kind, HTTPStatus}.
package billing

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Vendors with a billing source. Bedrock has no key (AWS credential chain).
const (
	Anthropic = "anthropic"
	OpenAI    = "openai"
	Bedrock   = "bedrock"
)

// Closed error kinds (also the vendor_billing_status.state values for
// failures).
const (
	KindAuthFailed    = "auth_failed"
	KindWrongKeyType  = "wrong_key_type"
	KindUnreachable   = "unreachable"
	KindVendorError   = "vendor_error"
	StateNotConfigure = "not_configured"
	StateOK           = "ok"
	StatePulling      = "pulling"
	// StatePendingMount: key stored (configured=true) but the cluster has not
	// mounted the Secret file into this pod yet; retried on the next poll.
	StatePendingMount = "pending_mount"
	KindStoreFailed   = "store_failed" // last_error_kind only
)

// Error is the only error type vendor clients return. Its text is fixed per
// kind: it can never carry a key or a vendor response body.
type Error struct {
	Kind       string
	HTTPStatus int // vendor's HTTP status for vendor_error / wrong_key_type / auth_failed; 0 when none
}

func (e *Error) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("billing: %s (http %d)", e.Kind, e.HTTPStatus)
	}
	return "billing: " + e.Kind
}

// AsError extracts the *Error, or wraps anything else as vendor_error.
func AsError(err error) *Error {
	var be *Error
	if errors.As(err, &be) {
		return be
	}
	return &Error{Kind: KindVendorError}
}

// UsageRow is one vendor-reported token-usage row for a UTC day.
type UsageRow struct {
	Day                                                  string // YYYY-MM-DD
	Model                                                string
	WorkspaceID, APIKeyID                                *string
	UncachedInput, CacheRead, CacheCreation, OutputToken int64
}

// CostRow is one vendor-reported cost row for a UTC day, in cents.
type CostRow struct {
	Day         string
	Description string
	Model       *string
	WorkspaceID *string
	Cents       *big.Rat
}

// Report is one vendor pull. CostsErr is set when the usage report succeeded
// but the cost report did not (usage rows are still stored).
type Report struct {
	Usage    []UsageRow
	Costs    []CostRow
	CostsErr *Error
}

// Sample is the verify-time proof the key can read the usage report.
type Sample struct {
	Day     string `json:"day"`
	Buckets int    `json:"buckets"`
}

// Vendor is an Anthropic/OpenAI report client.
type Vendor interface {
	Verify(ctx context.Context, key string, now time.Time) (Sample, error)
	Pull(ctx context.Context, key string, from, to time.Time) (*Report, error)
}

// KeyFile is the file name of a vendor's billing key inside the keys dir.
func KeyFile(vendor string) string { return vendor + "-billing-key" }

// KeyReader reads billing keys from the mounted Secret directory. Every call
// re-reads the file (Secret volume updates swap a symlink; a read during the
// swap can transiently fail, so it retries briefly).
type KeyReader struct{ Dir string }

// Read returns the vendor's key, or ok=false when absent/empty. The value is
// never logged by this package.
func (r KeyReader) Read(vendor string) (key string, ok bool) {
	if r.Dir == "" || (vendor != Anthropic && vendor != OpenAI) {
		return "", false
	}
	for attempt := 0; attempt < 3; attempt++ {
		b, err := os.ReadFile(filepath.Join(r.Dir, KeyFile(vendor)))
		if err == nil {
			k := strings.TrimSpace(string(b))
			return k, k != ""
		}
		if !os.IsNotExist(err) {
			return "", false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", false
}

// Last4 is the audit fingerprint of a key.
func Last4(key string) string {
	if len(key) <= 4 {
		return key
	}
	return key[len(key)-4:]
}

// MessageFor is the closed admin-readable text for an error kind.
func MessageFor(vendor string, e *Error) string {
	name := map[string]string{Anthropic: "Anthropic", OpenAI: "OpenAI", Bedrock: "AWS Cost Explorer"}[vendor]
	switch e.Kind {
	case KindWrongKeyType:
		if vendor == OpenAI {
			return "This key cannot read organization usage; the usage reports need an Admin key (sk-admin-…) created under Organization settings > Admin keys."
		}
		return "This looks like an inference key; the usage reports need an Admin API key (sk-ant-admin01-…) created under Settings > Admin keys."
	case KindAuthFailed:
		return name + " rejected this key. Check it was copied completely and has not been revoked."
	case KindUnreachable:
		return "Could not reach " + name + "'s usage API; try again."
	default:
		if e.HTTPStatus != 0 {
			return fmt.Sprintf("%s's usage API returned an error (HTTP %d).", name, e.HTTPStatus)
		}
		return name + "'s usage API returned an unexpected response."
	}
}

// classify maps a non-2xx vendor status to a closed error. inferenceKey says
// the key's prefix shows it is an inference (not admin) key.
func classify(status int, inferenceKey bool) *Error {
	switch {
	case status == 401 || status == 403:
		if inferenceKey || status == 403 {
			return &Error{Kind: KindWrongKeyType, HTTPStatus: status}
		}
		return &Error{Kind: KindAuthFailed, HTTPStatus: status}
	default:
		return &Error{Kind: KindVendorError, HTTPStatus: status}
	}
}

func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func parseRat(s string) (*big.Rat, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "/") {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}
