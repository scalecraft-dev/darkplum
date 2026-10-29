package handler

// SDK fixture gate (GitHub #3, Phase B): request bodies shaped like the ones
// openai-python, openai-node, LangChain ChatOpenAI and LiteLLM send, streaming
// and not, defaults and nulls included, must not be refused in default mode on
// any backend except where a contract refusal genuinely applies. The bodies in
// testdata/sdk are REPRESENTATIVE: assembled from each SDK's documented request
// serialisation, not packet captures (each file says so). Strict-mode
// expectations are asserted too.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/chatparams"
)

type sdkExpect struct {
	Status  int      `json:"status"`
	Dropped []string `json:"dropped"`
	Param   string   `json:"param"`
}

type sdkFixture struct {
	SDK            string                          `json:"sdk"`
	Representative bool                            `json:"representative"`
	Provenance     string                          `json:"provenance"`
	Description    string                          `json:"description"`
	Expect         map[string]map[string]sdkExpect `json:"expect"`
	Body           map[string]any                  `json:"body"`
}

func TestSDKFixtures_NoRefusalInDefaultModeExceptContractRefusals(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "sdk", "*.json"))
	if err != nil || len(files) < 8 {
		t.Fatalf("fixtures: %v (found %d)", err, len(files))
	}
	sdks := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx sdkFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if !fx.Representative || fx.Provenance == "" {
			t.Errorf("%s must state that it is a representative body, not a capture", f)
		}
		sdks[fx.SDK] = true
		stream, _ := fx.Body["stream"].(bool)
		for _, be := range allBackends() {
			for _, mode := range []string{"lenient", "strict"} {
				t.Run(filepath.Base(f)+"/"+be.name+"/"+mode, func(t *testing.T) {
					reg, _ := chatparams.BackendOfKind(be.kind)
					exp, ok := fx.Expect[mode][string(reg)]
					if !ok {
						exp, ok = fx.Expect[mode]["*"]
					}
					if !ok {
						t.Fatalf("fixture has no %s expectation for %s", mode, be.name)
					}
					h, prov := contractHandler(t, be, mode == "strict")
					body := map[string]any{}
					for k, v := range fx.Body {
						body[k] = v
					}
					w, _ := doRawChat(t, h, body)
					if w.Code != exp.Status {
						t.Fatalf("status %d, want %d (stream=%v): %s", w.Code, exp.Status, stream, w.Body.String())
					}
					if exp.Status == http.StatusBadRequest {
						assertParamRefused(t, w.Code, w.Body.Bytes(), &prov.capturingProvider, exp.Param)
						return
					}
					if prov.calls != 1 {
						t.Errorf("provider calls = %d, want 1", prov.calls)
					}
					got := entriesOf(w)
					if len(got) == 0 && len(exp.Dropped) == 0 {
						return
					}
					if !reflect.DeepEqual(got, exp.Dropped) {
						t.Errorf("dropped = %v, want %v", got, exp.Dropped)
					}
				})
			}
		}
	}
	for _, want := range []string{"openai-python", "openai-node", "LangChain ChatOpenAI", "LiteLLM"} {
		if !sdks[want] {
			t.Errorf("no fixture for %s", want)
		}
	}
}
