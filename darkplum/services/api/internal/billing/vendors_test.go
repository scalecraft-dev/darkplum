package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/smithy-go"
)

const sentinel = "sk-ant-admin01-SENTINELSECRETVALUE0123456789"

var from = time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
var to = time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)

// fakeVendor serves canned JSON per path+page and records requests.
type fakeVendor struct {
	*httptest.Server
	mu       sync.Mutex
	status   map[string]int    // path -> forced status
	bodies   map[string]string // path|page -> body
	requests []*http.Request
}

func newFakeVendor(t *testing.T) *fakeVendor {
	f := &fakeVendor{status: map[string]int{}, bodies: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(context.Background()))
		st := f.status[r.URL.Path]
		body, ok := f.bodies[r.URL.Path+"|"+r.URL.Query().Get("page")]
		f.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			// hostile vendor: echoes the credential back in the body
			fmt.Fprintf(w, `{"error":{"message":"bad key %s"}}`, sentinel)
			return
		}
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(f.Close)
	return f
}

func sp(s string) *string { return &s }

func TestAnthropic_PullPaginatesAndParses(t *testing.T) {
	f := newFakeVendor(t)
	f.bodies["/v1/organizations/usage_report/messages|"] = `{"data":[{"starting_at":"2026-10-01T00:00:00Z","results":[
		{"uncached_input_tokens":100,"cache_creation":{"ephemeral_1h_input_tokens":10,"ephemeral_5m_input_tokens":5},"cache_read_input_tokens":7,"output_tokens":50,"model":"claude-sonnet-4-5","workspace_id":"wrkspc_1","api_key_id":null}]}],
		"has_more":true,"next_page":"p2"}`
	f.bodies["/v1/organizations/usage_report/messages|p2"] = `{"data":[{"starting_at":"2026-10-01T00:00:00Z","results":[
		{"uncached_input_tokens":1,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":4,"model":"claude-sonnet-4-5","workspace_id":"wrkspc_1","api_key_id":null}]}],"has_more":false,"next_page":null}`
	f.bodies["/v1/organizations/cost_report|"] = `{"data":[{"starting_at":"2026-10-01T00:00:00Z","results":[
		{"amount":"12.5","description":"Claude Sonnet 4.5 Usage - claude-sonnet-4-5 input","workspace_id":null,"model":null},
		{"amount":"3","description":"Web search","workspace_id":null}]}],"has_more":false}`
	c := NewAnthropicClient(f.URL, nil)
	rep, err := c.Pull(context.Background(), sentinel, from, to)
	if err != nil || rep.CostsErr != nil {
		t.Fatalf("pull: %v %v", err, rep)
	}
	// both pages share a key -> merged: 100+1, cache_creation 15+2, read 7+3, out 50+4
	if len(rep.Usage) != 1 || rep.Usage[0].UncachedInput != 101 || rep.Usage[0].CacheCreation != 17 || rep.Usage[0].CacheRead != 10 || rep.Usage[0].OutputToken != 54 || rep.Usage[0].Day != "2026-10-01" {
		t.Fatalf("usage %+v", rep.Usage)
	}
	if len(rep.Costs) != 2 || rep.Costs[0].Cents.FloatString(1) != "12.5" || rep.Costs[0].Model == nil || *rep.Costs[0].Model != "claude-sonnet-4-5" || rep.Costs[1].Model != nil {
		t.Fatalf("costs %+v", rep.Costs)
	}
	r := f.requests[0]
	if r.Header.Get("x-api-key") != sentinel || r.Header.Get("anthropic-version") == "" {
		t.Fatal("auth headers")
	}
	q := r.URL.Query()
	if q.Get("bucket_width") != "1d" || len(q["group_by[]"]) != 3 || q.Get("starting_at") != "2026-09-17T00:00:00Z" {
		t.Fatalf("query %v", q)
	}
	if got := f.requests[1].URL.Query().Get("page"); got != "p2" {
		t.Fatalf("page param %q", got)
	}
	last := f.requests[len(f.requests)-1].URL.Query()
	if last["group_by[]"][0] != "description" || last["group_by[]"][1] != "workspace_id" {
		t.Fatalf("cost group_by %v", last)
	}
}

