package bedrock

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// Image and document parts: OpenAI content arrays in, Converse blocks out.

var (
	pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), 1, 2, 3)
	pdfBytes = []byte("%PDF-1.4\nbody\n%%EOF\n")
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// userMsg decodes a user message from content blocks the way the handler does,
// so the test exercises the real parser.
func userMsg(t *testing.T, blocks ...map[string]any) provider.Message {
	t.Helper()
	return msgOf(t, "user", blocks...)
}

func msgOf(t *testing.T, role string, blocks ...map[string]any) provider.Message {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"role": role, "content": blocks})
	if err != nil {
		t.Fatal(err)
	}
	var m provider.Message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func txt(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
func img(mt string, b []byte) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mt + ";base64," + b64(b)}}
}
func doc(name, mt string, b []byte) map[string]any {
	return map[string]any{"type": "file", "file": map[string]any{"filename": name, "file_data": "data:" + mt + ";base64," + b64(b)}}
}

// translateValidated mirrors the handler: ValidateContent first (the single
// decode), then the translator.
func translateValidated(t *testing.T, msgs ...provider.Message) ([]types.Message, error) {
	t.Helper()
	if param, msg := provider.ValidateContent(msgs); msg != "" {
		t.Fatalf("ValidateContent refused %s: %s", param, msg)
	}
	_, out, _, err := translateRequest(provider.ChatRequest{Model: "m", Messages: msgs})
	return out, err
}

