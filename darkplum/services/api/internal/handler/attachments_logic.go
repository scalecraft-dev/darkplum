package handler

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Attachment limits. They mirror the gateway's and Bedrock's (one image
// <= 3.75 MB, one document <= 4.5 MB, 20 images + 5 documents, 10 MiB of
// decoded bytes per turn) and migration 030's CHECKs; the count and the
// per-turn total are enforced only here.
const (
	attachmentKindImage = "image"
	attachmentKindFile  = "file"

	attachmentImageMaxBytes = 3_750_000
	attachmentFileMaxBytes  = 4_500_000
	attachmentTurnMaxBytes  = 10 << 20
	attachmentMaxImages     = 20
	attachmentMaxFiles      = 5
	attachmentMaxTotal      = 25
	attachmentNameMaxLen    = 255

	// attachmentFetchFilenameFallback names a download whose stored name
	// sanitises to nothing.
	attachmentFetchFilenameFallback = "attachment"
)

// attachmentInput is one attachment in a create/append request body. Data is
// standard base64 of the raw bytes (not a data: URL).
type attachmentInput struct {
	Kind      string `json:"kind"`
	MediaType string `json:"media_type"`
	Name      string `json:"name"`
	Data      string `json:"data"`
}

// decodedAttachment is a validated attachment ready to store.
type decodedAttachment struct {
	kind      string
	mediaType string
	name      string
	data      []byte
}

// attachmentMeta is what the API ever returns about a stored attachment
// outside the fetch route: never the bytes.
type attachmentMeta struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	MediaType string `json:"media_type"`
	Name      string `json:"name"`
	SizeBytes int    `json:"size_bytes"`
}

// attachmentTypes is the media-type allowlist: kind and the content check a
// body must pass. SVG, and anything else not listed, is refused.
var attachmentTypes = map[string]struct {
	kind  string
	sniff func([]byte) bool
}{
	"image/png":  {attachmentKindImage, func(b []byte) bool { return bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) }},
	"image/jpeg": {attachmentKindImage, func(b []byte) bool { return bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}) }},
	"image/gif": {attachmentKindImage, func(b []byte) bool {
		return bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a"))
	}},
	"image/webp": {attachmentKindImage, func(b []byte) bool {
		return len(b) >= 12 && bytes.Equal(b[0:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP"))
	}},
	"application/pdf": {attachmentKindFile, func(b []byte) bool { return bytes.HasPrefix(b, []byte("%PDF-")) }},
	"text/csv":        {attachmentKindFile, validTextBytes},
	"text/plain":      {attachmentKindFile, validTextBytes},
	"text/markdown":   {attachmentKindFile, validTextBytes},
	"text/html":       {attachmentKindFile, validTextBytes},
	"application/msword": {attachmentKindFile, func(b []byte) bool {
		return bytes.HasPrefix(b, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1})
	}},
	"application/vnd.ms-excel": {attachmentKindFile, func(b []byte) bool {
		return bytes.HasPrefix(b, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1})
	}},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": {attachmentKindFile, isZip},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       {attachmentKindFile, isZip},
}

func isZip(b []byte) bool { return bytes.HasPrefix(b, []byte("PK\x03\x04")) }

// validTextBytes accepts UTF-8 text without NUL bytes (which Postgres text
// and every text consumer downstream would choke on, and which mark binary).
func validTextBytes(b []byte) bool {
	return utf8.Valid(b) && bytes.IndexByte(b, 0) < 0
}

