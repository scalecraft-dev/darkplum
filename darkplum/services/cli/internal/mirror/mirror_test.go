package mirror

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/scalecraft-dev/darkplum/cli/internal/release"
	"github.com/scalecraft-dev/darkplum/cli/internal/verify"
)

type testRegistry struct {
	host            string
	blobPosts, puts atomic.Int64
}

func newRegistry(t *testing.T) *testRegistry {
	t.Helper()
	tr := &testRegistry{}
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/blobs/uploads") {
			tr.blobPosts.Add(1)
		}
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
			tr.puts.Add(1)
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	tr.host = strings.TrimPrefix(srv.URL, "http://")
	return tr
}

// release builds a manifest whose three images are multi-arch indexes pushed to src.
func pushSource(t *testing.T, src *testRegistry) release.Manifest {
	t.Helper()
	m := release.Manifest{Schema: 1, Channel: release.Release, Version: "1.2.3", Images: map[string]release.Image{}}
	for _, svc := range release.Services {
		idx, err := random.Index(256, 2, 2)
		if err != nil {
			t.Fatal(err)
		}
		repo := src.host + "/scalecraft-dev/darkplum-" + svc
		ref, _ := name.NewTag(repo + ":1.2.3")
		if err := remote.WriteIndex(ref, idx); err != nil {
			t.Fatal(err)
		}
		d, _ := idx.Digest()
		m.Images[svc] = release.Image{Repository: repo, Digest: d.String(), RunAttempt: "1"}
	}
	return m
}

func repos(dst *testRegistry) map[string]string {
	out := map[string]string{}
	for _, svc := range release.Services {
		out[svc] = dst.host + "/acme/darkplum-" + svc
	}
	return out
}

func TestMirrorCopiesByDigestAndIsIdempotent(t *testing.T) {
	src, dst := newRegistry(t), newRegistry(t)
	m := pushSource(t, src)
	got, err := Release(context.Background(), verify.ForTest(m), repos(dst), authn.Anonymous, Options{})
	if err != nil || len(got) != 3 {
		t.Fatalf("Release = %v, %v", got, err)
	}
	for _, g := range got {
		if !g.Copied || g.Ref != dst.host+"/acme/darkplum-"+g.Service+"@"+m.Images[g.Service].Digest {
			t.Errorf("mirrored = %+v: the digest must be preserved byte for byte", g)
		}
		tag, _ := name.NewTag(dst.host + "/acme/darkplum-" + g.Service + ":1.2.3")
		d, err := remote.Head(tag)
		if err != nil || d.Digest.String() != m.Images[g.Service].Digest {
			t.Errorf("%s: dst tag digest %v, %v", g.Service, d, err)
		}
	}
	// rerun: nothing is written
	b, p := dst.blobPosts.Load(), dst.puts.Load()
	again, err := Release(context.Background(), verify.ForTest(m), repos(dst), authn.Anonymous, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range again {
		if g.Copied {
			t.Errorf("%s was copied again", g.Service)
		}
	}
	if dst.blobPosts.Load() != b || dst.puts.Load() != p {
		t.Errorf("a rerun wrote to the registry (%d blob uploads, %d manifest puts)", dst.blobPosts.Load()-b, dst.puts.Load()-p)
	}
}

func TestMirrorAbortsWhenTheTagNamesOtherBytes(t *testing.T) {
	src, dst := newRegistry(t), newRegistry(t)
	m := pushSource(t, src)
	other, _ := random.Image(512, 1)
	tag, _ := name.NewTag(dst.host + "/acme/darkplum-api:1.2.3")
	if err := remote.Write(tag, other); err != nil {
		t.Fatal(err)
	}
	before, _ := other.Digest()
	got, err := Release(context.Background(), verify.ForTest(m), repos(dst), authn.Anonymous, Options{})
	if !errors.Is(err, ErrTagConflict) {
		t.Fatalf("err = %v, want ErrTagConflict", err)
	}
	// gateway comes before api in release.Services; api is where it stopped
	if len(got) != 1 || got[0].Service != "gateway" {
		t.Errorf("mirrored before stopping: %+v", got)
	}
	d, _ := remote.Head(tag)
	if d.Digest != before {
		t.Error("the conflicting tag was overwritten")
	}
	web, _ := name.NewTag(dst.host + "/acme/darkplum-web:1.2.3")
	if _, err := remote.Head(web); err == nil {
		t.Error("an image after the conflict was copied")
	}
}

func TestMirrorRefusesUnverifiedImagesAndBadInput(t *testing.T) {
	src, dst := newRegistry(t), newRegistry(t)
	m := pushSource(t, src)
	if _, err := Release(context.Background(), verify.Verified{}, repos(dst), authn.Anonymous, Options{}); err == nil || !strings.Contains(err.Error(), "not verified") {
		t.Errorf("unverified: %v", err)
	}
	delete(repos(dst), "web")
	r := repos(dst)
	delete(r, "web")
	if _, err := Release(context.Background(), verify.ForTest(m), r, authn.Anonymous, Options{}); err == nil || !strings.Contains(err.Error(), "no destination repository for web") {
		t.Errorf("missing repo: %v", err)
	}
	// a destination that is not a repository reference
	bad := repos(dst)
	bad["gateway"] = "Not A Repo"
	if _, err := Release(context.Background(), verify.ForTest(m), bad, authn.Anonymous, Options{}); err == nil {
		t.Error("an invalid destination was accepted")
	}
	// a source that is not repository@digest
	m2 := m
	m2.Images = map[string]release.Image{}
	for k, v := range m.Images {
		m2.Images[k] = v
	}
	g := m2.Images["gateway"]
	g.Digest = "latest"
	m2.Images["gateway"] = g
	if _, err := Release(context.Background(), verify.ForTest(m2), repos(dst), authn.Anonymous, Options{}); err == nil {
		t.Error("a tag was accepted as a source digest")
	}
	// a source that does not exist
	m3 := m
	m3.Images = map[string]release.Image{}
	for k, v := range m.Images {
		m3.Images[k] = v
	}
	gw := m3.Images["gateway"]
	gw.Digest = "sha256:" + strings.Repeat("a", 64)
	m3.Images["gateway"] = gw
	if _, err := Release(context.Background(), verify.ForTest(m3), repos(dst), authn.Anonymous, Options{}); err == nil {
		t.Error("a missing source image was reported as copied")
	}
}

func TestTag(t *testing.T) {
	for _, tc := range []struct {
		m    release.Manifest
		want string
		ok   bool
	}{
		{release.Manifest{Channel: release.Release, Version: "1.2.3"}, "1.2.3", true},
		{release.Manifest{Channel: release.RC, Version: "0.0.0-rc.7+abcdef0"}, "rc-7-abcdef0", true},
		{release.Manifest{Channel: release.RC, Version: "garbage"}, "", false},
		{release.Manifest{Channel: release.Dev, Version: "dev"}, "", false},
	} {
		got, err := Tag(tc.m)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("Tag(%+v) = %q, %v", tc.m, got, err)
		}
	}
}

