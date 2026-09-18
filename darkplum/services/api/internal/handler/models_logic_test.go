package handler

import (
	"testing"
	"time"
)

func TestModelIsAvailable(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		discoveredAt time.Time
		want         bool
	}{
		{"just now", now, true},
		{"1 minute ago", now.Add(-1 * time.Minute), true},
		{"just under the 2-minute cutoff", now.Add(-modelAvailabilityWindow + time.Second), true},
		{"exactly at the cutoff (exclusive)", now.Add(-modelAvailabilityWindow), false},
		{"just over the cutoff", now.Add(-modelAvailabilityWindow - time.Second), false},
		{"long stale (well within the 7-day purge window)", now.Add(-24 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelIsAvailable(tc.discoveredAt, now); got != tc.want {
				t.Errorf("modelIsAvailable(%v, now) = %v, want %v", tc.discoveredAt, got, tc.want)
			}
		})
	}
}

func TestNormalizeBackendClass(t *testing.T) {
	cases := map[string]string{
		"self_hosted":   backendClassSelfHosted,
		"cloud_managed": backendClassCloudManaged,
		"":              backendClassSelfHosted,
		"bogus":         backendClassSelfHosted,
		"Cloud_Managed": backendClassSelfHosted, // case-sensitive; not the exact literal
	}
	for input, want := range cases {
		if got := normalizeBackendClass(input); got != want {
			t.Errorf("normalizeBackendClass(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBuildModelsList_NoPolicyIncludesEverything(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	models := []modelDBRow{
		{ModelID: "llama3", Provider: "vllm", BackendClass: backendClassSelfHosted, DiscoveredAt: now},
		{ModelID: "claude-3-sonnet", Provider: "bedrock", BackendClass: backendClassCloudManaged, DiscoveredAt: now},
	}
	aliases := []aliasDBRow{
		{Alias: "default", TargetModelID: "llama3", OwnedBy: "vllm", BackendClass: backendClassSelfHosted, TargetDiscoveredAt: now},
		{Alias: "smart", TargetModelID: "claude-3-sonnet", OwnedBy: "bedrock", BackendClass: backendClassCloudManaged, TargetDiscoveredAt: now},
	}

	got := buildModelsList(models, aliases, nil, residencyAny, now, modelAvailabilityWindow, nil)
	if len(got) != 4 {
		t.Fatalf("len(got) = %d, want 4: %+v", len(got), got)
	}

	byID := map[string]openAIModelEntry{}
	for _, e := range got {
		byID[e.ID] = e
	}

	if e, ok := byID["llama3"]; !ok || e.AliasOf != nil {
		t.Errorf("llama3 entry = %+v, want a plain model entry with no alias_of", e)
	}
	alias, ok := byID["default"]
	if !ok {
		t.Fatalf("expected alias entry %q in result", "default")
	}
	if alias.AliasOf == nil || *alias.AliasOf != "llama3" {
		t.Errorf("alias entry AliasOf = %v, want llama3", alias.AliasOf)
	}
	if alias.OwnedBy != "vllm" || alias.BackendClass != backendClassSelfHosted {
		t.Errorf("alias entry did not copy target owned_by/backend_class: %+v", alias)
	}
}

func TestBuildModelsList_SelfHostedOnlyOmitsCloudManaged(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	models := []modelDBRow{
		{ModelID: "llama3", Provider: "vllm", BackendClass: backendClassSelfHosted, DiscoveredAt: now},
		{ModelID: "claude-3-sonnet", Provider: "bedrock", BackendClass: backendClassCloudManaged, DiscoveredAt: now},
	}
	aliases := []aliasDBRow{
		{Alias: "default", TargetModelID: "llama3", OwnedBy: "vllm", BackendClass: backendClassSelfHosted, TargetDiscoveredAt: now},
		{Alias: "smart", TargetModelID: "claude-3-sonnet", OwnedBy: "bedrock", BackendClass: backendClassCloudManaged, TargetDiscoveredAt: now},
	}

	got := buildModelsList(models, aliases, nil, residencySelfHostedOnly, now, modelAvailabilityWindow, nil)

	for _, e := range got {
		if e.BackendClass == backendClassCloudManaged {
			t.Fatalf("self_hosted_only leaked a cloud_managed entry: %+v", e)
		}
		if e.ID == "smart" || e.ID == "claude-3-sonnet" {
			t.Fatalf("self_hosted_only leaked cloud-managed entry %q", e.ID)
		}
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (llama3 + default): %+v", len(got), got)
	}
}

func TestBuildModelsList_EmptyInputsProduceEmptySlice(t *testing.T) {
	got := buildModelsList(nil, nil, nil, residencyAny, time.Now(), modelAvailabilityWindow, nil)
	if got == nil {
		t.Fatal("buildModelsList returned nil, want a non-nil empty slice so JSON encodes []")
	}
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
}

// TestBuildModelsList_UnavailableModelAndAliasOmitted exercises the
// availability cutoff itself (as opposed to residency filtering): a model
// that hasn't been re-discovered recently enough, and an alias pointing at
// one, must be omitted even though both are enabled and residency = any.
// This is the list-builder half of surviving a slow backend restart without
// the gateway routing chat traffic to a model that isn't actually up yet.
func TestBuildModelsList_UnavailableModelAndAliasOmitted(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-10 * time.Minute) // well past modelAvailabilityWindow

	models := []modelDBRow{
		{ModelID: "fresh", Provider: "vllm", BackendClass: backendClassSelfHosted, DiscoveredAt: now},
		{ModelID: "stale", Provider: "vllm", BackendClass: backendClassSelfHosted, DiscoveredAt: stale},
	}
	aliases := []aliasDBRow{
		{Alias: "default", TargetModelID: "fresh", OwnedBy: "vllm", BackendClass: backendClassSelfHosted, TargetDiscoveredAt: now},
		{Alias: "cold", TargetModelID: "stale", OwnedBy: "vllm", BackendClass: backendClassSelfHosted, TargetDiscoveredAt: stale},
	}

	got := buildModelsList(models, aliases, nil, residencyAny, now, modelAvailabilityWindow, nil)

	ids := map[string]bool{}
	for _, e := range got {
		ids[e.ID] = true
	}
	if ids["stale"] || ids["cold"] {
		t.Fatalf("unavailable model/alias leaked into list: %v", ids)
	}
	if !ids["fresh"] || !ids["default"] {
		t.Fatalf("expected available model+alias present, got %v", ids)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2: %+v", len(got), got)
	}
}

// TestNormalizeGatewayModels covers the field defaulting and de-dup
// normalizeGatewayModels applies before SyncModels ever touches the
// database: this is what protects the batch upsert from Postgres's
// "ON CONFLICT DO UPDATE command cannot affect row a second time" error if
// the gateway (against contract) reports the same model id twice.
func TestNormalizeGatewayModels(t *testing.T) {
	entries := []openAIModelEntry{
		{ID: "", OwnedBy: "vllm"}, // empty id must be dropped
		{ID: "llama3", OwnedBy: "vllm", BackendClass: backendClassSelfHosted},
		{ID: "no-owner"}, // OwnedBy defaults to "vllm", BackendClass defaults to self_hosted
		{ID: "llama3", OwnedBy: "bedrock", BackendClass: backendClassCloudManaged}, // duplicate id, last one wins
	}

	got := normalizeGatewayModels(entries)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (empty id dropped, duplicate id collapsed): %+v", len(got), got)
	}

	byID := map[string]normalizedModel{}
	for _, m := range got {
		byID[m.ID] = m
	}

	llama, ok := byID["llama3"]
	if !ok {
		t.Fatalf("expected llama3 in normalized output, got %+v", got)
	}
	if llama.Provider != "bedrock" || llama.BackendClass != backendClassCloudManaged {
		t.Errorf("llama3 = %+v, want the last-seen occurrence (bedrock/cloud_managed) to win", llama)
	}

	noOwner, ok := byID["no-owner"]
	if !ok {
		t.Fatalf("expected no-owner in normalized output, got %+v", got)
	}
	if noOwner.Provider != "vllm" {
		t.Errorf("no-owner Provider = %q, want vllm default", noOwner.Provider)
	}
	if noOwner.BackendClass != backendClassSelfHosted {
		t.Errorf("no-owner BackendClass = %q, want self_hosted default", noOwner.BackendClass)
	}
}

