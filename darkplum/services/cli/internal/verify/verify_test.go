package verify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"

	"github.com/scalecraft-dev/darkplum/cli/internal/release"
)

// The fixtures are a REAL signature: the sigstore project's own GitHub Actions
// workflow signed ghcr.io/sigstore/timestamp-server (keyless, public-good
// Fulcio and Rekor), and testdata holds that bundle and the trusted root as of
// the day it was captured. Verification is offline and exercises every
// certificate extension the product's identity pins.
const (
	fixtureDigest = "sha256:77c615f0bef6ddd907b531099cd1a8cf8f698ca9a08b1abf7397b222c9352306"
	fixtureRepo   = "https://github.com/sigstore/timestamp-authority"
)

func fixtureIdentity() release.Identity {
	return release.Identity{
		Issuer:              release.Issuer,
		SAN:                 fixtureRepo + "/.github/workflows/build_container.yml@refs/tags/v2.2.0",
		BuildConfigURI:      fixtureRepo + "/.github/workflows/release.yaml@refs/tags/v2.2.0",
		SourceRepositoryURI: fixtureRepo,
		SourceRepositoryRef: "refs/tags/v2.2.0",
		Trigger:             "push",
		Commit:              "526e9de76cded7d2fd7b3d9b21061305ea7c85b2",
		RunInvocationURI:    fixtureRepo + "/actions/runs/39717907327/attempts/1",
	}
}

