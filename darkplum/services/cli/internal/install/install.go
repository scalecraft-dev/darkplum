// Package install is the per-install workspace: everything one Darkplum install
// keeps on the operator's machine, under ~/.darkplum/installs/<name>/, and nothing
// anywhere else. The repository checkout is never written to, so a `git pull`,
// a second checkout or a cleaned directory cannot lose or mix up an install.
//
//	~/.darkplum/
//	  plugin-cache/                 shared Terraform provider cache
//	  installs/<name>/
//	    install.json                what this install is (Record), 0600
//	    terraform.tfvars            its settings, 0600
//	    backend.hcl                 -backend-config file for S3 state, 0600
//	    bundle/<version>/           the verified Terraform + Kubernetes tree, read-only
//	    tfdata/                     TF_DATA_DIR: terraform's own cache, outside the bundle
//	    kubeconfig                  private kubeconfig for this install's cluster, 0600
//	    logs/                       one log per command run
package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/bundle"
)

// ErrNoInstall means there is nothing to act on.
var ErrNoInstall = errors.New("no install")

// Home is the root of all workspaces, normally ~/.darkplum.
type Home string

// DefaultHome is $DARKPLUM_HOME when set (tests, CI and containers), otherwise
// ~/.darkplum. The variable moves the whole home; it never selects an install.
func DefaultHome() (Home, error) {
	if h := os.Getenv("DARKPLUM_HOME"); h != "" {
		if !filepath.IsAbs(h) {
			return "", fmt.Errorf("DARKPLUM_HOME must be an absolute path, got %q", h)
		}
		return Home(h), nil
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find your home directory: %w", err)
	}
	return Home(filepath.Join(u, ".darkplum")), nil
}

// PluginCache is shared by every install: provider binaries are large and
// verified against the lock file on every init anyway.
func (h Home) PluginCache() string { return filepath.Join(string(h), "plugin-cache") }

func (h Home) installs() string { return filepath.Join(string(h), "installs") }

// Same shape as the cluster_name variable in terraform/aws/variables.tf: an
// install is named after its cluster, and that name becomes a directory.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$`)

// ValidName rejects anything that is not a plain cluster-style name, so a name
// can never be a path ("../x", "a/b", ".") or collide with the workspace's own files.
func ValidName(name string) error {
	if !nameRe.MatchString(name) || strings.Contains(name, "--") {
		return fmt.Errorf("install name %q must be 2-40 characters of lowercase letters, digits and single hyphens, starting with a letter and not ending with a hyphen", name)
	}
	return nil
}

// Install is one workspace. Use Open or Resolve; the zero value is not usable.
type Install struct {
	home Home
	Name string
	// dir, when set, is the workspace directory itself (OpenDir): for tests and
	// for setups that keep the workspace somewhere other than under the home.
	dir string
}

// Open returns the workspace for name without creating anything.
func Open(h Home, name string) (Install, error) {
	if err := ValidName(name); err != nil {
		return Install{}, err
	}
	return Install{home: h, Name: name}, nil
}

// OpenDir returns the workspace for name living at dir (an absolute path), not
// under the home.
func OpenDir(dir, name string) (Install, error) {
	if err := ValidName(name); err != nil {
		return Install{}, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Install{}, err
	}
	return Install{Name: name, dir: abs}, nil
}