// TestBuildModelsList_RoutedAlias_ViableEntryShape covers a routed alias with
// all routes viable: it must appear with alias_kind="routed", alias_of set
// to the DEFAULT route's target (even though "code" isn't first), and a
// routes[] listing every route's name+description only (no target ids).
func TestBuildModelsList_RoutedAlias_ViableEntryShape(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	routed := []routedAliasDBRow{
		{
			Alias:        "smart",
			DefaultRoute: "general",
			Routes: []routedAliasRouteDBRow{
				{RouteName: "code", Description: "code stuff", TargetModelID: "llama3-code", TargetExists: true, TargetEnabled: true, TargetProvider: "vllm", TargetBackendClass: backendClassSelfHosted, TargetDiscoveredAt: now},
				{RouteName: "general", Description: "general stuff", TargetModelID: "llama3", TargetExists: true, TargetEnabled: true, TargetProvider: "vllm", TargetBackendClass: backendClassSelfHosted, TargetDiscoveredAt: now},
			},
		},
	}

	got := buildModelsList(nil, nil, routed, residencyAny, now, modelAvailabilityWindow, nil)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1: %+v", len(got), got)
	}
	e := got[0]
	if e.ID != "smart" || e.AliasKind != aliasKindRouted {
		t.Fatalf("entry = %+v, want id=smart alias_kind=routed", e)
	}
	if e.AliasOf == nil || *e.AliasOf != "llama3" {
		t.Fatalf("alias_of = %v, want the default route's target (llama3)", e.AliasOf)
	}
	if len(e.Routes) != 2 {
		t.Fatalf("routes = %+v, want 2 entries", e.Routes)
	}
	for _, rs := range e.Routes {
		if rs.RouteName == "" || rs.Description == "" || rs.BackendClass != backendClassSelfHosted {
			t.Errorf("route summary missing name/description/backend_class: %+v", rs)
		}
	}
}

