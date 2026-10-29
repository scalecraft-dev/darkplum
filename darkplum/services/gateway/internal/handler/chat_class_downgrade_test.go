package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft-dev/darkplum/gateway/internal/middleware"
	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/router"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

// The routing manager lets the lowest-ranked class win a contested model id,
// but once the self-hosted source expires its claim is dropped and a vendor
// listing the same id wins unopposed. These tests pin the request-time guard:
// an id registered as self_hosted is never served from a looser class.

const hfModel = "hf/llama-3-8b"

type downgradeFixture struct {
	h     *ChatHandler
	local *fakeKindProvider
	vend  *fakeKindProvider
}

func (f downgradeFixture) calls() int {
	return f.local.completeCalls + f.local.streamCalls + f.vend.completeCalls + f.vend.streamCalls
}

// downgradeSetup serves hfModel from servingClass. registered is the
// model_registered_classes value ("" = none). A local default route model
// "local-default" is always up, so a routed fallback has somewhere stricter to go.
func downgradeSetup(servingClass, registered string, alias policy.AliasDef, d router.Decision) downgradeFixture {
	local := &fakeKindProvider{name: "prov-local", kind: "vllm"}
	vend := &fakeKindProvider{name: "prov-vendor", kind: "openai"}
	serving := "prov-vendor"
	kind := "openai"
	if servingClass == routing.ClassSelfHosted {
		serving, kind = "prov-local", "vllm"
	}
	snap := policy.Snapshot{
		Enabled:   map[string]bool{hfModel: true, "local-default": true},
		Residency: policy.ResidencyAny,
		Aliases:   map[string]policy.AliasDef{"static-chat": {Kind: policy.AliasKindStatic, Target: hfModel}, "smart-chat": alias},
	}
	if registered != "" {
		snap.RegisteredClass = map[string]string{hfModel: registered}
	}
	routes := map[string]routing.Route{
		hfModel:         {ProviderKey: serving, Kind: kind, Class: servingClass},
		"local-default": {ProviderKey: "prov-local", Kind: "vllm", Class: routing.ClassSelfHosted},
	}
	h := newRoutedTestHandler(fakeProviders{"prov-local": local, "prov-vendor": vend}, routes, snap, &fakeClassifier{decisions: []router.Decision{d}})
	return downgradeFixture{h: h, local: local, vend: vend}
}

var downgradeAlias = policy.AliasDef{
	Kind:         policy.AliasKindRouted,
	DefaultRoute: "general",
	Routes: []policy.RouteDef{
		{Name: "code", Target: hfModel},
		{Name: "general", Target: "local-default"},
	},
}

func doStream(t *testing.T, h *ChatHandler, model string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = buildStreamingChatRequest(t, model)
	h.HandleChatCompletions(c)
	return w
}

func requireDowngradeRefusal(t *testing.T, w *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != wantCode {
		t.Errorf("code = %q, want %s", e.Error.Code, wantCode)
	}
	for _, s := range []string{hfModel, "normally runs in your own cluster", "not reachable", "only offered outside your cloud account", "not sent anywhere"} {
		if !strings.Contains(e.Error.Message, s) {
			t.Errorf("message %q missing %q", e.Error.Message, s)
		}
	}
	for _, raw := range []string{"self_hosted", "external_api", "cloud_managed"} {
		if strings.Contains(e.Error.Message, raw) {
			t.Errorf("message %q leaks the raw class %q", e.Error.Message, raw)
		}
	}
	if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "event-stream") {
		t.Errorf("a refusal must be a JSON error, not SSE: %q", ct)
	}
}

func TestClassDowngrade_DirectModelAndStaticAlias_Refused(t *testing.T) {
	for _, model := range []string{hfModel, "static-chat"} {
		t.Run(model+"/non-stream", func(t *testing.T) {
			f := downgradeSetup(routing.ClassExternalAPI, routing.ClassSelfHosted, downgradeAlias, router.Decision{})
			w, _ := doChat(t, f.h, model)
			requireDowngradeRefusal(t, w, codeModelClassDowngraded)
			if n := f.calls(); n != 0 {
				t.Errorf("a backend was called %d times", n)
			}
		})
		t.Run(model+"/stream", func(t *testing.T) {
			f := downgradeSetup(routing.ClassExternalAPI, routing.ClassSelfHosted, downgradeAlias, router.Decision{})
			requireDowngradeRefusal(t, doStream(t, f.h, model), codeModelClassDowngraded)
			if n := f.calls(); n != 0 {
				t.Errorf("a backend was called %d times", n)
			}
		})
	}
}

func TestClassDowngrade_RegisteredAsCloudManaged_VendorStillRefused(t *testing.T) {
	f := downgradeSetup(routing.ClassExternalAPI, routing.ClassCloudManaged, downgradeAlias, router.Decision{})
	w, _ := doChat(t, f.h, hfModel)
	if w.Code != http.StatusServiceUnavailable || f.calls() != 0 {
		t.Fatalf("got %d, calls %d", w.Code, f.calls())
	}
}