func TestClassification_AndNoKeyLeak(t *testing.T) {
	cases := []struct {
		name   string
		client func(url string) Vendor
		key    string
		status int
		kind   string
	}{
		{"anthropic 401 admin key", func(u string) Vendor { return NewAnthropicClient(u, nil) }, "sk-ant-admin01-" + strings.Repeat("a", 30), 401, KindAuthFailed},
		{"anthropic 401 inference key", func(u string) Vendor { return NewAnthropicClient(u, nil) }, "sk-ant-api03-" + strings.Repeat("a", 30), 401, KindWrongKeyType},
		{"anthropic 403", func(u string) Vendor { return NewAnthropicClient(u, nil) }, "sk-ant-admin01-" + strings.Repeat("a", 30), 403, KindWrongKeyType},
		{"anthropic 404", func(u string) Vendor { return NewAnthropicClient(u, nil) }, sentinel, 404, KindVendorError},
		{"anthropic 500", func(u string) Vendor { return NewAnthropicClient(u, nil) }, sentinel, 500, KindVendorError},
		{"anthropic 429", func(u string) Vendor { return NewAnthropicClient(u, nil) }, sentinel, 429, KindVendorError},
		{"openai 401 admin key", func(u string) Vendor { return NewOpenAIClient(u, nil) }, "sk-admin-" + strings.Repeat("a", 30), 401, KindAuthFailed},
		{"openai 401 project key", func(u string) Vendor { return NewOpenAIClient(u, nil) }, "sk-proj-" + strings.Repeat("a", 30), 401, KindWrongKeyType},
		{"openai 403", func(u string) Vendor { return NewOpenAIClient(u, nil) }, "sk-admin-" + strings.Repeat("a", 30), 403, KindWrongKeyType},
		{"openai 500", func(u string) Vendor { return NewOpenAIClient(u, nil) }, "sk-admin-" + strings.Repeat("a", 30), 500, KindVendorError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVendor(t)
			for _, p := range []string{"/v1/organizations/usage_report/messages", "/v1/organization/usage/completions"} {
				f.status[p] = tc.status
			}
			c := tc.client(f.URL)
			_, verr := c.Verify(context.Background(), tc.key, to)
			_, perr := c.Pull(context.Background(), tc.key, from, to)
			for _, err := range []error{verr, perr} {
				e := AsError(err)
				if e.Kind != tc.kind || e.HTTPStatus != tc.status {
					t.Fatalf("got %+v want %s/%d", e, tc.kind, tc.status)
				}
				if strings.Contains(err.Error(), "SENTINEL") || strings.Contains(MessageFor(Anthropic, e), "SENTINEL") || strings.Contains(err.Error(), tc.key) {
					t.Fatalf("leak: %v", err)
				}
			}
		})
	}
	// transport failure
	dead := httptest.NewServer(nil)
	dead.Close()
	if _, err := NewAnthropicClient(dead.URL, nil).Verify(context.Background(), sentinel, to); AsError(err).Kind != KindUnreachable {
		t.Fatalf("dead server: %v", err)
	}
	// wrong_key_type carries the closed text
	if m := MessageFor(Anthropic, &Error{Kind: KindWrongKeyType}); !strings.Contains(m, "This looks like an inference key; the usage reports need an Admin API key (sk-ant-admin01-…) created under Settings > Admin keys.") {
		t.Fatal(m)
	}
}

