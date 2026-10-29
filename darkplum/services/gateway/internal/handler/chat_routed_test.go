package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/router"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

// routedAlias is a routed alias with two candidate routes and a default,
// used across the tests below; the classifier's Decision determines which
// one gets exercised.
var routedAlias = policy.AliasDef{
	Kind:         policy.AliasKindRouted,
	DefaultRoute: "general",
	Routes: []policy.RouteDef{
		{Name: "code", Description: "code questions", Target: "qwen-coder"},
		{Name: "cloud-only", Description: "residency-blocked route", Target: "us.anthropic.claude-3-5-sonnet"},
		{Name: "general", Description: "everything else", Target: "llama-3-8b"},
	},
}

// ---------------------------------------------------------------------------
// Happy path: dispatch, ctx keys, headers (non-stream)
// ---------------------------------------------------------------------------

func TestHandleChatCompletions_RoutedAlias_DispatchesToClassifiedRoute(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "code", Reason: "rules:code:debug"}}}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{
			"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"qwen-coder": true, "llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	w, c := doChat(t, h, "smart-chat")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if vllm.completeCalls != 1 {
		t.Fatalf("expected exactly one dispatch, got %d", vllm.completeCalls)
	}

	if resolved, _ := c.Get("resolved_model"); resolved != "qwen-coder" {
		t.Errorf("resolved_model = %v, want qwen-coder", resolved)
	}
	if requested, _ := c.Get("requested_model"); requested != "smart-chat" {
		t.Errorf("requested_model = %v, want smart-chat", requested)
	}
	if rn, _ := c.Get("route_name"); rn != "code" {
		t.Errorf("route_name = %v, want code", rn)
	}
	if rr, _ := c.Get("router_reason"); rr != "rules:code:debug" {
		t.Errorf("router_reason = %v, want rules:code:debug", rr)
	}
	if _, ok := c.Get("router_latency_ms"); !ok {
		t.Error("expected router_latency_ms to be set")
	}

	if got := w.Header().Get("X-Darkplum-Model"); got != "qwen-coder" {
		t.Errorf("X-Darkplum-Model = %q, want qwen-coder", got)
	}
	if got := w.Header().Get("X-Darkplum-Route"); got != "code" {
		t.Errorf("X-Darkplum-Route = %q, want code", got)
	}
	if got := w.Header().Get("X-Darkplum-Router-Reason"); got != "rules:code:debug" {
		t.Errorf("X-Darkplum-Router-Reason = %q, want rules:code:debug", got)
	}

	if len(classifier.calls) != 1 {
		t.Fatalf("expected the classifier to be called exactly once, got %d", len(classifier.calls))
	}
	if classifier.calls[0].Text != "hello" {
		t.Errorf("expected the classifier to see the last user turn, got %q", classifier.calls[0].Text)
	}
}

