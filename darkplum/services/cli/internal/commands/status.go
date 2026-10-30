package commands

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// StatusCmd is `darkplum status`.
type StatusCmd struct {
	Context string `name:"context" help:"Kubeconfig context to read. Needed only where there are no install settings (run from darkplum/ after darkplum init)." default:""`
}

// Run executes `darkplum status`. Like every kube command it establishes and
// validates the install's cluster first, and refuses rather than read
// whatever cluster the current context points at.
func (s *StatusCmd) Run(g *Globals) error {
	c, err := installKube(os.Stdout, g.Namespace, s.Context)
	if err != nil {
		return err
	}

	// OIDC on/off is read from the auth secret (no DB access needed). make
	// deploy writes the placeholder "unconfigured" when SSO is not set up; the
	// api treats that as off (auth/config.go), so status must too.
	issuer, _ := c.NS(nil, "get", "secret", "darkplum-auth",
		"-o", "jsonpath={.data.OIDC_ISSUER}")
	if oidcConfigured(issuer) {
		fmt.Printf("%s     %s\n", sDim("auth"), sInfo("local + OIDC"))
	} else {
		fmt.Printf("%s     %s\n", sDim("auth"), sInfo("local only"))
	}

	anyNotReady := false
	for _, ns := range []string{g.Namespace, "darkplum-inference"} {
		fmt.Printf("\n%s\n", sHeader("▸ "+ns))
		out, err := c.Run(nil, "-n", ns, "get", "pods", "--no-headers")
		if err != nil {
			fmt.Printf("  (unavailable: %v)\n", err)
			continue
		}
		out = strings.TrimSpace(out)
		if out == "" {
			fmt.Println("  (no pods)")
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
			fmt.Printf("  %s %-40s %s %s\n", mark, pod, sDim(fmt.Sprintf("%-7s", ready)), sDim(phase))
			if !ok {
				logs, _ := c.Run(nil, "-n", ns, "logs", pod, "--tail=3", "--all-containers")
				for _, l := range strings.Split(strings.TrimSpace(logs), "\n") {
					if l != "" {
						fmt.Printf("      %s\n", sDim("│ "+l))
					}
				}
			}
		}
	}

	fmt.Println()
	if anyNotReady {
		return fmt.Errorf("one or more pods are not ready")
	}
	fmt.Println(sSuccess("✓ all pods ready"))
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
