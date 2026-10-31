package vendor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type vectors struct {
	BareWildcard []struct {
		Pattern string `json:"pattern"`
		Want    bool   `json:"want"`
	} `json:"bare_wildcard"`
	NextStamp []struct {
		Now  string `json:"now"`
		Old  string `json:"old"`
		Want string `json:"want"`
	} `json:"next_stamp"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("../../../provider-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.BareWildcard) == 0 || len(v.NextStamp) == 0 {
		t.Fatal("parity vectors are empty")
	}
	return v
}

func TestParityBareWildcard(t *testing.T) {
	for _, c := range loadVectors(t).BareWildcard {
		if got := BareWildcard(c.Pattern); got != c.Want {
			t.Errorf("BareWildcard(%q) = %v, want %v", c.Pattern, got, c.Want)
		}
	}
}

func TestParityNextStamp(t *testing.T) {
	for _, c := range loadVectors(t).NextStamp {
		now, err := time.Parse(time.RFC3339, c.Now)
		if err != nil {
			t.Fatal(err)
		}
		if got := NextStamp(now, c.Old); got != c.Want {
			t.Errorf("NextStamp(%s, %q) = %s, want %s", c.Now, c.Old, got, c.Want)
		}
	}
}

func TestPatterns(t *testing.T) {
	cases := map[string][]string{
		"claude-*":            {"claude-*"},
		" claude-* , gpt-4? ": {"claude-*", "gpt-4?"},
		"":                    nil,
		"a,,b":                {"a", "", "b"},
	}
	for in, want := range cases {
		if got := Patterns(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Patterns(%q) = %#v, want %#v", in, got, want)
		}
	}
}

func TestCheckPatterns(t *testing.T) {
	var bw *BareWildcardError
	for _, p := range []string{"*", "a,,b", "claude-*,*"} {
		if err := CheckPatterns(Patterns(p), false); !errors.As(err, &bw) {
			t.Errorf("%q accepted without allowAll: %v", p, err)
		}
		if err := CheckPatterns(Patterns(p), true); err != nil {
			t.Errorf("%q refused with allowAll: %v", p, err)
		}
	}
	if err := CheckPatterns([]string{"claude-[a"}, true); err == nil || errors.As(err, &bw) {
		t.Errorf("a malformed glob must be refused even with allowAll: %v", err)
	}
	if err := CheckPatterns([]string{"claude-*", "gpt-4?"}, false); err != nil {
		t.Error(err)
	}
}

// Match uses path.Match like the gateway: * does not cross a slash, and a model
// id the filter names exactly is admitted.
func TestMatchUsesPathMatchSemantics(t *testing.T) {
	ids := []string{"claude-3", "claude-sonnet-4", "gpt-4o", "ft:claude/x", "claude/x"}
	got := Match(ids, Patterns("claude-*,gpt-4o"))
	if want := []string{"claude-3", "claude-sonnet-4", "gpt-4o"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Match = %v, want %v", got, want)
	}
	if got := Match(ids, Patterns("claude*")); !reflect.DeepEqual(got, []string{"claude-3", "claude-sonnet-4"}) {
		t.Errorf("* crossed a slash: %v", got)
	}
	if got := Match(ids, []string{""}); got != nil {
		t.Errorf("an empty pattern matched %v", got)
	}
}

func TestValidKeyAndWorkspace(t *testing.T) {
	for k, want := range map[string]bool{"sk-ant-api03_AbC-1": true, "": false, "a b": false, "a\"b": false, "a\nb": false, "sk$": false} {
		if ValidKey(k) != want {
			t.Errorf("ValidKey(%q) != %v", k, want)
		}
	}
	for w, want := range map[string]bool{"wrkspc_01AbC": true, "": false, strings.Repeat("a", 65): false, "w/x": false} {
		if ValidWorkspace(w) != want {
			t.Errorf("ValidWorkspace(%q) != %v", w, want)
		}
	}
}

func checker(t *testing.T, h http.HandlerFunc) *Checker {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Checker{HTTP: srv.Client(), AnthropicURL: srv.URL, OpenAIURL: srv.URL,
		Now: func() time.Time { return time.Date(2026, 10, 31, 15, 4, 5, 0, time.UTC) }}
}

func TestModelsAnthropicHeadersAndParsing(t *testing.T) {
	var got *http.Request
	c := checker(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Write([]byte(`{"data":[{"id":"claude-a"},{"id":"claude-b"},{"id":""}]}`))
	})
	ids, err := c.Models(context.Background(), Anthropic, "sk-ant-secret", "wrkspc_1")
	if err != nil || !reflect.DeepEqual(ids, []string{"claude-a", "claude-b"}) {
		t.Fatalf("ids = %v, %v", ids, err)
	}
	if got.URL.Path != "/v1/models" || got.URL.Query().Get("limit") != "1000" {
		t.Errorf("url = %s", got.URL)
	}
	if got.Header.Get("x-api-key") != "sk-ant-secret" || got.Header.Get("anthropic-version") != "2023-06-01" ||
		got.Header.Get("anthropic-workspace-id") != "wrkspc_1" {
		t.Errorf("headers = %v", got.Header)
	}
	if strings.Contains(got.URL.String(), "sk-ant-secret") {
		t.Error("the key is in the URL")
	}
}

func TestModelsOpenAIBearer(t *testing.T) {
	var auth, ws string
	c := checker(t, func(w http.ResponseWriter, r *http.Request) {
		auth, ws = r.Header.Get("Authorization"), r.Header.Get("anthropic-workspace-id")
		w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	})
	if _, err := c.Models(context.Background(), OpenAI, "sk-oa", "ignored"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sk-oa" || ws != "" {
		t.Errorf("auth = %q workspace header = %q", auth, ws)
	}
}

func TestModelsErrors(t *testing.T) {
	c := checker(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("x-api-key") {
		case "org":
			http.Error(w, `{"error":"anthropic-workspace-id header is required"}`, 400)
		case "bad400":
			http.Error(w, `nope`, 400)
		default:
			http.Error(w, `no`, 401)
		}
	})
	if _, err := c.Models(context.Background(), Anthropic, "org", ""); !errors.Is(err, ErrWorkspaceNeeded) {
		t.Errorf("workspace error = %v", err)
	}
	var rej *RejectedError
	if _, err := c.Models(context.Background(), Anthropic, "bad400", ""); !errors.As(err, &rej) || rej.Code != 400 {
		t.Errorf("400 without the hint = %v", err)
	}
	if _, err := c.Models(context.Background(), Anthropic, "x", ""); !errors.As(err, &rej) || rej.Code != 401 {
		t.Errorf("401 = %v", err)
	}
	// An OpenAI 400 that mentions the header is not the workspace case.
	if _, err := c.Models(context.Background(), OpenAI, "x", ""); errors.Is(err, ErrWorkspaceNeeded) {
		t.Error("workspace hint applied to OpenAI")
	}
}

func TestUnreachableDoesNotLeakTheKey(t *testing.T) {
	c := &Checker{HTTP: &http.Client{Timeout: time.Second}, AnthropicURL: "http://127.0.0.1:1", Now: time.Now}
	_, err := c.Models(context.Background(), Anthropic, "sk-secret", "")
	var un *UnreachableError
	if !errors.As(err, &un) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "sk-secret") || strings.Contains(err.Error(), "127.0.0.1:1/v1") {
		t.Errorf("message leaks request details: %v", err)
	}
}

func TestBillingRequests(t *testing.T) {
	var gotA, gotO *http.Request
	c := checker(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "usage_report") {
			gotA = r
		} else {
			gotO = r
		}
	})
	if err := c.Billing(context.Background(), Anthropic, "adm"); err != nil {
		t.Fatal(err)
	}
	q := gotA.URL.Query()
	if gotA.URL.Path != "/v1/organizations/usage_report/messages" || q.Get("starting_at") != "2026-10-30T00:00:00Z" ||
		q.Get("ending_at") != "2026-10-31T00:00:00Z" || q.Get("bucket_width") != "1d" || gotA.Header.Get("x-api-key") != "adm" {
		t.Errorf("anthropic billing request = %s %v", gotA.URL, gotA.Header)
	}
	if err := c.Billing(context.Background(), OpenAI, "adm"); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 31, 15, 4, 5, 0, time.UTC).Unix() - 86400
	if gotO.URL.Path != "/v1/organization/costs" || gotO.URL.Query().Get("start_time") != itoa(want) ||
		gotO.URL.Query().Get("limit") != "1" || gotO.Header.Get("Authorization") != "Bearer adm" {
		t.Errorf("openai billing request = %s %v", gotO.URL, gotO.Header)
	}
	bad := checker(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 403) })
	var rej *RejectedError
	if err := bad.Billing(context.Background(), OpenAI, "k"); !errors.As(err, &rej) || rej.Code != 403 {
		t.Errorf("403 = %v", err)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestGetKnowsBothVendors(t *testing.T) {
	for _, n := range []Name{Anthropic, OpenAI} {
		i, ok := Get(n)
		if !ok || i.KeyFile == "" || i.BillingFile == "" || i.SetAtKey() != i.FilterKey+"_SET_AT" {
			t.Errorf("%s: %+v", n, i)
		}
	}
	if _, ok := Get("huggingface"); ok {
		t.Error("huggingface is not a vendor key")
	}
}