// TestHandleChatCompletions_PlainModel_HeaderSetWithoutRoute asserts
// X-Darkplum-Model is set on every dispatched chat response, and the two
// routed-only headers are absent when no routed alias was used.
func TestHandleChatCompletions_PlainModel_HeaderSetWithoutRoute(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)

	w, _ := doChat(t, h, "llama-3-8b")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("X-Darkplum-Model"); got != "llama-3-8b" {
		t.Errorf("X-Darkplum-Model = %q, want llama-3-8b", got)
	}
	if got := w.Header().Get("X-Darkplum-Route"); got != "" {
		t.Errorf("X-Darkplum-Route should be absent for a non-routed model, got %q", got)
	}
	if got := w.Header().Get("X-Darkplum-Router-Reason"); got != "" {
		t.Errorf("X-Darkplum-Router-Reason should be absent for a non-routed model, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Streaming path: headers set before any body byte
// ---------------------------------------------------------------------------

func buildStreamingChatRequest(t *testing.T, model string) *http.Request {
	t.Helper()
	payload := map[string]any{
		"model":    model,
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestHandleChatCompletions_RoutedAlias_HeadersPresentOnStream(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "code", Reason: "rules:code:debug"}}}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{
			Enabled:   map[string]bool{"qwen-coder": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = buildStreamingChatRequest(t, "smart-chat")
	h.HandleChatCompletions(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Darkplum-Model"); got != "qwen-coder" {
		t.Errorf("X-Darkplum-Model = %q, want qwen-coder", got)
	}
	if got := w.Header().Get("X-Darkplum-Route"); got != "code" {
		t.Errorf("X-Darkplum-Route = %q, want code", got)
	}
	if got := w.Header().Get("X-Darkplum-Router-Reason"); got != "rules:code:debug" {
		t.Errorf("X-Darkplum-Router-Reason = %q, want rules:code:debug", got)
	}
	if vllm.streamCalls != 1 {
		t.Errorf("expected exactly one stream dispatch, got %d", vllm.streamCalls)
	}
}

// ---------------------------------------------------------------------------
// Forbidden pick: never swapped for default_route or any other route
// ---------------------------------------------------------------------------

// fallbackFixture builds a handler for the disabled/unreachable-target
// fallback tests. table lists the models present in the route table; enabled
// the enabled flags.
func fallbackFixture(pickRoute string, table []string, enabled map[string]bool, residency, onUnavailable string) (*ChatHandler, *fakeKindProvider) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	routes := map[string]routing.Route{}
	for _, m := range table {
		routes[m] = routing.Route{ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}
	}
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: pickRoute, Reason: "rules:" + pickRoute + ":x"}}}
	alias := routedAlias
	alias.OnRouteUnavailable = onUnavailable
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		routes,
		policy.Snapshot{
			Enabled:    enabled,
			ModelClass: map[string]string{"qwen-coder": routing.ClassSelfHosted, "llama-3-8b": routing.ClassSelfHosted},
			Aliases:    map[string]policy.AliasDef{"smart-chat": alias},
			Residency:  residency,
		},
		classifier,
	)
	return h, vllm
}

// assertRouteTargetUnavailable pins the refusal shape shared by the
// disabled/unreachable pick tests: 503, route_target_unavailable, a message
// that names alias/route/target/state and says nothing was sent elsewhere,
// and the denial_code the usage log row is written from.
func assertRouteTargetUnavailable(t *testing.T, w *httptest.ResponseRecorder, c *gin.Context, state string) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "route_target_unavailable" || e.Error.Type != "service_unavailable" {
		t.Errorf("code/type = %q/%q", e.Error.Code, e.Error.Type)
	}
	for _, want := range []string{`alias "smart-chat"`, `route "code"`, `model "qwen-coder"`, state, "not sent anywhere else"} {
		if !strings.Contains(e.Error.Message, want) {
			t.Errorf("message %q missing %q", e.Error.Message, want)
		}
	}
	if dc, _ := c.Get("denial_code"); dc != "route_target_unavailable" {
		t.Errorf("denial_code = %v", dc)
	}
}

// Issue #13: a self-hosted route down with an external_api default and org
// residency any must refuse, never egress to the vendor.
func TestHandleChatCompletions_RoutedAlias_UnreachablePick_RefusedNotSentToVendorDefault(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	vendor := &fakeKindProvider{name: "anthropic", kind: "anthropic"}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm, "anthropic": vendor},
		map[string]routing.Route{ // qwen-coder (the pick) is absent
			"llama-3-8b": {ProviderKey: "anthropic", Kind: "anthropic", Class: routing.ClassExternalAPI},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		&fakeClassifier{decisions: []router.Decision{{RouteName: "code", Reason: "rules:code:x"}}},
	)
	w, c := doChat(t, h, "smart-chat")
	assertRouteTargetUnavailable(t, w, c, "not available on this gateway")
	if vendor.completeCalls != 0 || vendor.streamCalls != 0 || vllm.completeCalls != 0 || vllm.streamCalls != 0 {
		t.Errorf("no provider may be called: vendor=%d/%d vllm=%d/%d", vendor.completeCalls, vendor.streamCalls, vllm.completeCalls, vllm.streamCalls)
	}
	if rn, _ := c.Get("route_name"); rn != "code" {
		t.Errorf("route_name = %v, want the picked route code (not general)", rn)
	}
	if rr, _ := c.Get("router_reason"); rr != "rules:code:x" {
		t.Errorf("router_reason = %v", rr)
	}
	if strings.Contains(w.Header().Get("X-Darkplum-Router-Reason"), "fallback") {
		t.Errorf("reason must not claim a fallback: %q", w.Header().Get("X-Darkplum-Router-Reason"))
	}
}

