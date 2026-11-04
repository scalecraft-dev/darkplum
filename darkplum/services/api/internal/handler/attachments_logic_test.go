package handler

import (
	"encoding/base64"
	"strings"
	"testing"
)

var (
	pngHead  = []byte("\x89PNG\r\n\x1a\n")
	pdfHead  = []byte("%PDF-1.4\n")
	jpegHead = []byte{0xFF, 0xD8, 0xFF, 0xE0}
)

// fixture returns head padded to n bytes.
func fixture(head []byte, n int) []byte {
	b := make([]byte, n)
	copy(b, head)
	return b
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func imgAtt(name string, data []byte) attachmentInput {
	return attachmentInput{Kind: "image", MediaType: "image/png", Name: name, Data: b64(data)}
}

func pdfAtt(name string, data []byte) attachmentInput {
	return attachmentInput{Kind: "file", MediaType: "application/pdf", Name: name, Data: b64(data)}
}

func TestValidMessageInput_Attachments(t *testing.T) {
	png := fixture(pngHead, 64)
	pdf := fixture(pdfHead, 64)
	many := func(n int, a attachmentInput) []attachmentInput {
		out := make([]attachmentInput, n)
		for i := range out {
			out[i] = a
		}
		return out
	}
	cases := []struct {
		name    string
		m       messageInput
		wantErr string // substring; "" = valid
	}{
		{"text only", messageInput{Role: roleUser, Content: "hi"}, ""},
		{"attachment only, empty content", messageInput{Role: roleUser, Attachments: []attachmentInput{imgAtt("a.png", png)}}, ""},
		{"empty content and no attachments", messageInput{Role: roleUser}, "content is required"},
		{"assistant with attachment", messageInput{Role: roleAssistant, Content: "x", Attachments: []attachmentInput{imgAtt("a.png", png)}}, "only allowed on a user turn"},
		{"assistant empty content with attachment", messageInput{Role: roleAssistant, Attachments: []attachmentInput{imgAtt("a.png", png)}}, "only allowed on a user turn"},
		{"image and pdf", messageInput{Role: roleUser, Content: "x", Attachments: []attachmentInput{imgAtt("a.png", png), pdfAtt("a.pdf", pdf)}}, ""},
		{"20 images ok", messageInput{Role: roleUser, Attachments: many(20, imgAtt("a.png", png))}, ""},
		{"21 images", messageInput{Role: roleUser, Attachments: many(21, imgAtt("a.png", png))}, "at most 20 images"},
		{"5 documents ok", messageInput{Role: roleUser, Content: "x", Attachments: many(5, pdfAtt("a.pdf", pdf))}, ""},
		{"6 documents", messageInput{Role: roleUser, Content: "x", Attachments: many(6, pdfAtt("a.pdf", pdf))}, "at most 5 documents"},
		{"26 total", messageInput{Role: roleUser, Attachments: many(26, imgAtt("a.png", png))}, "at most 25 attachments"},
		{"image at limit", messageInput{Role: roleUser, Attachments: []attachmentInput{imgAtt("a.png", fixture(pngHead, attachmentImageMaxBytes))}}, ""},
		{"image over limit", messageInput{Role: roleUser, Attachments: []attachmentInput{imgAtt("a.png", fixture(pngHead, attachmentImageMaxBytes+1))}}, "exceeds 3750000 bytes"},
		{"document at limit", messageInput{Role: roleUser, Content: "x", Attachments: []attachmentInput{pdfAtt("a.pdf", fixture(pdfHead, attachmentFileMaxBytes))}}, ""},
		{"document over limit", messageInput{Role: roleUser, Content: "x", Attachments: []attachmentInput{pdfAtt("a.pdf", fixture(pdfHead, attachmentFileMaxBytes+1))}}, "exceeds 4500000 bytes"},
		{"total over 10 MiB", messageInput{Role: roleUser, Attachments: many(3, imgAtt("a.png", fixture(pngHead, attachmentImageMaxBytes)))}, "in total per turn"},
		{"svg refused", messageInput{Role: roleUser, Attachments: []attachmentInput{{Kind: "image", MediaType: "image/svg+xml", Name: "a.svg", Data: b64([]byte("<svg/>"))}}}, "not supported"},
		{"unknown media type", messageInput{Role: roleUser, Attachments: []attachmentInput{{Kind: "file", MediaType: "application/zip", Name: "a.zip", Data: b64([]byte("PK\x03\x04"))}}}, "not supported"},
		{"kind mismatch", messageInput{Role: roleUser, Attachments: []attachmentInput{{Kind: "file", MediaType: "image/png", Name: "a.png", Data: b64(png)}}}, "kind must be"},
		{"png bytes labelled jpeg", messageInput{Role: roleUser, Attachments: []attachmentInput{{Kind: "image", MediaType: "image/jpeg", Name: "a.jpg", Data: b64(png)}}}, "does not match media_type"},
		{"jpeg ok", messageInput{Role: roleUser, Attachments: []attachmentInput{{Kind: "image", MediaType: "image/jpeg", Name: "a.jpg", Data: b64(jpegHead)}}}, ""},
		{"gif ok", messageInput{Role: roleUser, Attachments: []attachmentInput{{Kind: "image", MediaType: "image/gif", Name: "a.gif", Data: b64([]byte("GIF89a...."))}}}, ""},
		{"webp ok", messageInput{Role: roleUser, Attachments: []attachmentInput{{Kind: "image", MediaType: "image/webp", Name: "a.webp", Data: b64([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "))}}}, ""},
		{"riff but not webp", messageInput{Role: roleUser, Attachments: []attachmentInput{{Kind: "image", MediaType: "image/webp", Name: "a.webp", Data: b64([]byte("RIFF\x00\x00\x00\x00WAVEfmt "))}}}, "does not match"},
		{"pdf labelled png", messageInput{Role: roleUser, Attachments: []attachmentInput{{Kind: "image", MediaType: "image/png", Name: "a.png", Data: b64(pdf)}}}, "does not match"},
		{"text/plain utf8", messageInput{Role: roleUser, Content: "x", Attachments: []attachmentInput{{Kind: "file", MediaType: "text/plain", Name: "a.txt", Data: b64([]byte("héllo"))}}}, ""},
		{"text/plain invalid utf8", messageInput{Role: roleUser, Content: "x", Attachments: []attachmentInput{{Kind: "file", MediaType: "text/plain", Name: "a.txt", Data: b64([]byte{0xff, 0xfe, 'a'})}}}, "does not match"},
		{"text/csv with NUL", messageInput{Role: roleUser, Content: "x", Attachments: []attachmentInput{{Kind: "file", MediaType: "text/csv", Name: "a.csv", Data: b64([]byte("a,b\x00"))}}}, "does not match"},
		{"docx needs zip", messageInput{Role: roleUser, Content: "x", Attachments: []attachmentInput{{Kind: "file", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Name: "a.docx", Data: b64([]byte("not a zip"))}}}, "does not match"},
		{"docx zip ok", messageInput{Role: roleUser, Content: "x", Attachments: []attachmentInput{{Kind: "file", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Name: "a.docx", Data: b64([]byte("PK\x03\x04...."))}}}, ""},
		{"bad base64", messageInput{Role: roleUser, Attachments: []attachmentInput{{Kind: "image", MediaType: "image/png", Name: "a.png", Data: "!!!not base64"}}}, "not valid base64"},
		{"empty data", messageInput{Role: roleUser, Attachments: []attachmentInput{{Kind: "image", MediaType: "image/png", Name: "a.png", Data: ""}}}, "is empty"},
		{"empty name", messageInput{Role: roleUser, Attachments: []attachmentInput{imgAtt("", png)}}, "name must be"},
		{"name 256 chars", messageInput{Role: roleUser, Attachments: []attachmentInput{imgAtt(strings.Repeat("a", 256), png)}}, "name must be"},
		{"name 255 chars", messageInput{Role: roleUser, Attachments: []attachmentInput{imgAtt(strings.Repeat("a", 255), png)}}, ""},
		{"name with NUL", messageInput{Role: roleUser, Attachments: []attachmentInput{imgAtt("a\x00.png", png)}}, "control characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validMessageInput(tc.m)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidMessageInput_AttachmentErrorsNeverEchoNameOrBytes(t *testing.T) {
	err := validMessageInput(messageInput{Role: roleUser, Attachments: []attachmentInput{
		{Kind: "image", MediaType: "image/png", Name: "secret-merger-plan.png", Data: b64(fixture(pdfHead, 64))},
	}})
	if err == nil || strings.Contains(err.Error(), "secret-merger-plan") {
		t.Fatalf("error must not carry the filename: %v", err)
	}
}

func TestDeriveConversationTitle_AttachmentOnlyTurnUsesFirstFileName(t *testing.T) {
	got := deriveConversationTitle([]messageInput{{Role: roleUser, Attachments: []attachmentInput{imgAtt("photo.png", pngHead)}}})
	if got != "photo.png" {
		t.Errorf("title = %q", got)
	}
}

func TestAttachmentDisposition(t *testing.T) {
	cases := []struct {
		mt, name string
		want     string
		notWant  string
	}{
		{"image/png", "../../x.png", "inline", ""},
		{"image/webp", "a.webp", "inline", ""},
		{"application/pdf", "report.pdf", `attachment; filename="report.pdf"`, ""},
		{"text/html", "x.html", `attachment; filename="x.html"`, ""},
		{"application/pdf", "a\"b\r\nX-Evil: 1.pdf", `attachment; filename="a_b__X-Evil_ 1.pdf"`, "\r"},
		{"application/pdf", "../etc/passwd", `filename="_etc_passwd"`, "/"},
		{"application/pdf", "résumé.pdf", `filename*=UTF-8''r%C3%A9sum%C3%A9.pdf`, ""},
		{"application/pdf", "...", `filename="attachment"`, ""},
	}
	for _, tc := range cases {
		got := attachmentDisposition(tc.mt, tc.name)
		if !strings.Contains(got, tc.want) {
			t.Errorf("disposition(%q,%q) = %q, want containing %q", tc.mt, tc.name, got, tc.want)
		}
		if tc.notWant != "" && strings.Contains(strings.SplitN(got, "; filename*", 2)[0], tc.notWant) {
			t.Errorf("disposition(%q,%q) = %q contains %q", tc.mt, tc.name, got, tc.notWant)
		}
	}
}
