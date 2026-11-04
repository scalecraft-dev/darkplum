// Package libkey builds object-store keys for library items and folders.
//
// A Key is opaque: the only way to get one is ForItem / ForFolder, which take
// values read from Postgres (scope, the owner's library_slug, folder slugs,
// kind, item slug) and refuse anything outside the layout's charset, depth and
// length rules. Nothing a client sent is ever concatenated into a key, and a
// display name is never key material.
//
// Layout (relative to the store prefix, normally "library/v1/"):
//
//	org/<folder>/.../<slug>.<kind>.<ext>
//	people/<person>/<folder>/.../<slug>.<kind>.<ext>
//
// Visibility lives in the first segment because a prefix is what an IAM
// policy or a compliance role's s3:GetObject can be scoped to.
package libkey

import (
	"errors"
	"regexp"
	"strings"
)

type Scope string

const (
	ScopeOrg  Scope = "org"
	ScopeUser Scope = "user"
)

const (
	// MaxDepth is the number of folder segments under a root.
	MaxDepth = 5
	// MaxKeyBytes is deliberately well under S3's 1024.
	MaxKeyBytes = 600
	// MaxSegment bounds a folder or person segment.
	MaxSegment = 64
	// MaxSlug bounds an item slug (the API's existing limit).
	MaxSlug = 100
)

var (
	segmentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	slugRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)
	reserved  = regexp.MustCompile(`^(con|prn|aux|nul|com[1-9]|lpt[1-9])$`)
)

var (
	ErrInvalid = errors.New("libkey: invalid key component")
	ErrTooLong = errors.New("libkey: key too long")
)

// Key is an opaque object key relative to the store prefix. Folder markers
// end in "/".
type Key struct{ s string }

func (k Key) String() string { return k.s }
func (k Key) IsZero() bool   { return k.s == "" }

// ValidSegment reports whether s may be a folder segment (also a person).
func ValidSegment(s string) bool { return segmentRE.MatchString(s) && !reserved.MatchString(s) }

// ValidSlug reports whether s may be an item slug.
func ValidSlug(s string) bool { return slugRE.MatchString(s) && !reserved.MatchString(s) }

// ValidFolderPath checks a slash-joined folder path ("" is the root).
func ValidFolderPath(p string) bool {
	if p == "" {
		return true
	}
	segs := strings.Split(p, "/")
	if len(segs) > MaxDepth {
		return false
	}
	for _, s := range segs {
		if !ValidSegment(s) {
			return false
		}
	}
	return true
}

// Suffix is the file-name suffix for a kind ("" for an unknown kind).
func Suffix(kind string) string {
	switch kind {
	case "skill":
		return ".skill.md"
	case "context":
		return ".context.md"
	case "tool":
		return ".tool.json"
	}
	return ""
}

func root(scope Scope, person string) (string, error) {
	switch scope {
	case ScopeOrg:
		return "org/", nil
	case ScopeUser:
		if !ValidSegment(person) {
			return "", ErrInvalid
		}
		return "people/" + person + "/", nil
	}
	return "", ErrInvalid
}

// ForItem builds the key of an item. person is the owner's users.library_slug
// and is required (and only used) for ScopeUser.
func ForItem(scope Scope, person, folderPath, kind, slug string) (Key, error) {
	r, err := root(scope, person)
	if err != nil {
		return Key{}, err
	}
	if !ValidFolderPath(folderPath) || !ValidSlug(slug) || Suffix(kind) == "" {
		return Key{}, ErrInvalid
	}
	s := r
	if folderPath != "" {
		s += folderPath + "/"
	}
	s += slug + Suffix(kind)
	if len(s) >= MaxKeyBytes {
		return Key{}, ErrTooLong
	}
	return Key{s}, nil
}

// ForFolder builds the zero-byte marker key of a folder ("<folder>/").
func ForFolder(scope Scope, person, folderPath string) (Key, error) {
	r, err := root(scope, person)
	if err != nil {
		return Key{}, err
	}
	if folderPath == "" || !ValidFolderPath(folderPath) {
		return Key{}, ErrInvalid
	}
	s := r + folderPath + "/"
	if len(s) >= MaxKeyBytes {
		return Key{}, ErrTooLong
	}
	return Key{s}, nil
}

// Location is a parsed object key. It exists for Sync, which reads keys from
// the bucket; the values are re-validated, never trusted.
type Location struct {
	Scope      Scope
	Person     string // ScopeUser only
	FolderPath string
	Kind       string
	Slug       string
}

// Parse splits an item key (relative to the store prefix).
func Parse(key string) (Location, error) {
	var loc Location
	if len(key) >= MaxKeyBytes || strings.HasSuffix(key, "/") {
		return loc, ErrInvalid
	}
	segs := strings.Split(key, "/")
	switch {
	case len(segs) >= 2 && segs[0] == "org":
		loc.Scope = ScopeOrg
		segs = segs[1:]
	case len(segs) >= 3 && segs[0] == "people":
		loc.Scope = ScopeUser
		loc.Person = segs[1]
		segs = segs[2:]
	default:
		return loc, ErrInvalid
	}
	file := segs[len(segs)-1]
	folders := segs[:len(segs)-1]
	for _, k := range []string{"skill", "context", "tool"} {
		if suf := Suffix(k); strings.HasSuffix(file, suf) {
			loc.Kind = k
			loc.Slug = strings.TrimSuffix(file, suf)
			break
		}
	}
	loc.FolderPath = strings.Join(folders, "/")
	if loc.Kind == "" {
		return loc, ErrInvalid
	}
	if _, err := ForItem(loc.Scope, loc.Person, loc.FolderPath, loc.Kind, loc.Slug); err != nil {
		return loc, ErrInvalid
	}
	return loc, nil
}

// Class is the access-boundary prefix a key lives under: "org/" or
// "people/<person>/". A move between classes is a share or unshare (or a
// re-keyed person), where the old object's versions are purged, not kept.
func Class(key string) string {
	if strings.HasPrefix(key, "people/") {
		parts := strings.SplitN(key, "/", 3)
		if len(parts) >= 2 {
			return "people/" + parts[1] + "/"
		}
	}
	if strings.HasPrefix(key, "org/") {
		return "org/"
	}
	return ""
}
