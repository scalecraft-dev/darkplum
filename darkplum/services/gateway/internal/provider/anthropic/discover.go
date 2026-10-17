package anthropic

import (
	"context"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DiscoverModels queries the Models API for every model the configured key
// can see, and records each model's max output tokens (used by
// resolveMaxTokens to clamp a caller's max_tokens) as a side effect. Bounded
// by its own discoveryTimeout regardless of whatever timeout the caller
// (routing.Manager) already applies, so a paginated listing that stalls can
// never itself stall another source's tick.
//
// The returned error is untranslated (unlike Complete/Stream, which always
// return a *APIError) — routing.NewAnthropicSource classifies it directly
// (auth_failed vs unreachable) via IsAuthError rather than through the
// wire-contract HTTP status translateError produces, since a discovery
// failure never reaches an HTTP caller.
func (p *Provider) DiscoverModels(ctx context.Context) (map[string]bool, error) {
	client, err := p.sdkClientFor()
	if err != nil {
		return nil, err
	}

	discoverCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	ids := make(map[string]bool)
	maxTokens := make(map[string]int64)

	iter := client.Models.ListAutoPaging(discoverCtx, sdk.ModelListParams{}, p.requestOptions(ctx)...)
	for iter.Next() {
		m := iter.Current()
		if m.ID == "" {
			continue
		}
		ids[m.ID] = true
		if m.MaxTokens > 0 {
			maxTokens[m.ID] = m.MaxTokens
		}
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}

	p.modelsMu.Lock()
	p.maxOutputTokens = maxTokens
	p.modelsMu.Unlock()

	return ids, nil
}

// VerifyKey checks that key is usable by calling the Models API with it
// directly, returning the RAW model ids that key can see — no glob filter or
// OpenAI-style denylist hygiene is applied here; callers (e.g.
// internal/handler.ProvidersHandler) apply those separately, exactly the way
// routing.NewAnthropicSource applies them to DiscoverModels' own raw result.
//
// A SCRATCH sdk.Client is built from key, reusing this Provider's proxy-aware
// *http.Client for connection-level behaviour (dial/TLS timeouts, proxy
// config) but NEVER the version-cached sdkClient/sdkVersion fields
// sdkClientFor maintains for the gateway's actually-configured credential —
// verifying a candidate key an admin is about to install must never read,
// write, or otherwise disturb that cache, and must never touch p.creds
// (credsource) at all. Nothing about key is retained once this call returns.
//
// workspaceID is the SUBMITTED workspace to scope the check to (sent as
// anthropic-workspace-id when non-empty); it is not defaulted from the
// effective workspace, so verifying a candidate never borrows installed config.
//
// Bounded by its own timeout, independent of discoveryTimeout (which governs
// the credential-driven DiscoverModels path) and of whatever timeout the
// caller already applies.
//
// Errors are translated exactly like Complete/Stream — a closed-vocabulary
// *APIError, never the SDK's own error text (which for a 401 renders the raw
// response body plus Request-ID/Workspace-ID headers verbatim) — status is
// passed as nil so a failed verification of a CANDIDATE key never reports
// auth_failed into this provider's real (credsource-backed) status.
func (p *Provider) VerifyKey(ctx context.Context, key, workspaceID string) ([]string, error) {
	opts := []option.RequestOption{
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey(key),
		option.WithHTTPClient(p.client),
		option.WithMaxRetries(sdkMaxRetries),
	}
	if p.cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(p.cfg.BaseURL))
	}
	client := sdk.NewClient(opts...)

	verifyCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	ids := make([]string, 0)
	iter := client.Models.ListAutoPaging(verifyCtx, sdk.ModelListParams{}, workspaceOptions(workspaceID)...)
	for iter.Next() {
		m := iter.Current()
		if m.ID == "" {
			continue
		}
		ids = append(ids, m.ID)
	}
	if err := iter.Err(); err != nil {
		return nil, translateError(err, nil, p.Name())
	}
	return ids, nil
}
