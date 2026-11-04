package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft-dev/darkplum/api/internal/mcp"
)

// chatAttachFixture: user A owns a conversation with a PNG and a PDF; user B
// exists too. gw records the body of the last request that reached the
// "gateway".
type chatAttachFixture struct {
	*attachFixture
	gw   *bearerGateway
	chat *ChatHandler
}

func newChatAttachFixture(t *testing.T) *chatAttachFixture {
	t.Helper()
	f := &chatAttachFixture{attachFixture: newAttachFixture(t)}
	f.gw = newBearerGateway(t, okJSON)
	f.chat = NewChatHandler(f.gw.URL, "service-key", f.pool)
	return f
}

func (f *chatAttachFixture) post(userID, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", userID); c.Next() })
	r.POST("/v1/chat/completions", f.chat.ProxyChat)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func refPartJSON(id string) string {
	return fmt.Sprintf(`{"type":"file","file":{"file_id":%q}}`, id)
}

// sent decodes the messages the gateway received.
func (f *chatAttachFixture) sent(t *testing.T) []map[string]json.RawMessage {
	t.Helper()
	body, _ := f.gw.seen()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("gateway body: %v: %s", err, body)
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(fields["messages"], &msgs); err != nil {
		t.Fatal(err)
	}
	return msgs
}

func TestChatExpand_RefsReplacedWithStoredBytes(t *testing.T) {
	f := newChatAttachFixture(t)
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"first"},` + refPartJSON(f.pngID) + `,` + refPartJSON(f.pdfID) + `]},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`
	rec := f.post(f.userA, body)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	msgs := f.sent(t) // system, user, assistant, user
	if len(msgs) != 4 {
		t.Fatalf("messages = %d", len(msgs))
	}
	var parts []map[string]any
	if err := json.Unmarshal(msgs[1]["content"], &parts); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 || parts[0]["text"] != "first" {
		t.Fatalf("parts = %v", parts)
	}
	img := parts[1]["image_url"].(map[string]any)["url"]
	if parts[1]["type"] != "image_url" || img != "data:image/png;base64,"+b64(f.png) {
		t.Errorf("image part = %v", parts[1])
	}
	file := parts[2]["file"].(map[string]any)
	if parts[2]["type"] != "file" || file["file_data"] != "data:application/pdf;base64,"+b64(f.pdf) {
		t.Errorf("file part = %v", parts[2])
	}
	// The stored name never reaches the gateway; a neutral constant does.
	if file["filename"] != "document" || strings.Contains(string(msgs[1]["content"]), "q3 report") || strings.Contains(string(msgs[1]["content"]), "file_id") {
		t.Errorf("file part filename/ref leaked: %v", file)
	}
	// Untouched turns are unchanged.
	if string(msgs[2]["content"]) != `"ok"` || string(msgs[3]["content"]) != `"next"` {
		t.Errorf("untouched turns changed: %s %s", msgs[2]["content"], msgs[3]["content"])
	}
}

func TestChatExpand_ForeignAndUnknownIDsAre404AndNothingIsSent(t *testing.T) {
	f := newChatAttachFixture(t)
	for name, id := range map[string]string{
		"foreign":   f.pngID, // owned by A, sent by B
		"unknown":   "00000000-0000-4000-8000-000000000000",
		"malformed": "not-a-uuid",
	} {
		body := `{"model":"m","messages":[{"role":"user","content":[` + refPartJSON(id) + `]}]}`
		rec := f.post(f.userB, body)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"attachment_not_found"`) || !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "PNG") || strings.Contains(rec.Body.String(), b64(f.png)) {
			t.Errorf("%s: bytes in refusal", name)
		}
	}
	if body, _ := f.gw.seen(); body != nil {
		t.Errorf("a refused request reached the gateway: %s", body)
	}
	// One foreign id among valid ones refuses the whole request.
	body := `{"model":"m","messages":[{"role":"user","content":[` + refPartJSON(f.pngID) + `]},{"role":"user","content":[` + refPartJSON(f.pdfID) + `]}]}`
	if rec := f.post(f.userB, body); rec.Code != http.StatusNotFound {
		t.Errorf("B using A's ids = %d", rec.Code)
	}
	// The owner is fine, including from a different conversation than the one
	// that stores the attachment.
	if rec := f.post(f.userA, body); rec.Code != 200 {
		t.Errorf("owner = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatExpand_FileIDOnNonUserRoleIs400(t *testing.T) {
	f := newChatAttachFixture(t)
	for _, role := range []string{"assistant", "system", "tool"} {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"` + role + `","content":[` + refPartJSON(f.pngID) + `]}]}`
		rec := f.post(f.userA, body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"invalid_request"`) {
			t.Errorf("%s: %d %s", role, rec.Code, rec.Body.String())
		}
	}
}