func TestTranslateRequest_EveryFormat(t *testing.T) {
	text := []byte("a,b\n1,2\n")
	for _, tc := range []struct {
		mt     string
		body   []byte
		image  types.ImageFormat
		format types.DocumentFormat
	}{
		{"image/png", pngBytes, types.ImageFormatPng, ""},
		{"image/jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0}, types.ImageFormatJpeg, ""},
		{"image/gif", []byte("GIF89a\x01\x00"), types.ImageFormatGif, ""},
		{"image/webp", []byte("RIFF\x04\x00\x00\x00WEBPVP8 "), types.ImageFormatWebp, ""},
		{"application/pdf", pdfBytes, "", types.DocumentFormatPdf},
		{"text/csv", text, "", types.DocumentFormatCsv},
		{"application/msword", []byte{0xD0, 0xCF, 0x11, 0xE0}, "", types.DocumentFormatDoc},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", []byte("PK\x03\x04[Content_Types].xml word/document.xml"), "", types.DocumentFormatDocx},
		{"application/vnd.ms-excel", []byte{0xD0, 0xCF, 0x11, 0xE0}, "", types.DocumentFormatXls},
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", []byte("PK\x03\x04[Content_Types].xml xl/workbook.xml"), "", types.DocumentFormatXlsx},
		{"text/html", []byte("<p>hi</p>"), "", types.DocumentFormatHtml},
		{"text/plain", []byte("hello"), "", types.DocumentFormatTxt},
		{"text/markdown", []byte("# hi"), "", types.DocumentFormatMd},
	} {
		t.Run(tc.mt, func(t *testing.T) {
			var part map[string]any
			if tc.image != "" {
				part = img(tc.mt, tc.body)
			} else {
				part = doc("secret-name.bin", tc.mt, tc.body)
			}
			msgs, err := translateValidated(t, userMsg(t, txt("look"), part))
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 || len(msgs[0].Content) != 2 {
				t.Fatalf("messages = %+v", msgs)
			}
			switch b := msgs[0].Content[0].(type) {
			case *types.ContentBlockMemberImage:
				src, ok := b.Value.Source.(*types.ImageSourceMemberBytes)
				if tc.image == "" || !ok || b.Value.Format != tc.image || !bytes.Equal(src.Value, tc.body) {
					t.Errorf("image block = %+v", b.Value)
				}
			case *types.ContentBlockMemberDocument:
				src, ok := b.Value.Source.(*types.DocumentSourceMemberBytes)
				if tc.format == "" || !ok || b.Value.Format != tc.format || !bytes.Equal(src.Value, tc.body) {
					t.Errorf("document block = %+v", b.Value)
				}
				if got := *b.Value.Name; got != "document 1" {
					t.Errorf("document name = %q, want the neutral \"document 1\"", got)
				}
			default:
				t.Fatalf("first block is %T, want the media block ahead of the text", b)
			}
			if textOf(t, msgs[0].Content[1]) != "look" {
				t.Errorf("text block missing or misplaced: %+v", msgs[0].Content)
			}
		})
	}
}

func TestTranslateRequest_ImageOnlyTurnIsNotDropped(t *testing.T) {
	msgs, err := translateValidated(t, userMsg(t, img("image/png", pngBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || len(msgs[0].Content) != 1 {
		t.Fatalf("an image-only turn was dropped or padded: %+v", msgs)
	}
	if _, ok := msgs[0].Content[0].(*types.ContentBlockMemberImage); !ok {
		t.Errorf("block = %T", msgs[0].Content[0])
	}
}

func TestTranslateRequest_OrderImagesDocumentsText(t *testing.T) {
	msgs, err := translateValidated(t, userMsg(t,
		txt("first"),
		doc("a.pdf", "application/pdf", pdfBytes),
		img("image/png", pngBytes),
		txt(" second"),
		img("image/png", pngBytes),
	))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range msgs[0].Content {
		got = append(got, fmt.Sprintf("%T", b))
	}
	want := []string{"*types.ContentBlockMemberImage", "*types.ContentBlockMemberImage", "*types.ContentBlockMemberDocument", "*types.ContentBlockMemberText"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
	if textOf(t, msgs[0].Content[3]) != "first second" {
		t.Errorf("text parts should flatten in order, got %q", textOf(t, msgs[0].Content[3]))
	}
}

func TestTranslateRequest_ConsecutiveUserTurnsMergeWithTheirMedia(t *testing.T) {
	msgs, err := translateValidated(t,
		userMsg(t, txt("one"), img("image/png", pngBytes)),
		userMsg(t, img("image/png", pngBytes)), // media only, no text
		userMsg(t, txt("two"), doc("x", "text/plain", []byte("hi"))),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("consecutive user turns must merge into one Converse message, got %d", len(msgs))
	}
	c := msgs[0].Content
	if len(c) != 4 {
		t.Fatalf("blocks = %d, want 2 images, 1 document, 1 text", len(c))
	}
	if _, ok := c[2].(*types.ContentBlockMemberDocument); !ok {
		t.Errorf("block 2 = %T, want the document", c[2])
	}
	if textOf(t, c[3]) != "one\ntwo" {
		t.Errorf("text = %q", textOf(t, c[3]))
	}
}

func TestTranslateRequest_DocumentNamesAreNeutralAndNumberedAcrossTheRequest(t *testing.T) {
	msgs, err := translateValidated(t,
		userMsg(t, txt("a"), doc("report_v2.final.pdf", "application/pdf", pdfBytes)),
		msgOf(t, "assistant", txt("ok")),
		userMsg(t, txt("b"), doc("Q3 (secret).pdf", "application/pdf", pdfBytes), doc("c.pdf", "application/pdf", pdfBytes)),
	)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if d, ok := b.(*types.ContentBlockMemberDocument); ok {
				names = append(names, *d.Value.Name)
			}
		}
	}
	if strings.Join(names, "|") != "document 1|document 2|document 3" {
		t.Errorf("names = %v", names)
	}
}

// The request never carries an S3Location (or any non-bytes source): input
// cannot make Bedrock read from a bucket.
func TestTranslateRequest_NeverBuildsAnS3Location(t *testing.T) {
	msgs, err := translateValidated(t, userMsg(t, txt("x"), img("image/png", pngBytes), doc("d.pdf", "application/pdf", pdfBytes)))
	if err != nil {
		t.Fatal(err)
	}
	dump := fmt.Sprintf("%#v", msgs)
	if strings.Contains(strings.ToLower(dump), "s3location") {
		t.Errorf("request contains an S3Location: %s", dump)
	}
	for _, b := range msgs[0].Content {
		switch v := b.(type) {
		case *types.ContentBlockMemberImage:
			if _, ok := v.Value.Source.(*types.ImageSourceMemberBytes); !ok {
				t.Errorf("image source = %T", v.Value.Source)
			}
		case *types.ContentBlockMemberDocument:
			if _, ok := v.Value.Source.(*types.DocumentSourceMemberBytes); !ok {
				t.Errorf("document source = %T", v.Value.Source)
			}
		}
	}
	// And a URL or an s3:// reference never becomes a block at all.
	for _, url := range []string{"s3://bucket/key.png", "https://example.com/x.png"} {
		m := userMsg(t, txt("x"), map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		_, _, _, err := translateRequest(provider.ChatRequest{Model: "m", Messages: []provider.Message{m}})
		assertContentError(t, err, "messages[0].content[1]")
	}
}

func assertContentError(t *testing.T, err error, wantParam string) {
	t.Helper()
	var apiErr *BedrockAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want a BedrockAPIError", err)
	}
	var body struct {
		Error struct{ Code, Param, Message string }
	}
	if jerr := json.Unmarshal(apiErr.Body, &body); jerr != nil {
		t.Fatal(jerr)
	}
	if apiErr.StatusCode != http.StatusBadRequest || body.Error.Code != "invalid_content" || body.Error.Param != wantParam {
		t.Errorf("status %d body %s, want 400 invalid_content %s", apiErr.StatusCode, apiErr.Body, wantParam)
	}
}

func TestTranslateRequest_PartsOnNonUserRolesAndUnvalidatedBytesAreRefused(t *testing.T) {
	asst := msgOf(t, "assistant", txt("a"), img("image/png", pngBytes))
	_, _, _, err := translateRequest(provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}, asst}})
	assertContentError(t, err, "messages[1].content[1]")

	bad := userMsg(t, txt("x"), map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,@@@"}})
	_, _, _, err = translateRequest(provider.ChatRequest{Model: "m", Messages: []provider.Message{bad}})
	assertContentError(t, err, "messages[0].content[1]")
}

func TestValidationErrorCode_DefaultShapeUnchanged(t *testing.T) {
	var a, b map[string]any
	_ = json.Unmarshal(validationError("boom").Body, &a)
	_, body := TranslateBedrockError("ValidationException", "boom")
	_ = json.Unmarshal(body, &b)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("validationError body %v differs from a ValidationException body %v", a, b)
	}
}

