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

// fakeAnthropicCreds is an anthropicCredentialChecker fake.
type fakeAnthropicCreds struct {
	key     string
	version uint64
	err     error
}

func (f *fakeAnthropicCreds) Key() (string, uint64, error) { return f.key, f.version, f.err }

// fakeAnthropicDiscoverer implements anthropicDiscoverer without any real
// SDK/network involvement.
type fakeAnthropicDiscoverer struct {
	models  map[string]bool
	discErr error
}

func (f *fakeAnthropicDiscoverer) Name() string { return "anthropic" }
func (f *fakeAnthropicDiscoverer) Complete(_ context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeAnthropicDiscoverer) Stream(_ context.Context, _ provider.ChatRequest, _ io.Writer) (*provider.StreamStats, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeAnthropicDiscoverer) DiscoverModels(_ context.Context) (map[string]bool, error) {
	if f.discErr != nil {
		return nil, f.discErr
	}
	return f.models, nil
}

// fakeAuthError implements the shape anthropic.IsAuthError checks for
// (*anthropic.Error) is unavailable to this package without importing the
// vendor SDK, so this fake instead exercises the classification boundary at
// the anthropicDiscoverer level: fakeAnthropicDiscoverer returns a plain
// error, and the test asserts the source falls back to "unreachable" for any
// error anthropic.IsAuthError does not itself recognise as a 401/403 — the
// real 401/403 classification is exercised in
// internal/provider/anthropic/discover_test.go instead, against the real SDK
// error type.
var errFakeGeneric = errors.New("network unreachable")

// fakeStatusReporter records every Report call.
type fakeStatusReporter struct {
	mu      sync.Mutex
	reports []provider.ProviderStatus
}

func (f *fakeStatusReporter) Report(name string, st provider.ProviderStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st.Name = name
	f.reports = append(f.reports, st)
}

func (f *fakeStatusReporter) last() provider.ProviderStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reports) == 0 {
		return provider.ProviderStatus{}
	}
	return f.reports[len(f.reports)-1]
}

func TestAnthropicSource_NotConfigured(t *testing.T) {
	creds := &fakeAnthropicCreds{err: credsource.ErrNotConfigured}
	disc := &fakeAnthropicDiscoverer{}
	status := &fakeStatusReporter{}
	src := NewAnthropicSource(creds, disc, staticVendorFilter([]string{"claude-*"}), status)

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

// TestAnthropicSource_CredentialUnreadable covers a key file that EXISTS but
// is empty/unreadable (as opposed to ErrNotConfigured, meaning the file does
// not exist at all — see TestAnthropicSource_NotConfigured). This must
// report auth_failed, not not_configured: an admin needs to see "there is a
// key problem" for a Secret that was mounted but is broken, not "nothing is
// set up" — there is no separate "degraded" state in the wire contract's
// closed vocabulary.
func TestAnthropicSource_CredentialUnreadable(t *testing.T) {
	creds := &fakeAnthropicCreds{err: errors.New("credsource: permission denied")}
	disc := &fakeAnthropicDiscoverer{}
	status := &fakeStatusReporter{}
	src := NewAnthropicSource(creds, disc, staticVendorFilter([]string{"claude-*"}), status)

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

func TestAnthropicSource_FilterMissing(t *testing.T) {
	creds := &fakeAnthropicCreds{key: "sk-ant-x", version: 1}
	disc := &fakeAnthropicDiscoverer{models: map[string]bool{"claude-haiku-4-5": true}}
	status := &fakeStatusReporter{}
	src := NewAnthropicSource(creds, disc, staticVendorFilter(nil), status)

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

func TestAnthropicSource_FilterMatchedNothing(t *testing.T) {
	creds := &fakeAnthropicCreds{key: "sk-ant-x", version: 1}
	disc := &fakeAnthropicDiscoverer{models: map[string]bool{"claude-haiku-4-5": true}}
	status := &fakeStatusReporter{}
	src := NewAnthropicSource(creds, disc, staticVendorFilter([]string{"gpt-*"}), status)

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

func TestAnthropicSource_Unreachable(t *testing.T) {
	creds := &fakeAnthropicCreds{key: "sk-ant-x", version: 1}
	disc := &fakeAnthropicDiscoverer{discErr: errFakeGeneric}
	status := &fakeStatusReporter{}
	src := NewAnthropicSource(creds, disc, staticVendorFilter([]string{"claude-*"}), status)

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

func TestAnthropicSource_OK(t *testing.T) {
	creds := &fakeAnthropicCreds{key: "sk-ant-x", version: 1}
	disc := &fakeAnthropicDiscoverer{models: map[string]bool{
		"claude-haiku-4-5": true,
		"claude-opus-4-5":  true,
		"gpt-5":            true,
	}}
	status := &fakeStatusReporter{}
	src := NewAnthropicSource(creds, disc, staticVendorFilter([]string{"claude-*"}), status)

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

func TestAnthropicSource_NilStatusIsSafe(t *testing.T) {
	creds := &fakeAnthropicCreds{key: "sk-ant-x", version: 1}
	disc := &fakeAnthropicDiscoverer{models: map[string]bool{"claude-haiku-4-5": true}}
	src := NewAnthropicSource(creds, disc, staticVendorFilter([]string{"claude-*"}), nil)

	if _, err := src.Discover(context.Background()); err != nil {
		t.Fatalf("unexpected error with a nil status reporter: %v", err)
	}
}

// TestManager_VendorOutageDoesNotAffectLocalSource proves the failure
// isolation ruling at the routing.Manager level: an Anthropic source that
// fails discovery every tick never prevents an unrelated self-hosted
// source's models from being (and staying) routable.
func TestManager_VendorOutageDoesNotAffectLocalSource(t *testing.T) {
	creds := &fakeAnthropicCreds{key: "sk-ant-x", version: 1}
	disc := &fakeAnthropicDiscoverer{discErr: errFakeGeneric}
	anthropicSrc := NewAnthropicSource(creds, disc, staticVendorFilter([]string{"claude-*"}), &fakeStatusReporter{})
	localSrc := staticSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", []string{"llama-3-8b"})

	m := NewManager([]Source{anthropicSrc, localSrc}, 0)
	m.Refresh(context.Background())

	route, ok := m.Snapshot().Lookup("llama-3-8b")
	if !ok {
		t.Fatal("expected the local model to remain routable despite the vendor outage")
	}
	if route.ProviderKey != "vllm-default" {
		t.Errorf("expected vllm-default, got %q", route.ProviderKey)
	}

	// A second refresh (vendor still failing) must not regress the local
	// model either.
	m.Refresh(context.Background())
	if _, ok := m.Snapshot().Lookup("llama-3-8b"); !ok {
		t.Fatal("local model should still be routable after a second failed vendor tick")
	}
}