func TestChatExpand_NonStringFileIDIs400(t *testing.T) {
	f := newChatAttachFixture(t)
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":42}}]}]}`
	if rec := f.post(f.userA, body); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatExpand_MixedInlineAndRefsAndDuplicates(t *testing.T) {
	f := newChatAttachFixture(t)
	inline := `{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64(f.png) + `"}}`
	body := `{"model":"m","messages":[{"role":"user","content":[` + refPartJSON(f.pngID) + `,` + inline + `,` + refPartJSON(f.pngID) + `]}]}`
	if rec := f.post(f.userA, body); rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var parts []map[string]any
	_ = json.Unmarshal(f.sent(t)[1]["content"], &parts)
	if len(parts) != 3 {
		t.Fatalf("parts = %d", len(parts))
	}
	want := "data:image/png;base64," + b64(f.png)
	for i, p := range parts {
		if p["type"] != "image_url" || p["image_url"].(map[string]any)["url"] != want {
			t.Errorf("part %d = %v", i, p)
		}
	}
}

func TestChatExpand_BodiesWithoutRefsAreNotRewritten(t *testing.T) {
	body := []byte(`{ "messages":[{"role":"user","content":[{"type":"text","text":"a <b> & c"}]}], "model":"m"}`)
	got, ref := expandAttachmentRefs(context.Background(), nil, "u", body)
	if ref != nil || string(got) != string(body) {
		t.Fatalf("body without references was changed: %q %+v", got, ref)
	}
	// Not JSON at all: left for withSystemPromptText to report.
	got, ref = expandAttachmentRefs(context.Background(), nil, "u", []byte(`not json "file_id"`))
	if ref != nil || string(got) != `not json "file_id"` {
		t.Fatalf("unparseable body: %q %+v", got, ref)
	}
	// "file_id" in text is not a reference.
	text := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"\"file_id\""}]}]}`)
	got, ref = expandAttachmentRefs(context.Background(), nil, "u", text)
	if ref != nil || string(got) != string(text) {
		t.Fatalf("text mentioning file_id: %q %+v", got, ref)
	}
}

func TestChatExpand_BearerPathDoesNoExpansion(t *testing.T) {
	f := newChatAttachFixture(t)
	body := `{"model":"m","messages":[{"role":"user","content":[` + refPartJSON(f.pngID) + `]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dp_user_x")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", f.userA); c.Next() })
	r.POST("/v1/chat/completions", f.chat.ProxyChat)
	r.ServeHTTP(httptest.NewRecorder(), req)
	got, _ := f.gw.seen()
	if string(got) != body {
		t.Errorf("Bearer body was touched: %s", got)
	}
}