func TestOpenAI_PullCachedSplitPaginationAndCosts404(t *testing.T) {
	f := newFakeVendor(t)
	f.bodies["/v1/organization/usage/completions|"] = `{"data":[{"start_time":1790000000,"results":[
		{"input_tokens":1000,"input_cached_tokens":400,"output_tokens":20,"model":"gpt-4o","project_id":"proj_1","api_key_id":"key_1"}]}],"has_more":true,"next_page":"n2"}`
	f.bodies["/v1/organization/usage/completions|n2"] = `{"data":[{"start_time":1790086400,"results":[
		{"input_tokens":5,"input_cached_tokens":0,"output_tokens":1,"model":"gpt-4o","project_id":null,"api_key_id":null}]}],"has_more":false}`
	c := NewOpenAIClient(f.URL, nil)
	// costs endpoint missing (404): unreachable, usage still returned.
	rep, err := c.Pull(context.Background(), "sk-admin-"+strings.Repeat("a", 30), from, to)
	if err != nil || rep.CostsErr == nil || rep.CostsErr.Kind != KindUnreachable || rep.CostsErr.HTTPStatus != 404 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	if len(rep.Usage) != 2 || rep.Usage[0].UncachedInput != 600 || rep.Usage[0].CacheRead != 400 {
		t.Fatalf("usage %+v", rep.Usage)
	}
	if got := f.requests[0].URL.Query()["group_by"]; len(got) != 3 {
		t.Fatalf("group_by %v", got)
	}
	if f.requests[0].Header.Get("Authorization") == "" {
		t.Fatal("no auth")
	}
	f.bodies["/v1/organization/costs|"] = `{"data":[{"start_time":1790000000,"results":[
		{"amount":{"value":0.06,"currency":"usd"},"line_item":"gpt-4o, input","project_id":null},
		{"amount":{"value":0.04,"currency":"usd"},"line_item":"gpt-4o, input","project_id":null}]}],"has_more":false}`
	rep, err = c.Pull(context.Background(), "sk-admin-"+strings.Repeat("a", 30), from, to)
	if err != nil || rep.CostsErr != nil || len(rep.Costs) != 1 || rep.Costs[0].Cents.FloatString(2) != "10.00" || *rep.Costs[0].Model != "gpt-4o" {
		t.Fatalf("costs %+v %v", rep, err)
	}
}

func TestVerifySample(t *testing.T) {
	f := newFakeVendor(t)
	f.bodies["/v1/organizations/usage_report/messages|"] = `{"data":[{"starting_at":"2026-10-17T00:00:00Z","results":[]}],"has_more":false}`
	s, err := NewAnthropicClient(f.URL, nil).Verify(context.Background(), sentinel, time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC))
	if err != nil || s.Day != "2026-10-17" || s.Buckets != 1 {
		t.Fatalf("%+v %v", s, err)
	}
}

type fakeCE struct {
	pages []*costexplorer.GetCostAndUsageOutput
	err   error
	calls []*costexplorer.GetCostAndUsageInput
}

func (f *fakeCE) GetCostAndUsage(_ context.Context, in *costexplorer.GetCostAndUsageInput, _ ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}
	p := f.pages[len(f.calls)-1]
	return p, nil
}

func cePage(day, usageType, amt string, next *string) *costexplorer.GetCostAndUsageOutput {
	return &costexplorer.GetCostAndUsageOutput{NextPageToken: next, ResultsByTime: []cetypes.ResultByTime{{
		TimePeriod: &cetypes.DateInterval{Start: aws.String(day)},
		Groups:     []cetypes.Group{{Keys: []string{usageType}, Metrics: map[string]cetypes.MetricValue{"UnblendedCost": {Amount: aws.String(amt)}}}},
	}}}
}

func TestBedrock_Pull(t *testing.T) {
	ce := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		cePage("2026-10-01", "USE1-ClaudeHaiku45-input-tokens", "1.25", aws.String("t2")),
		cePage("2026-10-01", "USE1-ClaudeHaiku45-output-tokens", "0.5", nil),
	}}
	b := &BedrockSource{CE: ce}
	rows, e := b.Pull(context.Background(), from, to)
	if e != nil || len(rows) != 2 || rows[0].Cents.FloatString(0) != "125" || rows[0].Day != "2026-10-01" {
		t.Fatalf("%+v %v", rows, e)
	}
	in := ce.calls[0]
	if *in.TimePeriod.Start != "2026-09-17" || *in.TimePeriod.End != "2026-10-18" || in.Granularity != cetypes.GranularityDaily ||
		in.Metrics[0] != "UnblendedCost" || in.Filter.Dimensions.Values[0] != "Amazon Bedrock" || *in.GroupBy[0].Key != "USAGE_TYPE" {
		t.Fatalf("input %+v", in)
	}
	if ce.calls[1].NextPageToken == nil || *ce.calls[1].NextPageToken != "t2" {
		t.Fatal("pagination token not forwarded")
	}
	for _, tc := range []struct {
		err  error
		kind string
	}{
		{&smithy.GenericAPIError{Code: "AccessDeniedException"}, KindAuthFailed},
		{&smithy.GenericAPIError{Code: "ThrottlingException"}, KindVendorError},
		{errors.New("dial tcp: boom"), KindUnreachable},
	} {
		_, e := (&BedrockSource{CE: &fakeCE{err: tc.err}}).Pull(context.Background(), from, to)
		if e == nil || e.Kind != tc.kind {
			t.Fatalf("%v -> %+v", tc.err, e)
		}
	}
}

