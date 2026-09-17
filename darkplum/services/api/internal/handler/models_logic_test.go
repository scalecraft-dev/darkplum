package handler

import "testing"

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
	models := []modelDBRow{
		{ModelID: "llama3", Provider: "vllm", BackendClass: backendClassSelfHosted},
		{ModelID: "claude-3-sonnet", Provider: "bedrock", BackendClass: backendClassCloudManaged},
	}
	aliases := []aliasDBRow{
		{Alias: "default", TargetModelID: "llama3", OwnedBy: "vllm", BackendClass: backendClassSelfHosted},
		{Alias: "smart", TargetModelID: "claude-3-sonnet", OwnedBy: "bedrock", BackendClass: backendClassCloudManaged},
	}

	got := buildModelsList(models, aliases, residencyAny, 1000)
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
	models := []modelDBRow{
		{ModelID: "llama3", Provider: "vllm", BackendClass: backendClassSelfHosted},
		{ModelID: "claude-3-sonnet", Provider: "bedrock", BackendClass: backendClassCloudManaged},
	}
	aliases := []aliasDBRow{
		{Alias: "default", TargetModelID: "llama3", OwnedBy: "vllm", BackendClass: backendClassSelfHosted},
		{Alias: "smart", TargetModelID: "claude-3-sonnet", OwnedBy: "bedrock", BackendClass: backendClassCloudManaged},
	}

	got := buildModelsList(models, aliases, residencySelfHostedOnly, 1000)

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
	got := buildModelsList(nil, nil, residencyAny, 1000)
	if got == nil {
		t.Fatal("buildModelsList returned nil, want a non-nil empty slice so JSON encodes []")
	}
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
}
