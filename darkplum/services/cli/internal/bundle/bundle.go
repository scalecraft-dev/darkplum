// Package bundle is the Terraform and Kubernetes tree that ships inside the CLI
// binary: hashing it, extracting it atomically into an install's workspace, and
// proving later that what is on disk is still exactly what shipped.
//
// The point of the hash is the trust boundary. The binary was verified when it
// was installed; the files Terraform and kubectl read are copies written to the
// customer's disk. Verify, run before every use, makes an edited or half-written
// copy a refusal instead of an apply.
package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	darkplum "github.com/scalecraft-dev/darkplum"
)

// Bundle is a filtered tree plus its content hash.
type Bundle struct {
	fsys    fs.FS
	entries []Entry
	hash    string
}

// Entry is one shipped file.
type Entry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Embedded is the bundle compiled into this binary.
func Embedded() (*Bundle, error) { return New(darkplum.Tree()) }

// New hashes fsys after dropping what must never ship (darkplum.Denied) and what
// is only for developing the bundle (module test files). Entries are sorted by
// path, so the hash does not depend on directory order.
func New(fsys fs.FS) (*Bundle, error) {
	fsys = darkplum.Filter(fsys)
	var entries []Entry
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if devOnly(p) {
				return fs.SkipDir
			}
			return nil
		}
		if devOnly(p) {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		entries = append(entries, Entry{Path: p, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("bundle is empty")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return &Bundle{fsys: fsys, entries: entries, hash: treeHash(entries)}, nil
}

// devOnly is test material that is embedded because directory patterns take
// everything, but is never extracted and never hashed.
func devOnly(p string) bool {
	if strings.HasSuffix(p, ".tftest.hcl") || strings.HasSuffix(p, ".tfmock.hcl") {
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "tests" {
			return true
		}
	}
	return false
}

func treeHash(entries []Entry) string {
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%s\n", e.Path, e.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Hash is the hex SHA-256 of the sorted (path, file hash) list.
func (b *Bundle) Hash() string { return b.hash }

// Entries is the file manifest, for `version` and the release-time allowlist check.
func (b *Bundle) Entries() []Entry { return append([]Entry(nil), b.entries...) }

// hashFile is what Extract leaves next to the tree: the expected hash, so a later
// Verify needs nothing but the directory.
const hashFile = ".bundle-sha256"

// Extract writes the bundle to dest, which must not exist yet or must already
// hold exactly this bundle. It writes a sibling temporary directory and renames
// it into place, so a crash or Ctrl-C never leaves a half-written dest that a
// later run would trust.
func (b *Bundle) Extract(dest string) error {
	if _, err := os.Stat(dest); err == nil {
		if err := Verify(dest, b.hash); err != nil {
			return fmt.Errorf("%s exists and is not this bundle: %w", dest, err)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".extract-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // no-op after a successful rename
	for _, e := range b.entries {
		data, err := fs.ReadFile(b.fsys, e.Path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != e.SHA256 {
			return fmt.Errorf("%s changed while extracting", e.Path)
		}
		target := filepath.Join(tmp, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		// Read-only: nothing in an extracted bundle is meant to be edited, and
		// Verify catches it if it is.
		if err := os.WriteFile(target, data, 0o400); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, hashFile), []byte(b.hash+"\n"), 0o400); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		// Another process won the race with the same bundle: fine, verify it.
		if verr := Verify(dest, b.hash); verr == nil {
			return nil
		}
		return err
	}
	return nil
}

// Verify re-hashes dir and compares it with want. Any difference is an error: a
// changed file, a missing file, and also an extra one (a stray .terraform, a
// backend.tf, a tfvars someone dropped in). Terraform and kubectl never run from
// a tree that does not verify.
func Verify(dir, want string) error {
	var entries []Entry
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == hashFile {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link", rel)
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		entries = append(entries, Entry{Path: path.Clean(rel), Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if got := treeHash(entries); got != want {
		return fmt.Errorf("bundle on disk does not match the one shipped in this binary (hash %s, expected %s)", short(got), short(want))
	}
	stored, err := os.ReadFile(filepath.Join(dir, hashFile))
	if err != nil {
		return fmt.Errorf("bundle marker missing: %w", err)
	}
	if !bytes.Equal(bytes.TrimSpace(stored), []byte(want)) {
		return errors.New("bundle marker does not match the shipped bundle")
	}
	return nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
