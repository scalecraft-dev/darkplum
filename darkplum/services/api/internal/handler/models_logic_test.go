package handler

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// residencyTruthTablePath is the SAME cross-service golden the gateway
// generates from policy.ResidencyPermits (see
// gateway/internal/policy/residency_truth_table_test.go) and
// services/web/scripts/check-policy.mts reads — guilfoyle's M3 review
// finding: one truth table, not three hand-typed copies that can drift out
// of sync with each other. Path is relative to this package
// (services/api/internal/handler).
const residencyTruthTablePath = "../../../gateway/internal/policy/testdata/residency_truth_table.json"

// residencyTruthTableCell/residencyTruthTable mirror the gateway's JSON
// shape exactly (see that package's doc comment on the field names being
// part of the cross-service contract).
type residencyTruthTableCell struct {
	Residency string `json:"residency"`
	Class     string `json:"class"`
	Permitted bool   `json:"permitted"`
}

type residencyTruthTable struct {
	ResidencyRank []string                  `json:"residency_rank"`
	KnownClasses  []string                  `json:"known_classes"`
	UnknownToken  string                    `json:"unknown_token"`
	Cells         []residencyTruthTableCell `json:"cells"`
}

func loadResidencyTruthTable(t *testing.T) residencyTruthTable {
	t.Helper()
	data, err := os.ReadFile(residencyTruthTablePath)
	if err != nil {
		t.Fatalf("read %s: %v", residencyTruthTablePath, err)
	}
	var table residencyTruthTable
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatalf("decode %s: %v", residencyTruthTablePath, err)
	}
	return table
}

