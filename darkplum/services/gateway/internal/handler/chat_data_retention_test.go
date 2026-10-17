package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/middleware"
	"github.com/scalecraft/darkplum/gateway/internal/policy"
	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/routing"
)

// drRefusal stands in for bedrock.DataRetentionError: its Error() and the
// wrapped cause carry text that must never reach a client.
type drRefusal struct{ model string }

func (e *drRefusal) Error() string { return "RAW-BEDROCK-TEXT data retention mode 'default' acct 1234" }
func (e *drRefusal) DataRetentionRefusal() (string, string, string) {
	return e.model, "default", "aws_review"
}

// availProvider is a fakeErrorProvider that also reports availability.
type availProvider struct {
	fakeErrorProvider
	av map[string]provider.Availability
}

func (p *availProvider) ModelAvailability(m string) provider.Availability { return p.av[m] }

const fableID = "us.anthropic.claude-fable-5-v1:0"

func drHandler(prov provider.Provider) *ChatHandler {
	return newTestHandler(
		fakeProviders{"bedrock": prov},
		map[string]routing.Route{
			fableID:                  {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged},
			"us.amazon.nova-lite-v1": {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged},
			"llama-3-8b":             {ProviderKey: "vllm", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{Enabled: map[string]bool{fableID: true, "us.amazon.nova-lite-v1": true, "llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)
}

func TestChat_DataRetentionRefusal_422NonStream(t *testing.T) {
	h := drHandler(&fakeErrorProvider{name: "bedrock", kind: "bedrock", completeErr: &drRefusal{model: fableID}})
	w, c := doChat(t, h, fableID)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	want := `{"error":{"message":"` + fableID + ` on Bedrock requires your AWS account's Bedrock data-retention mode to be raised before it can answer. An administrator changes that in AWS; other models still work.","type":"invalid_request_error","code":"model_requires_data_retention","model":"` + fableID + `","account_mode":"default","required_mode":"aws_review"}}`
	if w.Body.String() != want {
		t.Errorf("body mismatch\n got: %s\nwant: %s", w.Body.String(), want)
	}
	if strings.Contains(w.Body.String(), "RAW-BEDROCK-TEXT") {
		t.Errorf("raw text leaked: %s", w.Body.String())
	}
	if got := c.GetString("denial_code"); got != "model_requires_data_retention" {
		t.Errorf("denial_code = %q", got)
	}
	// Recorded as usual: backend_class/provider are on the context.
	if c.GetString("backend_class") != routing.ClassCloudManaged || c.GetString("provider_name") != "bedrock" {
		t.Errorf("backend_class/provider not recorded: %q %q", c.GetString("backend_class"), c.GetString("provider_name"))
	}
}

func TestChat_DataRetentionRefusal_422StreamBeforeFirstByte(t *testing.T) {
	h := drHandler(&fakeErrorProvider{name: "bedrock", kind: "bedrock", streamErr: &drRefusal{model: fableID}})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = buildStreamChatRequest(t, fableID)
	h.HandleChatCompletions(c)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q", w.Header().Get("Content-Type"))
	}
	var e struct {
		Error struct{ Code, AccountMode, RequiredMode string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Error.Code != "model_requires_data_retention" || strings.Contains(w.Body.String(), "RAW-BEDROCK-TEXT") {
		t.Errorf("body = %s", w.Body.String())
	}
	if c.GetString("denial_code") != "model_requires_data_retention" {
		t.Errorf("denial_code = %q", c.GetString("denial_code"))
	}
}

func TestChat_DataRetentionRefusal_MidStreamSSE(t *testing.T) {
	h := drHandler(&fakeErrorProvider{name: "bedrock", kind: "bedrock",
		streamPreWrite: "data: {\"id\":\"x\"}\n\n", streamErr: &drRefusal{model: fableID}})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = buildStreamChatRequest(t, fableID)
	h.HandleChatCompletions(c)
	body := w.Body.String()
	if !strings.Contains(body, `"code":"model_requires_data_retention"`) || !strings.Contains(body, `"required_mode":"aws_review"`) {
		t.Errorf("missing SSE refusal frame: %s", body)
	}
	if strings.Contains(body, "RAW-BEDROCK-TEXT") {
		t.Errorf("raw text leaked: %s", body)
	}
	if c.GetString("denial_code") != "model_requires_data_retention" {
		t.Errorf("denial_code = %q", c.GetString("denial_code"))
	}
}

func TestChat_DataRetentionRefusal_IncludesRouteWhenRouted(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("route_name", "code")
	handleProviderError(c, &drRefusal{model: fableID})
	if !strings.Contains(w.Body.String(), `"route":"code"`) {
		t.Errorf("route missing: %s", w.Body.String())
	}
	var sb strings.Builder
	_ = writeStreamError(&sb, &drRefusal{model: fableID}, "code")
	if !strings.Contains(sb.String(), `"route":"code"`) {
		t.Errorf("route missing in SSE: %s", sb.String())
	}
	// A wrapped error still classifies.
	if _, _, ok := dataRetentionResponse("", errors.Join(errors.New("x"), &drRefusal{model: "m"})); !ok {
		t.Error("wrapped refusal not classified")
	}
}

// TestListModels_Availability: Bedrock entries carry availability for service
// AND user keys; non-Bedrock entries do not.
func TestListModels_Availability(t *testing.T) {
	prov := &availProvider{
		fakeErrorProvider: fakeErrorProvider{name: "bedrock", kind: "bedrock"},
		av: map[string]provider.Availability{
			fableID:                  {State: "needs_aws_opt_in", Reason: "data_retention", AccountMode: "default", RequiredMode: "aws_review"},
			"us.amazon.nova-lite-v1": {State: "ok", AccountMode: "default", RequiredMode: "aws_review"},
		},
	}
	for _, keyType := range []string{middleware.KeyTypeService, middleware.KeyTypeUser} {
		h := drHandler(prov)
		w, c := modelsRequestWithHeaders(t, keyType, nil)
		c.Set(middleware.ContextUserID, "user-1")
		c.Set(middleware.ContextKeyID, "key-1")
		h.HandleListModels(c)

		var resp struct {
			Data []map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, m := range resp.Data {
			var id string
			_ = json.Unmarshal(m["id"], &id)
			got[id] = string(m["availability"])
		}
		if got[fableID] != `{"state":"needs_aws_opt_in","reason":"data_retention","account_mode":"default","required_mode":"aws_review"}` {
			t.Errorf("%s: fable availability = %s", keyType, got[fableID])
		}
		if got["us.amazon.nova-lite-v1"] != `{"state":"ok","account_mode":"default","required_mode":"aws_review"}` {
			t.Errorf("%s: nova availability = %s", keyType, got["us.amazon.nova-lite-v1"])
		}
		if v, ok := got["llama-3-8b"]; !ok || v != "" {
			t.Errorf("%s: non-Bedrock model must have no availability, got %q (present=%v)", keyType, v, ok)
		}
	}
}