// inlineImageType reports whether a media type is one of the four raster
// image types served inline by the fetch route.
func inlineImageType(mt string) bool {
	switch mt {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// decodeAttachments validates a message's attachments and returns them
// decoded. Errors name the index, the media type and the limit; never a
// filename or any content.
func decodeAttachments(m messageInput) ([]decodedAttachment, error) {
	if len(m.Attachments) == 0 {
		return nil, nil
	}
	if m.Role != roleUser {
		return nil, fmt.Errorf("attachments are only allowed on a user turn")
	}
	if len(m.Attachments) > attachmentMaxTotal {
		return nil, fmt.Errorf("at most %d attachments per turn", attachmentMaxTotal)
	}
	out := make([]decodedAttachment, 0, len(m.Attachments))
	var images, files, total int
	for i, a := range m.Attachments {
		spec, ok := attachmentTypes[a.MediaType]
		if !ok {
			return nil, fmt.Errorf("attachments[%d]: media_type %q is not supported", i, truncateForError(a.MediaType))
		}
		if a.Kind != spec.kind {
			return nil, fmt.Errorf("attachments[%d]: kind must be %q for %s", i, spec.kind, a.MediaType)
		}
		if err := validAttachmentName(a.Name); err != nil {
			return nil, fmt.Errorf("attachments[%d]: %w", i, err)
		}
		limit := attachmentFileMaxBytes
		if spec.kind == attachmentKindImage {
			limit = attachmentImageMaxBytes
			images++
		} else {
			files++
		}
		// Cheap bound before decoding: never allocate for an oversize item.
		if base64.StdEncoding.DecodedLen(len(a.Data)) > limit+2 {
			return nil, fmt.Errorf("attachments[%d]: %s exceeds %d bytes", i, a.MediaType, limit)
		}
		data, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return nil, fmt.Errorf("attachments[%d]: data is not valid base64", i)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("attachments[%d]: %s is empty", i, a.MediaType)
		}
		if len(data) > limit {
			return nil, fmt.Errorf("attachments[%d]: %s exceeds %d bytes", i, a.MediaType, limit)
		}
		if !spec.sniff(data) {
			return nil, fmt.Errorf("attachments[%d]: content does not match media_type %s", i, a.MediaType)
		}
		total += len(data)
		if total > attachmentTurnMaxBytes {
			return nil, fmt.Errorf("attachments exceed %d bytes in total per turn", attachmentTurnMaxBytes)
		}
		out = append(out, decodedAttachment{kind: a.Kind, mediaType: a.MediaType, name: a.Name, data: data})
	}
	if images > attachmentMaxImages {
		return nil, fmt.Errorf("at most %d images per turn", attachmentMaxImages)
	}
	if files > attachmentMaxFiles {
		return nil, fmt.Errorf("at most %d documents per turn", attachmentMaxFiles)
	}
	return out, nil
}

func validAttachmentName(name string) error {
	if !utf8.ValidString(name) {
		return fmt.Errorf("name must be valid UTF-8")
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > attachmentNameMaxLen {
		return fmt.Errorf("name must be 1-%d characters", attachmentNameMaxLen)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("name must not contain control characters")
		}
	}
	return nil
}

// truncateForError keeps a client-supplied value short in an error message.
func truncateForError(s string) string {
	if len(s) > 80 {
		return s[:80]
	}
	return s
}

// attachmentDisposition builds the Content-Disposition for a fetch: inline
// for the four raster image types, attachment for everything else, with a
// sanitised ASCII filename plus the RFC 5987 UTF-8 form.
func attachmentDisposition(mediaType, name string) string {
	if inlineImageType(mediaType) {
		return "inline"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == ' ', r == '(', r == ')':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	ascii := strings.Trim(b.String(), " .")
	if ascii == "" {
		ascii = attachmentFetchFilenameFallback
	}
	out := `attachment; filename="` + ascii + `"`
	if !utf8.ValidString(name) || name == "" {
		return out
	}
	// Control characters never get here (validAttachmentName), but be safe.
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '_'
		}
		return r
	}, name)
	// PathEscape leaves = : @ alone; RFC 5987 attr-char does not allow them.
	enc := strings.NewReplacer("=", "%3D", ":", "%3A", "@", "%40").Replace(url.PathEscape(clean))
	return out + "; filename*=UTF-8''" + enc
}