// ---------------------------------------------------------------------------
// Vendor ValidationException about image/document input
// ---------------------------------------------------------------------------

func TestTranslateBedrockError_MediaValidationIsUnsupportedParameterNeverRaw(t *testing.T) {
	for _, raw := range []string{
		"This model doesn't support the image content block",
		"The model returned the following errors: Input does not support document input",
		"messages.0.content.1.image: Could not process image",
	} {
		status, body := translateBedrockError("ValidationException", raw, "", true)
		var e struct {
			Error struct{ Message, Type, Code, Param string }
		}
		_ = json.Unmarshal(body, &e)
		if status != http.StatusBadRequest || e.Error.Code != "unsupported_parameter" || e.Error.Type != "invalid_request_error" {
			t.Errorf("%q -> %d %s", raw, status, body)
		}
		if strings.Contains(string(body), raw) || strings.Contains(string(body), "content.1") {
			t.Errorf("raw AWS text relayed: %s", body)
		}
		// The same text on a request with no media parts is relayed as-is.
		_, plain := translateBedrockError("ValidationException", raw, "", false)
		if !strings.Contains(string(plain), raw) || strings.Contains(string(plain), "unsupported_parameter") {
			t.Errorf("no-media branch changed %q: %s", raw, plain)
		}
	}
	// Unrelated validation text still passes through, as before.
	_, body := TranslateBedrockError("ValidationException", "maxTokens too large")
	if !strings.Contains(string(body), "maxTokens too large") || strings.Contains(string(body), "unsupported_parameter") {
		t.Errorf("unrelated validation changed: %s", body)
	}
}

