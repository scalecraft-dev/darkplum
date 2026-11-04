package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Multi-part message content: images and files in the OpenAI chat shape.
//
//	{"type":"text","text":"..."}
//	{"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}   (or "image_url":"data:...")
//	{"type":"file","file":{"filename":"x.pdf","file_data":"data:application/pdf;base64,..."}}
//
// Only inline data: URLs are accepted. A URL (http, https, file, s3), a
// file_id, input_audio and any other part type parse to Kind "unknown" so the
// registry refuses it by name (chatparams row unknown-content) before any
// backend is chosen; the gateway never fetches anything on a caller's behalf
// and never builds an S3 location from request input.
//
// Message.Content and Message.HasNonTextContent keep their historical meaning
// (the flattened text, and "some non-text block was present"); Message.Parts
// is the structured view the Bedrock translator reads.
//
// Token accounting: image and document parts are not counted toward any token
// estimate in v1 (the gateway reports the vendor's own usage numbers, and the
// router sees text only). Follow-up, not an oversight.

// ContentPart is one block of an array-form message content, in request order.
type ContentPart struct {
	// Kind is KindText, KindImage, KindFile or KindUnknown.
	Kind string
	// Text is the text of a KindText part.
	Text string
	// MediaType is the lower-cased media type of a data: URL ("image/png"),
	// "" when the URL was malformed.
	MediaType string
	// Data is the base64 payload exactly as sent. Decode it with Bytes.
	Data string
	// Filename is what the caller called a file. Informational only: it is
	// never sent to a backend (a vendor treats a document name as prompt
	// text), never logged, and never returned in an error.
	Filename string
	// WireType is the "type" the caller sent for a KindUnknown part.
	WireType string
	// Remote marks a KindUnknown part that referred to content by URL or id
	// instead of carrying it.
	Remote bool

	decoded []byte
}

// Bytes decodes Data. The result is cached on the part, so the validator and
// the translator share one decode. The error carries no payload bytes.
func (p *ContentPart) Bytes() ([]byte, error) {
	if p.decoded != nil {
		return p.decoded, nil
	}
	b, err := base64.StdEncoding.DecodeString(p.Data)
	if err != nil {
		return nil, fmt.Errorf("invalid base64")
	}
	if b == nil {
		b = []byte{}
	}
	p.decoded = b
	return b, nil
}

// parseContentArray decodes an array-form content value. ok is false when raw
// is not an array of objects; the caller must treat that as a malformed
// request, never as text.
func parseContentArray(raw json.RawMessage) (parts []ContentPart, text string, nonText bool, ok bool) {
	var blocks []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
		File     json.RawMessage `json:"file"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, "", false, false
	}
	var combined strings.Builder
	parts = make([]ContentPart, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			combined.WriteString(b.Text)
			parts = append(parts, ContentPart{Kind: KindText, Text: b.Text})
			continue
		case "image_url":
			parts = append(parts, imagePart(b.ImageURL))
		case "file":
			parts = append(parts, filePart(b.File))
		default:
			parts = append(parts, ContentPart{Kind: KindUnknown, WireType: b.Type})
		}
		nonText = true
	}
	return parts, combined.String(), nonText, true
}

func imagePart(raw json.RawMessage) ContentPart {
	unknown := ContentPart{Kind: KindUnknown, WireType: "image_url"}
	var url string
	if err := json.Unmarshal(raw, &url); err != nil {
		var obj struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return unknown
		}
		url = obj.URL
	}
	mt, data, kind := classifyURL(url)
	switch kind {
	case urlRemote:
		unknown.Remote = true
		return unknown
	case urlData:
		return ContentPart{Kind: KindImage, MediaType: mt, Data: data}
	}
	return ContentPart{Kind: KindImage} // malformed; the validator names it
}

func filePart(raw json.RawMessage) ContentPart {
	unknown := ContentPart{Kind: KindUnknown, WireType: "file"}
	var f struct {
		Filename string `json:"filename"`
		FileData string `json:"file_data"`
		FileID   string `json:"file_id"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return unknown
	}
	if f.FileID != "" {
		unknown.Remote = true
		return unknown
	}
	mt, data, kind := classifyURL(f.FileData)
	switch kind {
	case urlRemote:
		unknown.Remote = true
		return unknown
	case urlData:
		return ContentPart{Kind: KindFile, MediaType: mt, Data: data, Filename: f.Filename}
	}
	return ContentPart{Kind: KindFile, Filename: f.Filename}
}

const (
	urlMalformed = iota
	urlData
	urlRemote
)

