package billing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	maxResponseBytes = 16 << 20
	maxPages         = 40
	requestTimeout   = 30 * time.Second
)

// jsonGetter does authenticated GETs. It never follows redirects (the key
// header must not be replayed elsewhere) and never reads an error response
// body into anything.
type jsonGetter struct {
	base   string
	client *http.Client
}

func newGetter(base string, client *http.Client) *jsonGetter {
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &jsonGetter{base: base, client: &c}
}

// get returns the HTTP status; out is decoded only on 2xx. A transport
// failure is *Error{unreachable}; an undecodable 2xx is vendor_error.
func (g *jsonGetter) get(ctx context.Context, path string, q url.Values, headers map[string]string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return 0, &Error{Kind: KindUnreachable}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return 0, &Error{Kind: KindUnreachable}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return resp.StatusCode, &Error{Kind: KindVendorError, HTTPStatus: resp.StatusCode}
	}
	return resp.StatusCode, nil
}

func ok2xx(status int) bool { return status >= 200 && status <= 299 }