func TestHandleChatCompletions_RoutedAlias_DisabledPick_RefuseSetting_DefaultNotCalled(t *testing.T) {
	h, vllm := fallbackFixture("code", []string{"qwen-coder", "llama-3-8b"},
		map[string]bool{"qwen-coder": false, "llama-3-8b": true}, policy.ResidencyAny, policy.OnRouteUnavailableRefuse)

	w, c := doChat(t, h, "smart-chat")
	assertRouteTargetUnavailable(t, w, c, "disabled")
	if vllm.completeCalls != 0 || vllm.streamCalls != 0 {
		t.Errorf("default route must not be called, got %d/%d", vllm.completeCalls, vllm.streamCalls)
	}
	if bc, _ := c.Get("backend_class"); bc != routing.ClassSelfHosted {
		t.Errorf("backend_class = %v, want the picked target's class", bc)
	}
	if got := w.Header().Get("X-Darkplum-Model"); got != "qwen-coder" {
		t.Errorf("X-Darkplum-Model = %q", got)
	}
	if msg := decodeError(t, w.Body.Bytes()).Error.Message; !strings.Contains(msg, `on_route_unavailable setting is "refuse"`) {
		t.Errorf("message must name the setting: %q", msg)
	}
}

// Disabled and unreachable picks fall back to a same-class default under the
// default setting, with the reason vocabulary the usage log and UI read.
func TestHandleChatCompletions_RoutedAlias_DownPick_FallsBackToSameClassDefault(t *testing.T) {
	cases := []struct {
		name    string
		table   []string
		enabled map[string]bool
		reason  string
	}{
		{"disabled", []string{"qwen-coder", "llama-3-8b"}, map[string]bool{"qwen-coder": false, "llama-3-8b": true}, "fallback:route_target_disabled:code"},
		{"unreachable", []string{"llama-3-8b"}, map[string]bool{"llama-3-8b": true}, "fallback:route_target_unreachable:code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, vllm := fallbackFixture("code", tc.table, tc.enabled, policy.ResidencyAny, "")
			w, c := doChat(t, h, "smart-chat")
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
			}
			if vllm.completeCalls != 1 {
				t.Errorf("expected one dispatch, got %d", vllm.completeCalls)
			}
			if rn, _ := c.Get("route_name"); rn != "general" {
				t.Errorf("route_name = %v, want general", rn)
			}
			if rr, _ := c.Get("router_reason"); rr != tc.reason {
				t.Errorf("router_reason = %v, want %s", rr, tc.reason)
			}
			if got := w.Header().Get("X-Darkplum-Router-Reason"); got != tc.reason {
				t.Errorf("X-Darkplum-Router-Reason = %q", got)
			}
			if rm, _ := c.Get("resolved_model"); rm != "llama-3-8b" {
				t.Errorf("resolved_model = %v", rm)
			}
		})
	}
}

// matrixCase is one cell of the setting x class-relation table.
type matrixCase struct {
	name       string
	pickClass  string // class of the picked route's model, as served
	defClass   string
	defEnabled bool
	pickAbsent bool   // pick missing from the route table (unreachable)
	recorded   string // models.backend_class recorded for an absent pick
	// wantCode is "" when the default route must answer.
	wantCode map[string]string // setting -> code
}

const (
	unknownClass = "some_future_class"
	codeUnavail  = "route_target_unavailable"
)