func TestMapBedrockUsageType(t *testing.T) {
	cands := []string{"us.anthropic.claude-haiku-4-5-20251001-v1:0", "global.anthropic.claude-haiku-4-5-20251001-v1:0",
		"anthropic.claude-3-5-sonnet-20240620-v1:0", "anthropic.claude-3-5-sonnet-20241022-v2:0", "amazon.nova-pro-v1:0"}
	m := MapBedrockUsageType("USE1-ClaudeHaiku45-input-tokens", cands)
	if m == nil || *m != "global.anthropic.claude-haiku-4-5-20251001-v1:0" {
		t.Fatalf("haiku (profiles of one model are not ambiguous): %v", m)
	}
	if m := MapBedrockUsageType("USW2-Claude35Sonnet-output-tokens", cands); m != nil {
		t.Fatalf("two dated versions must be ambiguous: %v", *m)
	}
	if m := MapBedrockUsageType("USE1-NovaPro-input-tokens", cands); m == nil || *m != "amazon.nova-pro-v1:0" {
		t.Fatalf("vendor prefix is ignored: %v", m)
	}
	if m := MapBedrockUsageType("USE1-Mystery-input-tokens", cands); m != nil {
		t.Fatalf("unknown family must be nil")
	}
	if m := MapBedrockUsageType("USE1-AmazonNovaPro-input", []string{"amazon.nova-pro-v1:0"}); m != nil {
		t.Fatal("documented: prefix match only")
	}
	if MapBedrockUsageType("", cands) != nil {
		t.Fatal("empty")
	}
}

func TestKeyReader(t *testing.T) {
	dir := t.TempDir()
	r := KeyReader{Dir: dir}
	if _, ok := r.Read(Anthropic); ok {
		t.Fatal("absent must be not ok")
	}
	// kubelet-style: key file is a symlink into a swapped data dir
	d1, d2 := filepath.Join(dir, "d1"), filepath.Join(dir, "d2")
	_ = os.Mkdir(d1, 0o755)
	_ = os.Mkdir(d2, 0o755)
	_ = os.WriteFile(filepath.Join(d1, "anthropic-billing-key"), []byte("key-one\n"), 0o600)
	_ = os.WriteFile(filepath.Join(d2, "anthropic-billing-key"), []byte("key-two"), 0o600)
	link := filepath.Join(dir, "anthropic-billing-key")
	_ = os.Symlink(filepath.Join(d1, "anthropic-billing-key"), link)
	if k, ok := r.Read(Anthropic); !ok || k != "key-one" {
		t.Fatalf("%q %v", k, ok)
	}
	_ = os.Remove(link)
	_ = os.Symlink(filepath.Join(d2, "anthropic-billing-key"), link)
	if k, _ := r.Read(Anthropic); k != "key-two" {
		t.Fatalf("re-read after swap: %q", k)
	}
	if _, ok := r.Read("bedrock"); ok {
		t.Fatal("bedrock has no key")
	}
}

func TestNextRun(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 17, h, m, 0, 0, time.UTC) }
	if got := NextRun(at(2, 59)); !got.Equal(at(3, 0)) {
		t.Fatal(got)
	}
	if got := NextRun(at(3, 0)); !got.Equal(at(3, 0).Add(24 * time.Hour)) {
		t.Fatal(got)
	}
}
