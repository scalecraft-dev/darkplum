// Package darkplum embeds the Terraform and Kubernetes bundle that the darkplum
// CLI carries inside its binary.
//
// It lives at the darkplum/ root because go:embed patterns cannot reach upward.
// The services (api, gateway, cli) are separate Go modules and are therefore
// outside this one: nothing under services/ can be embedded by accident.
//
// The patterns are explicit, never `all:`. Three things still need the filter in
// Tree: `terraform/aws/*.tf` matches a backend.tf that `darkplum init` used to
// write next to the code; directory patterns (modules, k8s) embed any non-dot file
// in them, including a stray *.tfvars, *.tfstate* or *.bak; and a developer's
// working tree has all of those. Dot-directories (.terraform, .pre-init,
// .darkplum) are skipped by go:embed itself. TestEmbedHygiene pins all of it.
package darkplum

import (
	"embed"
	"io/fs"
	"path"
	"strings"
)

//go:embed terraform/aws/*.tf terraform/aws/.terraform.lock.hcl terraform/aws/modules k8s
var raw embed.FS

// Denied reports whether a bundle-relative path must never be part of the
// embedded tree: local configuration, state, plans, backups and tool caches.
func Denied(p string) bool {
	base := path.Base(p)
	switch {
	case base == "backend.tf", base == "tfplan",
		strings.HasSuffix(base, ".tfvars"), strings.HasSuffix(base, ".tfvars.json"),
		strings.Contains(base, ".tfstate"), strings.HasSuffix(base, ".bak"),
		strings.HasSuffix(base, ".tfplan"):
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case ".terraform", ".pre-init", ".darkplum", ".git":
			return true
		}
	}
	return false
}

// Tree is the embedded bundle with everything Denied removed: terraform/aws/**
// and k8s/**.
func Tree() fs.FS { return Filter(raw) }

// Filter hides Denied paths from fsys.
func Filter(fsys fs.FS) fs.FS { return filtered{fsys} }

// filtered hides denied paths from Open and ReadDir, so fs.WalkDir, fs.ReadFile
// and fs.Sub all see the same clean tree.
type filtered struct{ fs.FS }

func (f filtered) Open(name string) (fs.File, error) {
	if Denied(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return f.FS.Open(name)
}

func (f filtered) ReadDir(name string) ([]fs.DirEntry, error) {
	if Denied(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	entries, err := fs.ReadDir(f.FS, name)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, 0, len(entries))
	for _, e := range entries {
		if !Denied(path.Join(name, e.Name())) {
			out = append(out, e)
		}
	}
	return out, nil
}

// Unfiltered is the raw embed, for the test that proves Tree's filter is load-bearing.
func Unfiltered() fs.FS { return raw }
