package bundle

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// Pin is an image reference found in the shipped Kubernetes tree.
type Pin struct {
	File string `json:"file"`
	Ref  string `json:"ref"`
	// Chart marks a pin that is only an image tag (a Helm values file); the
	// repository comes from the chart.
	Chart bool `json:"chart_tag,omitempty"`
}

var (
	imageLineRe = regexp.MustCompile(`(?m)^\s*(?:-\s+)?image:[ \t]*["']?([^\s"'#]+)`)
	// The semantic router's chart renders "<repository>:<tag>"; its values pin the
	// image with a tag of the form "v0.3.0@sha256:<digest>".
	pinnedTagRe = regexp.MustCompile(`(?m)^\s*tag:\s*"([^"\s]+@sha256:[0-9a-f]{64})"`)
	digestRefRe = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

// Pins lists the third-party images the bundle pins by digest, sorted. These are
// the images Darkplum deploys but does not build: `darkplum version` shows them so
// an operator can see exactly what will be pulled from other registries.
func (b *Bundle) Pins() ([]Pin, error) {
	var out []Pin
	err := b.eachK8sFile(func(file, text string) {
		for _, m := range imageLineRe.FindAllStringSubmatch(text, -1) {
			if digestRefRe.MatchString(m[1]) {
				out = append(out, Pin{File: file, Ref: m[1]})
			}
		}
		for _, m := range pinnedTagRe.FindAllStringSubmatch(text, -1) {
			out = append(out, Pin{File: file, Ref: m[1], Chart: true})
		}
	})
	// A chart's tag-only pin is the same image as the full reference its rendered
	// objects carry; listing it twice would show one image as two.
	kept := out[:0]
	for _, p := range out {
		if p.Chart && coveredByFullRef(p.Ref, out) {
			continue
		}
		kept = append(kept, p)
	}
	out = kept
	sortPins(out)
	return out, err
}

func coveredByFullRef(tag string, pins []Pin) bool {
	for _, q := range pins {
		if !q.Chart && strings.HasSuffix(q.Ref, ":"+tag) {
			return true
		}
	}
	return false
}

// Unpinned lists image references that are neither Darkplum's own (darkplum-*,
// replaced with release digests at deploy) nor pinned by digest.
func (b *Bundle) Unpinned() ([]Pin, error) {
	var out []Pin
	err := b.eachK8sFile(func(file, text string) {
		for _, m := range imageLineRe.FindAllStringSubmatch(text, -1) {
			ref := m[1]
			if digestRefRe.MatchString(ref) || strings.HasPrefix(ref, "darkplum-") || ref == "" {
				continue
			}
			out = append(out, Pin{File: file, Ref: ref})
		}
	})
	sortPins(out)
	return out, err
}

func (b *Bundle) eachK8sFile(fn func(file, text string)) error {
	for _, e := range b.entries {
		if !strings.HasPrefix(e.Path, "k8s/") || !(strings.HasSuffix(e.Path, ".yaml") || strings.HasSuffix(e.Path, ".yml")) {
			continue
		}
		data, err := fs.ReadFile(b.fsys, e.Path)
		if err != nil {
			return err
		}
		fn(e.Path, string(data))
	}
	return nil
}

func sortPins(p []Pin) {
	sort.Slice(p, func(i, j int) bool {
		if p[i].Ref != p[j].Ref {
			return p[i].Ref < p[j].Ref
		}
		return p[i].File < p[j].File
	})
}
