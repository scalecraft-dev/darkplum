package provider

import "bytes"

// Limits and the media-type allowlist for image and file content parts, in
// one place so a limit is changed once and the tests, the validator and the
// Bedrock translator cannot disagree.
//
// The per-part and per-request numbers are Bedrock Converse's documented
// limits for image and document blocks (3.75 MB per image, 20 images; 4.5 MB
// per document, 5 documents). They are enforced here, before dispatch, so a
// caller gets a named refusal instead of a vendor ValidationException. The
// 10 MiB total is the gateway's own: the 16 MiB body cap applies to the whole
// request including re-sent history, and base64 inflates by a third.

const (
	// MaxImageBytes is the largest decoded image part.
	MaxImageBytes = 3_750_000
	// MaxDocumentBytes is the largest decoded document part.
	MaxDocumentBytes = 4_500_000
	// MaxImagesPerRequest and MaxDocumentsPerRequest bound the parts of one
	// kind across every message of a request.
	MaxImagesPerRequest    = 20
	MaxDocumentsPerRequest = 5
	// MaxContentBytesPerRequest bounds the decoded bytes of every image and
	// document part of a request together.
	MaxContentBytesPerRequest = 10 << 20
)

// Part kinds.
const (
	KindText    = "text"
	KindImage   = "image"
	KindFile    = "file"
	KindUnknown = "unknown"
)

// mediaSpec describes one accepted media type.
type mediaSpec struct {
	kind   string // KindImage or KindFile
	format string // the Converse format name
	// sniff checks the leading bytes; nil means no check beyond the type
	// allowlist. utf8 marks a text type whose bytes must be valid UTF-8.
	sniff func(b []byte) bool
	utf8  bool
}

// mediaTypes is the allowlist. SVG is deliberately absent: it is a script
// carrier, and no backend takes it as an image.
var mediaTypes = map[string]mediaSpec{
	"image/png":  {kind: KindImage, format: "png", sniff: sniffPNG},
	"image/jpeg": {kind: KindImage, format: "jpeg", sniff: sniffJPEG},
	"image/gif":  {kind: KindImage, format: "gif", sniff: sniffGIF},
	"image/webp": {kind: KindImage, format: "webp", sniff: sniffWebP},

	"application/pdf":    {kind: KindFile, format: "pdf", sniff: sniffPDF},
	"text/csv":           {kind: KindFile, format: "csv", utf8: true},
	"text/html":          {kind: KindFile, format: "html", sniff: sniffHTML, utf8: true},
	"text/plain":         {kind: KindFile, format: "txt", utf8: true},
	"text/markdown":      {kind: KindFile, format: "md", utf8: true},
	"application/msword": {kind: KindFile, format: "doc", sniff: sniffOLE},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": {kind: KindFile, format: "docx", sniff: sniffDOCX},
	"application/vnd.ms-excel": {kind: KindFile, format: "xls", sniff: sniffOLE},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": {kind: KindFile, format: "xlsx", sniff: sniffXLSX},
}

// ContentFormat returns the Converse format name ("png", "pdf", ...) for an
// allowed media type, or "" for one that is not allowed.
func ContentFormat(mediaType string) string { return mediaTypes[mediaType].format }

func sniffPNG(b []byte) bool {
	return len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n"
}

func sniffJPEG(b []byte) bool {
	return len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF
}

func sniffGIF(b []byte) bool {
	return len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a")
}

func sniffWebP(b []byte) bool {
	return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP"
}

func sniffPDF(b []byte) bool {
	return len(b) >= 5 && string(b[:5]) == "%PDF-"
}

// sniffOLE matches the OLE compound-file header of legacy .doc and .xls.
func sniffOLE(b []byte) bool {
	return len(b) >= 4 && b[0] == 0xD0 && b[1] == 0xCF && b[2] == 0x11 && b[3] == 0xE0
}

func sniffZip(b []byte) bool {
	return len(b) >= 4 && string(b[:4]) == "PK\x03\x04"
}

// ooxmlScanBytes bounds how much of a file is searched for the package's
// part names: local file headers carry the names inline, and Office writes
// [Content_Types].xml first.
const ooxmlScanBytes = 8192

// sniffDOCX and sniffXLSX require the zip signature and, within the first
// bytes, the part name of the right package kind. A plain zip, or an xlsx
// sent as docx, is refused.
func sniffDOCX(b []byte) bool { return sniffOOXML(b, "word/") }
func sniffXLSX(b []byte) bool { return sniffOOXML(b, "xl/") }

func sniffOOXML(b []byte, dir string) bool {
	if !sniffZip(b) {
		return false
	}
	head := b[:min(len(b), ooxmlScanBytes)]
	return bytes.Contains(head, []byte(dir)) || bytes.Contains(head, []byte("[Content_Types].xml"))
}

// sniffHTML: after an optional UTF-8 BOM and leading whitespace, a '<' within
// the first 512 bytes. UTF-8 validity is checked separately (spec.utf8).
func sniffHTML(b []byte) bool {
	b = bytes.TrimSpace(bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF")))
	return bytes.IndexByte(b[:min(len(b), 512)], '<') >= 0
}