func fixtureBundle(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/timestamp-server.bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureVerifier(t *testing.T) *Verifier {
	t.Helper()
	b, err := os.ReadFile("testdata/trusted_root.json")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := root.NewTrustedRootFromJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	v, err := New(tr)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyBundlePositive(t *testing.T) {
	res, err := fixtureVerifier(t).VerifyBundle(fixtureBundle(t), fixtureDigest, fixtureIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if res.SAN != fixtureIdentity().SAN || res.RunInvocationURI != fixtureIdentity().RunInvocationURI || res.BuildConfigURI != fixtureIdentity().BuildConfigURI {
		t.Errorf("result = %+v", res)
	}
}

// Every field of the identity matters, each on its own.
func TestVerifyBundleRefusesAnyDifferentIdentityField(t *testing.T) {
	v := fixtureVerifier(t)
	for name, mut := range map[string]func(*release.Identity){
		"issuer":              func(i *release.Identity) { i.Issuer = "https://accounts.google.com" },
		"workflow (SAN) file": func(i *release.Identity) { i.SAN = strings.Replace(i.SAN, "build_container.yml", "evil.yml", 1) },
		"workflow (SAN) ref":  func(i *release.Identity) { i.SAN = strings.Replace(i.SAN, "refs/tags/v2.2.0", "refs/heads/main", 1) },
		"caller workflow": func(i *release.Identity) {
			i.BuildConfigURI = strings.Replace(i.BuildConfigURI, "release.yaml", "prerelease.yaml", 1)
		},
		"source repository":            func(i *release.Identity) { i.SourceRepositoryURI = "https://github.com/evil/timestamp-authority" },
		"source ref":                   func(i *release.Identity) { i.SourceRepositoryRef = "refs/tags/v2.2.1" },
		"trigger":                      func(i *release.Identity) { i.Trigger = "workflow_dispatch" },
		"commit (workflow sha)":        func(i *release.Identity) { i.Commit = strings.Repeat("0", 40) },
		"run id":                       func(i *release.Identity) { i.RunInvocationURI = fixtureRepo + "/actions/runs/1/attempts/1" },
		"run attempt":                  func(i *release.Identity) { i.RunInvocationURI = fixtureRepo + "/actions/runs/39717907327/attempts/2" },
		"a pattern is not a pattern":   func(i *release.Identity) { i.SAN = fixtureRepo + "/.*" },
		"a prefix is not the identity": func(i *release.Identity) { i.SAN = strings.TrimSuffix(i.SAN, "v2.2.0") },
	} {
		t.Run(name, func(t *testing.T) {
			id := fixtureIdentity()
			mut(&id)
			if _, err := v.VerifyBundle(fixtureBundle(t), fixtureDigest, id); err == nil {
				t.Error("verified under a different identity")
			}
		})
	}
}

func TestVerifyBundleRefusesTheWrongSubjectAndBrokenBundles(t *testing.T) {
	v := fixtureVerifier(t)
	other := fixtureDigest[:len(fixtureDigest)-1] + "0"
	for name, tc := range map[string]struct {
		raw    []byte
		digest string
	}{
		"another digest":     {fixtureBundle(t), other},
		"not a sha256":       {fixtureBundle(t), "latest"},
		"short digest":       {fixtureBundle(t), "sha256:abc"},
		"garbage":            {[]byte("not json"), fixtureDigest},
		"empty":              {nil, fixtureDigest},
		"an empty object":    {[]byte("{}"), fixtureDigest},
		"a tampered payload": {tamper(t, fixtureBundle(t)), fixtureDigest},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := v.VerifyBundle(tc.raw, tc.digest, fixtureIdentity()); err == nil {
				t.Error("verified")
			}
		})
	}
}

// A signature that chains to some other CA is not a signature, however well its
// identity reads.
func TestVerifyBundleRefusesAnUntrustedRoot(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	v, err := New(vs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.VerifyBundle(fixtureBundle(t), fixtureDigest, fixtureIdentity()); err == nil {
		t.Error("a bundle verified against a trust root that did not issue it")
	}
}

// --- fetching bundles from a registry -------------------------------------

// registryWith serves an in-process registry. referrersAPI false hides the
// Referrers API behind a 404, as GHCR does, so only the fallback tag works.
func registryWith(t *testing.T, referrersAPI bool) string {
	t.Helper()
	reg := registry.New(registry.Logger(nopLogger()))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !referrersAPI && strings.Contains(r.URL.Path, "/referrers/") {
			http.NotFound(w, r)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// rawManifest lets a test store exactly the bytes it built.
type rawManifest struct {
	b  []byte
	mt types.MediaType
}

func (r rawManifest) RawManifest() ([]byte, error)        { return r.b, nil }
func (r rawManifest) MediaType() (types.MediaType, error) { return r.mt, nil }

// pushBundle stores each raw as a sigstore bundle referrer of digest the way the
// release workflow's attest step does on GHCR: a one-layer image, listed with its
// artifactType in an OCI image index at the fallback tag sha256-<digest>.
func pushBundle(t *testing.T, host, repo, digest string, artifactType string, raws ...[]byte) {
	t.Helper()
	var entries []map[string]any
	for _, raw := range raws {
		img, err := mutate.Append(empty.Image, mutate.Addendum{Layer: static.NewLayer(raw, types.MediaType(artifactType))})
		if err != nil {
			t.Fatal(err)
		}
		d, _ := img.Digest()
		sz, _ := img.Size()
		mt, _ := img.MediaType()
		ref, _ := name.NewDigest(host + "/" + repo + "@" + d.String())
		if err := remote.Write(ref, img); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, map[string]any{"mediaType": string(mt), "digest": d.String(), "size": sz, "artifactType": artifactType})
	}
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": entries})
	tag, _ := name.NewTag(host + "/" + repo + ":sha256-" + strings.TrimPrefix(digest, "sha256:"))
	if err := remote.Put(tag, rawManifest{b: idx, mt: types.OCIImageIndex}); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryFetcherFindsBundlesWithAndWithoutTheReferrersAPI(t *testing.T) {
	for _, api := range []bool{false, true} {
		name := map[bool]string{false: "fallback tag only (GHCR)", true: "referrers API available"}[api]
		t.Run(name, func(t *testing.T) {
			host := registryWith(t, api)
			pushBundle(t, host, "darkplum/api", fixtureDigest, "application/vnd.dev.sigstore.bundle.v0.3+json", fixtureBundle(t))
			got, err := RegistryFetcher()(context.Background(), host+"/darkplum/api", fixtureDigest)
			if err != nil || len(got) != 1 || string(got[0]) != string(fixtureBundle(t)) {
				t.Fatalf("fetch = %d bundles, %v", len(got), err)
			}
		})
	}
}

// Only sigstore bundles count; other referrers (SBOMs, notes) are ignored, and an
// image with no referrers at all yields nothing rather than an error.
func TestRegistryFetcherIgnoresOtherReferrers(t *testing.T) {
	host := registryWith(t, false)
	pushBundle(t, host, "darkplum/api", fixtureDigest, "application/spdx+json", []byte(`{"sbom":true}`))
	got, err := RegistryFetcher()(context.Background(), host+"/darkplum/api", fixtureDigest)
	if err != nil || len(got) != 0 {
		t.Fatalf("fetch = %d bundles, %v", len(got), err)
	}
	got, err = RegistryFetcher()(context.Background(), host+"/darkplum/api", "sha256:"+strings.Repeat("9", 64))
	if err != nil || len(got) != 0 {
		t.Fatalf("an image with no referrers: %d bundles, %v", len(got), err)
	}
	if _, err := RegistryFetcher()(context.Background(), host+"/darkplum/api", "latest"); err == nil {
		t.Error("a tag was accepted as a digest")
	}
}

func TestVerifyImageEndToEndThroughARegistry(t *testing.T) {
	host := registryWith(t, false)
	pushBundle(t, host, "darkplum/api", fixtureDigest, "application/vnd.dev.sigstore.bundle.v0.3+json", fixtureBundle(t))
	img := release.Image{Repository: host + "/darkplum/api", Digest: fixtureDigest, RunAttempt: "1"}
	v := fixtureVerifier(t)
	if _, err := v.VerifyImage(context.Background(), RegistryFetcher(), img, fixtureIdentity()); err != nil {
		t.Fatal(err)
	}
	// unsigned: nothing stored for this digest
	un := release.Image{Repository: host + "/darkplum/api", Digest: "sha256:" + strings.Repeat("7", 64)}
	if _, err := v.VerifyImage(context.Background(), RegistryFetcher(), un, fixtureIdentity()); err == nil || !strings.Contains(err.Error(), "no signature") {
		t.Errorf("unsigned image: %v", err)
	}
	// signed by someone else: a valid signature, the wrong identity
	id := fixtureIdentity()
	id.SAN = strings.Replace(id.SAN, "sigstore", "someone-else", 1)
	if _, err := v.VerifyImage(context.Background(), RegistryFetcher(), img, id); err == nil || !strings.Contains(err.Error(), "no signature matches") {
		t.Errorf("wrong signer: %v", err)
	}
	// a planted bundle next to the real one cannot make the image fail, and cannot make another pass
	pushBundle(t, host, "darkplum/web", fixtureDigest, "application/vnd.dev.sigstore.bundle.v0.3+json", []byte("planted"), fixtureBundle(t))
	img.Repository = host + "/darkplum/web"
	if _, err := v.VerifyImage(context.Background(), RegistryFetcher(), img, fixtureIdentity()); err != nil {
		t.Errorf("a junk bundle beside the real one broke verification: %v", err)
	}
	pushBundle(t, host, "darkplum/gateway", fixtureDigest, "application/vnd.dev.sigstore.bundle.v0.3+json", []byte("planted"))
	img.Repository = host + "/darkplum/gateway"
	if _, err := v.VerifyImage(context.Background(), RegistryFetcher(), img, fixtureIdentity()); err == nil {
		t.Error("a junk bundle verified an image")
	}
}

// The whole manifest: each image is checked under its own identity and every
// failure is reported, not just the first.
func TestVerifyReportsEveryImageThatFails(t *testing.T) {
	raw := func(t *testing.T) []byte {
		b, err := os.ReadFile("../../../../../.github/scripts/testdata/release.golden.json")
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	m, err := release.Parse(raw(t))
	if err != nil {
		t.Fatal(err)
	}
	none := func(context.Context, string, string) ([][]byte, error) { return nil, nil }
	_, err = fixtureVerifier(t).VerifyRelease(context.Background(), none, m)
	if err == nil {
		t.Fatal("an unsigned release verified")
	}
	for _, svc := range release.Services {
		if !strings.Contains(err.Error(), svc+":") {
			t.Errorf("no failure reported for %s in:\n%v", svc, err)
		}
	}
	// the fixture is a real signature, but not by this release's workflow
	wrong := func(context.Context, string, string) ([][]byte, error) { return [][]byte{fixtureBundle(t)}, nil }
	if _, err := fixtureVerifier(t).VerifyRelease(context.Background(), wrong, m); err == nil || strings.Count(err.Error(), "no signature matches") != 3 {
		t.Errorf("a real signature from another workflow: %v", err)
	}
	// a dev build verifies nothing
	if _, err := fixtureVerifier(t).VerifyRelease(context.Background(), none, release.Manifest{Schema: 1, Channel: release.Dev, Version: "dev"}); err == nil {
		t.Error("a dev manifest verified")
	}
}

func TestIdentityMapsToEveryPinnedExtension(t *testing.T) {
	e := extensions(fixtureIdentity())
	for name, v := range map[string]string{
		"SourceRepositoryURI": e.SourceRepositoryURI, "SourceRepositoryRef": e.SourceRepositoryRef, "SourceRepositoryDigest": e.SourceRepositoryDigest,
		"RunnerEnvironment": e.RunnerEnvironment, "BuildConfigURI": e.BuildConfigURI, "BuildConfigDigest": e.BuildConfigDigest, "BuildTrigger": e.BuildTrigger, "RunInvocationURI": e.RunInvocationURI,
	} {
		if v == "" {
			t.Errorf("%s is not pinned", name)
		}
	}
}

// tamper flips one character inside the signed payload and checks it really changed.
func tamper(t *testing.T, raw []byte) []byte {
	t.Helper()
	i := strings.Index(string(raw), "\"payload\":")
	if i < 0 {
		t.Fatal("fixture has no payload field")
	}
	j := i + strings.Index(string(raw[i:]), ":") + 3 // after the opening quote
	out := append([]byte(nil), raw...)
	if out[j] == 'A' {
		out[j] = 'B'
	} else {
		out[j] = 'A'
	}
	return out
}

func TestUnverifiedIsForTheDevChannelOnly(t *testing.T) {
	for _, ch := range []string{release.Release, release.RC, ""} {
		if _, err := Unverified(release.Manifest{Channel: ch}); err == nil {
			t.Errorf("channel %q was allowed to skip verification", ch)
		}
	}
	v, err := Unverified(release.Manifest{Channel: release.Dev})
	if err != nil || !v.Valid() || !v.Unsigned() {
		t.Fatalf("%+v %v", v, err)
	}
	if ForTest(release.Manifest{}).Unsigned() {
		t.Error("a verified release must not report unsigned")
	}
}

func TestUnverifiedRefusesOnAReleaseOrCandidateBinary(t *testing.T) {
	old := binaryChannel
	defer func() { binaryChannel = old }()
	for _, ch := range []string{release.Release, release.RC, ""} {
		binaryChannel = func() string { return ch }
		if _, err := Unverified(release.Manifest{Channel: release.Dev}); err == nil {
			t.Errorf("a %q binary accepted a hand-built dev manifest", ch)
		}
	}
	binaryChannel = func() string { return release.Dev }
	if _, err := Unverified(release.Manifest{Channel: release.Dev}); err != nil {
		t.Error(err)
	}
}