// Expanded bytes must survive the rest of the session pipeline untouched:
// the system prompt, the library message and the tool loop all re-marshal.
func TestChatExpand_BytesSurviveSystemPromptLibraryAndToolLoop(t *testing.T) {
	f := newChatAttachFixture(t)
	body := []byte(`{"model":"mock-model","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"read"},` + refPartJSON(f.pngID) + `,` + refPartJSON(f.pdfID) + `]},{"role":"assistant","content":"ok"},{"role":"user","content":"again"}]}`)
	expanded, ref := expandAttachmentRefs(f.ctx, f.pool, f.userA, body)
	if ref != nil {
		t.Fatal(ref)
	}
	want := userContent(t, expanded, 0)
	if !strings.Contains(want, b64(f.png)) || !strings.Contains(want, b64(f.pdf)) {
		t.Fatal("expansion did not inline the bytes")
	}

	withPrompt, _, err := withSystemPromptText(expanded, "prompt")
	if err != nil {
		t.Fatal(err)
	}
	if got := userContent(t, withPrompt, 1); got != want {
		t.Errorf("withSystemPromptText changed the content parts")
	}
	withLib, err := withLibraryMessage(withPrompt, "library text")
	if err != nil {
		t.Fatal(err)
	}
	if got := userContent(t, withLib, 2); got != want {
		t.Errorf("withLibraryMessage changed the content parts")
	}

	// Tool loop: the first round sent to the gateway carries the same bytes.
	gw := &fakeGateway{}
	gwSrv := httptest.NewServer(gw.handler(t))
	defer gwSrv.Close()
	mcpSrv := fakeMCP(t)
	defer mcpSrv.Close()
	mcpHost, _ := url.Parse(mcpSrv.URL)
	h := NewChatHandler(gwSrv.URL, "service-key", nil)
	tools := newToolService(nil, nil, nil, toolCredentialReader{})
	tools.policyOverride = &mcp.HostPolicy{Allowed: []string{mcpHost.Hostname()}}
	h.WithTools(tools)
	att := []attachedItem{{id: "item-1", kind: "tool", slug: "srv", name: "Server", cfg: toolConfig{ServerURL: mcpSrv.URL}, catalog: toolCatalog{Tools: []mcp.Tool{{Name: "echo"}}}}}
	tools.rememberCredential("item-1", "tok-1")
	defs, bindings, rf := buildWireTools(att)
	if rf != nil {
		t.Fatal(rf)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set("user_id", f.userA)
	c.Set("request_id", "req-1")
	h.proxyWithTools(c, withLib, true, defs, bindings)
	if len(gw.requests) == 0 {
		t.Fatalf("no gateway request; response %d %s", w.Code, w.Body.String())
	}
	var msgs []map[string]json.RawMessage
	_ = json.Unmarshal(gw.requests[0]["messages"], &msgs)
	if got := string(msgs[2]["content"]); got != want {
		t.Errorf("proxyWithTools changed the content parts")
	}
}

func userContent(t *testing.T, body []byte, idx int) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(fields["messages"], &msgs); err != nil {
		t.Fatal(err)
	}
	return string(msgs[idx]["content"])
}

func TestChatExpand_RewrittenBodyOverCapIs413FromTheAPI(t *testing.T) {
	f := newChatAttachFixture(t)
	// One 3.75 MB image referenced five times expands to ~25 MB.
	big := fixture(pngHead, attachmentImageMaxBytes)
	rec := doJSON(t, f.rA, http.MethodPost, "/api/v1/conversations/"+f.convID+"/messages", map[string]any{
		"messages": []map[string]any{{"role": "user", "attachments": []attachmentInput{imgAtt("big.png", big)}}},
	})
	var resp struct {
		Messages []conversationMessage `json:"messages"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	id := resp.Messages[0].Attachments[0].ID

	parts := make([]string, 5)
	for i := range parts {
		parts[i] = refPartJSON(id)
	}
	body := `{"model":"m","messages":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}]}`
	if len(body) > 1<<10 {
		t.Fatal("request must be small; it is the expansion that is large")
	}
	out := f.post(f.userA, body)
	if out.Code != http.StatusRequestEntityTooLarge || !strings.Contains(out.Body.String(), `"code":"request_too_large"`) {
		t.Fatalf("status = %d: %.200s", out.Code, out.Body.String())
	}
	if body, _ := f.gw.seen(); body != nil {
		t.Error("the oversize body reached the gateway")
	}
}

func TestChatExpand_NoLogLineCarriesPartContent(t *testing.T) {
	f := newChatAttachFixture(t)
	logs := captureLogs(t)
	_ = f.post(f.userB, `{"model":"m","messages":[{"role":"user","content":[`+refPartJSON(f.pngID)+`]}]}`)
	_ = f.post(f.userA, `{"model":"m","messages":[{"role":"user","content":[`+refPartJSON(f.pdfID)+`]}]}`)
	out := logs.String()
	if strings.Contains(out, b64(f.pdf)) || strings.Contains(out, b64(f.png)) || strings.Contains(out, "q3 report") || strings.Contains(out, "shot.png") {
		t.Errorf("a log line carries attachment content:\n%s", out)
	}
}

// storeBigImage stores one maximum-size image in the fixture's conversation
// and returns its id.
func (f *chatAttachFixture) storeBigImage(t *testing.T) string {
	t.Helper()
	big := fixture(pngHead, attachmentImageMaxBytes)
	rec := doJSON(t, f.rA, http.MethodPost, "/api/v1/conversations/"+f.convID+"/messages", map[string]any{
		"messages": []map[string]any{{"role": "user", "attachments": []attachmentInput{imgAtt("big.png", big)}}},
	})
	var resp struct {
		Messages []conversationMessage `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Messages) == 0 || len(resp.Messages[0].Attachments) == 0 {
		t.Fatalf("store big image = %d: %.200s", rec.Code, rec.Body.String())
	}
	return resp.Messages[0].Attachments[0].ID
}