// Every setting x relation of default class to picked class. Residency is
// `any` throughout, so a refusal here is the setting, not the residency
// check — except the unknown-class default under fallback_any_permitted,
// which the residency check itself refuses (an unrecognised class is refused
// under every residency).
var matrixCases = []matrixCase{
	{name: "default same class", pickClass: routing.ClassSelfHosted, defClass: routing.ClassSelfHosted, defEnabled: true,
		wantCode: map[string]string{policy.OnRouteUnavailableRefuse: codeUnavail, policy.OnRouteUnavailableSameOrStricter: "", policy.OnRouteUnavailableAnyPermitted: ""}},
	{name: "default stricter", pickClass: routing.ClassCloudManaged, defClass: routing.ClassSelfHosted, defEnabled: true,
		wantCode: map[string]string{policy.OnRouteUnavailableRefuse: codeUnavail, policy.OnRouteUnavailableSameOrStricter: "", policy.OnRouteUnavailableAnyPermitted: ""}},
	{name: "default looser (issue 13)", pickClass: routing.ClassSelfHosted, defClass: routing.ClassExternalAPI, defEnabled: true,
		wantCode: map[string]string{policy.OnRouteUnavailableRefuse: codeUnavail, policy.OnRouteUnavailableSameOrStricter: codeUnavail, policy.OnRouteUnavailableAnyPermitted: ""}},
	{name: "default one rung looser", pickClass: routing.ClassSelfHosted, defClass: routing.ClassCloudManaged, defEnabled: true,
		wantCode: map[string]string{policy.OnRouteUnavailableRefuse: codeUnavail, policy.OnRouteUnavailableSameOrStricter: codeUnavail, policy.OnRouteUnavailableAnyPermitted: ""}},
	{name: "default class unknown", pickClass: routing.ClassSelfHosted, defClass: unknownClass, defEnabled: true,
		wantCode: map[string]string{policy.OnRouteUnavailableRefuse: codeUnavail, policy.OnRouteUnavailableSameOrStricter: codeUnavail, policy.OnRouteUnavailableAnyPermitted: "model_residency_violation"}},
	{name: "picked class unknown", pickClass: unknownClass, defClass: routing.ClassSelfHosted, defEnabled: true,
		wantCode: map[string]string{policy.OnRouteUnavailableRefuse: codeUnavail, policy.OnRouteUnavailableSameOrStricter: codeUnavail, policy.OnRouteUnavailableAnyPermitted: ""}},
	{name: "default unusable", pickClass: routing.ClassSelfHosted, defClass: routing.ClassSelfHosted, defEnabled: false,
		wantCode: map[string]string{policy.OnRouteUnavailableRefuse: codeUnavail, policy.OnRouteUnavailableSameOrStricter: codeUnavail, policy.OnRouteUnavailableAnyPermitted: codeUnavail}},
	{name: "unreachable pick, recorded stricter than default", pickAbsent: true, recorded: routing.ClassSelfHosted, defClass: routing.ClassExternalAPI, defEnabled: true,
		wantCode: map[string]string{policy.OnRouteUnavailableRefuse: codeUnavail, policy.OnRouteUnavailableSameOrStricter: codeUnavail, policy.OnRouteUnavailableAnyPermitted: ""}},
	{name: "unreachable pick, recorded looser than default", pickAbsent: true, recorded: routing.ClassCloudManaged, defClass: routing.ClassSelfHosted, defEnabled: true,
		wantCode: map[string]string{policy.OnRouteUnavailableRefuse: codeUnavail, policy.OnRouteUnavailableSameOrStricter: "", policy.OnRouteUnavailableAnyPermitted: ""}},
	{name: "unreachable pick, class unrecorded", pickAbsent: true, recorded: "", defClass: routing.ClassSelfHosted, defEnabled: true,
		wantCode: map[string]string{policy.OnRouteUnavailableRefuse: codeUnavail, policy.OnRouteUnavailableSameOrStricter: codeUnavail, policy.OnRouteUnavailableAnyPermitted: ""}},
}

// matrixFixture builds one provider per class so a test can tell exactly
// which backend saw the prompt. The pick (qwen-coder) is down: disabled, or
// absent from the route table.
func matrixFixture(tc matrixCase, setting string, c router.Decision) (*ChatHandler, map[string]*fakeKindProvider) {
	provs := map[string]*fakeKindProvider{}
	fp := fakeProviders{}
	for _, cls := range []string{routing.ClassSelfHosted, routing.ClassCloudManaged, routing.ClassExternalAPI, unknownClass} {
		p := &fakeKindProvider{name: "prov-" + cls, kind: "vllm"}
		provs[cls] = p
		fp["prov-"+cls] = p
	}
	routes := map[string]routing.Route{
		"llama-3-8b": {ProviderKey: "prov-" + tc.defClass, Kind: "vllm", Class: tc.defClass},
	}
	snap := policy.Snapshot{
		Enabled:   map[string]bool{"qwen-coder": false, "llama-3-8b": tc.defEnabled},
		Residency: policy.ResidencyAny,
	}
	if tc.pickAbsent {
		if tc.recorded != "" {
			snap.ModelClass = map[string]string{"qwen-coder": tc.recorded}
		}
	} else {
		routes["qwen-coder"] = routing.Route{ProviderKey: "prov-" + tc.pickClass, Kind: "vllm", Class: tc.pickClass}
	}
	alias := routedAlias
	alias.OnRouteUnavailable = setting
	snap.Aliases = map[string]policy.AliasDef{"smart-chat": alias}
	return newRoutedTestHandler(fp, routes, snap, &fakeClassifier{decisions: []router.Decision{c}}), provs
}

