package handler

// Image and file content parts through the handler: the pre-existing
// "undecodable content becomes prompt text" bypass, the content validator's
// 400 invalid_content, and the registry's by-name refusal on every backend
// that does not take images.

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/router"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

// A real 1x1 PNG and the smallest thing that sniffs as a PDF.
const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

var tinyPDFBase64 = base64.StdEncoding.EncodeToString([]byte("%PDF-1.4\n%%EOF\n"))

// secretFilename and secretMarker must appear in no response body and no log.
const (
	secretFilename = "SECRET-q3-financials.pdf"
	secretMarker   = "UNIQUEPAYLOADMARKER"
)

func secretPDFBase64() string {
	return base64.StdEncoding.EncodeToString([]byte("%PDF-1.4\n" + secretMarker + "\n%%EOF\n"))
}

func imagePart(url string) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

func filePartMap(name, dataURL string) map[string]any {
	return map[string]any{"type": "file", "file": map[string]any{"filename": name, "file_data": dataURL}}
}

func textPart(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func userTurn(parts ...map[string]any) map[string]any {
	return map[string]any{"role": "user", "content": parts}
}

func chatBodyWith(model string, msgs ...map[string]any) map[string]any {
	return map[string]any{"model": model, "messages": msgs}
}

// doRawBytes posts exact bytes, for bodies a map cannot express.
func doRawBytes(t *testing.T, h *ChatHandler, body []byte) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	h.HandleChatCompletions(c)
	return w, c
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func assertNoSecrets(t *testing.T, where string, b []byte) {
	t.Helper()
	for _, s := range []string{secretFilename, secretMarker, tinyPNGBase64, tinyPDFBase64, secretPDFBase64()} {
		if strings.Contains(string(b), s) {
			t.Errorf("%s contains attachment content %q: %s", where, s, b)
		}
	}
}

// The bypass: a content that is neither a string nor a decodable array used to
// become prompt text (raw JSON, base64 included) and skip the non-text refusal.
func TestHandleChatCompletions_UndecodableContentIsRefused(t *testing.T) {
	for name, content := range map[string]string{
		"number":              `123`,
		"bool":                `true`,
		"object":              `{"type":"image_url","image_url":{"url":"data:image/png;base64,` + tinyPNGBase64 + `"}}`,
		"array of strings":    `["a","b"]`,
		"text is a number":    `[{"type":"text","text":5}]`,
		"array of arrays":     `[[1,2]]`,
		"nested object value": `{"a":{"b":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h, prov := requestShapeTestHandler(t)
			logs := captureLogs(t)
			body := `{"model":"llama-3-8b","messages":[{"role":"user","content":"hi"},{"role":"user","content":` + content + `}]}`
			w, c := doRawBytes(t, h, []byte(body))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400; body=%s", w.Code, w.Body.String())
			}
			e := decodeError(t, w.Body.Bytes())
			if e.Error.Code != "invalid_request" || e.Error.Param != "messages[1].content" {
				t.Errorf("code %q param %q, want invalid_request / messages[1].content", e.Error.Code, e.Error.Param)
			}
			if got := c.GetString("denial_code"); got != "invalid_request" {
				t.Errorf("denial_code = %q", got)
			}
			if prov.completeCalls != 0 || prov.streamCalls != 0 {
				t.Error("provider was dispatched to")
			}
			assertNoSecrets(t, "response", w.Body.Bytes())
			assertNoSecrets(t, "logs", logs.Bytes())
		})
	}
}

func TestHandleChatCompletions_ContentStringArrayAndNullStillAccepted(t *testing.T) {
	for _, content := range []string{`"hello"`, `[{"type":"text","text":"hello"}]`, `[]`, `null`} {
		h, prov := requestShapeTestHandler(t)
		w, _ := doRawBytes(t, h, []byte(`{"model":"llama-3-8b","messages":[{"role":"user","content":`+content+`}]}`))
		if w.Code != http.StatusOK || prov.completeCalls != 1 {
			t.Errorf("content %s: status %d calls %d, body=%s", content, w.Code, prov.completeCalls, w.Body.String())
		}
	}
}

// An image sent to a backend that does not take images is refused by name
// with the part's index, in both params modes, before any provider call, and
// neither the response nor the logs carry the bytes or the filename.
func TestHandleChatCompletions_ImageAndFileRefusedByNameOnNonBedrock(t *testing.T) {
	cases := map[string]struct {
		body      map[string]any
		wantParam string
	}{
		"image": {
			body:      chatBodyWith("m", userTurn(textPart("what is this?"), imagePart("data:image/png;base64,"+tinyPNGBase64))),
			wantParam: "messages[0].content[1]",
		},
		"image only": {
			body:      chatBodyWith("m", userTurn(imagePart("data:image/png;base64,"+tinyPNGBase64))),
			wantParam: "messages[0].content[0]",
		},
		"file": {
			body:      chatBodyWith("m", map[string]any{"role": "user", "content": "first"}, userTurn(textPart("summarise"), filePartMap(secretFilename, "data:application/pdf;base64,"+secretPDFBase64()))),
			wantParam: "messages[1].content[1]",
		},
		"file beats image in priority": {
			body: chatBodyWith("m", userTurn(textPart("x"),
				imagePart("data:image/png;base64,"+tinyPNGBase64),
				filePartMap(secretFilename, "data:application/pdf;base64,"+secretPDFBase64()))),
			wantParam: "messages[0].content[2]",
		},
	}
	for name, tc := range cases {
		for _, be := range allBackends() {
			if be.name == "bedrock" {
				continue
			}
			for _, strict := range []bool{false, true} {
				t.Run(name+"/"+be.name+"/strict="+map[bool]string{false: "false", true: "true"}[strict], func(t *testing.T) {
					h, prov := contractHandler(t, be, strict)
					logs := captureLogs(t)
					w, _ := doRawChat(t, h, tc.body)
					assertParamRefused(t, w.Code, w.Body.Bytes(), &prov.capturingProvider, tc.wantParam)
					assertNoSecrets(t, "response", w.Body.Bytes())
					assertNoSecrets(t, "logs", logs.Bytes())
				})
			}
		}
	}
}

// Bedrock takes the same parts: they reach the provider request intact, in
// both params modes.
func TestHandleChatCompletions_ImageAndFileReachBedrockProvider(t *testing.T) {
	for _, be := range allBackends() {
		if be.name != "bedrock" {
			continue
		}
		for _, strict := range []bool{false, true} {
			h, prov := contractHandler(t, be, strict)
			body := chatBodyWith("m", userTurn(
				textPart("compare"),
				imagePart("data:image/png;base64,"+tinyPNGBase64),
				filePartMap("a.pdf", "data:application/pdf;base64,"+tinyPDFBase64)))
			w, _ := doRawChat(t, h, body)
			if w.Code != http.StatusOK {
				t.Fatalf("strict=%v status %d: %s", strict, w.Code, w.Body.String())
			}
			parts := prov.lastReq.Messages[0].Parts
			if len(parts) != 3 || parts[1].Kind != provider.KindImage || parts[2].Kind != provider.KindFile {
				t.Fatalf("strict=%v parts = %+v", strict, parts)
			}
			if b, err := parts[1].Bytes(); err != nil || len(b) == 0 {
				t.Errorf("image bytes: %v", err)
			}
		}
	}
}

// The remote-URL, file-id and audio shapes are refused by the registry before
// a backend is chosen, on every route.
func TestHandleChatCompletions_UnknownContentRefusedEarly(t *testing.T) {
	for name, part := range map[string]map[string]any{
		"http url":   imagePart("https://example.com/x.png"),
		"s3 url":     imagePart("s3://bucket/key.png"),
		"file url":   imagePart("file:///etc/passwd"),
		"file id":    {"type": "file", "file": map[string]any{"file_id": "file-123"}},
		"audio":      {"type": "input_audio", "input_audio": map[string]any{"data": "AAAA", "format": "wav"}},
		"other type": {"type": "video_url", "video_url": map[string]any{"url": "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			for _, be := range allBackends() {
				h, prov := contractHandler(t, be, false)
				w, _ := doRawChat(t, h, chatBodyWith("m", userTurn(textPart("hi"), part)))
				assertParamRefused(t, w.Code, w.Body.Bytes(), &prov.capturingProvider, "messages[].content")
			}
		})
	}
}

// invalid_content: every validator refusal is a 400 with the part named, the
// denial recorded as invalid_request, and nothing dispatched.
func TestHandleChatCompletions_InvalidContent(t *testing.T) {
	bigPNG := "data:image/png;base64," + base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, provider.MaxImageBytes)...))
	tooMany := []map[string]any{textPart("x")}
	for i := 0; i < provider.MaxImagesPerRequest+1; i++ {
		tooMany = append(tooMany, imagePart("data:image/png;base64,"+tinyPNGBase64))
	}
	cases := map[string]struct {
		body      map[string]any
		wantParam string
		wantIn    string
	}{
		"oversize image":  {chatBodyWith("m", userTurn(textPart("x"), imagePart(bigPNG))), "messages[0].content[1]", "3750000"},
		"too many images": {chatBodyWith("m", userTurn(tooMany...)), "messages[0].content[21]", "at most 20"},
		"document without text": {
			chatBodyWith("m", userTurn(filePartMap(secretFilename, "data:application/pdf;base64,"+secretPDFBase64()))),
			"messages[0].content[0]", "needs a text message"},
		"bad base64":             {chatBodyWith("m", userTurn(textPart("x"), imagePart("data:image/png;base64,@@@@"))), "messages[0].content[1]", "not valid base64"},
		"wrong magic":            {chatBodyWith("m", userTurn(textPart("x"), imagePart("data:image/jpeg;base64,"+tinyPNGBase64))), "messages[0].content[1]", "image/jpeg"},
		"svg":                    {chatBodyWith("m", userTurn(textPart("x"), imagePart("data:image/svg+xml;base64,PHN2Zy8+"))), "messages[0].content[1]", "SVG"},
		"media type not allowed": {chatBodyWith("m", userTurn(textPart("x"), imagePart("data:image/bmp;base64,Qk0="))), "messages[0].content[1]", "image/bmp"},
		"part on assistant": {chatBodyWith("m",
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []map[string]any{textPart("a"), imagePart("data:image/png;base64," + tinyPNGBase64)}}),
			"messages[1].content[1]", "only in user messages"},
		"not a data url": {chatBodyWith("m", userTurn(textPart("x"), imagePart("not a url"))), "messages[0].content[1]", "malformed data URL"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			be := allBackends()[3] // bedrock; the validator is backend-independent
			h, prov := contractHandler(t, be, false)
			logs := captureLogs(t)
			w, c := doRawChat(t, h, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			e := decodeError(t, w.Body.Bytes())
			if e.Error.Code != "invalid_content" || e.Error.Param != tc.wantParam {
				t.Errorf("code %q param %q, want invalid_content / %s (%s)", e.Error.Code, e.Error.Param, tc.wantParam, w.Body.String())
			}
			if !strings.Contains(e.Error.Message, tc.wantIn) || !strings.Contains(e.Error.Message, tc.wantParam) {
				t.Errorf("message %q should name %q and %q", e.Error.Message, tc.wantParam, tc.wantIn)
			}
			if got := c.GetString("denial_code"); got != "invalid_request" {
				t.Errorf("denial_code = %q, want invalid_request", got)
			}
			if prov.calls != 0 {
				t.Error("provider was dispatched to")
			}
			assertNoSecrets(t, "response", w.Body.Bytes())
			assertNoSecrets(t, "logs", logs.Bytes())
		})
	}
}

// Invalid content is the same 400 whatever the backend: it is decided before
// the model is resolved, never by a backend's own wording.
func TestHandleChatCompletions_InvalidContentBeforeBackendRefusal(t *testing.T) {
	h, prov := requestShapeTestHandler(t) // vLLM
	w, _ := doRawChat(t, h, chatBodyWith("llama-3-8b", userTurn(textPart("x"), imagePart("data:image/png;base64,@@@@"))))
	e := decodeError(t, w.Body.Bytes())
	if w.Code != 400 || e.Error.Code != "invalid_content" || prov.completeCalls != 0 {
		t.Errorf("status %d code %q calls %d", w.Code, e.Error.Code, prov.completeCalls)
	}
}

// The router sees the flattened text only, never a part, even when the same
// request is then refused by the backend the route resolves to.
func TestHandleChatCompletions_RoutedAliasClassifierSeesTextOnly(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "general", Reason: "rules:default"}}}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{
			"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"qwen-coder": true, "llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)
	w, _ := doRawChat(t, h, chatBodyWith("smart-chat", userTurn(textPart("describe this"), imagePart("data:image/png;base64,"+tinyPNGBase64))))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if vllm.completeCalls != 0 {
		t.Error("a refused image reached the provider")
	}
	if len(classifier.calls) != 1 {
		t.Fatalf("classifier calls = %d", len(classifier.calls))
	}
	in := classifier.calls[0]
	if in.Text != "describe this" || !in.HasNonText || in.OriginalLen != len([]rune("describe this")) {
		t.Errorf("classifier input = %+v, want text only with HasNonText", in)
	}
}

// A caller-supplied file_id is unknown content: refused as unsupported_parameter
// on messages[].content before any provider is called, whichever route the
// model resolves to, with no completion and no stream dispatched.
func TestHandleChatCompletions_FileIDRefusedEarlyNoProviderCalls(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"file","file":{"file_id":"abc"}}]}]}`)
	for _, be := range allBackends() {
		h, prov := contractHandler(t, be, false)
		w, _ := doRawBytes(t, h, body)
		assertParamRefused(t, w.Code, w.Body.Bytes(), &prov.capturingProvider, "messages[].content")
	}
}