// classifyURL splits a data: URL into media type and base64 payload. A string
// with another URL scheme is remote; anything else (empty, no ";base64," ) is
// malformed.
func classifyURL(u string) (mediaType, data string, kind int) {
	if len(u) >= 5 && strings.EqualFold(u[:5], "data:") {
		comma := strings.IndexByte(u, ',')
		if comma < 0 {
			return "", "", urlMalformed
		}
		header := strings.Split(u[5:comma], ";")
		if len(header) < 2 || !strings.EqualFold(header[len(header)-1], "base64") {
			return "", "", urlMalformed
		}
		mt := strings.ToLower(strings.TrimSpace(header[0]))
		if mt == "" {
			return "", "", urlMalformed
		}
		return mt, u[comma+1:], urlData
	}
	if i := strings.Index(u, ":"); i > 0 && !strings.ContainsAny(u[:i], " /") {
		return "", "", urlRemote // http:, https:, file:, s3:, ...
	}
	return "", "", urlMalformed
}

// ValidateContentShape reports a message whose content is neither a string
// nor a decodable array of parts. Such a value used to be passed on as prompt
// text, which turned raw JSON (including base64) into the prompt and skipped
// the non-text refusal; it is a 400 invalid_request naming the field.
func ValidateContentShape(msgs []Message) (param, msg string) {
	for i, m := range msgs {
		if m.MalformedContent {
			return fmt.Sprintf("messages[%d].content", i),
				fmt.Sprintf("messages[%d].content: must be a string or an array of content parts", i)
		}
	}
	return "", ""
}

// ValidateContent checks every image and file part of a request and returns a
// caller-readable message (with the offending parameter) when one cannot be
// sent: not a user turn, not an inline data: URL, bad base64, a media type
// outside the allowlist, bytes that are not that type, over a size, count or
// total limit, or a document with no text beside it. It does not decide
// whether the backend takes images; the registry does that.
//
// It decodes each part once (ContentPart.Bytes caches), so a translator that
// runs afterwards does not decode again. Messages name the index, media type
// and limit, never a filename or any bytes.
func ValidateContent(msgs []Message) (param, msg string) {
	var images, docs, total int
	for i := range msgs {
		m := &msgs[i]
		firstDoc := -1
		for j := range m.Parts {
			p := &m.Parts[j]
			if p.Kind == KindText {
				continue
			}
			at := fmt.Sprintf("messages[%d].content[%d]", i, j)
			refuse := func(format string, args ...any) (string, string) {
				return at, at + ": " + fmt.Sprintf(format, args...)
			}
			if m.Role != "user" {
				return refuse("images and files are accepted only in user messages")
			}
			if p.Kind == KindUnknown {
				if p.Remote {
					return refuse("content by URL or file id is not accepted; send the bytes inline as a data: URL")
				}
				return refuse("this content part type is not supported")
			}
			if p.MediaType == "" {
				return refuse("malformed data URL; expected data:<media type>;base64,<data>")
			}
			mt := safeMediaType(p.MediaType)
			if p.MediaType == "image/svg+xml" {
				return refuse("SVG images are not accepted")
			}
			spec, allowed := mediaTypes[p.MediaType]
			if !allowed || spec.kind != p.Kind {
				return refuse("media type %s is not accepted; images: png, jpeg, gif, webp; files: pdf, csv, doc, docx, xls, xlsx, html, txt, md", mt)
			}

			limit, maxCount, count, noun := MaxImageBytes, MaxImagesPerRequest, &images, "images"
			if p.Kind == KindFile {
				limit, maxCount, count, noun = MaxDocumentBytes, MaxDocumentsPerRequest, &docs, "files"
				if firstDoc < 0 {
					firstDoc = j
				}
			}
			*count++
			if *count > maxCount {
				return refuse("at most %d %s per request", maxCount, noun)
			}
			// A part far past its limit is refused without allocating its
			// decoded form; one near it is decoded and measured exactly.
			if base64.StdEncoding.DecodedLen(len(p.Data)) > 2*limit {
				return refuse("%s exceeds the %d-byte limit per part", mt, limit)
			}
			b, err := p.Bytes()
			if err != nil {
				return refuse("%s data is not valid base64", mt)
			}
			if len(b) == 0 {
				return refuse("%s data is empty", mt)
			}
			if len(b) > limit {
				return refuse("%s exceeds the %d-byte limit per part", mt, limit)
			}
			if spec.sniff != nil && !spec.sniff(b) {
				return refuse("content is not a valid %s file", mt)
			}
			if spec.utf8 && !utf8.Valid(b) {
				return refuse("%s content is not valid UTF-8 text", mt)
			}
			total += len(b)
			if total > MaxContentBytesPerRequest {
				return refuse("images and files total more than the %d-byte limit per request", MaxContentBytesPerRequest)
			}
		}
		if firstDoc >= 0 && strings.TrimSpace(m.Content) == "" {
			at := fmt.Sprintf("messages[%d].content[%d]", i, firstDoc)
			return at, at + ": a document needs a text message with it"
		}
	}
	return "", ""
}

// safeMediaType makes a caller-chosen media type safe to echo: at most 96
// bytes of letters, digits and . + - / _.
func safeMediaType(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 96 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '+', r == '-', r == '/', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