func totalCalls(provs map[string]*fakeKindProvider) int {
	n := 0
	for _, p := range provs {
		n += p.completeCalls + p.streamCalls
	}
	return n
}

func TestHandleChatCompletions_RoutedAlias_OnRouteUnavailableMatrix(t *testing.T) {
	pick := router.Decision{RouteName: "code", Reason: "rules:code:x"}
	for _, tc := range matrixCases {
		for _, setting := range []string{policy.OnRouteUnavailableRefuse, policy.OnRouteUnavailableSameOrStricter, policy.OnRouteUnavailableAnyPermitted} {
			t.Run(tc.name+"/"+setting, func(t *testing.T) {
				want := tc.wantCode[setting]
				h, provs := matrixFixture(tc, setting, pick)
				w, c := doChat(t, h, "smart-chat")
				if want == "" {
					if w.Code != http.StatusOK {
						t.Fatalf("expected the default route to answer, got %d: %s", w.Code, w.Body.String())
					}
					if got := provs[tc.defClass].completeCalls; got != 1 || totalCalls(provs) != 1 {
						t.Errorf("exactly one call, on the default's backend: default=%d total=%d", got, totalCalls(provs))
					}
					if rr, _ := c.Get("router_reason"); !strings.HasPrefix(rr.(string), "fallback:route_target_") || !strings.HasSuffix(rr.(string), ":code") {
						t.Errorf("router_reason = %v", rr)
					}
					return
				}
				if w.Code == http.StatusOK {
					t.Fatalf("expected refusal %s, got 200", want)
				}
				if e := decodeError(t, w.Body.Bytes()); e.Error.Code != want {
					t.Errorf("code = %q, want %q (%s)", e.Error.Code, want, e.Error.Message)
				}
				if n := totalCalls(provs); n != 0 {
					t.Errorf("a refused request must reach no backend, got %d calls", n)
				}
				if want == codeUnavail {
					if w.Code != http.StatusServiceUnavailable {
						t.Errorf("status = %d, want 503", w.Code)
					}
					msg := decodeError(t, w.Body.Bytes()).Error.Message
					for _, s := range []string{`route "code"`, `"general"`, "on_route_unavailable", setting} {
						if !strings.Contains(msg, s) {
							t.Errorf("message %q missing %q", msg, s)
						}
					}
					if setting == policy.OnRouteUnavailableSameOrStricter && tc.defEnabled {
						for _, cls := range []string{tc.defClass, classText(tc.pickClass)} {
							if tc.pickAbsent {
								cls = classText(tc.recorded)
							}
							if !strings.Contains(msg, cls) {
								t.Errorf("message %q must name class %q", msg, cls)
							}
						}
					}
				}
			})
		}
	}
}