func TestECRAuth(t *testing.T) {
	var gotArgs []string
	auth, err := ECRAuth(func(args ...string) (string, error) { gotArgs = args; return "s3cr3t-token\n", nil }, "acme", "us-east-2")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := auth.Authorization()
	if cfg.Username != "AWS" || cfg.Password != "s3cr3t-token" {
		t.Errorf("auth = %+v", cfg)
	}
	if strings.Join(gotArgs, " ") != "--profile acme ecr get-login-password --region us-east-2" {
		t.Errorf("aws args = %v", gotArgs)
	}
	if _, err := ECRAuth(func(...string) (string, error) { return "", errors.New("expired") }, "", "us-east-2"); err == nil {
		t.Error("a failed login was accepted")
	}
	if _, err := ECRAuth(func(...string) (string, error) { return "  ", nil }, "", "us-east-2"); err == nil {
		t.Error("an empty token was accepted")
	}
}

func TestPlatformsListsRealPlatformsNotAttestations(t *testing.T) {
	src := newRegistry(t)
	var idx v1.ImageIndex = empty.Index
	for _, p := range []v1.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}, {OS: "unknown", Architecture: "unknown"}} {
		im, err := random.Image(128, 1)
		if err != nil {
			t.Fatal(err)
		}
		p := p
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{Add: im, Descriptor: v1.Descriptor{Platform: &p}})
	}
	repo := src.host + "/scalecraft-dev/darkplum-gateway"
	ref, _ := name.NewTag(repo + ":1")
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}
	d, _ := idx.Digest()
	got, err := Platforms(context.Background(), release.Image{Repository: repo, Digest: d.String()}, Options{})
	if err != nil || strings.Join(got, ",") != "linux/amd64,linux/arm64" {
		t.Fatalf("Platforms = %v, %v", got, err)
	}
	if _, err := Platforms(context.Background(), release.Image{Repository: repo, Digest: "sha256:" + strings.Repeat("0", 64)}, Options{}); err == nil {
		t.Error("an unknown digest must be an error")
	}
	if _, err := Platforms(context.Background(), release.Image{Repository: repo, Digest: "latest"}, Options{}); err == nil {
		t.Error("a tag must be refused: the source is always repository@digest")
	}
}

func TestDevTagIsPerSetOfDigests(t *testing.T) {
	mk := func(a, b, c string) release.Manifest {
		return release.Manifest{Channel: release.Dev, Images: map[string]release.Image{
			"gateway": {Digest: "sha256:" + strings.Repeat(a, 64)}, "api": {Digest: "sha256:" + strings.Repeat(b, 64)}, "web": {Digest: "sha256:" + strings.Repeat(c, 64)}}}
	}
	t1, err := Tag(mk("1", "2", "3"))
	t2, _ := Tag(mk("1", "2", "3"))
	t3, _ := Tag(mk("1", "2", "4"))
	if err != nil || t1 != t2 || t1 == t3 || !strings.HasPrefix(t1, "dev-") {
		t.Errorf("tags = %q %q %q (%v)", t1, t2, t3, err)
	}
}