// exampleUnknownResidency/exampleUnknownClass exercise a golden cell whose
// residency/class is table.UnknownToken — see the gateway's identical
// constants for why any other unrecognised string classifies the same way.
const (
	exampleUnknownResidency = "not-a-real-residency"
	exampleUnknownClass     = "some_future_class"
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
		"external_api":  backendClassExternalAPI,
		// Unrecognized/absent defaults to the STRICTEST known class
		// (external_api), not self_hosted -- see normalizeBackendClass's doc
		// comment for why that flipped once a third class existed.
		"":              backendClassExternalAPI,
		"bogus":         backendClassExternalAPI,
		"Cloud_Managed": backendClassExternalAPI, // case-sensitive; not the exact literal
	}
	for input, want := range cases {
		if got := normalizeBackendClass(input); got != want {
			t.Errorf("normalizeBackendClass(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestResidencyRank asserts this package's residencyRank ladder against the
// SAME cross-service residency_rank order the gateway generates from its own
// residencyByRank (see residencyTruthTablePath) -- not a hand-typed
// duplicate, so a ladder change in one implementation without the other
// fails here.
func TestResidencyRank(t *testing.T) {
	table := loadResidencyTruthTable(t)
	for i, residency := range table.ResidencyRank {
		if got := residencyRank(residency); got != i {
			t.Errorf("residencyRank(%q) = %d, want %d (per %s's residency_rank order)", residency, got, i, residencyTruthTablePath)
		}
		for j := i + 1; j < len(table.ResidencyRank); j++ {
			if residencyRank(residency) >= residencyRank(table.ResidencyRank[j]) {
				t.Errorf("%q must rank below %q", residency, table.ResidencyRank[j])
			}
		}
	}
	if residencyRank(exampleUnknownResidency) != residencyRank(table.ResidencyRank[len(table.ResidencyRank)-1]) {
		t.Errorf("unknown residency must rank as strict as %q", table.ResidencyRank[len(table.ResidencyRank)-1])
	}
}

// TestResidencyPermitsClass asserts residencyPermitsClass against EVERY cell
// of the shared cross-service truth table (residencyTruthTablePath) -- the
// SAME artifact the gateway's policy.ResidencyPermits and
// services/web/scripts/check-policy.mts are asserted against -- rather than
// a hand-typed table that could silently drift from either. See
// guilfoyle's M3 review finding.
func TestResidencyPermitsClass(t *testing.T) {
	table := loadResidencyTruthTable(t)
	if len(table.Cells) == 0 {
		t.Fatalf("%s has no cells -- did the gateway generator run?", residencyTruthTablePath)
	}
	for _, cell := range table.Cells {
		residency := cell.Residency
		if residency == table.UnknownToken {
			residency = exampleUnknownResidency
		}
		class := cell.Class
		if class == table.UnknownToken {
			class = exampleUnknownClass
		}
		if got := residencyPermitsClass(residency, class); got != cell.Permitted {
			t.Errorf("residencyPermitsClass(%q, %q) = %v, want %v (per %s)", residency, class, got, cell.Permitted, residencyTruthTablePath)
		}
	}
}

func TestRequestPermitsExternalAPI(t *testing.T) {
	cases := map[string]bool{
		"":                          false,
		"external_api":              true,
		" external_api ":            true,
		"external_api,other":        true,
		"other,external_api":        true,
		"External_API":              false, // case-sensitive; not the exact token
		"externalapi":               false,
		"self_hosted,cloud_managed": false,
	}
	for input, want := range cases {
		if got := requestPermitsExternalAPI(input); got != want {
			t.Errorf("requestPermitsExternalAPI(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestModelVisibleInList(t *testing.T) {
	// The capability header hides external_api regardless of how permissive
	// residency is.
	if modelVisibleInList(residencyAny, false, backendClassExternalAPI) {
		t.Error("external_api must be hidden when the caller didn't send the classes header, even under residency=any")
	}
	if !modelVisibleInList(residencyAny, true, backendClassExternalAPI) {
		t.Error("external_api must be visible under residency=any when the caller sent the classes header")
	}
	// Residency still applies once the header is present.
	if modelVisibleInList(residencyCloudAccountOnly, true, backendClassExternalAPI) {
		t.Error("cloud_account_only must still refuse external_api even with the header present")
	}
	// The header has no bearing on non-external_api classes.
	if !modelVisibleInList(residencyAny, false, backendClassSelfHosted) {
		t.Error("self_hosted must remain visible regardless of the classes header")
	}
	if !modelVisibleInList(residencyAny, false, backendClassCloudManaged) {
		t.Error("cloud_managed must remain visible regardless of the classes header")
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

	got := buildModelsList(models, aliases, nil, residencyAny, true, now, modelAvailabilityWindow, nil)
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

	got := buildModelsList(models, aliases, nil, residencySelfHostedOnly, true, now, modelAvailabilityWindow, nil)

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

// TestBuildModelsList_CloudAccountOnlyPermitsCloudManagedRefusesExternalAPI
// covers the third residency rung specifically: cloud_account_only must
// still show a cloud_managed model (it never left the cloud account) but
// must refuse an external_api one (it does).
func TestBuildModelsList_CloudAccountOnlyPermitsCloudManagedRefusesExternalAPI(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	models := []modelDBRow{
		{ModelID: "llama3", Provider: "vllm", BackendClass: backendClassSelfHosted, DiscoveredAt: now},
		{ModelID: "claude-3-sonnet", Provider: "bedrock", BackendClass: backendClassCloudManaged, DiscoveredAt: now},
		{ModelID: "claude-3-opus", Provider: "anthropic", BackendClass: backendClassExternalAPI, DiscoveredAt: now},
	}

	got := buildModelsList(models, nil, nil, residencyCloudAccountOnly, true, now, modelAvailabilityWindow, nil)

	ids := map[string]bool{}
	for _, e := range got {
		ids[e.ID] = true
	}
	if !ids["llama3"] || !ids["claude-3-sonnet"] {
		t.Fatalf("expected self_hosted and cloud_managed present under cloud_account_only, got %v", ids)
	}
	if ids["claude-3-opus"] {
		t.Fatalf("expected external_api omitted under cloud_account_only, got %v", ids)
	}
}

// TestBuildModelsList_ExternalAPIHiddenWithoutCapabilityHeader covers the
// rollout-safety gate independently of residency: even under the loosest
// residency (any), an external_api model must not appear unless
// permitExternalAPI is true.
func TestBuildModelsList_ExternalAPIHiddenWithoutCapabilityHeader(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	models := []modelDBRow{
		{ModelID: "claude-3-opus", Provider: "anthropic", BackendClass: backendClassExternalAPI, DiscoveredAt: now},
	}

	got := buildModelsList(models, nil, nil, residencyAny, false, now, modelAvailabilityWindow, nil)
	if len(got) != 0 {
		t.Fatalf("expected external_api hidden without the capability header, got %+v", got)
	}

	got = buildModelsList(models, nil, nil, residencyAny, true, now, modelAvailabilityWindow, nil)
	if len(got) != 1 {
		t.Fatalf("expected external_api present with the capability header under residency=any, got %+v", got)
	}
}

func TestBuildModelsList_EmptyInputsProduceEmptySlice(t *testing.T) {
	got := buildModelsList(nil, nil, nil, residencyAny, true, time.Now(), modelAvailabilityWindow, nil)
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

	got := buildModelsList(models, aliases, nil, residencyAny, true, now, modelAvailabilityWindow, nil)

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
		{ID: "no-owner"}, // OwnedBy defaults to "vllm", BackendClass defaults to external_api (strictest)
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
	if noOwner.BackendClass != backendClassExternalAPI {
		t.Errorf("no-owner BackendClass = %q, want external_api default (strictest known class)", noOwner.BackendClass)
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

	got := buildModelsList(nil, nil, routed, residencyAny, true, now, modelAvailabilityWindow, nil)
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
			got := buildModelsList(nil, nil, tc.routed, residencyAny, true, now, modelAvailabilityWindow, nil)
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

	got := buildModelsList(nil, nil, routed, residencySelfHostedOnly, true, now, modelAvailabilityWindow, nil)
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
	got = buildModelsList(nil, nil, routed, residencySelfHostedOnly, true, now, modelAvailabilityWindow, nil)
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

	got := buildModelsList(models, aliases, nil, residencyAny, true, now, modelAvailabilityWindow, nil)
	for _, e := range got {
		if e.ID == "default" && e.AliasKind != aliasKindStatic {
			t.Errorf("static alias entry AliasKind = %q, want %q", e.AliasKind, aliasKindStatic)
		}
		if e.ID == "llama3" && e.AliasKind != "" {
			t.Errorf("plain model entry AliasKind = %q, want empty", e.AliasKind)
		}
	}
}
