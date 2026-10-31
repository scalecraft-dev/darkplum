package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/scalecraft-dev/darkplum/cli/internal/bundle"
	"github.com/scalecraft-dev/darkplum/cli/internal/release"
)

// VersionCmd is `darkplum version`: what this binary is, which images it will
// deploy (by digest), and what else it pulls from other registries.
type VersionCmd struct {
	JSON           bool `name:"json" help:"Machine-readable output."`
	BundleManifest bool `name:"bundle-manifest" help:"List every file embedded in this binary with its SHA-256 (the release pipeline checks this list against an allowlist)."`
}

// Run prints the version information.
func (c *VersionCmd) Run(_ *Globals) error {
	m, err := release.Current()
	if err != nil {
		return err
	}
	b, err := bundle.Embedded()
	if err != nil {
		return err
	}
	return c.render(os.Stdout, m, b)
}

type versionDoc struct {
	Manifest release.Manifest `json:"release"`
	Bundle   struct {
		SHA256 string         `json:"sha256"`
		Files  int            `json:"files"`
		List   []bundle.Entry `json:"list,omitempty"`
	} `json:"bundle"`
	ThirdParty []bundle.Pin `json:"third_party_images"`
}

func (c *VersionCmd) render(w io.Writer, m release.Manifest, b *bundle.Bundle) error {
	pins, err := b.Pins()
	if err != nil {
		return err
	}
	var doc versionDoc
	doc.Manifest, doc.ThirdParty = m, pins
	doc.Bundle.SHA256, doc.Bundle.Files = b.Hash(), len(b.Entries())
	if c.BundleManifest {
		doc.Bundle.List = b.Entries()
		if !c.JSON {
			for _, e := range doc.Bundle.List {
				fmt.Fprintf(w, "%s  %s\n", e.SHA256, e.Path)
			}
			return nil
		}
	}
	if c.JSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}

	fmt.Fprintf(w, "darkplum %s (%s)\n", m.Version, m.Channel)
	switch m.Channel {
	case release.Dev:
		fmt.Fprintln(w, sWarn("DEV BUILD")+" built without a release manifest: it has no images to deploy and verifies no signatures.")
	case release.RC:
		fmt.Fprintln(w, sWarn("RELEASE CANDIDATE")+" for testing; it deploys darkplum-rc-* images and refuses release images.")
	}
	if m.Channel != release.Dev {
		fmt.Fprintf(w, "  commit      %s\n", m.Commit)
		fmt.Fprintf(w, "  built by    %s %s (run %s, attempt %s)\n", m.Repository, m.Workflow, m.RunID, m.RunAttempt)
		fmt.Fprintf(w, "  ref         %s\n", m.Ref)
		fmt.Fprintln(w, "  images")
		for _, svc := range release.Services {
			fmt.Fprintf(w, "    %-8s  %s\n", svc, m.Images[svc].Ref())
		}
	}
	fmt.Fprintf(w, "  bundle      %s (%d files)\n", b.Hash()[:12], len(b.Entries()))
	if len(pins) > 0 {
		fmt.Fprintln(w, "  third-party images (pinned by digest, pulled from their own registries)")
		seen := map[string]bool{}
		for _, p := range pins {
			if seen[p.Ref] {
				continue
			}
			seen[p.Ref] = true
			if p.Chart {
				fmt.Fprintf(w, "    %s  (image tag in %s)\n", p.Ref, p.File)
				continue
			}
			fmt.Fprintf(w, "    %s\n", p.Ref)
		}
	}
	return nil
}
