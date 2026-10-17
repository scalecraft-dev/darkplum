package routing

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/provider/credsource"
)

// fakeOpenAICreds is an openaiCredentialChecker fake.
type fakeOpenAICreds struct {
	key     string
	version uint64
	err     error
}

func (f *fakeOpenAICreds) Key() (string, uint64, error) { return f.key, f.version, f.err }

// fakeOpenAIDiscoverer implements openaiDiscoverer without any real
// HTTP/network involvement.
type fakeOpenAIDiscoverer struct {
	models  map[string]bool
	discErr error
}

func (f *fakeOpenAIDiscoverer) Name() string { return "openai" }
func (f *fakeOpenAIDiscoverer) Complete(_ context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeOpenAIDiscoverer) Stream(_ context.Context, _ provider.ChatRequest, _ io.Writer) (*provider.StreamStats, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeOpenAIDiscoverer) DiscoverModels(_ context.Context) (map[string]bool, error) {
	if f.discErr != nil {
		return nil, f.discErr
	}
	return f.models, nil
}

// fakeOpenAIStatusReporter records every Report call.
type fakeOpenAIStatusReporter struct {
	mu      sync.Mutex
	reports []provider.ProviderStatus
}

func (f *fakeOpenAIStatusReporter) Report(name string, st provider.ProviderStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st.Name = name
	f.reports = append(f.reports, st)
}

func (f *fakeOpenAIStatusReporter) last() provider.ProviderStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reports) == 0 {
		return provider.ProviderStatus{}
	}
	return f.reports[len(f.reports)-1]
}

func TestOpenAISource_NotConfigured(t *testing.T) {
	creds := &fakeOpenAICreds{err: credsource.ErrNotConfigured}
	disc := &fakeOpenAIDiscoverer{}
	status := &fakeOpenAIStatusReporter{}
	src := NewOpenAISource(creds, disc, staticVendorFilter([]string{"gpt-*"}), status)

	ids, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("not-configured must not be a discovery error, got %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected no ids, got %v", ids)
	}
	if got := status.last(); got.State != provider.ProviderStateNotConfigured || got.Configured {
		t.Errorf("expected not_configured/Configured=false, got %+v", got)
	}
}

// TestOpenAISource_CredentialUnreadable covers a key file that EXISTS but is
// empty/unreadable (as opposed to ErrNotConfigured). Must report auth_failed,
// not not_configured — same STEP 0 ruling as anthropic's identical test.
func TestOpenAISource_CredentialUnreadable(t *testing.T) {
	creds := &fakeOpenAICreds{err: errors.New("credsource: permission denied")}
	disc := &fakeOpenAIDiscoverer{}
	status := &fakeOpenAIStatusReporter{}
	src := NewOpenAISource(creds, disc, staticVendorFilter([]string{"gpt-*"}), status)

	ids, err := src.Discover(context.Background())
	if err == nil {
		t.Fatal("expected an unreadable-key error to surface as a discovery failure")
	}
	if len(ids) != 0 {
		t.Errorf("expected no ids, got %v", ids)
	}
	if got := status.last(); got.State != provider.ProviderStateAuthFailed || !got.Configured {
		t.Errorf("expected auth_failed/Configured=true for an unreadable key, got %+v", got)
	}
}

func TestOpenAISource_FilterMissing(t *testing.T) {
	creds := &fakeOpenAICreds{key: "sk-x", version: 1}
	disc := &fakeOpenAIDiscoverer{models: map[string]bool{"gpt-5": true}}
	status := &fakeOpenAIStatusReporter{}
	src := NewOpenAISource(creds, disc, staticVendorFilter(nil), status)

	ids, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected no ids when the filter is empty, got %v", ids)
	}
	if got := status.last(); got.State != provider.ProviderStateFilterMissing || !got.Configured {
		t.Errorf("expected filter_missing/Configured=true, got %+v", got)
	}
}

func TestOpenAISource_FilterMatchedNothing(t *testing.T) {
	creds := &fakeOpenAICreds{key: "sk-x", version: 1}
	disc := &fakeOpenAIDiscoverer{models: map[string]bool{"gpt-5": true}}
	status := &fakeOpenAIStatusReporter{}
	src := NewOpenAISource(creds, disc, staticVendorFilter([]string{"claude-*"}), status)

	ids, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected no ids, got %v", ids)
	}
	if got := status.last(); got.State != provider.ProviderStateFilterMatchedNothing {
		t.Errorf("expected filter_matched_nothing, got %+v", got)
	}
	if status.last().LastSyncAt == nil {
		t.Error("expected LastSyncAt to be set: a real discovery call was made")
	}
}

