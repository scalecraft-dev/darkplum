// Package vendor holds what the CLI must agree on with the api and the gateway
// about vendor API keys (Anthropic, OpenAI): the Secret and ConfigMap keys, the
// catalog-filter rules, the skew-proof stamp, and the check that a key works.
//
// The api (services/api/internal/handler/providers.go) and the gateway
// (services/gateway/internal/config) are separate Go modules, so the rules are
// duplicated here and pinned by shared test vectors: services/provider-parity.json
// is read by this package's tests and by the api's. Change one, change the file.
//
// A key travels in an HTTP header and nowhere else: never argv, never a process.
package vendor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// Name identifies a vendor.
type Name string

const (
	Anthropic Name = "anthropic"
	OpenAI    Name = "openai"
)

// Cluster object names. Identical to the ones the api and the gateway read.
const (
	ProviderKeysSecret   = "darkplum-provider-keys"
	ProviderConfigMap    = "darkplum-provider-config"
	BillingKeysSecret    = "darkplum-billing-keys"
	AnthropicWorkspaceID = "ANTHROPIC_WORKSPACE_ID"
)

// Info is everything vendor-specific the commands need.
type Info struct {
	Name        Name
	Display     string
	KeyFile     string // darkplum-provider-keys data key
	BillingFile string // darkplum-billing-keys data key
	FilterKey   string // darkplum-provider-config data key
	EnvKey      string // env fallback for the inference key
	EnvBilling  string // env fallback for the billing key
	Example     string // example catalog filter
	Console     string // where to scope the key (closing advice)
	BillingHint string // what the billing key needs
}

// SetAtKey is the ConfigMap key holding the filter's last-write stamp.
func (i Info) SetAtKey() string { return i.FilterKey + "_SET_AT" }

// Get returns the vendor's constants.
func Get(n Name) (Info, bool) {
	switch n {
	case Anthropic:
		return Info{Name: Anthropic, Display: "Anthropic", KeyFile: "anthropic-api-key", BillingFile: "anthropic-billing-key",
			FilterKey: "ANTHROPIC_MODEL_FILTER", EnvKey: "ANTHROPIC_API_KEY", EnvBilling: "ANTHROPIC_ADMIN_KEY", Example: "claude-*",
			Console:     "a workspace-scoped key with a workspace spend limit (console.anthropic.com > Workspaces)",
			BillingHint: "an Admin API key (sk-ant-admin01-...) from console.anthropic.com > Settings > Admin keys; an inference key is refused by the usage endpoint"}, true
	case OpenAI:
		return Info{Name: OpenAI, Display: "OpenAI", KeyFile: "openai-api-key", BillingFile: "openai-billing-key",
			FilterKey: "OPENAI_MODEL_FILTER", EnvKey: "OPENAI_API_KEY", EnvBilling: "OPENAI_ADMIN_KEY", Example: "gpt-*",
			Console:     "a project-scoped key with a model allowlist and a project budget (platform.openai.com > Projects)",
			BillingHint: "an organisation admin key from platform.openai.com > Settings > Organization > Admin keys; a project key is refused by the costs endpoint"}, true
	}
	return Info{}, false
}

var (
	keyRe       = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	workspaceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// ValidKey is the character set every real vendor key fits in. It rejects the
// whitespace, quotes and control characters a copy-paste carries in.
func ValidKey(k string) bool { return keyRe.MatchString(k) }

// ValidWorkspace is an Anthropic workspace id such as wrkspc_01AbC.
func ValidWorkspace(w string) bool { return workspaceRe.MatchString(w) }

// BareWildcard reports whether p, with every '*' and '?' removed, is empty: a
// pattern that admits everything the key can reach. Mirrors the api's
// bareWildcardPattern; an empty pattern counts.
func BareWildcard(p string) bool {
	return strings.NewReplacer("*", "", "?", "").Replace(p) == ""
}

// Patterns splits a catalog filter on commas after removing every space, as the
// Makefile did. Empty entries are kept so that BareWildcard can refuse them.
func Patterns(filter string) []string {
	filter = strings.ReplaceAll(filter, " ", "")
	if filter == "" {
		return nil
	}
	return strings.Split(filter, ",")
}

// CheckPatterns refuses a bare wildcard (unless allowAll) and a pattern that is
// not a valid path.Match glob (the gateway would only report it per tick).
func CheckPatterns(patterns []string, allowAll bool) error {
	for _, p := range patterns {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("the pattern %q is not a valid glob: %w", p, err)
		}
		if BareWildcard(p) && !allowAll {
			return &BareWildcardError{Pattern: p}
		}
	}
	return nil
}

// BareWildcardError is the refusal for a filter that admits every model.
type BareWildcardError struct{ Pattern string }

func (e *BareWildcardError) Error() string {
	return fmt.Sprintf("the filter %q admits every model this key can reach", e.Pattern)
}

