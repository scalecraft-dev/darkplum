// Package verify checks that a container image was built and signed by the
// workflow run its release manifest names, and by nothing else.
//
// The signature is a sigstore bundle (DSSE, the image digest as the in-toto
// subject) stored in the registry as a referrer of the image. The bundle is
// verified against the public-good sigstore trust root (fetched with TUF), with
// the certificate's identity matched EXACTLY on every field release.Identity
// lists. There are no patterns anywhere: a regular expression in an identity
// check is how "any workflow in the org" gets accepted by accident.
//
// The same code runs in the CLI before it mirrors anything and, anonymously, in
// the release workflows right after signing, so what CI accepted is what a
// customer's machine will accept.
package verify

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	sgverify "github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/scalecraft-dev/darkplum/cli/internal/release"
)

const (
	bundleArtifactPrefix = "application/vnd.dev.sigstore.bundle"
	// slsaPredicate is what actions/attest-build-provenance signs. A bundle from
	// the right workflow carrying some other predicate is not the build signature.
	slsaPredicate = "https://slsa.dev/provenance/v1"
)

// Verifier verifies sigstore bundles against one trust root.
type Verifier struct {
	sg  *sgverify.Verifier
	ext bool
}

// New builds a Verifier on a trust root: signed certificate timestamp, transparency
// log entry and an observer timestamp are each required once.
func New(tm root.TrustedMaterial) (*Verifier, error) {
	sg, err := sgverify.NewVerifier(tm, sgverify.WithSignedCertificateTimestamps(1), sgverify.WithTransparencyLog(1), sgverify.WithObserverTimestamps(1))
	if err != nil {
		return nil, err
	}
	return &Verifier{sg: sg, ext: true}, nil
}

// NewLive fetches the public-good trust root with TUF into cacheDir (a fresh
// directory is a first run: the embedded root is updated over the network).
func NewLive(cacheDir string) (*Verifier, error) {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, err
	}
	tr, err := root.FetchTrustedRootWithOptions(tuf.DefaultOptions().WithCachePath(cacheDir))
	if err != nil {
		return nil, fmt.Errorf("cannot fetch the sigstore trust root: %w", err)
	}
	return New(tr)
}

// DefaultCacheDir is where NewLive keeps the TUF cache.
func DefaultCacheDir() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "darkplum", "tuf"), nil
}

// Result is what a successful verification established.
type Result struct {
	SAN, BuildConfigURI, RunInvocationURI string
}

// extensions are the Fulcio certificate extensions the identity pins besides the
// SAN and the issuer. Each is matched exactly.
func extensions(id release.Identity) certificate.Extensions {
	return certificate.Extensions{
		SourceRepositoryURI:    id.SourceRepositoryURI,
		SourceRepositoryRef:    id.SourceRepositoryRef,
		SourceRepositoryDigest: id.Commit,
		BuildConfigURI:         id.BuildConfigURI,
		BuildConfigDigest:      id.Commit,
		BuildTrigger:           id.Trigger,
		RunInvocationURI:       id.RunInvocationURI,
		// Signed by a GitHub-hosted runner: a self-hosted runner added to the
		// repository could otherwise produce a certificate with every other field right.
		RunnerEnvironment: "github-hosted",
	}
}

// VerifyBundle verifies one bundle (JSON) as a signature over the image digest
// (sha256:<hex>) by exactly the given identity.
func (v *Verifier) VerifyBundle(raw []byte, digest string, id release.Identity) (Result, error) {
	hexd := strings.TrimPrefix(digest, "sha256:")
	if hexd == digest || len(hexd) != 64 {
		return Result{}, fmt.Errorf("%q is not a sha256 digest", digest)
	}
	d, err := hex.DecodeString(hexd)
	if err != nil {
		return Result{}, fmt.Errorf("%q is not a sha256 digest", digest)
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		return Result{}, fmt.Errorf("not a sigstore bundle: %w", err)
	}
	sm, err := sgverify.NewSANMatcher(id.SAN, "")
	if err != nil {
		return Result{}, err
	}
	im, err := sgverify.NewIssuerMatcher(id.Issuer, "")
	if err != nil {
		return Result{}, err
	}
	ext := certificate.Extensions{}
	if v.ext {
		ext = extensions(id)
	}
	ci, err := sgverify.NewCertificateIdentity(sm, im, ext)
	if err != nil {
		return Result{}, err
	}
	res, err := v.sg.Verify(&b, sgverify.NewPolicy(sgverify.WithArtifactDigest("sha256", d), sgverify.WithCertificateIdentity(ci)))
	if err != nil {
		return Result{}, err
	}
	if res.Statement == nil || res.Statement.PredicateType != slsaPredicate {
		return Result{}, errors.New("the signed statement is not a build provenance attestation")
	}
	c := res.Signature.Certificate
	return Result{SAN: c.SubjectAlternativeName, BuildConfigURI: c.BuildConfigURI, RunInvocationURI: c.RunInvocationURI}, nil
}