func TestOpenAISource_Unreachable(t *testing.T) {
	creds := &fakeOpenAICreds{key: "sk-x", version: 1}
	disc := &fakeOpenAIDiscoverer{discErr: errFakeGeneric}
	status := &fakeOpenAIStatusReporter{}
	src := NewOpenAISource(creds, disc, staticVendorFilter([]string{"gpt-*"}), status)

	ids, err := src.Discover(context.Background())
	if err == nil {
		t.Fatal("expected a discovery error")
	}
	if len(ids) != 0 {
		t.Errorf("expected no ids, got %v", ids)
	}
	if got := status.last(); got.State != provider.ProviderStateUnreachable {
		t.Errorf("expected unreachable for a non-auth discovery error, got %+v", got)
	}
}

func TestOpenAISource_OK(t *testing.T) {
	creds := &fakeOpenAICreds{key: "sk-x", version: 1}
	disc := &fakeOpenAIDiscoverer{models: map[string]bool{
		"gpt-5":      true,
		"gpt-5-mini": true,
		"claude-x":   true,
	}}
	status := &fakeOpenAIStatusReporter{}
	src := NewOpenAISource(creds, disc, staticVendorFilter([]string{"gpt-*"}), status)

	ids, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 filtered ids, got %v", ids)
	}
	if got := status.last(); got.State != provider.ProviderStateOK || got.ModelCount != 2 {
		t.Errorf("expected ok/model_count=2, got %+v", got)
	}
}

func TestOpenAISource_NilStatusIsSafe(t *testing.T) {
	creds := &fakeOpenAICreds{key: "sk-x", version: 1}
	disc := &fakeOpenAIDiscoverer{models: map[string]bool{"gpt-5": true}}
	src := NewOpenAISource(creds, disc, staticVendorFilter([]string{"gpt-*"}), nil)

	if _, err := src.Discover(context.Background()); err != nil {
		t.Fatalf("unexpected error with a nil status reporter: %v", err)
	}
}

// TestOpenAISource_DenylistAppliedAfterGlob proves the wire-contract-adjacent
// hygiene rule: a broad "*" OPENAI_MODEL_FILTER alone must not put a
// non-chat model (here, an embeddings model) into the routable set — the
// denylist (openai.FilterChatModels) is applied by this Source AFTER
// ApplyGlobFilter, not left to the operator's glob alone.
func TestOpenAISource_DenylistAppliedAfterGlob(t *testing.T) {
	creds := &fakeOpenAICreds{key: "sk-x", version: 1}
	disc := &fakeOpenAIDiscoverer{models: map[string]bool{
		"gpt-5":                  true,
		"text-embedding-3-large": true,
		"whisper-1":              true,
	}}
	status := &fakeOpenAIStatusReporter{}
	src := NewOpenAISource(creds, disc, staticVendorFilter([]string{"*"}), status)

	ids, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 1 || ids[0] != "gpt-5" {
		t.Errorf("expected only gpt-5 to survive the denylist under a broad glob, got %v", ids)
	}
	if got := status.last(); got.ModelCount != 1 {
		t.Errorf("expected model_count=1 (post-denylist), got %+v", got)
	}
}

// TestManager_OpenAIOutageDoesNotAffectLocalSource mirrors
// TestManager_VendorOutageDoesNotAffectLocalSource (anthropic) for the
// openai Source.
func TestManager_OpenAIOutageDoesNotAffectLocalSource(t *testing.T) {
	creds := &fakeOpenAICreds{key: "sk-x", version: 1}
	disc := &fakeOpenAIDiscoverer{discErr: errFakeGeneric}
	openaiSrc := NewOpenAISource(creds, disc, staticVendorFilter([]string{"gpt-*"}), &fakeOpenAIStatusReporter{})
	localSrc := staticSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", []string{"llama-3-8b"})

	m := NewManager([]Source{openaiSrc, localSrc}, 0)
	m.Refresh(context.Background())

	route, ok := m.Snapshot().Lookup("llama-3-8b")
	if !ok {
		t.Fatal("expected the local model to remain routable despite the vendor outage")
	}
	if route.ProviderKey != "vllm-default" {
		t.Errorf("expected vllm-default, got %q", route.ProviderKey)
	}

	m.Refresh(context.Background())
	if _, ok := m.Snapshot().Lookup("llama-3-8b"); !ok {
		t.Fatal("local model should still be routable after a second failed vendor tick")
	}
}
