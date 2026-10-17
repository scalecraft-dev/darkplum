package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AnthropicBaseURL is the production API origin.
const AnthropicBaseURL = "https://api.anthropic.com"

// AnthropicClient reads the Admin API usage and cost reports. Requires an
// Admin API key (sk-ant-admin01-…); inference and workspace keys are refused
// by the endpoints.
type AnthropicClient struct{ g *jsonGetter }

func NewAnthropicClient(baseURL string, client *http.Client) *AnthropicClient {
	return &AnthropicClient{g: newGetter(baseURL, client)}
}

func anthHeaders(key string) map[string]string {
	return map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
}

func anthInferenceKey(key string) bool { return strings.HasPrefix(key, "sk-ant-api") }

type anthUsageResp struct {
	Data []struct {
		StartingAt string `json:"starting_at"`
		Results    []struct {
			UncachedInputTokens      int64 `json:"uncached_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheCreation            struct {
				E1h int64 `json:"ephemeral_1h_input_tokens"`
				E5m int64 `json:"ephemeral_5m_input_tokens"`
			} `json:"cache_creation"`
			CacheReadInputTokens int64   `json:"cache_read_input_tokens"`
			OutputTokens         int64   `json:"output_tokens"`
			Model                *string `json:"model"`
			WorkspaceID          *string `json:"workspace_id"`
			APIKeyID             *string `json:"api_key_id"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool    `json:"has_more"`
	NextPage *string `json:"next_page"`
}

type anthCostResp struct {
	Data []struct {
		StartingAt string `json:"starting_at"`
		Results    []struct {
			Amount      json.RawMessage `json:"amount"`
			Description string          `json:"description"`
			Model       *string         `json:"model"`
			WorkspaceID *string         `json:"workspace_id"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool    `json:"has_more"`
	NextPage *string `json:"next_page"`
}

func (c *AnthropicClient) usage(ctx context.Context, key string, from, to time.Time, limit int, groups bool) ([]UsageRow, int, *Error) {
	var rows []UsageRow
	buckets := 0
	page := ""
	for i := 0; i < maxPages; i++ {
		q := url.Values{}
		q.Set("starting_at", from.UTC().Format(time.RFC3339))
		q.Set("ending_at", to.UTC().Format(time.RFC3339))
		q.Set("bucket_width", "1d")
		q.Set("limit", strconv.Itoa(limit))
		if groups {
			q["group_by[]"] = []string{"model", "api_key_id", "workspace_id"}
		}
		if page != "" {
			q.Set("page", page)
		}
		var resp anthUsageResp
		status, err := c.g.get(ctx, "/v1/organizations/usage_report/messages", q, anthHeaders(key), &resp)
		if err != nil {
			return nil, 0, AsError(err)
		}
		if !ok2xx(status) {
			return nil, 0, classify(status, anthInferenceKey(key))
		}
		for _, b := range resp.Data {
			buckets++
			t, perr := time.Parse(time.RFC3339, b.StartingAt)
			if perr != nil {
				return nil, 0, &Error{Kind: KindVendorError, HTTPStatus: status}
			}
			for _, r := range b.Results {
				cc := r.CacheCreationInputTokens
				if cc == 0 {
					cc = r.CacheCreation.E1h + r.CacheCreation.E5m
				}
				m := "(unknown)"
				if r.Model != nil && *r.Model != "" {
					m = *r.Model
				}
				rows = append(rows, UsageRow{Day: dayOf(t), Model: m, WorkspaceID: r.WorkspaceID, APIKeyID: r.APIKeyID,
					UncachedInput: r.UncachedInputTokens, CacheRead: r.CacheReadInputTokens, CacheCreation: cc, OutputToken: r.OutputTokens})
			}
		}
		if !resp.HasMore || resp.NextPage == nil || *resp.NextPage == "" {
			return rows, buckets, nil
		}
		page = *resp.NextPage
	}
	return rows, buckets, nil
}

// Verify reads one day of the usage report.
func (c *AnthropicClient) Verify(ctx context.Context, key string, now time.Time) (Sample, error) {
	d := startOfDay(now)
	_, buckets, err := c.usage(ctx, key, d, d.Add(24*time.Hour), 1, false)
	if err != nil {
		return Sample{}, err
	}
	return Sample{Day: dayOf(d), Buckets: buckets}, nil
}

var claudeIDRe = regexp.MustCompile(`claude-[a-z0-9][a-z0-9.\-]*`)

// modelFromDescription is the best-effort description -> model parse: a
// claude-… id embedded in the description, else nil (the reconciliation then
// keys the row by its description).
func modelFromDescription(d string) *string {
	if m := claudeIDRe.FindString(strings.ToLower(d)); m != "" {
		m = strings.TrimRight(m, "-.")
		return &m
	}
	return nil
}

func (c *AnthropicClient) costs(ctx context.Context, key string, from, to time.Time) ([]CostRow, *Error) {
	var rows []CostRow
	page := ""
	for i := 0; i < maxPages; i++ {
		q := url.Values{}
		q.Set("starting_at", from.UTC().Format(time.RFC3339))
		q.Set("ending_at", to.UTC().Format(time.RFC3339))
		q.Set("limit", "31")
		q["group_by[]"] = []string{"description", "workspace_id"}
		if page != "" {
			q.Set("page", page)
		}
		var resp anthCostResp
		status, err := c.g.get(ctx, "/v1/organizations/cost_report", q, anthHeaders(key), &resp)
		if err != nil {
			return nil, AsError(err)
		}
		if !ok2xx(status) {
			return nil, classify(status, anthInferenceKey(key))
		}
		for _, b := range resp.Data {
			t, perr := time.Parse(time.RFC3339, b.StartingAt)
			if perr != nil {
				return nil, &Error{Kind: KindVendorError, HTTPStatus: status}
			}
			for _, r := range b.Results {
				amt := strings.Trim(strings.TrimSpace(string(r.Amount)), `"`)
				cents, ok := parseRat(amt)
				if !ok {
					return nil, &Error{Kind: KindVendorError, HTTPStatus: status}
				}
				model := r.Model
				if model == nil || *model == "" {
					model = modelFromDescription(r.Description)
				}
				rows = append(rows, CostRow{Day: dayOf(t), Description: r.Description, Model: model, WorkspaceID: r.WorkspaceID, Cents: cents})
			}
		}
		if !resp.HasMore || resp.NextPage == nil || *resp.NextPage == "" {
			return rows, nil
		}
		page = *resp.NextPage
	}
	return rows, nil
}

// Pull fetches usage and cost for [from, to).
func (c *AnthropicClient) Pull(ctx context.Context, key string, from, to time.Time) (*Report, error) {
	usage, _, uerr := c.usage(ctx, key, from, to, 31, true)
	if uerr != nil {
		return nil, uerr
	}
	rep := &Report{Usage: AggregateUsage(usage)}
	costs, cerr := c.costs(ctx, key, from, to)
	if cerr != nil {
		rep.CostsErr = cerr
		return rep, nil
	}
	rep.Costs = AggregateCosts(costs)
	return rep, nil
}
