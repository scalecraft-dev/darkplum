package commands

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SSOCmd groups SSO/OIDC operations.
type SSOCmd struct {
	Configure SSOConfigureCmd `cmd:"" help:"Configure or update OIDC/SSO (client secret read from stdin, never argv)."`
}

// SSOConfigureCmd is `darkplum sso configure`.
type SSOConfigureCmd struct {
	Issuer      string `help:"OIDC issuer URL."`
	ClientID    string `name:"client-id" help:"OIDC client ID."`
	RedirectURL string `name:"redirect-url" help:"OIDC redirect URL."`
	SecretStdin bool   `name:"secret-stdin" help:"Read the client secret from stdin instead of prompting."`
	Context     string `name:"context" help:"Kubeconfig context to act on. Needed only where there are no install settings (run from darkplum/ after darkplum init)." default:""`
}

// Run executes `darkplum sso configure`.
func (c *SSOConfigureCmd) Run(g *Globals) error {
	kc, err := installKube(os.Stdout, g.Namespace, c.Context)
	if err != nil {
		return err
	}
	fmt.Println(sDim(fmt.Sprintf("Configuring SSO in namespace %q", g.Namespace)))

	issuer := promptIfEmpty(c.Issuer, "OIDC Issuer URL (e.g. https://acme.us.auth0.com/)")
	clientID := promptIfEmpty(c.ClientID, "OIDC Client ID")
	clientSecret, err := readPassword(c.SecretStdin, "OIDC Client Secret")
	if err != nil {
		return err
	}
	redirect := promptIfEmpty(c.RedirectURL, "OIDC Redirect URL (e.g. https://darkplum.acme.com/auth/callback)")

	if issuer == "" || clientID == "" || clientSecret == "" || redirect == "" {
		return errors.New("issuer, client-id, client-secret and redirect-url are all required")
	}

	oidc := map[string]string{
		"OIDC_ISSUER":        issuer,
		"OIDC_CLIENT_ID":     clientID,
		"OIDC_CLIENT_SECRET": clientSecret,
		"OIDC_REDIRECT_URL":  redirect,
	}

	// If the auth secret already exists, MERGE-patch only the OIDC keys. This
	// preserves SESSION_SIGNING_KEY (and SECURE_COOKIES etc.) untouched — so we
	// can never accidentally rotate the signing key and log everyone out.
	_, getErr := kc.NS(nil, "get", "secret", "darkplum-auth")
	switch {
	case getErr == nil:
		patch, _ := json.Marshal(map[string]any{"stringData": oidc})
		if _, err := kc.NS(patch, "patch", "secret", "darkplum-auth",
			"--type", "merge", "--patch-file", "/dev/stdin"); err != nil {
			return fmt.Errorf("patch auth secret: %w", err)
		}
	case strings.Contains(getErr.Error(), "NotFound"):
		// Genuinely absent — create it with a freshly generated signing key. We
		// only reach the key-generating path on a confirmed NotFound, never on a
		// transient error (which could otherwise rotate the key and log everyone out).
		key, err := randomKey()
		if err != nil {
			return err
		}
		fmt.Println(sDim("note: darkplum-auth did not exist; generating a new SESSION_SIGNING_KEY."))
		if _, err := kc.Run(newAuthSecretYAML(g.Namespace, oidc, key), "apply", "-f", "-"); err != nil {
			return fmt.Errorf("create auth secret: %w", err)
		}
	default:
		return fmt.Errorf("could not read darkplum-auth secret (refusing to proceed): %w", getErr)
	}

	fmt.Println(sInfo("→ Restarting darkplum-api …"))
	if _, err := kc.NS(nil, "rollout", "restart", "deployment/darkplum-api"); err != nil {
		return fmt.Errorf("rollout restart: %w", err)
	}
	if _, err := kc.NS(nil, "rollout", "status", "deployment/darkplum-api", "--timeout=180s"); err != nil {
		return fmt.Errorf("darkplum-api did not become ready after restart — the OIDC config is likely invalid.\n"+
			"Fix the values and re-run, or run `darkplum admin create` to recover local access.\n%w", err)
	}

	// The api fails closed at boot on a bad OIDC config (provider discovery
	// errors -> process exits -> never Ready), so the rollout reaching Ready above
	// already proves OIDC initialized. Confirm from logs as a courtesy, but don't
	// fail on a miss — `kubectl logs deployment/...` can briefly read the
	// terminating pod right after a rollout cutover.
	logs, _ := kc.NS(nil, "logs", "deployment/darkplum-api", "--tail=200")
	if !strings.Contains(logs, "OIDC authentication enabled") {
		fmt.Println(sDim("note: pod is Ready (so OIDC initialized) but the startup log line was not captured — this is cosmetic."))
	}

	fmt.Println(sSuccess("✓ SSO configured.") + " " + sDim("The login page now offers 'Sign in with SSO' alongside password login."))
	fmt.Printf("%s\n  %s\n", sDim("Add this redirect URL to your identity provider:"), redirect)
	return nil
}

func randomKey() (string, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func newAuthSecretYAML(ns string, oidc map[string]string, signingKey string) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: darkplum-auth\n  namespace: %s\ntype: Opaque\nstringData:\n", ns)
	for _, k := range []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_REDIRECT_URL"} {
		fmt.Fprintf(&sb, "  %s: %s\n", k, yamlSingleQuote(oidc[k]))
	}
	fmt.Fprintf(&sb, "  SESSION_SIGNING_KEY: %s\n", yamlSingleQuote(signingKey))
	return []byte(sb.String())
}
