package commands

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
)

// StatusCmd is `darkplum status`.
type StatusCmd struct {
	Context string `name:"context" help:"Local kind context (kind-*) to read, for a cluster Darkplum did not install. An install is found from its settings. A loopback address can still be an ssh tunnel to a remote cluster: the flag cannot tell, so do not point it at one. A loopback address can still be an ssh tunnel to a remote cluster: the flag cannot tell, so do not point it at one." default:""`
}

// Run executes `darkplum status`. Like every kube command it establishes and
// validates the install's cluster first, and refuses rather than read
// whatever cluster the current context points at.
func (s *StatusCmd) Run(g *Globals) error {
	c, err := installKube(os.Stdout, g.Namespace, s.Context, g.Install)
	if err != nil {
		return err
	}
	e := &statusEnv{out: os.Stdout, kc: c, ns: g.Namespace}
	// The install's own settings tell which sections apply (web address, GPU). A
	// cluster reached with --context has none; those sections are read live.
	if s.Context == "" {
		if in, _, err := openInstall(g.Install); err == nil {
			if tf, err := readTFVars(in.TFVars()); err == nil {
				e.tf = &tf
			}
		}
	}
	return e.run()
}

// statusEnv is what status reads, so tests can replace the cluster.
type statusEnv struct {
	out io.Writer
	kc  *kube.Client
	ns  string
	tf  *tfvarsInfo // nil when there is no install record (--context)
}

func (e *statusEnv) run() error {
	c := e.kc
	failed := e.release()

	// OIDC on/off is read from the auth secret (no DB access needed). deploy
	// writes the placeholder "unconfigured" when SSO is not set up; the api
	// treats that as off (auth/config.go), so status must too.
	issuer, _ := c.NS(nil, "get", "secret", "darkplum-auth",
		"-o", "jsonpath={.data.OIDC_ISSUER}")
	if oidcConfigured(issuer) {
		fmt.Fprintf(e.out, "%s     %s\n", sDim("auth"), sInfo("local + OIDC"))
	} else {
		fmt.Fprintf(e.out, "%s     %s\n", sDim("auth"), sInfo("local only"))
	}
	if e.imageTrust() {
		failed = true
	}

	anyNotReady := false
	for _, ns := range []string{e.ns, inferenceNamespace} {
		fmt.Fprintf(e.out, "\n%s\n", sHeader("▸ "+ns))
		out, err := c.Run(nil, "-n", ns, "get", "pods", "--no-headers")
		if err != nil {
			fmt.Fprintf(e.out, "  (unavailable: %v)\n", err)
			continue
		}
		out = strings.TrimSpace(out)
		if out == "" {
			fmt.Fprintln(e.out, "  (no pods)")
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			pod, ready, phase := fields[0], fields[1], fields[2]
			ok := phase == "Running" || phase == "Completed"
			if r := strings.SplitN(ready, "/", 2); ok && len(r) == 2 && phase == "Running" {
				ok = r[0] == r[1] // all containers ready
			}
			mark := sSuccess("✓")
			if !ok {
				mark = sFail("✗ not ready")
				anyNotReady = true
			}
			fmt.Fprintf(e.out, "  %s %-40s %s %s\n", mark, pod, sDim(fmt.Sprintf("%-7s", ready)), sDim(phase))
			if !ok {
				logs, _ := c.Run(nil, "-n", ns, "logs", pod, "--tail=3", "--all-containers")
				for _, l := range strings.Split(strings.TrimSpace(logs), "\n") {
					if l != "" {
						fmt.Fprintf(e.out, "      %s\n", sDim("│ "+l))
					}
				}
			}
		}
	}

	e.webUI()
	e.models()

	fmt.Fprintln(e.out)
	switch {
	case anyNotReady:
		return fmt.Errorf("one or more pods are not ready")
	case failed:
		return reportedError{"the images this install runs were not verified"}
	}
	fmt.Fprintln(e.out, sSuccess("✓ all pods ready"))
	return nil
}

// oidcConfigured reports whether a base64 OIDC_ISSUER from the auth secret
// names a real issuer rather than being empty or make deploy's placeholder.
func oidcConfigured(b64 string) bool {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return false
	}
	v := strings.TrimSpace(string(raw))
	return v != "" && v != "unconfigured"
}
