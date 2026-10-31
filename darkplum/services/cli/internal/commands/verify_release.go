package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/release"
	"github.com/scalecraft-dev/darkplum/cli/internal/verify"
)

// VerifyReleaseCmd is `darkplum verify-release`: verify, anonymously, that the
// three images this build deploys were signed by the workflow run its manifest
// names. The release workflows run it right after signing, with the binary they
// just built, so CI accepts exactly what a customer's machine will accept. It
// sends no credentials and changes nothing.
type VerifyReleaseCmd struct {
	CacheDir string `name:"cache-dir" help:"Where to keep the sigstore trust root (TUF cache). Defaults to the user cache directory." default:""`
	Timeout  string `help:"Give up after this long." default:"5m"`
}

// Run verifies the release this binary was built for.
func (c *VerifyReleaseCmd) Run(_ *Globals) error {
	m, err := release.Current()
	if err != nil {
		return err
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil {
		return fmt.Errorf("--timeout: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	dir := c.CacheDir
	if dir == "" {
		if dir, err = verify.DefaultCacheDir(); err != nil {
			return err
		}
	}
	return c.run(ctx, os.Stdout, m, func() (*verify.Verifier, error) { return verify.NewLive(dir) }, verify.RegistryFetcher())
}

func (c *VerifyReleaseCmd) run(ctx context.Context, w io.Writer, m release.Manifest, newVerifier func() (*verify.Verifier, error), f verify.Fetcher) error {
	fmt.Fprintf(w, "%s\n\n", sHeader("▸ verify-release · "+m.Version+" ("+m.Channel+")"))
	if m.Channel == release.Dev {
		printCheckTo(w, checkResult{name: "release", status: checkFail, detail: "this is a development build",
			hint: "only a release or release-candidate binary carries the images and the identity to verify"})
		return reportedError{"nothing to verify"}
	}
	v, err := newVerifier()
	if err != nil {
		printCheckTo(w, checkResult{name: "trust root", status: checkFail, detail: firstLine(err.Error()),
			hint: "needs https://tuf-repo-cdn.sigstore.dev; check the network and retry"})
		return reportedError{"cannot establish the sigstore trust root"}
	}
	results, err := v.VerifyRelease(ctx, f, m)
	failed := 0
	for _, svc := range release.Services {
		img := m.Images[svc]
		if res, ok := results[svc]; ok {
			printCheckTo(w, checkResult{name: svc, status: checkPass, detail: img.Ref() + " · " + res.RunInvocationURI})
			continue
		}
		failed++
		printCheckTo(w, checkResult{name: svc, status: checkFail, detail: img.Ref(),
			hint: "no signature from the release workflow's identity; do not deploy this image"})
	}
	if err != nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, sDim(err.Error()))
	}
	if failed > 0 || err != nil {
		return reportedError{fmt.Sprintf("%d of %d images failed verification", failed, len(release.Services))}
	}
	fmt.Fprintf(w, "\n%s\n", sSuccess("✓ all three images are signed by "+m.Repository+" "+m.Workflow+"@"+m.Ref))
	return nil
}
