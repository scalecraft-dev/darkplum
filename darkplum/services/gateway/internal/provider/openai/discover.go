package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DiscoverModels queries GET /v1/models for every model id the configured
// key can see. Bounded by its own discoveryTimeout regardless of whatever
// timeout the caller (routing.Manager) already applies, so a slow response
// can never itself stall another source's tick.
//
// The returned ids are the RAW set the vendor reports — OpenAI's Models API
// lists embeddings/tts/whisper/image/moderation/realtime/audio models
// alongside chat-completions models, with no field reliably distinguishing
// them. Neither the operator's OPENAI_MODEL_FILTER glob nor the
// FilterChatModels denylist hygiene is applied here: both are applied by
// routing.NewOpenAISource, in that order (glob first, denylist after — see
// vendorsource.go), so this function's contract stays "everything this key
// can see" regardless of how routing chooses to narrow it.
//
// The returned error is untranslated (unlike Complete/Stream, which always
// return an *APIError) — routing.NewOpenAISource classifies it directly
// (auth_failed vs unreachable) via IsAuthError rather than through the
// wire-contract HTTP status translateVendorError produces, since a discovery
// failure never reaches an HTTP caller.
func (p *Provider) DiscoverModels(ctx context.Context) (map[string]bool, error) {
	key, _, err := p.creds.Key()
	if err != nil {
		return nil, err
	}

	discoverCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	endpoint := p.baseURL() + "/v1/models"
	httpReq, err := http.NewRequestWithContext(discoverCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("openai: building discovery request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: discovery request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai: reading discovery response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// body is deliberately discarded here, not attached to discoveryError
		// — see that type's doc comment on why the vendor's response body
		// (which may echo back part of the submitted key on a 401) must never
		// be retained anywhere on the discovery path.
		return nil, &discoveryError{statusCode: resp.StatusCode}
	}

	var listResp modelsListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		return nil, fmt.Errorf("openai: decoding discovery response: %w", err)
	}

	ids := make(map[string]bool, len(listResp.Data))
	for _, m := range listResp.Data {
		if m.ID == "" {
			continue
		}
		ids[m.ID] = true
	}
	return ids, nil
}

// VerifyKey checks that key is usable by calling GET /v1/models with it
// directly, returning the RAW model ids that key can see — no glob filter or
// FilterChatModels denylist hygiene is applied here; callers (e.g.
// internal/handler.ProvidersHandler) apply those separately, exactly the way
// routing.NewOpenAISource applies them to DiscoverModels' own raw result.
//
// A SCRATCH request is built from key: this Provider's shared, proxy-aware
// *http.Client (p.client) is reused for connection-level behaviour, but
// p.creds (credsource) is never read — verifying a candidate key an admin is
// about to install must never touch the gateway's actually-configured
// credential. Nothing about key is retained once this call returns.
//
// Bounded by its own timeout, independent of discoveryTimeout (the
// credential-driven DiscoverModels path) and of whatever timeout the caller
// already applies.
//
// Errors are translated exactly like Complete — a closed-vocabulary
// *APIError, never the vendor's own response body (which for a 401 can echo
// back part of the submitted key) — status is passed as nil so a failed
// verification of a CANDIDATE key never reports auth_failed into this
// provider's real (credsource-backed) status.
func (p *Provider) VerifyKey(ctx context.Context, key string) ([]string, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	endpoint := p.baseURL() + "/v1/models"
	httpReq, err := http.NewRequestWithContext(verifyCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("openai: building verify request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, translateTransportError(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai: reading verify response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// The vendor's response body is deliberately discarded — see
		// discoveryError's doc comment on why a 401 body (which can echo
		// back part of the submitted key) must never be retained anywhere.
		return nil, translateVendorError(resp.StatusCode, resp.Header, nil)
	}

	var listResp modelsListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		return nil, fmt.Errorf("openai: decoding verify response: %w", err)
	}

	ids := make([]string, 0, len(listResp.Data))
	for _, m := range listResp.Data {
		if m.ID == "" {
			continue
		}
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// chatModelDenylistSubstrings names the (lowercased) substrings a model id
// containing any of them is assumed NOT to be a chat-completions model.
// Deliberately small and conservative: this is HYGIENE, not a security
// bound — the operator's own OPENAI_MODEL_FILTER is the actual admission
// control (see FilterChatModels and the "no filter => no vendor models"
// ruling in routing.vendorSourceState.discover). A model id that happens to
// contain one of these substrings but IS a legitimate chat model would be
// wrongly excluded; that failure mode is a visible "model missing from the
// catalog" the operator can report, not a silent security gap.
var chatModelDenylistSubstrings = []string{
	"embedding", "tts", "whisper", "dall-e", "image", "moderation",
	"realtime", "audio", "transcribe", "davinci-002", "babbage-002", "sora",
}

// FilterChatModels drops ids that are obviously not chat-completions models,
// per chatModelDenylistSubstrings. Applied by routing.NewOpenAISource AFTER
// the operator's OPENAI_MODEL_FILTER glob (see vendorsource.go's postFilter
// hook), so a broad "*" filter alone can never put a non-chat model into the
// gateway's chat catalog.
func FilterChatModels(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if isLikelyNonChatModel(id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

func isLikelyNonChatModel(id string) bool {
	lower := strings.ToLower(id)
	for _, s := range chatModelDenylistSubstrings {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}
