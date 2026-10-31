package commands

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
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

// imageTrust reads the annotations on the platform Deployments. Unsigned images
// are a failure for as long as they run: nothing is signed-or-else, so the row
// stays until a release deploy replaces them. It returns true for that failure.
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
		} `json:"items"`
	}
	if json.Unmarshal([]byte(out), &dl) != nil {
		return false
	}
	var unsigned, verified, unrecorded, rc []string
	for _, it := range dl.Items {
		if !strings.HasPrefix(it.Metadata.Name, "darkplum-") {
			continue
		}
		switch it.Metadata.Annotations[annImageTrust] {
		case "unsigned":
			unsigned = append(unsigned, it.Metadata.Name)
		case "verified":
			verified = append(verified, it.Metadata.Name)
		default:
			unrecorded = append(unrecorded, it.Metadata.Name)
		}
		if it.Metadata.Annotations[annChannel] == "rc" {
			rc = append(rc, it.Metadata.Name)
		}
	}
	sort.Strings(unsigned)
	if len(unsigned) > 0 {
		fmt.Fprintf(e.out, "%s  %s %s\n", sDim("images"), sFail("✗ image-trust: unsigned"), sDim("("+strings.Join(unsigned, ", ")+")"))
		fmt.Fprintf(e.out, "        %s\n", sDim("These images were deployed without release signature verification (a development deploy). "+
			"Run `darkplum deploy` from a release build to replace them."))
		failed = true
	}
	switch {
	case len(verified) > 0 && len(unsigned) == 0 && len(unrecorded) == 0:
		fmt.Fprintf(e.out, "%s  %s\n", sDim("images"), sSuccess("✓ verified release images"))
	case len(unrecorded) > 0:
		fmt.Fprintf(e.out, "%s  %s\n", sDim("images"), sDim("image trust not recorded for "+strings.Join(unrecorded, ", ")+" (deployed before releases were verified)"))
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
