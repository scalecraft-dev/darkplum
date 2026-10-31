package commands

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/scalecraft-dev/darkplum/cli/internal/release"
)

// Annotations `darkplum deploy` puts on the Deployments it applies, and the
// ConfigMap that records what release the install runs.
const (
	annImageTrust = "darkplum.ai/image-trust" // "verified" or "unsigned"
	annRelease    = "darkplum.ai/release"
	annChannel    = "darkplum.ai/release-channel"
	annCommit     = "darkplum.ai/source-commit"
	installRecord = "darkplum-install"
)

// release shows what release the install runs, from the darkplum-install
// ConfigMap when `darkplum deploy` wrote one. It returns false: a missing record
// (an install from before releases were recorded) is not a failure.
func (e *statusEnv) release() bool {
	out, err := e.kc.NS(nil, "get", "configmap", installRecord, "-o", "json")
	if err != nil {
		if !kubeNotFound(err, "configmaps", installRecord) {
			fmt.Fprintf(e.out, "%s  %s\n", sDim("release"), sDim("(unavailable: "+firstLine(err.Error())+")"))
		}
		return false
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal([]byte(out), &cm) != nil || cm.Data["version"] == "" {
		return false
	}
	e.recorded = cm.Data["version"]
	if e.build != nil {
		e.proof.want = wantFromRecord(*e.build, cm.Data)
	}
	line := sInfo(cm.Data["version"])
	if ch := cm.Data["channel"]; ch != "" {
		line += sDim(" · channel " + ch)
	}
	if c := cm.Data["commit"]; c != "" {
		if len(c) > 12 {
			c = c[:12]
		}
		line += sDim(" · commit " + c)
	}
	if t := cm.Data["time"]; t != "" {
		line += sDim(" · deployed " + t)
	}
	fmt.Fprintf(e.out, "%s  %s\n", sDim("release"), line)
	return false
}

// imageProof is what status can prove about the first-party images. An
// annotation on a Deployment is a claim that anyone who can patch it can make, so
// it never counts as proof: an image is verified when the image the Deployment's
// pod template runs is exactly the one this build verified and mirrored, that is
// <this install's registry>/<cluster>/darkplum-<service>@<the release's digest>.
type imageProof struct {
	want    map[string]string // service -> the full image reference this build deploys; empty for a dev build
	version string            // the version of this build
}

// wantFromRecord is wantImages for an install whose registry is read from its
// darkplum-install record: the repository each service was mirrored into
// (image-<svc>, written by deploy), at the digest in this binary's manifest. A
// record that does not name every service's repository confirms nothing (nil),
// so "verified" is never said about a subset.
func wantFromRecord(m release.Manifest, data map[string]string) map[string]string {
	if len(m.Images) == 0 {
		return nil
	}
	out := map[string]string{}
	for svc, img := range m.Images {
		ref := data["image-"+svc]
		at := strings.Index(ref, "@")
		if at < 0 || !strings.HasSuffix(ref[:at], "/darkplum-"+svc) {
			return nil
		}
		out[svc] = ref[:at] + "@" + img.Digest
	}
	return out
}

// imageTrust judges the platform Deployments. It returns true for a failure.
func (e *statusEnv) imageTrust() (failed bool) {
	out, err := e.kc.NS(nil, "get", "deploy", "-o", "json")
	if err != nil {
		fmt.Fprintf(e.out, "%s  %s\n", sDim("images"), sDim("(unavailable: "+firstLine(err.Error())+")"))
		return false
	}
	var dl struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(out), &dl) != nil {
		return false
	}
	var unsigned, differ, rc []string
	matched, seen := 0, 0
	for _, it := range dl.Items {
		name := it.Metadata.Name
		svc := strings.TrimPrefix(name, "darkplum-")
		if !strings.HasPrefix(name, "darkplum-") {
			continue
		}
		seen++
		if it.Metadata.Annotations[annImageTrust] == "unsigned" {
			unsigned = append(unsigned, name)
		}
		if it.Metadata.Annotations[annChannel] == "rc" {
			rc = append(rc, name)
		}
		want, known := e.proof.want[svc]
		if !known {
			continue
		}
		ok := false
		for _, c := range it.Spec.Template.Spec.Containers {
			if strings.HasSuffix(strings.SplitN(strings.SplitN(c.Image, "@", 2)[0], ":", 2)[0], "/darkplum-"+svc) {
				ok = c.Image == want
			}
		}
		if ok {
			matched++
		} else {
			differ = append(differ, name)
		}
	}
	sort.Strings(unsigned)
	sort.Strings(differ)
	if len(unsigned) > 0 {
		fmt.Fprintf(e.out, "%s  %s %s\n", sDim("images"), sFail("✗ image-trust: unsigned"), sDim("("+strings.Join(unsigned, ", ")+")"))
		fmt.Fprintf(e.out, "        %s\n", sDim("These images were deployed without release signature verification (a development deploy). "+
			"Run `darkplum deploy` from a release build to replace them."))
		failed = true
	}
	switch {
	case len(e.proof.want) == 0 && seen > 0 && e.build != nil:
		fmt.Fprintf(e.out, "%s  %s\n", sDim("images"), sWarn("! the install has no record of where its images were copied (deployed by an older darkplum?); run `darkplum deploy` to record it, then status can confirm them"))
	case len(e.proof.want) == 0 && seen > 0:
		fmt.Fprintf(e.out, "%s  %s\n", sDim("images"), sWarn("! this darkplum is a development build: it cannot confirm which images run"))
	case len(differ) > 0 && e.recorded != "" && e.recorded == e.proof.version:
		// The install says it runs this very release, and the images are not its images.
		fmt.Fprintf(e.out, "%s  %s %s\n", sDim("images"), sFail("✗ not this release's images"), sDim("("+strings.Join(differ, ", ")+" run a different image than "+e.proof.version+" verified)"))
		failed = true
	case len(differ) > 0:
		fmt.Fprintf(e.out, "%s  %s\n", sDim("images"), sWarn("! "+strings.Join(differ, ", ")+" run images this darkplum ("+e.proof.version+") did not verify; run `darkplum deploy` to bring them to this release"))
	case matched > 0 && matched == len(e.proof.want) && len(unsigned) == 0:
		fmt.Fprintf(e.out, "%s  %s\n", sDim("images"), sSuccess("✓ verified: running the images "+e.proof.version+" signed (digests match)"))
	case matched > 0 && matched < len(e.proof.want):
		// Verified is a claim about every first-party image of the release. Some
		// being absent (not deployed, or unlisted) is not that.
		fmt.Fprintf(e.out, "%s  %s\n", sDim("images"), sWarn(fmt.Sprintf("! only %d of %d first-party images are confirmed as %s's; the rest are not running", matched, len(e.proof.want), e.proof.version)))
	}
	if len(rc) > 0 {
		fmt.Fprintf(e.out, "%s  %s\n", sDim("images"), sWarn("! channel rc: a prerelease build, not for production"))
	}
	return failed
}

