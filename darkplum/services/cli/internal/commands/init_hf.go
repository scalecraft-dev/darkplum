package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// hfModel is what init learns about a Hugging Face repo.
type hfModel struct {
	SHA         string
	Gated       bool // 401/403 anonymously, or gated flag set
	Safetensors bool
	WeightBytes int64 // 0 when the API does not report parameter counts
}

var (
	errHFNotFound = errors.New("model not found on Hugging Face")
	errHFGated    = errors.New("model is gated")
)

// hfClient talks to the public Hugging Face API. It never sends a token: init
// must not need, read or leak one, and an anonymous call is exactly what the
// cluster's model download does for a non-gated model.
type hfClient struct {
	base string
	http *http.Client
}

func newHFClient() *hfClient {
	base := os.Getenv("HF_ENDPOINT")
	if base == "" {
		base = "https://huggingface.co"
	}
	return &hfClient{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 10 * time.Second}}
}

func (c *hfClient) endpoint() string { return c.base }

var dtypeBytes = map[string]int64{"F64": 8, "I64": 8, "F32": 4, "I32": 4, "F16": 2, "BF16": 2, "I16": 2, "F8_E4M3": 1, "F8_E5M2": 1, "I8": 1, "U8": 1}

// Lookup resolves id to its current commit SHA. A gated repo returns
// errHFGated together with whatever was learned; a missing one errHFNotFound.
func (c *hfClient) Lookup(ctx context.Context, id string) (hfModel, error) {
	if err := validateModelID(id); err != nil {
		return hfModel{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/models/"+url.PathEscape(strings.SplitN(id, "/", 2)[0])+"/"+url.PathEscape(strings.SplitN(id, "/", 2)[1]), nil)
	if err != nil {
		return hfModel{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return hfModel{}, fmt.Errorf("could not reach %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return hfModel{Gated: true}, errHFGated
	case http.StatusNotFound:
		return hfModel{}, errHFNotFound
	default:
		return hfModel{}, fmt.Errorf("%s answered HTTP %d", c.base, resp.StatusCode)
	}
	var body struct {
		SHA      string          `json:"sha"`
		Gated    json.RawMessage `json:"gated"`
		Siblings []struct {
			Name string `json:"rfilename"`
		} `json:"siblings"`
		Safetensors struct {
			Parameters map[string]int64 `json:"parameters"`
		} `json:"safetensors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return hfModel{}, fmt.Errorf("unreadable response from %s: %w", c.base, err)
	}
	m := hfModel{SHA: body.SHA}
	if g := strings.TrimSpace(string(body.Gated)); g != "" && g != "false" && g != "null" {
		m.Gated = true
	}
	for _, s := range body.Siblings {
		if strings.HasSuffix(s.Name, ".safetensors") {
			m.Safetensors = true
			break
		}
	}
	for dt, n := range body.Safetensors.Parameters {
		m.WeightBytes += n * dtypeBytes[dt]
	}
	if m.Gated {
		return m, errHFGated
	}
	if err := validateSHA(m.SHA); err != nil {
		return m, fmt.Errorf("%s returned no usable commit SHA for %s", c.base, id)
	}
	return m, nil
}
