package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

var (
	pngHead = []byte("\x89PNG\r\n\x1a\n")
	pdfHead = []byte("%PDF-1.4\n")
)

func enc(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func sized(head []byte, n int) []byte {
	out := make([]byte, n)
	copy(out, head)
	return out
}

func decodeMsg(t *testing.T, role, content string) Message {
	t.Helper()
	var m Message
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"role":%q,"content":%s}`, role, content)), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func imgBlock(mt string, b []byte) string {
	return fmt.Sprintf(`{"type":"image_url","image_url":{"url":"data:%s;base64,%s"}}`, mt, enc(b))
}

func fileBlock(name, mt string, b []byte) string {
	return fmt.Sprintf(`{"type":"file","file":{"filename":%q,"file_data":"data:%s;base64,%s"}}`, name, mt, enc(b))
}

func arr(blocks ...string) string { return "[" + strings.Join(blocks, ",") + "]" }

const textBlock = `{"type":"text","text":"look"}`

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

func TestMessageParts_Shapes(t *testing.T) {
	m := decodeMsg(t, "user", arr(
		textBlock,
		imgBlock("image/png", pngHead),
		`{"type":"image_url","image_url":"data:image/jpeg;base64,/9j/"}`,
		fileBlock("n.pdf", "application/pdf", pdfHead),
		`{"type":"image_url","image_url":{"url":"https://x/y.png"}}`,
		`{"type":"image_url","image_url":{"url":"s3://b/k.png"}}`,
		`{"type":"file","file":{"file_id":"file-1"}}`,
		`{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}`,
		`{"type":"image_url","image_url":{"url":"junk"}}`,
	))
	want := []struct {
		kind, mt, wire string
		remote         bool
	}{
		{KindText, "", "", false},
		{KindImage, "image/png", "", false},
		{KindImage, "image/jpeg", "", false},
		{KindFile, "application/pdf", "", false},
		{KindUnknown, "", "image_url", true},
		{KindUnknown, "", "image_url", true},
		{KindUnknown, "", "file", true},
		{KindUnknown, "", "input_audio", false},
		{KindImage, "", "", false}, // malformed: the validator refuses it by name
	}
	if len(m.Parts) != len(want) {
		t.Fatalf("parts = %d, want %d", len(m.Parts), len(want))
	}
	for i, w := range want {
		p := m.Parts[i]
		if p.Kind != w.kind || p.MediaType != w.mt || p.WireType != w.wire || p.Remote != w.remote {
			t.Errorf("part %d = %+v, want %+v", i, p, w)
		}
	}
	if m.Parts[3].Filename != "n.pdf" {
		t.Errorf("filename = %q", m.Parts[3].Filename)
	}
	// Content and HasNonTextContent behave exactly as before.
	if m.Content != "look" || !m.HasNonTextContent {
		t.Errorf("Content = %q HasNonTextContent = %v", m.Content, m.HasNonTextContent)
	}
}

func TestMessageParts_TextOnlyAndStringKeepOldBehaviour(t *testing.T) {
	s := decodeMsg(t, "user", `"hello"`)
	if s.Content != "hello" || s.HasNonTextContent || s.Parts != nil || s.MalformedContent {
		t.Errorf("string content: %+v", s)
	}
	a := decodeMsg(t, "user", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)
	if a.Content != "ab" || a.HasNonTextContent || len(a.Parts) != 2 {
		t.Errorf("text array: %+v", a)
	}
}

func TestContentPartBytes_DecodesOnceAndCaches(t *testing.T) {
	p := ContentPart{Kind: KindImage, Data: enc(pngHead)}
	b1, err := p.Bytes()
	if err != nil || string(b1) != string(pngHead) {
		t.Fatalf("bytes = %q err %v", b1, err)
	}
	p.Data = "changed-after-decode"
	b2, _ := p.Bytes()
	if &b1[0] != &b2[0] {
		t.Error("second Bytes call decoded again")
	}
	bad := ContentPart{Data: "@@@@"}
	if _, err := bad.Bytes(); err == nil || strings.Contains(err.Error(), "@@@@") {
		t.Errorf("err = %v", err)
	}
}

// The bypass: a content that is neither a string nor a decodable array is
// flagged, never turned into prompt text.
func TestMessageUnmarshal_UndecodableContentIsFlaggedNotText(t *testing.T) {
	for _, content := range []string{`123`, `true`, `{"a":1}`, `["x"]`, `[{"type":"text","text":5}]`, `[[1]]`} {
		m := decodeMsg(t, "user", content)
		if !m.MalformedContent || m.Content != "" || m.HasNonTextContent || m.Parts != nil {
			t.Errorf("content %s: %+v", content, m)
		}
	}
	for _, content := range []string{`"x"`, `[]`, `null`, `[{"type":"text","text":"x"}]`} {
		if m := decodeMsg(t, "user", content); m.MalformedContent {
			t.Errorf("content %s flagged malformed", content)
		}
	}
	param, msg := ValidateContentShape([]Message{decodeMsg(t, "user", `"ok"`), decodeMsg(t, "user", `{"a":1}`)})
	if param != "messages[1].content" || msg == "" {
		t.Errorf("param %q msg %q", param, msg)
	}
	if p, m := ValidateContentShape([]Message{decodeMsg(t, "user", `"ok"`)}); p != "" || m != "" {
		t.Errorf("clean messages refused: %q %q", p, m)
	}
}

// ---------------------------------------------------------------------------
// ValidateContent
// ---------------------------------------------------------------------------

func TestValidateContent(t *testing.T) {
	pdfAt := func(n int) string { return fileBlock("a.pdf", "application/pdf", sized(pdfHead, n)) }
	pngAt := func(n int) string { return imgBlock("image/png", sized(pngHead, n)) }
	repeat := func(s string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = s
		}
		return out
	}

	cases := []struct {
		name      string
		role      string
		content   string
		wantParam string // "" = accepted
		wantIn    string
	}{
		{"image ok", "user", arr(textBlock, pngAt(100)), "", ""},
		{"image only ok", "user", arr(pngAt(100)), "", ""},
		{"pdf with text ok", "user", arr(textBlock, pdfAt(100)), "", ""},
		{"csv utf8 ok", "user", arr(textBlock, fileBlock("a.csv", "text/csv", []byte("a,b\n"))), "", ""},
		{"image at the size limit", "user", arr(pngAt(MaxImageBytes)), "", ""},
		{"image over the size limit", "user", arr(pngAt(MaxImageBytes + 1)), "messages[0].content[0]", "3750000"},
		{"document at the size limit", "user", arr(textBlock, pdfAt(MaxDocumentBytes)), "", ""},
		{"document over the size limit", "user", arr(textBlock, pdfAt(MaxDocumentBytes+1)), "messages[0].content[1]", "4500000"},
		{"images at the count limit", "user", arr(repeat(pngAt(10), MaxImagesPerRequest)...), "", ""},
		{"images over the count limit", "user", arr(repeat(pngAt(10), MaxImagesPerRequest+1)...), fmt.Sprintf("messages[0].content[%d]", MaxImagesPerRequest), "at most 20 images"},
		{"documents at the count limit", "user", arr(append([]string{textBlock}, repeat(pdfAt(10), MaxDocumentsPerRequest)...)...), "", ""},
		{"documents over the count limit", "user", arr(append([]string{textBlock}, repeat(pdfAt(10), MaxDocumentsPerRequest+1)...)...), fmt.Sprintf("messages[0].content[%d]", MaxDocumentsPerRequest+1), "at most 5 files"},
		{"bad base64", "user", `[{"type":"image_url","image_url":{"url":"data:image/png;base64,@@@@"}}]`, "messages[0].content[0]", "not valid base64"},
		{"empty payload", "user", `[{"type":"image_url","image_url":{"url":"data:image/png;base64,"}}]`, "messages[0].content[0]", "empty"},
		{"png bytes labelled jpeg", "user", arr(imgBlock("image/jpeg", pngHead)), "messages[0].content[0]", "image/jpeg"},
		{"jpeg bytes labelled png", "user", arr(imgBlock("image/png", []byte{0xFF, 0xD8, 0xFF})), "messages[0].content[0]", "image/png"},
		{"not a pdf", "user", arr(textBlock, fileBlock("a.pdf", "application/pdf", []byte("hello"))), "messages[0].content[1]", "application/pdf"},
		{"csv not utf8", "user", arr(textBlock, fileBlock("a.csv", "text/csv", []byte{0xff, 0xfe, 0xfd})), "messages[0].content[1]", "UTF-8"},
		{"svg", "user", arr(imgBlock("image/svg+xml", []byte("<svg/>"))), "messages[0].content[0]", "SVG"},
		{"type outside the allowlist", "user", arr(imgBlock("image/bmp", []byte("BM"))), "messages[0].content[0]", "image/bmp"},
		{"image type used as a file", "user", arr(textBlock, fileBlock("a.png", "image/png", pngHead)), "messages[0].content[1]", "image/png"},
		{"remote url", "user", `[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]`, "messages[0].content[0]", "data: URL"},
		{"s3 url", "user", `[{"type":"image_url","image_url":{"url":"s3://b/k.png"}}]`, "messages[0].content[0]", "data: URL"},
		{"file id", "user", `[{"type":"file","file":{"file_id":"file-1"}}]`, "messages[0].content[0]", "data: URL"},
		{"audio", "user", `[{"type":"input_audio","input_audio":{}}]`, "messages[0].content[0]", "not supported"},
		{"malformed url", "user", `[{"type":"image_url","image_url":{"url":"data:image/png,abc"}}]`, "messages[0].content[0]", "malformed data URL"},
		{"part on assistant", "assistant", arr(textBlock, pngAt(10)), "messages[0].content[1]", "only in user messages"},
		{"part on system", "system", arr(pngAt(10)), "messages[0].content[0]", "only in user messages"},
		{"document without text", "user", arr(pdfAt(10)), "messages[0].content[0]", "needs a text message"},
		{"document with blank text", "user", arr(`{"type":"text","text":"  "}`, pdfAt(10)), "messages[0].content[1]", "needs a text message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			param, msg := ValidateContent([]Message{decodeMsg(t, tc.role, tc.content)})
			if tc.wantParam == "" {
				if param != "" || msg != "" {
					t.Fatalf("refused: %q %q", param, msg)
				}
				return
			}
			if param != tc.wantParam {
				t.Fatalf("param = %q, want %q (msg %q)", param, tc.wantParam, msg)
			}
			if !strings.Contains(msg, tc.wantIn) || !strings.HasPrefix(msg, tc.wantParam+":") {
				t.Errorf("msg = %q, want it to name %q", msg, tc.wantIn)
			}
			if strings.Contains(msg, "a.pdf") || strings.Contains(msg, "a.csv") || strings.Contains(msg, enc(pngHead)) {
				t.Errorf("msg leaks a filename or bytes: %q", msg)
			}
		})
	}
}

func TestValidateContent_TotalBudgetAcrossMessages(t *testing.T) {
	// Three 3.7 MB images are 11.1 MB decoded: each part and the count are
	// fine, the request total is not.
	part := imgBlock("image/png", sized(pngHead, 3_700_000))
	msgs := []Message{
		decodeMsg(t, "user", arr(textBlock, part, part)),
		decodeMsg(t, "assistant", `"ok"`),
		decodeMsg(t, "user", arr(textBlock, part)),
	}
	param, msg := ValidateContent(msgs)
	if param != "messages[2].content[1]" || !strings.Contains(msg, "10485760") {
		t.Errorf("param %q msg %q", param, msg)
	}
	// At the total: 2 x 3.7 MB = 7.4 MB is accepted.
	if p, m := ValidateContent(msgs[:1]); p != "" {
		t.Errorf("under the total refused: %q %q", p, m)
	}
}

func TestValidateContent_CountsAreAcrossMessages(t *testing.T) {
	var msgs []Message
	for i := 0; i < MaxImagesPerRequest+1; i++ {
		msgs = append(msgs, decodeMsg(t, "user", arr(textBlock, imgBlock("image/png", pngHead))))
	}
	param, _ := ValidateContent(msgs)
	if param != fmt.Sprintf("messages[%d].content[1]", MaxImagesPerRequest) {
		t.Errorf("param = %q", param)
	}
}

func TestValidateContent_DecodesOnceAndSharesTheResult(t *testing.T) {
	msgs := []Message{decodeMsg(t, "user", arr(textBlock, imgBlock("image/png", pngHead)))}
	if p, m := ValidateContent(msgs); p != "" {
		t.Fatal(m)
	}
	b1, _ := msgs[0].Parts[1].Bytes()
	msgs[0].Parts[1].Data = "" // a translator must be reading the validator's decode
	b2, err := msgs[0].Parts[1].Bytes()
	if err != nil || &b1[0] != &b2[0] {
		t.Errorf("the validator's decode was not reused: %v", err)
	}
}

func TestContentFormat(t *testing.T) {
	for mt, want := range map[string]string{
		"image/png": "png", "image/jpeg": "jpeg", "image/gif": "gif", "image/webp": "webp",
		"application/pdf": "pdf", "text/csv": "csv", "text/html": "html", "text/plain": "txt", "text/markdown": "md",
		"application/msword": "doc", "application/vnd.ms-excel": "xls",
		"image/svg+xml": "", "image/bmp": "", "": "",
	} {
		if got := ContentFormat(mt); got != want {
			t.Errorf("ContentFormat(%q) = %q, want %q", mt, got, want)
		}
	}
}

// Every allowed media type: bytes that match are accepted, bytes that do not
// are a refusal naming the media type and the part.
func TestValidateContent_SniffsEveryMediaType(t *testing.T) {
	const (
		docxMT = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
		xlsxMT = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	)
	ole := []byte("\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1rest")
	docx := []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00[Content_Types].xml....word/document.xml")
	xlsx := []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00[Content_Types].xml....xl/workbook.xml")
	zipOnly := []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00hello.txt")
	cases := []struct {
		name, mt string
		good     [][]byte
		bad      [][]byte
	}{
		{"png", "image/png", [][]byte{[]byte("\x89PNG\r\n\x1a\nxx")}, [][]byte{[]byte("GIF89a"), []byte("junk")}},
		{"jpeg", "image/jpeg", [][]byte{{0xFF, 0xD8, 0xFF, 0xE0}}, [][]byte{[]byte("\x89PNG\r\n\x1a\n"), []byte("junk")}},
		{"gif", "image/gif", [][]byte{[]byte("GIF87a.."), []byte("GIF89a..")}, [][]byte{[]byte("GIF90a"), []byte("junk")}},
		{"webp", "image/webp", [][]byte{[]byte("RIFF\x00\x00\x00\x00WEBPVP8 ")}, [][]byte{[]byte("RIFF\x00\x00\x00\x00WAVEfmt "), []byte("junk")}},
		{"pdf", "application/pdf", [][]byte{[]byte("%PDF-1.7")}, [][]byte{[]byte("hello"), ole}},
		{"doc", "application/msword", [][]byte{ole}, [][]byte{docx, []byte("plain text"), {0xD0, 0xCF, 0x11}}},
		{"xls", "application/vnd.ms-excel", [][]byte{ole}, [][]byte{xlsx, []byte("plain text")}},
		{"docx", docxMT, [][]byte{docx}, [][]byte{ole, []byte("plain text"), zipOnly, xlsx[:4]}},
		{"xlsx", xlsxMT, [][]byte{xlsx}, [][]byte{ole, []byte("plain text"), zipOnly, docx[:4]}},
		{"html", "text/html", [][]byte{[]byte("<html></html>"), []byte("\xEF\xBB\xBF \n<p>hi</p>"), []byte("hello <b>x</b>")}, [][]byte{[]byte("no markup here"), {0xff, 0xfe, '<'}, append(bytes.Repeat([]byte("a"), 600), '<')}},
		{"csv", "text/csv", [][]byte{[]byte("a,b\n1,2\n"), []byte("héllo")}, [][]byte{{0xff, 0xfe, 0xfd}}},
		{"txt", "text/plain", [][]byte{[]byte("hello")}, [][]byte{{0xc3, 0x28}}},
		{"md", "text/markdown", [][]byte{[]byte("# title")}, [][]byte{{0xff, 0xfe}}},
	}
	for _, tc := range cases {
		block := func(b []byte) string {
			if strings.HasPrefix(tc.mt, "image/") {
				return imgBlock(tc.mt, b)
			}
			return fileBlock("f", tc.mt, b)
		}
		content := func(b []byte) string {
			if strings.HasPrefix(tc.mt, "image/") {
				return arr(textBlock, block(b))
			}
			return arr(textBlock, block(b))
		}
		for i, b := range tc.good {
			if param, msg := ValidateContent([]Message{decodeMsg(t, "user", content(b))}); param != "" {
				t.Errorf("%s good[%d] refused: %s %s", tc.name, i, param, msg)
			}
		}
		for i, b := range tc.bad {
			param, msg := ValidateContent([]Message{decodeMsg(t, "user", content(b))})
			if param != "messages[0].content[1]" || !strings.Contains(msg, tc.mt) {
				t.Errorf("%s bad[%d]: param %q msg %q, want refusal naming %s", tc.name, i, param, msg, tc.mt)
			}
		}
	}
}
