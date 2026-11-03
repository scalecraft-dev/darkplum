package commands

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"

	"github.com/scalecraft-dev/darkplum/cli/internal/release"
	"github.com/scalecraft-dev/darkplum/cli/internal/verify"
)

func virtualVerifier(t *testing.T) func() (*verify.Verifier, error) {
	return func() (*verify.Verifier, error) {
		vs, err := ca.NewVirtualSigstore()
		if err != nil {
			t.Fatal(err)
		}
		return verify.New(vs)
	}
}

func TestVerifyReleaseRefusesWhatIsNotSigned(t *testing.T) {
	m := goldenManifest(t, "release.golden.json")
	var out bytes.Buffer
	unsigned := func(context.Context, string, string) ([][]byte, error) { return nil, nil }
	err := (&VerifyReleaseCmd{}).run(context.Background(), &out, m, virtualVerifier(t), unsigned)
	if err == nil || !IsReported(err) {
		t.Fatalf("unsigned images verified: %v", err)
	}
	for _, svc := range release.Services {
		if !strings.Contains(out.String(), "✗ "+svc) {
			t.Errorf("no failure row for %s:\n%s", svc, out.String())
		}
	}
	if strings.Contains(out.String(), "✓ all three") {
		t.Errorf("success line printed:\n%s", out.String())
	}
}

func TestVerifyReleaseRefusesADevBuildAndAMissingTrustRoot(t *testing.T) {
	var out bytes.Buffer
	dev := release.Manifest{Schema: 1, Channel: release.Dev, Version: "dev"}
	if err := (&VerifyReleaseCmd{}).run(context.Background(), &out, dev, virtualVerifier(t), nil); err == nil || !strings.Contains(out.String(), "development build") {
		t.Errorf("dev build: %v\n%s", err, out.String())
	}
	out.Reset()
	m := goldenManifest(t, "release.golden.json")
	offline := func() (*verify.Verifier, error) { return nil, errors.New("dial tcp: no route to host") }
	err := (&VerifyReleaseCmd{}).run(context.Background(), &out, m, offline, nil)
	if err == nil || !strings.Contains(out.String(), "trust root") || !strings.Contains(out.String(), "tuf-repo-cdn.sigstore.dev") {
		t.Errorf("no trust root: %v\n%s", err, out.String())
	}
}

// A fetch error for one image fails the run; the others are still reported.
func TestVerifyReleaseReportsRegistryErrors(t *testing.T) {
	m := goldenManifest(t, "release.golden.json")
	var out bytes.Buffer
	broken := func(_ context.Context, repo, _ string) ([][]byte, error) {
		return nil, errors.New("registry unreachable: " + repo)
	}
	if err := (&VerifyReleaseCmd{}).run(context.Background(), &out, m, virtualVerifier(t), broken); err == nil {
		t.Fatal("verified with an unreachable registry")
	}
	if !strings.Contains(out.String(), "registry unreachable") {
		t.Errorf("the cause is not shown:\n%s", out.String())
	}
}