func TestClassDowngrade_NotADowngrade_Served(t *testing.T) {
	cases := []struct {
		name, serving, registered string
		wantProv                  func(downgradeFixture) int
	}{
		{"both live, self-hosted wins the collision", routing.ClassSelfHosted, routing.ClassSelfHosted, func(f downgradeFixture) int { return f.local.completeCalls }},
		{"only a vendor ever claimed it", routing.ClassExternalAPI, routing.ClassExternalAPI, func(f downgradeFixture) int { return f.vend.completeCalls }},
		{"no registration (migration 028 not applied)", routing.ClassExternalAPI, "", func(f downgradeFixture) int { return f.vend.completeCalls }},
		{"registered looser than serving", routing.ClassSelfHosted, routing.ClassExternalAPI, func(f downgradeFixture) int { return f.local.completeCalls }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := downgradeSetup(tc.serving, tc.registered, downgradeAlias, router.Decision{})
			w, _ := doChat(t, f.h, hfModel)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if tc.wantProv(f) != 1 || f.calls() != 1 {
				t.Errorf("exactly one call on the expected backend: local=%d vendor=%d", f.local.completeCalls, f.vend.completeCalls)
			}
		})
	}
}

// A routed pick that is downgraded is a pick that cannot answer: the alias's
// setting decides, comparing the DEFAULT against the class the pick normally
// runs in (not the vendor class now serving it).
func TestClassDowngrade_RoutedPick_SettingDecides(t *testing.T) {
	pick := router.Decision{RouteName: "code", Reason: "rules:code:x"}
	for _, tc := range []struct {
		setting  string
		wantCode int
	}{
		{policy.OnRouteUnavailableRefuse, http.StatusServiceUnavailable},
		{policy.OnRouteUnavailableSameOrStricter, http.StatusOK}, // default is self-hosted: same class as the pick's home
		{policy.OnRouteUnavailableAnyPermitted, http.StatusOK},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			alias := downgradeAlias
			alias.OnRouteUnavailable = tc.setting
			f := downgradeSetup(routing.ClassExternalAPI, routing.ClassSelfHosted, alias, pick)
			w, c := doChat(t, f.h, "smart-chat")
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantCode, w.Body.String())
			}
			if f.vend.completeCalls+f.vend.streamCalls != 0 {
				t.Errorf("the vendor was called")
			}
			if tc.wantCode == http.StatusOK {
				if f.local.completeCalls != 1 {
					t.Errorf("default (self-hosted) route must answer, local calls = %d", f.local.completeCalls)
				}
				if rr, _ := c.Get("router_reason"); rr != "fallback:route_target_unreachable:code" {
					t.Errorf("router_reason = %v", rr)
				}
			}
		})
	}
}

// A vendor DEFAULT route must not rescue a downgraded self-hosted pick under
// the default setting: the pick's home class is self_hosted, the default's is
// external_api.
func TestClassDowngrade_RoutedPick_VendorDefaultNotUsedUnderDefaultSetting(t *testing.T) {
	alias := policy.AliasDef{
		Kind: policy.AliasKindRouted, DefaultRoute: "general",
		Routes: []policy.RouteDef{{Name: "code", Target: hfModel}, {Name: "general", Target: "vendor-default"}},
	}
	f := downgradeSetup(routing.ClassExternalAPI, routing.ClassSelfHosted, alias, router.Decision{RouteName: "code", Reason: "rules:code:x"})
	snap, _ := f.h.policy.Current()
	snap.Enabled["vendor-default"] = true
	f.h.policy = &fakePolicyStore{snap: snap, loaded: true}
	f.h.routes = staticRoutes{table: routing.NewTable(map[string]routing.Route{
		hfModel:          {ProviderKey: "prov-vendor", Kind: "openai", Class: routing.ClassExternalAPI},
		"vendor-default": {ProviderKey: "prov-vendor", Kind: "openai", Class: routing.ClassExternalAPI},
	})}
	w, _ := doChat(t, f.h, "smart-chat")
	if w.Code != http.StatusServiceUnavailable || f.calls() != 0 {
		t.Fatalf("got %d, calls %d: %s", w.Code, f.calls(), w.Body.String())
	}
}

func TestClassDowngrade_RoutedDefaultRoutePick_Refused(t *testing.T) {
	alias := policy.AliasDef{
		Kind: policy.AliasKindRouted, DefaultRoute: "general",
		Routes: []policy.RouteDef{{Name: "general", Target: hfModel}, {Name: "code", Target: "local-default"}},
	}
	f := downgradeSetup(routing.ClassExternalAPI, routing.ClassSelfHosted, alias, router.Decision{RouteName: "general", Reason: "rules:default"})
	w, _ := doChat(t, f.h, "smart-chat")
	requireDowngradeRefusal(t, w, codeRouteTargetUnavailable) // a routed pick: the route is down
	if f.calls() != 0 {
		t.Errorf("a backend was called")
	}
}

