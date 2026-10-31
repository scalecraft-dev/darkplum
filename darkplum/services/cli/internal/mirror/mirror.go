// Package mirror copies the verified release images into the customer's own
// registry (the Terraform-owned ECR repositories), byte for byte, by digest.
//
// Three rules, each the answer to a way this goes wrong quietly:
//   - It takes a verify.Verified and nothing else, so an image is never copied on
//     the strength of its digest alone: the signature check came first, structurally.
//   - The source is always repository@digest. A tag is never resolved here.
//   - A destination tag that already exists at a DIFFERENT digest aborts the copy.
//     ECR repositories are IMMUTABLE, so this would fail at the registry anyway; the
//     check makes the failure a clear one before anything is written, and stops a
//     registry without immutability from silently re-pointing a release tag.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/scalecraft-dev/darkplum/cli/internal/release"
	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
	"github.com/scalecraft-dev/darkplum/cli/internal/verify"
)

// ErrTagConflict is returned when the destination tag already names other bytes.
var ErrTagConflict = errors.New("destination tag exists at a different digest")

// Mirrored is one image in the customer's registry.
type Mirrored struct {
	Service string
	// Ref is repository@digest in the customer's registry: what workloads run.
	Ref string
	// Copied is false when the tag was already there at the right digest.
	Copied bool
}

// Tag is the destination tag for a release: X.Y.Z for a release, rc-N-sha7 for a
// release candidate. These are the only tags the repositories' lifecycle policy
// counts, and the only ones ever pushed.
func Tag(m release.Manifest) (string, error) {
	switch m.Channel {
	case release.Release:
		return m.Version, nil
	case release.RC:
		// 0.0.0-rc.7+abcdef0 -> rc-7-abcdef0
		v := strings.TrimPrefix(m.Version, "0.0.0-rc.")
		if v == m.Version || !strings.Contains(v, "+") {
			return "", fmt.Errorf("unexpected release candidate version %q", m.Version)
		}
		return "rc-" + strings.Replace(v, "+", "-", 1), nil
	}
	return "", fmt.Errorf("a %s build has no release tag", m.Channel)
}

// ECRAuth gets a registry login from the operator's own AWS session. It is held in
// memory only: never written to a docker config, never passed on a command line
// beyond the single get-login-password call.
func ECRAuth(aws runner.AWSFunc, profile, region string) (authn.Authenticator, error) {
	pw, err := aws(runner.ProfileArgs(profile, "ecr", "get-login-password", "--region", region)...)
	if err != nil || strings.TrimSpace(pw) == "" {
		return nil, fmt.Errorf("cannot get an ECR login from your AWS session: %v", err)
	}
	return authn.FromConfig(authn.AuthConfig{Username: "AWS", Password: strings.TrimSpace(pw)}), nil
}

// Options are the registry knobs; the zero value is production.
type Options struct {
	// Source reads the (public) release images; defaults to anonymous.
	Source []remote.Option
	// Dest writes the customer's registry.
	Dest []remote.Option
}

// Release copies every image of a verified release. repos maps a service to its
// destination repository URL (no tag). All three are attempted in order and the
// first failure stops the run: a half-mirrored release is reported, not hidden.
func Release(ctx context.Context, v verify.Verified, repos map[string]string, auth authn.Authenticator, o Options) ([]Mirrored, error) {
	if !v.Valid() {
		return nil, errors.New("mirror: refusing images whose signatures were not verified")
	}
	m := v.Manifest()
	tag, err := Tag(m)
	if err != nil {
		return nil, err
	}
	var out []Mirrored
	for _, svc := range release.Services {
		repo, ok := repos[svc]
		if !ok || repo == "" {
			return out, fmt.Errorf("mirror: no destination repository for %s", svc)
		}
		r, err := copyPinned(ctx, svc, m.Images[svc], repo, tag, auth, o)
		if err != nil {
			return out, fmt.Errorf("%s: %w", svc, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func copyPinned(ctx context.Context, svc string, img release.Image, repo, tag string, auth authn.Authenticator, o Options) (Mirrored, error) {
	srcRef, err := name.NewDigest(img.Ref(), name.StrictValidation)
	if err != nil {
		return Mirrored{}, fmt.Errorf("source %q is not repository@digest: %w", img.Ref(), err)
	}
	dstTag, err := name.NewTag(repo+":"+tag, name.StrictValidation)
	if err != nil {
		return Mirrored{}, err
	}
	dstOpts := append([]remote.Option{remote.WithContext(ctx), remote.WithAuth(auth)}, o.Dest...)
	srcOpts := append([]remote.Option{remote.WithContext(ctx), remote.WithAuth(authn.Anonymous)}, o.Source...)
	want := srcRef.DigestStr()
	result := Mirrored{Service: svc, Ref: repo + "@" + want}

	// Already there? Same bytes: nothing to do. Other bytes: stop.
	if d, err := remote.Head(dstTag, dstOpts...); err == nil {
		if d.Digest.String() == want {
			return result, nil
		}
		return Mirrored{}, fmt.Errorf("%w: %s is %s, this release is %s", ErrTagConflict, dstTag, d.Digest, want)
	} else if !notFound(err) {
		return Mirrored{}, fmt.Errorf("cannot look at %s: %w", dstTag, err)
	}

	desc, err := remote.Get(srcRef, srcOpts...)
	if err != nil {
		return Mirrored{}, fmt.Errorf("cannot read %s: %w", img.Ref(), err)
	}
	if desc.Digest.String() != want {
		return Mirrored{}, fmt.Errorf("registry answered %s with digest %s", img.Ref(), desc.Digest)
	}
	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return Mirrored{}, err
		}
		err = remote.WriteIndex(dstTag, idx, dstOpts...)
		if err != nil {
			return Mirrored{}, fmt.Errorf("cannot write %s: %w", dstTag, err)
		}
	} else {
		im, err := desc.Image()
		if err != nil {
			return Mirrored{}, err
		}
		if err := remote.Write(dstTag, im, dstOpts...); err != nil {
			return Mirrored{}, fmt.Errorf("cannot write %s: %w", dstTag, err)
		}
	}
	got, err := remote.Head(dstTag, dstOpts...)
	if err != nil {
		return Mirrored{}, fmt.Errorf("cannot confirm %s after copying: %w", dstTag, err)
	}
	if got.Digest.String() != want {
		return Mirrored{}, fmt.Errorf("%s landed as %s, not %s", dstTag, got.Digest, want)
	}
	result.Copied = true
	return result, nil
}

func notFound(err error) bool {
	var te *transport.Error
	return errors.As(err, &te) && te.StatusCode == http.StatusNotFound
}

var _ = v1.Hash{}