// TestBuildModelsList_RoutedAlias_OmittedWhenNoViableRoute covers the omission
// rule: a routed alias where every route's target is disabled, missing, or
// residency-forbidden must not appear at all, even though its default route
// names a real (but currently unreachable) target.
func TestBuildModelsList_RoutedAlias_OmittedWhenNoViableRoute(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-10 * time.Minute)

	cases := []struct {
		name   string
		routed []routedAliasDBRow
	}{
		{
			"target disabled",
			[]routedAliasDBRow{{Alias: "smart", DefaultRoute: "general", Routes: []routedAliasRouteDBRow{
				{RouteName: "general", Description: "d", TargetModelID: "llama3", TargetExists: true, TargetEnabled: false, TargetBackendClass: backendClassSelfHosted, TargetDiscoveredAt: now},
			}}},
		},
		{
			"target missing",
			[]routedAliasDBRow{{Alias: "smart", DefaultRoute: "general", Routes: []routedAliasRouteDBRow{
				{RouteName: "general", Description: "d", TargetModelID: "llama3", TargetExists: false, TargetEnabled: false, TargetDiscoveredAt: now},
			}}},
		},
		{
			"target stale (unavailable)",
			[]routedAliasDBRow{{Alias: "smart", DefaultRoute: "general", Routes: []routedAliasRouteDBRow{
				{RouteName: "general", Description: "d", TargetModelID: "llama3", TargetExists: true, TargetEnabled: true, TargetBackendClass: backendClassSelfHosted, TargetDiscoveredAt: stale},
			}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildModelsList(nil, nil, tc.routed, residencyAny, now, modelAvailabilityWindow, nil)
			if len(got) != 0 {
				t.Fatalf("expected the routed alias to be omitted, got %+v", got)
			}
		})
	}
}

// TestBuildModelsList_RoutedAlias_ResidencyOmitsCloudManagedRouteButAliasSurvives
// covers the partial-viability case: under self_hosted_only, a route whose
// target is cloud_managed can't make the alias viable by itself, but the
// alias still appears if at least one OTHER route is self_hosted+viable.
func TestBuildModelsList_RoutedAlias_ResidencyPartialViability(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	routed := []routedAliasDBRow{
		{
			Alias:        "smart",
			DefaultRoute: "general",
			Routes: []routedAliasRouteDBRow{
				{RouteName: "general", Description: "d", TargetModelID: "llama3", TargetExists: true, TargetEnabled: true, TargetProvider: "vllm", TargetBackendClass: backendClassSelfHosted, TargetDiscoveredAt: now},
				{RouteName: "cloud", Description: "d", TargetModelID: "claude-3-sonnet", TargetExists: true, TargetEnabled: true, TargetProvider: "bedrock", TargetBackendClass: backendClassCloudManaged, TargetDiscoveredAt: now},
			},
		},
	}

	got := buildModelsList(nil, nil, routed, residencySelfHostedOnly, now, modelAvailabilityWindow, nil)
	if len(got) != 1 {
		t.Fatalf("expected the alias to survive on its self_hosted route, got %+v", got)
	}
	if len(got[0].Routes) != 2 {
		t.Fatalf("routes summary should still list both routes (names/descriptions only), got %+v", got[0].Routes)
	}
	// Routes' backend_class must reflect each route's own target (mixed here),
	// even though the alias as a whole survives on the self_hosted one — this
	// is what lets the picker say "varies by route".
	if got[0].Routes[0].BackendClass != backendClassSelfHosted || got[0].Routes[1].BackendClass != backendClassCloudManaged {
		t.Fatalf("routes backend_class mismatch: %+v", got[0].Routes)
	}

	// Now BOTH routes are cloud_managed: nothing is viable, alias omitted.
	routed[0].Routes[0].TargetBackendClass = backendClassCloudManaged
	got = buildModelsList(nil, nil, routed, residencySelfHostedOnly, now, modelAvailabilityWindow, nil)
	if len(got) != 0 {
		t.Fatalf("expected the alias to be omitted when no route is residency-permitted, got %+v", got)
	}
}

// TestBuildModelsList_StaticAliasGetsAliasKind covers the "static entries
// gain alias_kind:'static'" rule.
func TestBuildModelsList_StaticAliasGetsAliasKind(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	models := []modelDBRow{{ModelID: "llama3", Provider: "vllm", BackendClass: backendClassSelfHosted, DiscoveredAt: now}}
	aliases := []aliasDBRow{{Alias: "default", TargetModelID: "llama3", OwnedBy: "vllm", BackendClass: backendClassSelfHosted, TargetDiscoveredAt: now}}

	got := buildModelsList(models, aliases, nil, residencyAny, now, modelAvailabilityWindow, nil)
	for _, e := range got {
		if e.ID == "default" && e.AliasKind != aliasKindStatic {
			t.Errorf("static alias entry AliasKind = %q, want %q", e.AliasKind, aliasKindStatic)
		}
		if e.ID == "llama3" && e.AliasKind != "" {
			t.Errorf("plain model entry AliasKind = %q, want empty", e.AliasKind)
		}
	}
}
