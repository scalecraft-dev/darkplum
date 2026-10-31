package bundle

import (
	"strings"
	"testing"
	"testing/fstest"
)

const digest = "@sha256:6956052a3491118df885b37e240abebaf839b62495c3d54e617cb8401c54717b"

func TestPinsFindsDigestPinnedImages(t *testing.T) {
	b, err := New(fstest.MapFS{
		"terraform/aws/main.tf": {Data: []byte("# x")},
		"k8s/a/d.yaml":          {Data: []byte("spec:\n  containers:\n    - name: a\n      image: vllm/vllm-openai:v0.6.6" + digest + "\n    - image: \"x/y:1\"\n")},
		"k8s/b/values.yaml":     {Data: []byte("image:\n  tag: \"v0.3.0" + digest + "\"\n")},
		"k8s/c/own.yaml":        {Data: []byte("image: darkplum-api:latest\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	pins, err := b.Pins()
	if err != nil || len(pins) != 2 {
		t.Fatalf("pins = %v, %v", pins, err)
	}
	if !strings.HasPrefix(pins[0].Ref, "v0.3.0@") && !strings.HasPrefix(pins[1].Ref, "v0.3.0@") {
		t.Errorf("the chart tag pin is missing: %v", pins)
	}
	un, _ := b.Unpinned()
	if len(un) != 1 || un[0].Ref != "x/y:1" {
		t.Errorf("unpinned = %v (darkplum-* and digest-pinned images must not be listed)", un)
	}
}

// Everything the real bundle pulls from another registry is pinned by digest,
// except the local kind overlay's database, which is never deployed to AWS.
func TestEmbeddedBundlePinsEveryThirdPartyImage(t *testing.T) {
	b, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	un, err := b.Unpinned()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range un {
		if !strings.HasPrefix(u.File, "k8s/overlays/dev/") {
			t.Errorf("%s uses %s, which is not pinned by digest", u.File, u.Ref)
		}
	}
	pins, _ := b.Pins()
	var have []string
	for _, p := range pins {
		have = append(have, p.Ref)
	}
	joined := strings.Join(have, "\n")
	for _, want := range []string{
		"vllm/vllm-openai:v0.6.6@sha256:6956052a3491118df885b37e240abebaf839b62495c3d54e617cb8401c54717b",
		"nvcr.io/nvidia/k8s-device-plugin:v0.20.1@sha256:",
		"v0.3.0@sha256:5c5a6421f7a1fc0596fc7e9053b35952fef4bc12564c01a3f7315c51e3602ae6", // semantic router
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("pin %q missing from the bundle; found:\n%s", want, joined)
		}
	}
}
