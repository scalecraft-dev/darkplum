package commands

import (
	"fmt"
	"strings"

	"github.com/scalecraft/darkplum/cli/internal/kube"
)

// StatusCmd is `darkplum status`.
type StatusCmd struct{}

// Run executes `darkplum status`.
func (s *StatusCmd) Run(g *Globals) error {
	c := kube.New(g.Namespace)

	if ctx, err := c.Run(nil, "config", "current-context"); err == nil {
		fmt.Printf("%s  %s\n", sDim("context"), strings.TrimSpace(ctx))
	}

	// OIDC on/off is read from the auth secret (no DB access needed).
	issuer, _ := c.NS(nil, "get", "secret", "darkplum-auth",
		"-o", "jsonpath={.data.OIDC_ISSUER}")
	if strings.TrimSpace(issuer) != "" {
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
