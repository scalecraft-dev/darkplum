// Package release is the manifest baked into a released CLI and the identity it
// implies.
//
// release.yml and prerelease.yml build the binary with
//
//	-X github.com/scalecraft-dev/darkplum/cli/internal/release.encoded=<base64 JSON>
//
// where the JSON comes from .github/scripts/release-json.py. The binary therefore
// knows which images it deploys (by digest), which repository built them, and
// exactly which workflow run is allowed to have signed them. A build without the
// manifest is a dev build: channel "dev", and it can verify nothing.
//
// Validation here mirrors the script's, rule for rule; a parity test feeds both
// the same golden manifest.
package release

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// encoded is set by the linker; see the package comment.
var encoded string

const (
	// Channels. A binary and the images it verifies are on the same channel.
	Release = "release"
	RC      = "rc"
	Dev     = "dev"

	// Issuer is the OIDC issuer of GitHub Actions' keyless certificates.
	Issuer = "https://token.actions.githubusercontent.com"

	buildSignWorkflow = ".github/workflows/_build-sign.yml"
	registry          = "ghcr.io"
	owner             = "scalecraft-dev"
)

// Services are the first-party images, in display order.
var Services = []string{"gateway", "api", "web"}

// Image is one released image.
type Image struct {
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
}

// Ref is repository@digest, the only way the CLI ever names an image.
func (i Image) Ref() string { return i.Repository + "@" + i.Digest }

// Manifest is release.json.
type Manifest struct {
	Schema     int              `json:"schema"`
	Channel    string           `json:"channel"`
	Version    string           `json:"version"`
	Commit     string           `json:"commit"`
	Repository string           `json:"repository"`
	Ref        string           `json:"ref"`
	Workflow   string           `json:"workflow"`
	RunID      string           `json:"run_id"`
	RunAttempt string           `json:"run_attempt"`
	Images     map[string]Image `json:"images"`
}

var (
	digestRe  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitRe  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	numRe     = regexp.MustCompile(`^[0-9]+$`)
	repoRe    = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	releaseRe = regexp.MustCompile(`^refs/tags/v([0-9]+\.[0-9]+\.[0-9]+)$`)
	rcRefRe   = regexp.MustCompile(`^refs/heads/prerelease/[A-Za-z0-9._/-]+$`)
	relVerRe  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	rcVerRe   = regexp.MustCompile(`^0\.0\.0-rc\.[0-9]+\+[0-9a-f]{7}$`)
)

// Current is the manifest of this binary. A binary built without one is a dev
// build: Channel "dev", no images, no identity.
func Current() (Manifest, error) {
	if encoded == "" {
		return Manifest{Schema: 1, Channel: Dev, Version: "dev"}, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return Manifest{}, fmt.Errorf("release manifest is not base64: %w", err)
	}
	return Parse(raw)
}

// Parse decodes and validates a manifest. Unknown fields, a channel/ref/workflow
// mismatch, a missing or extra image, or a malformed digest is an error: a
// binary with a bad manifest must not deploy anything.
func Parse(raw []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("release manifest: %w", err)
	}
	if dec.More() {
		return Manifest{}, errors.New("release manifest: trailing data")
	}
	return m, m.Validate()
}

// Validate applies the same rules as .github/scripts/release-json.py.
func (m Manifest) Validate() error {
	if m.Schema != 1 {
		return fmt.Errorf("release manifest has schema %d, this darkplum understands 1", m.Schema)
	}
	var workflow, prefix string
	switch m.Channel {
	case Release:
		workflow, prefix = "release.yml", "darkplum"
		sub := releaseRe.FindStringSubmatch(m.Ref)
		if !relVerRe.MatchString(m.Version) || sub == nil || sub[1] != m.Version {
			return fmt.Errorf("release manifest: version %q does not match ref %q for the release channel", m.Version, m.Ref)
		}
	case RC:
		workflow, prefix = "prerelease.yml", "darkplum-rc"
		if !rcVerRe.MatchString(m.Version) || !rcRefRe.MatchString(m.Ref) {
			return fmt.Errorf("release manifest: version %q / ref %q are not a release candidate", m.Version, m.Ref)
		}
	default:
		return fmt.Errorf("release manifest: channel %q is not release or rc", m.Channel)
	}
	if m.Workflow != ".github/workflows/"+workflow {
		return fmt.Errorf("release manifest: workflow %q does not belong to the %s channel", m.Workflow, m.Channel)
	}
	if !commitRe.MatchString(m.Commit) {
		return fmt.Errorf("release manifest: commit %q is not a 40-hex sha", m.Commit)
	}
	if !numRe.MatchString(m.RunID) || !numRe.MatchString(m.RunAttempt) {
		return errors.New("release manifest: run id and attempt must be numeric")
	}
	if !repoRe.MatchString(m.Repository) || !strings.HasPrefix(m.Repository, owner+"/") {
		return fmt.Errorf("release manifest: repository %q is not in %s", m.Repository, owner)
	}
	var have []string
	for k := range m.Images {
		have = append(have, k)
	}
	sort.Strings(have)
	if strings.Join(have, ",") != "api,gateway,web" {
		return fmt.Errorf("release manifest: images must be exactly api, gateway, web, got %v", have)
	}
	for _, svc := range Services {
		img := m.Images[svc]
		if !digestRe.MatchString(img.Digest) {
			return fmt.Errorf("release manifest: digest for %s is not sha256:<64 hex>", svc)
		}
		if img.Repository != registry+"/"+owner+"/"+prefix+"-"+svc {
			return fmt.Errorf("release manifest: repository %q for %s is not %s/%s/%s-%s", img.Repository, svc, registry, owner, prefix, svc)
		}
	}
	return nil
}

// Identity is what a signature on one of this release's images must say, field
// for field. Every value is matched exactly; there are no patterns.
//
// SAN is the reusable workflow that ran the attest step (_build-sign.yml);
// BuildConfigURI is the caller (release.yml at the tag, or prerelease.yml at the
// branch). The caller is what separates a release from a release candidate.
type Identity struct {
	Issuer              string
	SAN                 string
	BuildConfigURI      string
	SourceRepositoryURI string
	SourceRepositoryRef string
	Trigger             string
	// Commit is both the workflow SHA (BuildConfigDigest) and the source digest.
	Commit           string
	RunInvocationURI string
}

// Identity returns the signing identity this release must have been signed with.
func (m Manifest) Identity() (Identity, error) {
	if m.Channel == Dev {
		return Identity{}, errors.New("this is a development build; it has no release identity and verifies nothing")
	}
	if err := m.Validate(); err != nil {
		return Identity{}, err
	}
	gh := "https://github.com/" + m.Repository
	return Identity{
		Issuer:              Issuer,
		SAN:                 gh + "/" + buildSignWorkflow + "@" + m.Ref,
		BuildConfigURI:      gh + "/" + m.Workflow + "@" + m.Ref,
		SourceRepositoryURI: gh,
		SourceRepositoryRef: m.Ref,
		Trigger:             "push",
		Commit:              m.Commit,
		RunInvocationURI:    gh + "/actions/runs/" + m.RunID + "/attempts/" + m.RunAttempt,
	}, nil
}

// Short is the first 7 characters of the commit, for display.
func (m Manifest) Short() string {
	if len(m.Commit) >= 7 {
		return m.Commit[:7]
	}
	return m.Commit
}
