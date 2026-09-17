package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// NewHTTPModelSource builds a Source that discovers models from an
// openai-compatible upstream's GET /v1/models endpoint (vLLM, LM Studio).
//
// A transport-level failure (dial error, non-2xx status, malformed body) is
// reported as an error so Manager retains the source's last-good model list
// instead of wiping it to empty. A clean 200 response is always a success —
// including one with zero models, which legitimately means the upstream has
// unloaded every model and the table should shrink to match.
func NewHTTPModelSource(name, kind, class, providerKey, baseURL string) Source {
	// Dial timeout bounds the TCP connect phase so a NetworkPolicy-dropped
	// SYN fails in ~5s rather than the OS default (~120s), matching the
	// pattern previously inlined in cmd/main.go's discovery helper.
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext},
	}
	target := baseURL + "/v1/models"

	discover := func(ctx context.Context) ([]string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: build models request: %w", name, err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s: models request failed: %w", name, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: models request returned %d", name, resp.StatusCode)
		}

		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, fmt.Errorf("%s: decode models response: %w", name, err)
		}

		ids := make([]string, 0, len(body.Data))
		for _, m := range body.Data {
			if m.ID != "" {
				ids = append(ids, m.ID)
			}
		}
		return ids, nil
	}

	return Source{Name: name, Kind: kind, Class: class, ProviderKey: providerKey, Discover: discover}
}