// The issue's case exactly: self-hosted pick down, vendor-key default,
// residency any, default setting -> refused, zero vendor calls; also
// streaming (the refusal happens before any SSE byte).
func TestHandleChatCompletions_RoutedAlias_Issue13_SelfHostedDownVendorDefault(t *testing.T) {
	tc := matrixCases[2]
	pick := router.Decision{RouteName: "code", Reason: "rules:code:x"}
	h, provs := matrixFixture(tc, "", pick) // "" = column default
	w, _ := doChat(t, h, "smart-chat")
	if w.Code != http.StatusServiceUnavailable || decodeError(t, w.Body.Bytes()).Error.Code != codeUnavail {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if n := totalCalls(provs); n != 0 {
		t.Errorf("vendor (or anyone) called %d times", n)
	}

	h, provs = matrixFixture(tc, policy.OnRouteUnavailableSameOrStricter, pick)
	sw := httptest.NewRecorder()
	sc, _ := gin.CreateTestContext(sw)
	sc.Request = buildStreamingChatRequest(t, "smart-chat")
	h.HandleChatCompletions(sc)
	if sw.Code != http.StatusServiceUnavailable || decodeError(t, sw.Body.Bytes()).Error.Code != codeUnavail {
		t.Fatalf("stream: got %d %s", sw.Code, sw.Body.String())
	}
	if ct := sw.Header().Get("Content-Type"); strings.Contains(ct, "event-stream") {
		t.Errorf("a refused stream must be a JSON error, not SSE: %q", ct)
	}
	if n := totalCalls(provs); n != 0 {
		t.Errorf("stream: backends called %d times", n)
	}
}

// Classifier failure always uses the default route, whatever the setting, and
// the default is still residency-checked afterwards.
func TestHandleChatCompletions_RoutedAlias_ClassifierFailureUsesDefaultRoute(t *testing.T) {
	for _, reason := range []string{router.ReasonFallbackTimeout, router.ReasonFallbackCircuitOpen} {
		for _, setting := range []string{policy.OnRouteUnavailableRefuse, policy.OnRouteUnavailableSameOrStricter, policy.OnRouteUnavailableAnyPermitted} {
			t.Run(reason+"/"+setting, func(t *testing.T) {
				// default route is a looser (vendor) class than the other route
				tc := matrixCase{pickClass: routing.ClassSelfHosted, defClass: routing.ClassExternalAPI, defEnabled: true}
				d := router.Decision{RouteName: "general", Reason: reason}
				h, provs := matrixFixture(tc, setting, d)
				w, c := doChat(t, h, "smart-chat")
				if w.Code != http.StatusOK {
					t.Fatalf("classifier failure must reach the default route, got %d: %s", w.Code, w.Body.String())
				}
				if provs[routing.ClassExternalAPI].completeCalls != 1 || totalCalls(provs) != 1 {
					t.Errorf("default backend must get the one call, got %d", totalCalls(provs))
				}
				if rr, _ := c.Get("router_reason"); rr != reason {
					t.Errorf("router_reason = %v", rr)
				}
			})
		}
	}
	// ...and residency still applies to that default route, under every
	// setting and every classifier-failure reason: the exemption is from the
	// on_route_unavailable setting, never from residency.
	for _, reason := range []string{router.ReasonFallbackTimeout, router.ReasonFallbackCircuitOpen} {
		for _, setting := range []string{policy.OnRouteUnavailableRefuse, policy.OnRouteUnavailableSameOrStricter, policy.OnRouteUnavailableAnyPermitted} {
			t.Run("residency/"+reason+"/"+setting, func(t *testing.T) {
				tc := matrixCase{pickClass: routing.ClassSelfHosted, defClass: routing.ClassExternalAPI, defEnabled: true}
				h, provs := matrixFixture(tc, setting, router.Decision{RouteName: "general", Reason: reason})
				snap, _ := h.policy.Current()
				snap.Residency = policy.ResidencyCloudAccountOnly
				h.policy = &fakePolicyStore{snap: snap, loaded: true}
				w, _ := doChat(t, h, "smart-chat")
				if w.Code != http.StatusForbidden || decodeError(t, w.Body.Bytes()).Error.Code != "model_residency_violation" {
					t.Fatalf("got %d %s", w.Code, w.Body.String())
				}
				if n := totalCalls(provs); n != 0 {
					t.Errorf("backends called %d times", n)
				}
			})
		}
	}
}

// The dryrun shares resolveRoutedPick: for every cell of the matrix it
// reports exactly what a real completion decides.
func TestHandleRoutingDryrun_OnRouteUnavailableMatrix(t *testing.T) {
	pick := router.Decision{RouteName: "code", Reason: "rules:code:x"}
	for _, tc := range matrixCases {
		for _, setting := range []string{policy.OnRouteUnavailableRefuse, policy.OnRouteUnavailableSameOrStricter, policy.OnRouteUnavailableAnyPermitted} {
			t.Run(tc.name+"/"+setting, func(t *testing.T) {
				want := tc.wantCode[setting]
				h, provs := matrixFixture(tc, setting, pick)
				_, resp := doDryrun(t, h, "smart-chat", "fix this")
				if want == "" {
					if resp.WouldBeRefused != nil || resp.RouteName != "general" || !strings.HasPrefix(resp.Reason, "fallback:route_target_") {
						t.Errorf("got %+v", resp)
					}
				} else if resp.WouldBeRefused == nil || resp.WouldBeRefused.Code != want {
					t.Errorf("want refusal %s, got %+v", want, resp.WouldBeRefused)
				}
				if n := totalCalls(provs); n != 0 {
					t.Errorf("dryrun called a backend %d times", n)
				}
			})
		}
	}
}

// Picking the default route itself still dispatches when it is usable.
func TestHandleChatCompletions_RoutedAlias_DefaultRoutePick_StillServed(t *testing.T) {
	h, vllm := fallbackFixture("general", []string{"llama-3-8b"},
		map[string]bool{"qwen-coder": false, "llama-3-8b": true}, policy.ResidencyAny, "")

	w, c := doChat(t, h, "smart-chat")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if vllm.completeCalls != 1 {
		t.Errorf("expected one dispatch, got %d", vllm.completeCalls)
	}
	if rm, _ := c.Get("resolved_model"); rm != "llama-3-8b" {
		t.Errorf("resolved_model = %v", rm)
	}
}

// Residency is still checked on a normal (usable) pick.
func TestHandleChatCompletions_RoutedAlias_UsablePickStillResidencyChecked(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{
			"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassCloudManaged},
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"qwen-coder": true, "llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencySelfHostedOnly,
		},
		&fakeClassifier{decisions: []router.Decision{{RouteName: "code", Reason: "rules:code:x"}}},
	)
	w, _ := doChat(t, h, "smart-chat")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_residency_violation" {
		t.Errorf("code = %q", e.Error.Code)
	}
	if want := `route "code" of alias "smart-chat" selects model "qwen-coder"`; !strings.Contains(e.Error.Message, want) {
		t.Errorf("message %q should contain %q", e.Error.Message, want)
	}
	if vllm.completeCalls != 0 {
		t.Errorf("provider must not be called, got %d", vllm.completeCalls)
	}
}