// webUI shows where the team reaches the web UI.
func (e *statusEnv) webUI() {
	fmt.Fprintf(e.out, "\n%s\n", sHeader("▸ Web UI"))
	if e.tf != nil {
		if w := webAddressFrom(*e.tf); w.hostname != "" {
			fmt.Fprintf(e.out, "  %s\n", w.url())
			return
		}
	}
	fmt.Fprintln(e.out, "  cluster-internal; `darkplum configure` opens a tunnel to it")
}

// models shows the self-hosted side: nodes, GPUs and the default model's log,
// or that there is none.
func (e *statusEnv) models() {
	c := e.kc
	gpu := false
	if e.tf != nil {
		gpu = e.tf.GPUDesired > 0
	} else if _, err := c.Run(nil, "-n", inferenceNamespace, "get", "deploy", "vllm-inference", "-o", "name"); err == nil {
		gpu = true
	}
	if !gpu {
		fmt.Fprintf(e.out, "\n%s\n  none (Bedrock-only install)\n", sHeader("▸ Self-hosted models"))
		return
	}
	fmt.Fprintf(e.out, "\n%s\n", sHeader("▸ Nodes"))
	if out, err := c.Run(nil, "get", "nodes", "-L", "darkplum.ai/node-type"); err == nil {
		indent(e.out, out)
	} else {
		fmt.Fprintf(e.out, "  (unavailable: %s)\n", firstLine(err.Error()))
	}
	fmt.Fprintf(e.out, "\n%s\n", sHeader("▸ GPU allocatable"))
	out, err := c.Run(nil, "get", "nodes", "-l", "darkplum.ai/node-type=gpu-inference", "-o",
		`custom-columns=NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu,STATUS:.status.conditions[-1].type`)
	switch {
	case err != nil:
		fmt.Fprintf(e.out, "  (unavailable: %s)\n", firstLine(err.Error()))
	case strings.TrimSpace(out) == "" || len(strings.Split(strings.TrimSpace(out), "\n")) < 2:
		fmt.Fprintln(e.out, "  No GPU nodes found.")
	default:
		indent(e.out, out)
	}
	fmt.Fprintf(e.out, "\n%s\n", sHeader("▸ vLLM log (last 5 lines)"))
	logs, err := c.Run(nil, "-n", inferenceNamespace, "logs", "deploy/vllm-inference", "-c", "vllm", "--tail=5")
	if err != nil || strings.TrimSpace(logs) == "" {
		logs, err = c.Run(nil, "-n", inferenceNamespace, "logs", "deploy/vllm-inference", "-c", "model-download", "--tail=5")
	}
	if err != nil || strings.TrimSpace(logs) == "" {
		fmt.Fprintln(e.out, "  No vLLM pods running yet.")
		return
	}
	for _, l := range strings.Split(strings.TrimSpace(logs), "\n") {
		fmt.Fprintf(e.out, "  %s\n", sDim("│ "+l))
	}
}

func indent(w interface{ Write([]byte) (int, error) }, s string) {
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		fmt.Fprintf(w, "  %s\n", l)
	}
}