// Without image or file parts in the request, a ValidationException that merely
// contains "image" or "document" is not a media refusal: its text is relayed.
func TestComplete_MediaWordsWithoutMediaPartsAreRelayed(t *testing.T) {
	const raw = "The document field of the guardrail configuration is invalid"
	fake := &fakeConverseAPI{converseErr: &smithy.GenericAPIError{Code: "ValidationException", Message: raw}}
	_, err := newProviderWithFake(fake).Complete(context.Background(), provider.ChatRequest{
		Model: "m", Messages: []provider.Message{userMsg(t, txt("hello"))},
	})
	var apiErr *BedrockAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(string(apiErr.Body), raw) || strings.Contains(string(apiErr.Body), "unsupported_parameter") {
		t.Errorf("body = %s, want raw text relayed with no unsupported_parameter", apiErr.Body)
	}
	// Same text, request with an image part: fixed refusal, raw text hidden.
	fake = &fakeConverseAPI{converseErr: &smithy.GenericAPIError{Code: "ValidationException", Message: raw}}
	_, err = newProviderWithFake(fake).Complete(context.Background(), provider.ChatRequest{
		Model: "m", Messages: []provider.Message{userMsg(t, txt("x"), img("image/png", pngBytes))},
	})
	if !errors.As(err, &apiErr) || !strings.Contains(string(apiErr.Body), `"code":"unsupported_parameter"`) || strings.Contains(string(apiErr.Body), raw) {
		t.Errorf("with media: err %v body %s", err, apiErr.Body)
	}
}

func mediaValidationErr() error {
	return &smithy.GenericAPIError{Code: "ValidationException", Message: "This model doesn't support the image content block"}
}

func TestComplete_MediaValidationNamesThePartWhenThereIsOne(t *testing.T) {
	one := userMsg(t, txt("x"), img("image/png", pngBytes))
	two := userMsg(t, txt("x"), img("image/png", pngBytes), img("image/png", pngBytes))
	for _, tc := range []struct {
		m    provider.Message
		want string
	}{{one, "messages[0].content[1]"}, {two, ""}} {
		fake := &fakeConverseAPI{converseErr: mediaValidationErr()}
		_, err := newProviderWithFake(fake).Complete(context.Background(), provider.ChatRequest{Model: "m", Messages: []provider.Message{tc.m}})
		var apiErr *BedrockAPIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("err = %v", err)
		}
		var e struct {
			Error struct{ Code, Param, Message string }
		}
		_ = json.Unmarshal(apiErr.Body, &e)
		if e.Error.Code != "unsupported_parameter" || e.Error.Param != tc.want || strings.Contains(e.Error.Message, "doesn't support") {
			t.Errorf("body = %s, want unsupported_parameter param %q", apiErr.Body, tc.want)
		}
	}
}

// The streaming path surfaces the code before the first byte: ConverseStream
// fails ahead of any event, so nothing has been written and the handler can
// still answer with a JSON error.
func TestStream_MediaValidationSurfacesCodeBeforeFirstByte(t *testing.T) {
	fake := &fakeConverseAPI{streamErr: mediaValidationErr()}
	var w bytes.Buffer
	_, err := newProviderWithFake(fake).Stream(context.Background(), provider.ChatRequest{
		Model: "m", Stream: true,
		Messages: []provider.Message{userMsg(t, txt("x"), img("image/png", pngBytes))},
	}, &w)
	var apiErr *BedrockAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	if w.Len() != 0 {
		t.Errorf("%d bytes were written before the error", w.Len())
	}
	if !strings.Contains(string(apiErr.Body), `"code":"unsupported_parameter"`) || !strings.Contains(string(apiErr.Body), `"param":"messages[0].content[1]"`) {
		t.Errorf("body = %s", apiErr.Body)
	}
}