func TestHandleChatCompletions_RoutedAlias_DisabledPickIsDefault_Refused(t *testing.T) {
	h, vllm := fallbackFixture("general", []string{"qwen-coder", "llama-3-8b"},
		map[string]bool{"qwen-coder": true, "llama-3-8b": false}, policy.ResidencyAny, "")

	w, _ := doChat(t, h, "smart-chat")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_not_enabled" {
		t.Errorf("code = %q", e.Error.Code)
	}
	want := `route "general" of alias "smart-chat" is the alias's default route and selects model "llama-3-8b", which is disabled. Enable it on the Models page or retarget the route.`
	if e.Error.Message != want {
		t.Errorf("message = %q, want %q", e.Error.Message, want)
	}
	if vllm.completeCalls != 0 {
		t.Errorf("provider must not be called, got %d", vllm.completeCalls)
	}
}

// Policy refusals are not fallback cases: a residency-forbidden pick is
// refused naming the picked route even though the default route is usable.
func TestHandleChatCompletions_RoutedAlias_PolicyForbiddenPick_NoFallback(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{
			"us.anthropic.claude-3-5-sonnet": {ProviderKey: "vllm-default", Kind: "bedrock", Class: routing.ClassCloudManaged},
			"llama-3-8b":                     {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"us.anthropic.claude-3-5-sonnet": true, "llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencySelfHostedOnly,
		},
		&fakeClassifier{decisions: []router.Decision{{RouteName: "cloud-only", Reason: "rules:cloud-only:x"}}},
	)
	w, c := doChat(t, h, "smart-chat")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	if e := decodeError(t, w.Body.Bytes()); e.Error.Code != "model_residency_violation" {
		t.Errorf("code = %q", e.Error.Code)
	}
	if rn, _ := c.Get("route_name"); rn != "cloud-only" {
		t.Errorf("route_name = %v, want cloud-only (no fallback)", rn)
	}
	if rr, _ := c.Get("router_reason"); rr != "rules:cloud-only:x" {
		t.Errorf("router_reason = %v, want unchanged", rr)
	}
	if vllm.completeCalls != 0 {
		t.Errorf("provider must not be called, got %d", vllm.completeCalls)
	}
}

