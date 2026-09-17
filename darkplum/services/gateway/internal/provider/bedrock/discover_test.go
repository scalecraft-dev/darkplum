package bedrock

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
)

// fakeControlAPI implements controlAPI, returning one canned page per call in
// pages order (indexed by call count) so pagination can be exercised without
// any AWS SDK network involvement.
type fakeControlAPI struct {
	pages [][]bedrocktypes.InferenceProfileSummary
	err   error

	calls          int
	lastNextTokens []*string
}

func (f *fakeControlAPI) ListInferenceProfiles(_ context.Context, params *bedrock.ListInferenceProfilesInput, _ ...func(*bedrock.Options)) (*bedrock.ListInferenceProfilesOutput, error) {
	f.lastNextTokens = append(f.lastNextTokens, params.NextToken)
	if f.err != nil {
		return nil, f.err
	}
	idx := f.calls
	f.calls++
	if idx >= len(f.pages) {
		return &bedrock.ListInferenceProfilesOutput{}, nil
	}

	out := &bedrock.ListInferenceProfilesOutput{InferenceProfileSummaries: f.pages[idx]}
	if idx < len(f.pages)-1 {
		tok := "page-token-" + string(rune('0'+idx+1))
		out.NextToken = &tok
	}
	return out, nil
}

func activeProfile(id string) bedrocktypes.InferenceProfileSummary {
	return bedrocktypes.InferenceProfileSummary{
		InferenceProfileId: aws.String(id),
		Status:             bedrocktypes.InferenceProfileStatusActive,
	}
}

func TestDiscoverModels_FollowsPagination(t *testing.T) {
	fake := &fakeControlAPI{
		pages: [][]bedrocktypes.InferenceProfileSummary{
			{activeProfile("us.anthropic.claude-3-5-sonnet"), activeProfile("us.anthropic.claude-3-haiku")},
			{activeProfile("us.meta.llama3-70b"), activeProfile("us.mistral.mistral-large")},
		},
	}
	p := &BedrockProvider{controlClient: fake, region: "us-east-1"}

	models, err := p.DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 4 {
		t.Fatalf("expected 4 models across 2 pages, got %d: %v", len(models), models)
	}
	for _, id := range []string{
		"us.anthropic.claude-3-5-sonnet", "us.anthropic.claude-3-haiku",
		"us.meta.llama3-70b", "us.mistral.mistral-large",
	} {
		if !models[id] {
			t.Errorf("expected %q to be discovered", id)
		}
	}
	if fake.calls != 2 {
		t.Errorf("expected exactly 2 ListInferenceProfiles calls (one per page), got %d", fake.calls)
	}

	// Second call must forward the first page's NextToken.
	if fake.lastNextTokens[0] != nil {
		t.Errorf("expected the first call's NextToken to be nil, got %v", fake.lastNextTokens[0])
	}
	if fake.lastNextTokens[1] == nil || *fake.lastNextTokens[1] != "page-token-1" {
		t.Errorf("expected the second call to forward NextToken %q, got %v", "page-token-1", fake.lastNextTokens[1])
	}
}

func TestDiscoverModels_SinglePageNoNextToken(t *testing.T) {
	fake := &fakeControlAPI{
		pages: [][]bedrocktypes.InferenceProfileSummary{
			{activeProfile("us.anthropic.claude-3-5-sonnet")},
		},
	}
	p := &BedrockProvider{controlClient: fake, region: "us-east-1"}

	models, err := p.DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if fake.calls != 1 {
		t.Errorf("expected exactly 1 call when there is no NextToken, got %d", fake.calls)
	}
}

func TestDiscoverModels_InactiveProfilesExcluded(t *testing.T) {
	inactive := bedrocktypes.InferenceProfileSummary{
		InferenceProfileId: aws.String("us.anthropic.claude-legacy"),
		Status:             bedrocktypes.InferenceProfileStatus("NOT_ACTIVE"),
	}
	fake := &fakeControlAPI{
		pages: [][]bedrocktypes.InferenceProfileSummary{
			{activeProfile("us.anthropic.claude-3-5-sonnet"), inactive},
		},
	}
	p := &BedrockProvider{controlClient: fake, region: "us-east-1"}

	models, err := p.DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 1 || !models["us.anthropic.claude-3-5-sonnet"] {
		t.Errorf("expected only the active profile, got %v", models)
	}
}

func TestDiscoverModels_ErrorOnFirstPagePropagates(t *testing.T) {
	fake := &fakeControlAPI{err: errors.New("access denied")}
	p := &BedrockProvider{controlClient: fake, region: "us-east-1"}

	if _, err := p.DiscoverModels(context.Background()); err == nil {
		t.Fatal("expected an error to propagate")
	}
}

// compile-time check: the real SDK client satisfies controlAPI structurally.
var _ controlAPI = (*bedrock.Client)(nil)