// Fetcher returns the raw sigstore bundles stored for an image.
type Fetcher func(ctx context.Context, repository, digest string) ([][]byte, error)

// RegistryFetcher reads bundles from the registry as OCI referrers. It uses the
// Referrers API where there is one and the fallback tag sha256-<digest> where there
// is not (GHCR has none). Credentials are never sent: signed images are public.
func RegistryFetcher(opts ...remote.Option) Fetcher {
	return func(ctx context.Context, repository, digest string) ([][]byte, error) {
		opts := append([]remote.Option{remote.WithContext(ctx), remote.WithAuth(authn.Anonymous)}, opts...)
		dref, err := name.NewDigest(repository+"@"+digest, name.StrictValidation)
		if err != nil {
			return nil, err
		}
		idx, err := remote.Referrers(dref, opts...)
		if err != nil {
			return nil, fmt.Errorf("cannot list signatures of %s@%s: %w", repository, digest, err)
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return nil, err
		}
		var out [][]byte
		for _, m := range im.Manifests {
			if !strings.HasPrefix(m.ArtifactType, bundleArtifactPrefix) {
				continue
			}
			ref, err := name.NewDigest(repository+"@"+m.Digest.String(), name.StrictValidation)
			if err != nil {
				return nil, err
			}
			img, err := remote.Image(ref, opts...)
			if err != nil {
				return nil, err
			}
			layers, err := img.Layers()
			if err != nil || len(layers) != 1 {
				return nil, fmt.Errorf("signature manifest %s has an unexpected shape", m.Digest)
			}
			rc, err := layers[0].Uncompressed()
			if err != nil {
				return nil, err
			}
			b, err := io.ReadAll(io.LimitReader(rc, 4<<20))
			rc.Close()
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		}
		return out, nil
	}
}

// VerifyImage requires at least one bundle of the image to verify under id.
// Bundles by other signers are ignored; they cannot make an image pass, and a
// planted one cannot make it fail.
func (v *Verifier) VerifyImage(ctx context.Context, f Fetcher, img release.Image, id release.Identity) (Result, error) {
	bundles, err := f(ctx, img.Repository, img.Digest)
	if err != nil {
		return Result{}, err
	}
	if len(bundles) == 0 {
		return Result{}, fmt.Errorf("%s has no signature: refusing an unsigned image", img.Ref())
	}
	var why []string
	for _, raw := range bundles {
		res, err := v.VerifyBundle(raw, img.Digest, id)
		if err == nil {
			return res, nil
		}
		why = append(why, firstLine(err.Error()))
	}
	return Result{}, fmt.Errorf("%s: no signature matches the release identity (%s)", img.Ref(), strings.Join(why, "; "))
}

// Verified is the proof that every image of a release manifest carries a valid
// signature by the manifest's own identity. Only VerifyRelease returns a valid
// one, and the mirror takes nothing else: an image is never copied into the
// customer's registry on the strength of its digest alone.
type Verified struct {
	manifest release.Manifest
	// Results is what each verified image established (partial on failure).
	Results map[string]Result
	ok      bool
}

// Valid reports whether every image verified.
func (v Verified) Valid() bool { return v.ok }

// Manifest is the manifest that was verified.
func (v Verified) Manifest() release.Manifest { return v.manifest }

// ForTest returns a valid Verified for a manifest without checking any
// signature. It is for _test.go files only; internal/lint fails the build on any
// other use.
func ForTest(m release.Manifest) Verified {
	return Verified{manifest: m, Results: map[string]Result{}, ok: true}
}

// VerifyRelease verifies all three images of a manifest, each under its own
// identity. Every image is checked and every failure reported; the Verified it
// returns is valid only when none failed.
func (v *Verifier) VerifyRelease(ctx context.Context, f Fetcher, m release.Manifest) (Verified, error) {
	out := Verified{manifest: m, Results: map[string]Result{}}
	var errs []error
	for _, svc := range release.Services {
		id, err := m.Identity(svc)
		if err != nil {
			return out, err
		}
		res, err := v.VerifyImage(ctx, f, m.Images[svc], id)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", svc, err))
			continue
		}
		out.Results[svc] = res
	}
	if len(errs) == 0 {
		out.ok = true
	}
	return out, errors.Join(errs...)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