// A body of thousands of references to one maximum-size image used to load the
// image once and encode it once per reference, tens of gigabytes. It must now
// be refused before any bytes are read, promptly and in bounded memory.
func TestChatExpand_AdversarialRepeatedRefsAreRefusedBeforeAnyBytesAreRead(t *testing.T) {
	f := newChatAttachFixture(t)
	id := f.storeBigImage(t)
	part := refPartJSON(id)

	run := func(name string, n, wantStatus int, wantCode string) {
		t.Helper()
		parts := make([]string, n)
		for i := range parts {
			parts[i] = part
		}
		body := `{"model":"m","messages":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}]}`
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		start := time.Now()
		rec := f.post(f.userA, body)
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)
		if rec.Code != wantStatus || !strings.Contains(rec.Body.String(), `"code":"`+wantCode+`"`) {
			t.Fatalf("%s: status = %d: %.200s", name, rec.Code, rec.Body.String())
		}
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 48<<20 {
			t.Errorf("%s: allocated %d MiB for a refusal", name, alloc>>20)
		}
		if elapsed > 3*time.Second {
			t.Errorf("%s: took %s", name, elapsed)
		}
		if seen, _ := f.gw.seen(); seen != nil {
			t.Errorf("%s: reached the gateway", name)
		}
	}
	// Thousands of references: over the count cap.
	run("5000 refs", 5000, http.StatusBadRequest, "invalid_request")
	// Within the count cap but 25 x 5 MB expanded: over the size budget.
	run("25 refs", attachmentRefMax, http.StatusRequestEntityTooLarge, "request_too_large")
}

func TestChatExpand_RefCountCapNamesTheOffendingPart(t *testing.T) {
	f := newChatAttachFixture(t)
	parts := make([]string, attachmentRefMax+1)
	for i := range parts {
		parts[i] = refPartJSON(f.pngID)
	}
	// Spread over two messages: the 26th reference is messages[1].content[0].
	body := `{"model":"m","messages":[{"role":"user","content":[` + strings.Join(parts[:attachmentRefMax], ",") + `]},{"role":"user","content":[` + parts[attachmentRefMax] + `]}]}`
	rec := f.post(f.userA, body)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"param":"messages[1].content[0]"`) {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	// Exactly the cap is fine.
	if rec := f.post(f.userA, `{"model":"m","messages":[{"role":"user","content":[`+strings.Join(parts[:attachmentRefMax], ",")+`]}]}`); rec.Code != 200 {
		t.Errorf("at the cap = %d: %.200s", rec.Code, rec.Body.String())
	}
}

// Distinct ids over the cap are refused as well; the cap counts references,
// not distinct attachments.
func TestChatExpand_DistinctIDsOverTheCapAreRefused(t *testing.T) {
	f := newChatAttachFixture(t)
	var ids []string
	for turn := 0; turn < 2; turn++ {
		atts := make([]attachmentInput, 13)
		for i := range atts {
			atts[i] = imgAtt(fmt.Sprintf("p%d-%d.png", turn, i), fixture(pngHead, 50+turn*20+i))
		}
		rec := doJSON(t, f.rA, http.MethodPost, "/api/v1/conversations/"+f.convID+"/messages", map[string]any{
			"messages": []map[string]any{{"role": "user", "attachments": atts}},
		})
		var resp struct {
			Messages []conversationMessage `json:"messages"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Messages) == 0 {
			t.Fatalf("store = %d: %.200s", rec.Code, rec.Body.String())
		}
		for _, a := range resp.Messages[0].Attachments {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) != 26 {
		t.Fatalf("ids = %d", len(ids))
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = refPartJSON(id)
	}
	rec := f.post(f.userA, `{"model":"m","messages":[{"role":"user","content":[`+strings.Join(parts, ",")+`]}]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"param":"messages[0].content[25]"`) {
		t.Fatalf("26 distinct = %d: %.200s", rec.Code, rec.Body.String())
	}
	if rec := f.post(f.userA, `{"model":"m","messages":[{"role":"user","content":[`+strings.Join(parts[:25], ",")+`]}]}`); rec.Code != 200 {
		t.Errorf("25 distinct = %d: %.200s", rec.Code, rec.Body.String())
	}
}
