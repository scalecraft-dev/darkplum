package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
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
	Context     string `name:"context" help:"Local kind context (kind-*) to act on, for a cluster Darkplum did not install. An install is found from its settings. A loopback address can still be an ssh tunnel to a remote cluster: the flag cannot tell, so do not point it at one. A loopback address can still be an ssh tunnel to a remote cluster: the flag cannot tell, so do not point it at one." default:""`
}

// Run executes `darkplum sso configure`.
func (c *SSOConfigureCmd) Run(g *Globals) error {
	kc, err := installKube(os.Stdout, g.Namespace, c.Context, g.Install)
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

	if err := applySSO(os.Stdout, kc, g.Namespace, oidc); err != nil {
		return err
	}
	fmt.Println(sSuccess("✓ SSO configured.") + " " + sDim("The login page now offers 'Sign in with SSO' alongside password login."))
	fmt.Printf("%s\n  %s\n", sDim("Add this redirect URL to your identity provider:"), redirect)
	return nil
}

// applySSO stores the OIDC settings and restarts the api so they take effect.
func applySSO(w io.Writer, kc *kube.Client, ns string, oidc map[string]string) error {
	// The shared helper decides how: a Secret that does not exist (confirmed
	// NotFound, never a transient error) is created with every key the api reads,
	// SERVICE_KEY_SECRET included; one that exists gets the four OIDC values
	// written and any missing key added, and SESSION_SIGNING_KEY is never rotated.
	res, err := ensureAuthSecret(kc, ns, oidc)
	if err != nil {
		return err
	}
	if res.Created {
		fmt.Fprintln(w, sDim("note: darkplum-auth did not exist; generated a new SESSION_SIGNING_KEY and SERVICE_KEY_SECRET."))
	} else if len(res.Added) > 0 {
		fmt.Fprintln(w, sDim("note: darkplum-auth was missing "+strings.Join(res.Added, ", ")+"; added."))
	}

	fmt.Fprintln(w, sInfo("→ Restarting darkplum-api …"))
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
		fmt.Fprintln(w, sDim("note: pod is Ready (so OIDC initialized) but the startup log line was not captured — this is cosmetic."))
	}
	return nil
}
