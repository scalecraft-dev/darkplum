package commands

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
)

// AdminCmd groups admin-account operations.
type AdminCmd struct {
	Create AdminCreateCmd `cmd:"" help:"Create a local break-glass admin (works even when SSO is down)."`
}

// AdminCreateCmd is `darkplum admin create`.
type AdminCreateCmd struct {
	Email         string `help:"Admin email address." required:""`
	Name          string `help:"Admin display name." required:""`
	PasswordStdin bool   `name:"password-stdin" help:"Read the password from stdin instead of prompting."`
	Context       string `name:"context" help:"Local kind context (kind-*) to act on, for a cluster Darkplum did not install. An install is found from its settings. A loopback address can still be an ssh tunnel to a remote cluster: the flag cannot tell, so do not point it at one." default:""`
}

// waitForJob polls until the Job reaches Complete or Failed. We poll rather than
// `kubectl wait --for=condition=complete` because that blocks the full timeout
// when the Job fails — unacceptable during a lockout recovery, where the
// operator needs the failure (and its diagnostics) immediately.
func waitForJob(c *kube.Client, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := c.NS(nil, "get", "job", name, "-o",
			"jsonpath={range .status.conditions[*]}{.type}={.status} {end}")
		switch {
		case strings.Contains(out, "Complete=True"):
			return nil
		case strings.Contains(out, "Failed=True"):
			return errors.New("bootstrap job failed")
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("bootstrap job did not finish within %s", timeout)
}

// Run executes `darkplum admin create`.
func (c *AdminCreateCmd) Run(g *Globals) error {
	// Resolve the cluster first, so a wrong context fails before the prompt.
	kc, err := installKube(os.Stdout, g.Namespace, c.Context, g.Install)
	if err != nil {
		return err
	}

	password, err := readPassword(c.PasswordStdin, "New admin password")
	if err != nil {
		return err
	}
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}

	// Run the bootstrap from the SAME image the api deployment runs, so it rides
	// the existing signed-image admission gate (no second artifact).
	image, err := kc.NS(nil, "get", "deploy", "darkplum-api",
		"-o", "jsonpath={.spec.template.spec.containers[0].image}")
	if err != nil {
		return fmt.Errorf("read darkplum-api image: %w", err)
	}
	image = strings.TrimSpace(image)
	if image == "" {
		return errors.New("darkplum-api deployment not found in namespace " + g.Namespace)
	}

	res := "darkplum-admin-bootstrap-" + randSuffix()

	// Short-lived Secret carries the password into the cluster (over the TLS
	// kube API) so it never appears in argv; deleted on the way out.
	if _, err := kc.Run(secretYAML(res, g.Namespace, password), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("create bootstrap secret: %w", err)
	}
	defer kc.NS(nil, "delete", "secret", res, "--ignore-not-found") //nolint:errcheck

	// One-shot Job. The admin subcommand never initializes OIDC, so this works
	// even when the api deployment itself is crash-looping on bad SSO config.
	if _, err := kc.Run(jobYAML(res, g.Namespace, image, c.Email, c.Name), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("create bootstrap job: %w", err)
	}
	defer kc.NS(nil, "delete", "job", res, "--ignore-not-found") //nolint:errcheck

	fmt.Println(sInfo("→ Creating break-glass admin " + c.Email + " …"))
	if err := waitForJob(kc, res, 90*time.Second); err != nil {
		logs, _ := kc.NS(nil, "logs", "job/"+res)
		detail := strings.TrimSpace(logs)
		if detail == "" {
			// Pod never produced logs (e.g. admission-rejected before start);
			// surface the Job's events instead.
			events, _ := kc.NS(nil, "get", "events", "--field-selector", "involvedObject.name="+res, "--no-headers")
			detail = strings.TrimSpace(events)
		}
		return fmt.Errorf("%w\n%s", err, detail)
	}

	fmt.Println(sSuccess("✓ Created break-glass admin " + c.Email + "."))
	fmt.Println(sDim("  Recorded in the audit log. Keep at least one local password admin so SSO failure can't lock you out."))
	return nil
}

func randSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func secretYAML(name, ns, password string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
  labels:
    darkplum.ai/bootstrap: "true"
type: Opaque
stringData:
  password: %s
`, name, ns, yamlSingleQuote(password)))
}

// jobYAML renders the one-shot bootstrap Job. It deliberately specifies no
// serviceAccountName, so it runs as the same `default` SA as the api deployment
// and on the same (signed) image digest — inheriting the api's exact admission
// profile, so it passes any image-trust gate the api already passes.
func jobYAML(name, ns, image, email, adminName string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %s
  namespace: %s
  labels:
    darkplum.ai/bootstrap: "true"
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 120
  template:
    metadata:
      labels:
        darkplum.ai/bootstrap: "true"
    spec:
      restartPolicy: Never
      containers:
        - name: admin
          image: %s
          imagePullPolicy: IfNotPresent
          args: ["admin", "create", "--email", %s, "--name", %s]
          env:
            - name: ADMIN_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: %s
                  key: password
          envFrom:
            - secretRef:
                name: darkplum-auth
            - secretRef:
                name: postgres-credentials
`, name, ns, image, yamlSingleQuote(email), yamlSingleQuote(adminName), name))
}