// Match returns the ids admitted by the filter, in order, with the gateway's
// semantics (path.Match per pattern, an empty pattern matches nothing).
func Match(ids, patterns []string) []string {
	var out []string
	for _, id := range ids {
		for _, p := range patterns {
			if p == "" {
				continue
			}
			if ok, err := path.Match(p, id); err == nil && ok {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// StampLayout is the filter stamp format: RFC 3339, UTC, whole seconds.
const StampLayout = "2006-01-02T15:04:05Z"

// NextStamp is max(now, old+1s) formatted for the ConfigMap. A stamp that does
// not parse counts as the epoch. The +1s keeps successive writes strictly
// increasing and outranks a stamp written by a skewed clock; the gateway uses
// whichever stamp is later. Same rule as the api's nextFilterSetAt.
func NextStamp(now time.Time, old string) string {
	next := now.UTC()
	prev := time.Unix(0, 0).UTC()
	if t, err := time.Parse(StampLayout, strings.TrimSpace(old)); err == nil {
		prev = t
	}
	if c := prev.Add(time.Second); c.After(next) {
		next = c
	}
	return next.Truncate(time.Second).Format(StampLayout)
}

// ---- checking a key against the vendor --------------------------------------

// Checker calls the vendor APIs. Base URLs are fields so tests can point it at a
// local server; production uses NewChecker.
type Checker struct {
	HTTP         *http.Client
	AnthropicURL string
	OpenAIURL    string
	Now          func() time.Time
}

// NewChecker uses the vendors' real endpoints and a 30 s timeout.
func NewChecker() *Checker {
	return &Checker{HTTP: &http.Client{Timeout: 30 * time.Second},
		AnthropicURL: "https://api.anthropic.com", OpenAIURL: "https://api.openai.com", Now: time.Now}
}

const maxBody = 8 << 20

// UnreachableError means the vendor could not be reached at all.
type UnreachableError struct{ Err error }

func (e *UnreachableError) Error() string { return "could not reach the vendor: " + e.Err.Error() }
func (e *UnreachableError) Unwrap() error { return e.Err }

// RejectedError is any answer other than 200.
type RejectedError struct{ Code int }

func (e *RejectedError) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }

// ErrWorkspaceNeeded means Anthropic wants an anthropic-workspace-id header with
// this (organisation-level) key.
var ErrWorkspaceNeeded = errors.New("the key needs an Anthropic workspace id")

func (c *Checker) get(ctx context.Context, target string, header http.Header) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, &UnreachableError{Err: stripURL(err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, &UnreachableError{Err: err}
	}
	return resp.StatusCode, body, nil
}

// stripURL keeps the request URL (which can carry query values) out of messages.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

func (c *Checker) inferenceRequest(v Name, workspace string) (string, http.Header) {
	h := http.Header{}
	if v == Anthropic {
		h.Set("anthropic-version", "2023-06-01")
		if workspace != "" {
			h.Set("anthropic-workspace-id", workspace)
		}
		return c.AnthropicURL + "/v1/models?limit=1000", h
	}
	return c.OpenAIURL + "/v1/models", h
}

func setKey(v Name, h http.Header, key string) {
	if v == Anthropic {
		h.Set("x-api-key", key)
		return
	}
	h.Set("Authorization", "Bearer "+key)
}

// Models lists the model ids the key can reach.
func (c *Checker) Models(ctx context.Context, v Name, key, workspace string) ([]string, error) {
	u, h := c.inferenceRequest(v, workspace)
	setKey(v, h, key)
	code, body, err := c.get(ctx, u, h)
	if err != nil {
		return nil, err
	}
	if code == http.StatusBadRequest && v == Anthropic && strings.Contains(string(body), "anthropic-workspace-id") {
		return nil, ErrWorkspaceNeeded
	}
	if code != http.StatusOK {
		return nil, &RejectedError{Code: code}
	}
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("the vendor's model list is not JSON: %w", err)
	}
	ids := make([]string, 0, len(r.Data))
	for _, d := range r.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	return ids, nil
}

// Billing checks the key can read the vendor's usage (admin scope).
func (c *Checker) Billing(ctx context.Context, v Name, key string) error {
	now := c.Now().UTC()
	h := http.Header{}
	setKey(v, h, key)
	var u string
	if v == Anthropic {
		h.Set("anthropic-version", "2023-06-01")
		today := now.Truncate(24 * time.Hour)
		u = fmt.Sprintf("%s/v1/organizations/usage_report/messages?starting_at=%s&ending_at=%s&bucket_width=1d",
			c.AnthropicURL, today.Add(-24*time.Hour).Format("2006-01-02T00:00:00Z"), today.Format("2006-01-02T00:00:00Z"))
	} else {
		u = fmt.Sprintf("%s/v1/organization/costs?start_time=%d&limit=1", c.OpenAIURL, now.Unix()-86400)
	}
	code, _, err := c.get(ctx, u, h)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return &RejectedError{Code: code}
	}
	return nil
}
