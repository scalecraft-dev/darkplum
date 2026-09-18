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

	got := buildModelsList(models, aliases, residencyAny, now, modelAvailabilityWindow)
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

	got := buildModelsList(models, aliases, residencySelfHostedOnly, now, modelAvailabilityWindow)

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
	got := buildModelsList(nil, nil, residencyAny, time.Now(), modelAvailabilityWindow)
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

	got := buildModelsList(models, aliases, residencyAny, now, modelAvailabilityWindow)

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