// The dry run reports what a real completion decides.
func TestClassDowngrade_Dryrun_Parity(t *testing.T) {
	pick := router.Decision{RouteName: "code", Reason: "rules:code:x"}
	t.Run("refuse", func(t *testing.T) {
		alias := downgradeAlias
		alias.OnRouteUnavailable = policy.OnRouteUnavailableRefuse
		f := downgradeSetup(routing.ClassExternalAPI, routing.ClassSelfHosted, alias, pick)
		_, resp := doDryrun(t, f.h, "smart-chat", "fix this")
		if resp.WouldBeRefused == nil || resp.WouldBeRefused.Code != codeRouteTargetUnavailable {
			t.Fatalf("got %+v", resp.WouldBeRefused)
		}
		if f.calls() != 0 {
			t.Errorf("dryrun dispatched")
		}
	})
	t.Run("default pick", func(t *testing.T) {
		alias := policy.AliasDef{
			Kind: policy.AliasKindRouted, DefaultRoute: "general",
			Routes: []policy.RouteDef{{Name: "general", Target: hfModel}, {Name: "code", Target: "local-default"}},
		}
		f := downgradeSetup(routing.ClassExternalAPI, routing.ClassSelfHosted, alias, router.Decision{RouteName: "general", Reason: "rules:default"})
		_, resp := doDryrun(t, f.h, "smart-chat", "hi")
		if resp.WouldBeRefused == nil || resp.WouldBeRefused.Code != codeRouteTargetUnavailable ||
			!strings.Contains(resp.WouldBeRefused.Message, "normally runs in your own cluster") {
			t.Fatalf("got %+v", resp.WouldBeRefused)
		}
	})
	t.Run("both live", func(t *testing.T) {
		f := downgradeSetup(routing.ClassSelfHosted, routing.ClassSelfHosted, downgradeAlias, pick)
		_, resp := doDryrun(t, f.h, "smart-chat", "fix this")
		if resp.WouldBeRefused != nil || resp.RouteName != "code" {
			t.Fatalf("got %+v", resp)
		}
	})
}

// Listing and counting match serving: an id chat would refuse as
// model_class_downgraded is not offered to a user key, not counted as
// reachable/allowed, and does not keep a principal from being "locked out".
func TestClassDowngrade_ListingAndCountsMatchServing(t *testing.T) {
	f := downgradeSetup(routing.ClassExternalAPI, routing.ClassSelfHosted, downgradeAlias, router.Decision{})
	snap, _ := f.h.policy.Current()
	tbl := f.h.routes.Snapshot()
	eff := policy.Effective(snap.ChainFor("", ""))

	for _, id := range reachableModels(eff, snap, tbl) {
		if id == hfModel {
			t.Errorf("reachableModels offers %s, which chat refuses", id)
		}
	}
	for _, id := range allowedModels(eff, snap, tbl) {
		if id == hfModel {
			t.Errorf("allowedModels counts %s, which chat refuses", id)
		}
	}
	// Both live: it is counted again.
	live := downgradeSetup(routing.ClassSelfHosted, routing.ClassSelfHosted, downgradeAlias, router.Decision{})
	lsnap, _ := live.h.policy.Current()
	found := false
	for _, id := range reachableModels(eff, lsnap, live.h.routes.Snapshot()) {
		found = found || id == hfModel
	}
	if !found {
		t.Errorf("a served-from-its-own-class model must be reachable")
	}

	// Only the downgraded model is enabled -> nothing routable, and a policy
	// is not blamed for the lockout.
	only := policy.Snapshot{Enabled: map[string]bool{hfModel: true}, RegisteredClass: snap.RegisteredClass, Residency: policy.ResidencyAny}
	if anyModelRoutable(only, tbl) {
		t.Errorf("a downgraded model must not count as routable")
	}
	if lockedOut(eff, only, tbl) {
		t.Errorf("nothing routable at all must not be blamed on the policy")
	}
}

func TestClassDowngrade_V1Models_UserKeyListFiltered_ServiceKeyFeedKept(t *testing.T) {
	list := func(keyType string, class string) []string {
		f := downgradeSetup(class, routing.ClassSelfHosted, downgradeAlias, router.Decision{})
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		c.Request.Header.Set("X-Darkplum-Classes", "external_api")
		c.Set(middleware.ContextKeyType, keyType)
		f.h.HandleListModels(c)
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		var ids []string
		for _, m := range body.Data {
			ids = append(ids, m.ID)
		}
		return ids
	}
	has := func(ids []string) bool {
		for _, id := range ids {
			if id == hfModel {
				return true
			}
		}
		return false
	}
	if has(list(middleware.KeyTypeUser, routing.ClassExternalAPI)) {
		t.Errorf("a user key must not be offered a model chat refuses")
	}
	if !has(list(middleware.KeyTypeUser, routing.ClassSelfHosted)) {
		t.Errorf("a user key is offered a model served from its own class")
	}
	if !has(list(middleware.KeyTypeService, routing.ClassExternalAPI)) {
		t.Errorf("the service-key discovery feed must keep reporting what each backend serves")
	}
}
