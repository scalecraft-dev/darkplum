package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// retiredProvider counts calls so a test can prove AWS was never reached.
type retiredProvider struct {
	fakeErrorProvider
	retired map[string]bool
	calls   int
}

func (p *retiredProvider) ModelRetired(m string) bool { return p.retired[m] }
func (p *retiredProvider) Complete(ctx context.Context, r provider.ChatRequest) (*provider.ChatResponse, error) {
	p.calls++
	return p.fakeErrorProvider.Complete(ctx, r)
}

// A retired model is refused with model_retired before the cloud is called.
func TestChat_RetiredModelRefusedBeforeTheCloud(t *testing.T) {
	prov := &retiredProvider{
		fakeErrorProvider: fakeErrorProvider{name: "bedrock", kind: "bedrock", completeErr: errors.New("boom")},
		retired:           map[string]bool{fableID: true},
	}
	h := drHandler(prov)
	w, c := doChat(t, h, fableID)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_retired" || !strings.Contains(e.Error.Message, "retired by AWS") || !strings.Contains(e.Error.Message, "Models page") {
		t.Errorf("error = %+v", e.Error)
	}
	if prov.calls != 0 {
		t.Errorf("the cloud was called %d time(s) for a retired model", prov.calls)
	}
	if got := c.GetString("denial_code"); got != "model_retired" {
		t.Errorf("denial_code = %q", got)
	}
	// another model on the same provider is untouched
	w2, _ := doChat(t, h, "us.amazon.nova-lite-v1")
	if strings.Contains(w2.Body.String(), "model_retired") || prov.calls != 1 {
		t.Errorf("a different model must reach the provider: calls=%d body=%s", prov.calls, w2.Body.String())
	}
}
