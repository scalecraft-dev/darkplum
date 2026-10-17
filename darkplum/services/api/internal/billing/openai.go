package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// OpenAIBaseURL is the production API origin.
const OpenAIBaseURL = "https://api.openai.com"

// OpenAIClient reads the organization usage and costs endpoints. Requires an
// org Admin key (sk-admin-…).
type OpenAIClient struct{ g *jsonGetter }

func NewOpenAIClient(baseURL string, client *http.Client) *OpenAIClient {
	return &OpenAIClient{g: newGetter(baseURL, client)}
}

func oaiHeaders(key string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + key}
}

// An OpenAI inference key is any sk- key that is not an sk-admin- key.
func oaiInferenceKey(key string) bool {
	return strings.HasPrefix(key, "sk-") && !strings.HasPrefix(key, "sk-admin")
}

type oaiUsageResp struct {
	Data []struct {
		StartTime int64 `json:"start_time"`
		Results   []struct {
			InputTokens       int64   `json:"input_tokens"`
			InputCachedTokens int64   `json:"input_cached_tokens"`
			OutputTokens      int64   `json:"output_tokens"`
			ProjectID         *string `json:"project_id"`
			APIKeyID          *string `json:"api_key_id"`
			Model             *string `json:"model"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool    `json:"has_more"`
	NextPage *string `json:"next_page"`
}

type oaiCostResp struct {
	Data []struct {
		StartTime int64 `json:"start_time"`
		Results   []struct {
			Amount struct {
				Value json.Number `json:"value"`
			} `json:"amount"`
			LineItem  *string `json:"line_item"`
			ProjectID *string `json:"project_id"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool    `json:"has_more"`
	NextPage *string `json:"next_page"`
}

func (c *OpenAIClient) usage(ctx context.Context, key string, from, to time.Time, limit int) ([]UsageRow, int, *Error) {
	var rows []UsageRow
	buckets := 0
	page := ""
	for i := 0; i < maxPages; i++ {
		q := url.Values{}
		q.Set("start_time", strconv.FormatInt(from.Unix(), 10))
		q.Set("end_time", strconv.FormatInt(to.Unix(), 10))
		q.Set("bucket_width", "1d")
		q.Set("limit", strconv.Itoa(limit))
		q["group_by"] = []string{"model", "project_id", "api_key_id"}
		if page != "" {
			q.Set("page", page)
		}
		var resp oaiUsageResp
		status, err := c.g.get(ctx, "/v1/organization/usage/completions", q, oaiHeaders(key), &resp)
		if err != nil {
			return nil, 0, AsError(err)
		}
		if !ok2xx(status) {
			return nil, 0, classify(status, oaiInferenceKey(key))
		}
		for _, b := range resp.Data {
			buckets++
			day := dayOf(time.Unix(b.StartTime, 0))
			for _, r := range b.Results {
				m := "(unknown)"
				if r.Model != nil && *r.Model != "" {
					m = *r.Model
				}
				// OpenAI input_tokens INCLUDES cached tokens; normalise to
				// uncached + cache_read.
				cached := min(r.InputCachedTokens, r.InputTokens)
				rows = append(rows, UsageRow{Day: day, Model: m, WorkspaceID: r.ProjectID, APIKeyID: r.APIKeyID,
					UncachedInput: r.InputTokens - cached, CacheRead: cached, OutputToken: r.OutputTokens})
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
func (c *OpenAIClient) Verify(ctx context.Context, key string, now time.Time) (Sample, error) {
	d := startOfDay(now)
	_, buckets, err := c.usage(ctx, key, d, d.Add(24*time.Hour), 1)
	if err != nil {
		return Sample{}, err
	}
	return Sample{Day: dayOf(d), Buckets: buckets}, nil
}

// modelFromLineItem: line items look like "gpt-4o-2024-08-06, input"; the
// part before the first ", " is the model when it has no spaces.
func modelFromLineItem(li string) *string {
	head, _, found := strings.Cut(li, ", ")
	head = strings.TrimSpace(head)
	if !found || head == "" || strings.Contains(head, " ") {
		return nil
	}
	return &head
}

// costs returns a nil rows + unreachable error on 404 (some orgs report the
// endpoint missing): the caller keeps the usage rows.
func (c *OpenAIClient) costs(ctx context.Context, key string, from, to time.Time) ([]CostRow, *Error) {
	var rows []CostRow
	page := ""
	for i := 0; i < maxPages; i++ {
		q := url.Values{}
		q.Set("start_time", strconv.FormatInt(from.Unix(), 10))
		q.Set("end_time", strconv.FormatInt(to.Unix(), 10))
		q.Set("bucket_width", "1d")
		q.Set("limit", "31")
		q.Set("group_by", "line_item")
		if page != "" {
			q.Set("page", page)
		}
		var resp oaiCostResp
		status, err := c.g.get(ctx, "/v1/organization/costs", q, oaiHeaders(key), &resp)
		if err != nil {
			return nil, AsError(err)
		}
		if status == http.StatusNotFound {
			return nil, &Error{Kind: KindUnreachable, HTTPStatus: status}
		}
		if !ok2xx(status) {
			return nil, classify(status, oaiInferenceKey(key))
		}
		for _, b := range resp.Data {
			day := dayOf(time.Unix(b.StartTime, 0))
			for _, r := range b.Results {
				usd, ok := parseRat(r.Amount.Value.String())
				if !ok {
					return nil, &Error{Kind: KindVendorError, HTTPStatus: status}
				}
				desc, model := "(total)", (*string)(nil)
				if r.LineItem != nil && *r.LineItem != "" {
					desc = *r.LineItem
					model = modelFromLineItem(desc)
				}
				rows = append(rows, CostRow{Day: day, Description: desc, Model: model, WorkspaceID: r.ProjectID,
					Cents: usd.Mul(usd, newRat100())})
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
func (c *OpenAIClient) Pull(ctx context.Context, key string, from, to time.Time) (*Report, error) {
	usage, _, uerr := c.usage(ctx, key, from, to, 31)
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