// List returns the names of every install in the home, sorted.
func List(h Home) ([]string, error) {
	entries, err := os.ReadDir(h.installs())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && ValidName(e.Name()) == nil {
			if _, err := os.Stat(filepath.Join(h.installs(), e.Name(), recordFile)); err == nil {
				names = append(names, e.Name())
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// Resolve picks the install a command acts on. An explicit name must exist. With
// none given there must be exactly one install: the answer is never taken from
// the environment, the current directory or "the last one used", because acting
// on the wrong install is the failure this tool exists to prevent.
func Resolve(h Home, flag string) (Install, error) {
	names, err := List(h)
	if err != nil {
		return Install{}, err
	}
	if flag != "" {
		in, err := Open(h, flag)
		if err != nil {
			return Install{}, err
		}
		for _, n := range names {
			if n == flag {
				return in, nil
			}
		}
		return Install{}, fmt.Errorf("%w named %q (installs: %s)", ErrNoInstall, flag, listOrNone(names))
	}
	switch len(names) {
	case 0:
		return Install{}, fmt.Errorf("%w: run `darkplum init` first", ErrNoInstall)
	case 1:
		return Open(h, names[0])
	default:
		return Install{}, fmt.Errorf("several installs (%s): choose one with --install NAME", strings.Join(names, ", "))
	}
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// Paths. Nothing here touches the disk.

func (i Install) Dir() string {
	if i.dir != "" {
		return i.dir
	}
	return filepath.Join(i.home.installs(), i.Name)
}
func (i Install) RecordPath() string { return filepath.Join(i.Dir(), recordFile) }
func (i Install) TFVars() string     { return filepath.Join(i.Dir(), "terraform.tfvars") }
func (i Install) BackendHCL() string { return filepath.Join(i.Dir(), "backend.hcl") }

func (i Install) Kubeconfig() string { return filepath.Join(i.Dir(), "kubeconfig") }
func (i Install) LogsDir() string    { return filepath.Join(i.Dir(), "logs") }
func (i Install) PluginCache() string {
	return i.home.PluginCache()
}

// BundleDir is where one build of the shipped tree is extracted. key is
// bundle.Key(version, hash): the release version and the content hash, so two
// builds never share a directory.
func (i Install) BundleDir(key string) string { return filepath.Join(i.Dir(), "bundle", key) }

// TFData is Terraform's data directory (TF_DATA_DIR) for one bundle key. It lives
// beside the bundle, never inside it: terraform init writes there, and the
// bundle is verified read-only.
func (i Install) TFData(key string) string { return filepath.Join(i.Dir(), "tfdata", key) }

const recordFile = "install.json"

// Record is what an install is: enough to know which cluster, account and
// state it means before any command touches AWS.
type Record struct {
	Schema      int       `json:"schema"`
	Name        string    `json:"name"`
	ClusterName string    `json:"cluster_name"`
	Region      string    `json:"region"`
	Profile     string    `json:"profile,omitempty"`
	AccountIDs  []string  `json:"account_ids"`
	Backend     Backend   `json:"backend"`
	Created     time.Time `json:"created"`
	// AdoptedFrom is the Terraform root of a Makefile-era install this one took
	// over; empty for a new install.
	AdoptedFrom string `json:"adopted_from,omitempty"`
	// Bundle is the version of the shipped tree the install was last applied
	// with (empty until the first deploy).
	Bundle string `json:"bundle,omitempty"`
	// CreatedZoneID is the Route 53 hosted zone `darkplum init` itself created
	// (the domain's DNS is elsewhere). Only a zone recorded here is reported by
	// destroy as Darkplum's to mention; a zone the customer supplied is theirs.
	CreatedZoneID string `json:"created_zone_id,omitempty"`
	// SemanticRouter is whether the install runs the optional in-cluster intent
	// classifier. It is a setting of the install, not of Terraform: `darkplum
	// deploy` applies the router when it is true and removes it when it is false.
	// Off by default, as `make deploy-semantic-router` was opt-in.
	SemanticRouter bool `json:"semantic_router,omitempty"`
}

// Backend is where Terraform keeps this install's state.
type Backend struct {
	Type        string `json:"type"` // "s3"
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	Region      string `json:"region"`
	KMSKeyID    string `json:"kms_key_id,omitempty"`
	Encrypt     bool   `json:"encrypt"`
	UseLockfile bool   `json:"use_lockfile"`
}

const recordSchema = 1

// Validate is what makes a record trustworthy to act on.
func (r Record) Validate() error {
	if r.Schema != recordSchema {
		return fmt.Errorf("install record has schema %d, this darkplum understands %d", r.Schema, recordSchema)
	}
	if err := ValidName(r.Name); err != nil {
		return err
	}
	if ValidName(r.ClusterName) != nil {
		return fmt.Errorf("install record has an invalid cluster name %q", r.ClusterName)
	}
	if r.Region == "" {
		return errors.New("install record has no region")
	}
	if len(r.AccountIDs) == 0 {
		return errors.New("install record pins no AWS account")
	}
	for _, a := range r.AccountIDs {
		if !regexp.MustCompile(`^[0-9]{12}$`).MatchString(a) {
			return fmt.Errorf("install record has an invalid account id %q", a)
		}
	}
	if r.Backend.Type != "s3" {
		return fmt.Errorf("install record has unsupported backend %q", r.Backend.Type)
	}
	if r.Backend.Bucket == "" || r.Backend.Key == "" || r.Backend.Region == "" {
		return errors.New("install record has an incomplete S3 backend")
	}
	return nil
}

// Load reads and validates the record. A missing install is ErrNoInstall.
func (i Install) Load() (Record, error) {
	b, err := os.ReadFile(i.RecordPath())
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, fmt.Errorf("%w named %q", ErrNoInstall, i.Name)
	}
	if err != nil {
		return Record{}, err
	}
	var r Record
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Record{}, fmt.Errorf("%s is unreadable: %w", i.RecordPath(), err)
	}
	if r.Name != i.Name {
		return Record{}, fmt.Errorf("%s names install %q, not %q", i.RecordPath(), r.Name, i.Name)
	}
	if err := r.Validate(); err != nil {
		return Record{}, err
	}
	return r, nil
}

// Save validates and writes the record atomically (0600; directory 0700).
func (i Install) Save(r Record) error {
	if r.Schema == 0 {
		r.Schema = recordSchema
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Name != i.Name {
		return fmt.Errorf("record is for %q, workspace is %q", r.Name, i.Name)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return i.WriteFile(recordFile, append(b, '\n'))
}

// WriteFile atomically writes a 0600 file directly inside the workspace.
func (i Install) WriteFile(name string, data []byte) error {
	if name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("workspace file name %q must not contain a path", name)
	}
	return writeAtomic(filepath.Join(i.Dir(), name), data, 0o600)
}

// UseBundle returns b extracted for version and verified, ready to hand to
// runner.Terraform. A directory that is missing is extracted. One that exists but
// does not verify (edited, truncated, something dropped into it) is moved aside to
// <key>.rejected-<time>, never trusted and never deleted, and the bundle is
// extracted afresh; note says so, for the operator.
func (i Install) UseBundle(b *bundle.Bundle, version string, now func() time.Time) (v bundle.Verified, note string, err error) {
	if version == "" {
		return bundle.Verified{}, "", errors.New("bundle version is empty")
	}
	if err := os.MkdirAll(i.Dir(), 0o700); err != nil {
		return bundle.Verified{}, "", err
	}
	key := bundle.Key(version, b.Hash())
	dir := i.BundleDir(key)
	if _, statErr := os.Lstat(dir); statErr == nil {
		if v, err := bundle.Verify(dir, b.Hash()); err == nil {
			return v, "", nil
		}
		rejected := dir + ".rejected-" + now().UTC().Format("20060102T150405Z")
		if err := os.Rename(dir, rejected); err != nil {
			return bundle.Verified{}, "", fmt.Errorf("the extracted bundle %s does not match this binary and cannot be moved aside: %w", dir, err)
		}
		note = "the extracted bundle did not match this binary; moved it to " + rejected + " and extracted a fresh copy"
	}
	if err := b.Extract(dir); err != nil {
		return bundle.Verified{}, note, err
	}
	v, err = bundle.Verify(dir, b.Hash())
	return v, note, err
}

// PruneBundles removes every extracted bundle except those whose key is named
// (callers keep the current and the previous), leaving rejected copies alone.
func (i Install) PruneBundles(keepKeys ...string) error {
	root := filepath.Join(i.Dir(), "bundle")
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, k := range keepKeys {
		want[k] = true
	}
	for _, e := range entries {
		if want[e.Name()] || strings.Contains(e.Name(), ".rejected-") {
			continue
		}
		p := filepath.Join(root, e.Name())
		// Extracted trees are read-only; make them removable.
		_ = filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(q, 0o700)
			}
			return nil
		})
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op after the rename
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