// TestHandleChatCompletions_RoutedAlias_ForbiddenPick_Residency is the
// residency-on-selected-target case from the contract's test list.
func TestHandleChatCompletions_RoutedAlias_ForbiddenPick_Residency(t *testing.T) {
	brk := &fakeNameOnlyProvider{name: "bedrock"}
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "cloud-only", Reason: "rules:cloud-only:cloud"}}}
	h := newRoutedTestHandler(
		fakeProviders{"bedrock": brk},
		map[string]routing.Route{
			"us.anthropic.claude-3-5-sonnet": {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged},
			"llama-3-8b":                     {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"us.anthropic.claude-3-5-sonnet": true, "llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencySelfHostedOnly,
		},
		classifier,
	)

	w, _ := doChat(t, h, "smart-chat")

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_residency_violation" {
		t.Errorf("expected code model_residency_violation, got %q", e.Error.Code)
	}
	wantMsg := `route "cloud-only" of alias "smart-chat" selects model "us.anthropic.claude-3-5-sonnet", which is not enabled/allowed by residency`
	if e.Error.Message != wantMsg {
		t.Errorf("message = %q, want %q", e.Error.Message, wantMsg)
	}
	if brk.completeCalls != 0 {
		t.Errorf("provider must never be called for a residency-forbidden pick, got %d calls", brk.completeCalls)
	}
	if got := w.Header().Get("X-Darkplum-Route"); got != "cloud-only" {
		t.Errorf("expected X-Darkplum-Route to still be set on a 4xx refusal where a route was chosen, got %q", got)
	}
}

func TestHandleChatCompletions_RoutedAlias_PickedTargetNotFound(t *testing.T) {
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "code", Reason: "rules:code:debug"}}}
	h := newRoutedTestHandler(
		fakeProviders{},
		map[string]routing.Route{}, // "code"'s target was never discovered
		policy.Snapshot{
			Enabled:   map[string]bool{},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	w, _ := doChat(t, h, "smart-chat")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "route_target_unavailable" {
		t.Errorf("expected code route_target_unavailable, got %q", e.Error.Code)
	}
}

// TestHandleChatCompletions_RoutedAlias_BogusRouteName covers the defensive
// fail-closed path: a classifier names a route that isn't in alias.Routes
// and isn't alias.DefaultRoute — normally unreachable per
// policy.buildAliases's invariants, but a Classifier implementation could
// still hand back an out-of-band string if that invariant were ever
// violated. The request must still be refused with 404 model_not_found, and
// the provider must never be dispatched to.
func TestHandleChatCompletions_RoutedAlias_BogusRouteName(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	classifier := &fakeClassifier{decisions: []router.Decision{{
		RouteName: "does-not-exist",
		Reason:    "rules:does-not-exist:x",
	}}}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{
			"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"qwen-coder": true, "llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	w, _ := doChat(t, h, "smart-chat")

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_not_found" {
		t.Errorf("expected code model_not_found, got %q", e.Error.Code)
	}
	if vllm.completeCalls != 0 {
		t.Errorf("provider must never be dispatched to for an unresolved route, got %d calls", vllm.completeCalls)
	}
}

// ---------------------------------------------------------------------------
// Aborted classification: never dispatch
// ---------------------------------------------------------------------------

func TestHandleChatCompletions_RoutedAlias_ClassifierAborted_NeverDispatches(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	classifier := &fakeClassifier{errs: []error{router.ErrAborted}}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{
			"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"qwen-coder": true, "llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	w, c := doChat(t, h, "smart-chat")
	if w.Code != 499 {
		t.Fatalf("expected 499, got %d; body=%s", w.Code, w.Body.String())
	}
	if vllm.completeCalls != 0 {
		t.Errorf("provider must never be called when classification is aborted, got %d calls", vllm.completeCalls)
	}
	if code, _ := c.Get("denial_code"); code != "request_canceled" {
		t.Errorf("denial_code = %v, want request_canceled", code)
	}
}
