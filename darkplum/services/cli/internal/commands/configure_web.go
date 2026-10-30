package commands

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The install's private HTTPS address, as configure sees it. The settings come
// from the install settings (web_*), the NLB's addresses from Terraform's
// outputs; neither from the cluster.

const (
	trustedProxiesConfigMap = "darkplum-web-trusted-proxies"
	trustedProxiesKey       = "trusted-proxies.conf"
	webIngressPolicy        = "web-ingress"
)

// webAddress is the team's address from the install settings; zero when the
// install has none (web_domain unset).
type webAddress struct {
	hostname string
	allowed  []string
}

func (a webAddress) url() string { return "https://" + a.hostname }

// webAddressFrom reads web_domain / web_hostname / web_allowed_cidrs.
func webAddressFrom(tf tfvarsInfo) webAddress {
	first := func(k string) string {
		if v := tfStrings(tf.Raw[k]); len(v) > 0 {
			return v[0]
		}
		return ""
	}
	domain := first("web_domain")
	if domain == "" {
		return webAddress{}
	}
	a := webAddress{hostname: first("web_hostname"), allowed: tfStrings(tf.Raw["web_allowed_cidrs"])}
	if a.hostname == "" {
		a.hostname = "darkplum." + domain
	}
	if len(a.allowed) == 0 {
		vpc := first("vpc_cidr")
		if vpc == "" {
			vpc = defaultVPCCIDR
		}
		a.allowed = []string{vpc}
	}
	return a
}

// terraformOutputs runs `terraform output -json` in the install's Terraform
// root, under the install's AWS profile. It reads state; it changes nothing.
func terraformOutputs(tfvarsPath, profile string) (map[string]json.RawMessage, error) {
	cmd := exec.Command("terraform", "-chdir="+filepath.Dir(tfvarsPath), "output", "-json")
	cmd.Env = os.Environ()
	if profile != "" {
		cmd.Env = append(cmd.Env, "AWS_PROFILE="+profile)
	}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, errors.New(firstLine(strings.TrimSpace(string(ee.Stderr))))
		}
		return nil, err
	}
	var raw map[string]struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, errors.New("unexpected terraform output")
	}
	vals := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		vals[k] = v.Value
	}
	return vals, nil
}

// lbIPs reads web_lb_private_ips. Only private (RFC 1918) IPv4 addresses are
// accepted, IPv4-mapped IPv6 forms included in the refusal, and each is
// returned in canonical dotted form: these become nginx's trust list.
func lbIPs(outputs map[string]json.RawMessage) ([]string, error) {
	var raw []string
	if v, ok := outputs["web_lb_private_ips"]; ok {
		if err := json.Unmarshal(v, &raw); err != nil {
			return nil, errors.New("unexpected web_lb_private_ips output")
		}
	}
	ips := make([]string, 0, len(raw))
	for _, ip := range raw {
		p := net.ParseIP(ip)
		if p == nil || strings.Contains(ip, ":") || p.To4() == nil || !p.IsPrivate() {
			return nil, fmt.Errorf("unexpected load balancer address %q (want a private IPv4 address)", ip)
		}
		ips = append(ips, p.To4().String())
	}
	return ips, nil
}

// renderTrustedProxies is the nginx include: the only peers that may name the
// client in a PROXY protocol header.
func renderTrustedProxies(ips []string) string {
	var b strings.Builder
	b.WriteString("# Written by `darkplum configure` from the install's Terraform outputs\n# (web_lb_private_ips): the internal NLB's pinned addresses.\n")
	for _, ip := range ips {
		fmt.Fprintf(&b, "set_real_ip_from %s/32;\n", ip)
	}
	return b.String()
}

func labels() map[string]string {
	return map[string]string{"app.kubernetes.io/part-of": "darkplum", "app.kubernetes.io/managed-by": "darkplum-configure"}
}

func trustedProxiesManifest(ns, content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": trustedProxiesConfigMap, "namespace": ns, "labels": labels()},
		"data":     map[string]string{trustedProxiesKey: content},
	})
	return b
}

// webIngressManifest: 8443 (the NLB's PROXY-protocol port) only from the
// NLB's addresses. 80 stays open in-cluster: kubelet probes reach it from the
// node, and whether a NetworkPolicy sees those depends on the CNI's policy
// engine, so narrowing it risks failing readiness. It is not reachable from
// outside the cluster (darkplum-web is ClusterIP), and a peer on 80 is logged
// as itself: it cannot claim another address.
func webIngressManifest(ns string, ips []string) []byte {
	from := make([]map[string]any, len(ips))
	for i, ip := range ips {
		from[i] = map[string]any{"ipBlock": map[string]string{"cidr": ip + "/32"}}
	}
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": webIngressPolicy, "namespace": ns, "labels": labels()},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]string{"app.kubernetes.io/name": "darkplum-web"}},
			"policyTypes": []string{"Ingress"},
			"ingress": []any{
				map[string]any{"ports": []any{map[string]any{"protocol": "TCP", "port": 80}}},
				map[string]any{"from": from, "ports": []any{map[string]any{"protocol": "TCP", "port": 8443}}},
			},
		},
	})
	return b
}

// trustProxies points nginx at the NLB's addresses so the audit log records
// each browser's address, and limits the PROXY-protocol port to the NLB. It
// restarts web only when the list changed. Skipped for installs without a
// private address.
func (r *configureRun) trustProxies() {
	if r.web.hostname == "" {
		return
	}
	outputs, err := r.tfOutputs()
	var ips []string
	if err == nil {
		ips, err = lbIPs(outputs)
	}
	if err != nil {
		r.problem("audit", "cannot read the install's Terraform outputs: "+firstLine(err.Error()),
			"configure runs `terraform output` in terraform/aws, which the deploy initialised; check your AWS session for the install's profile, then re-run ./bin/darkplum configure")
		return
	}
	have, _ := r.kc.NS(nil, "get", "configmap", trustedProxiesConfigMap, "-o", `jsonpath={.data.trusted-proxies\.conf}`)
	if len(ips) == 0 {
		if strings.TrimSpace(have) != "" {
			// The address was taken away: do not keep trusting addresses that
			// may now belong to something else.
			r.untrustProxies()
			return
		}
		printCheckTo(r.out, checkResult{name: "audit", status: checkWarn, detail: "the private address is not deployed yet; client addresses are not recorded",
			hint: "deploy the install with its new settings, then re-run ./bin/darkplum configure"})
		return
	}
	want := renderTrustedProxies(ips)
	if _, err := r.kc.Run(webIngressManifest(r.namespace, ips), "apply", "-f", "-"); err != nil {
		r.problem("audit", "could not apply the web-ingress policy: "+firstLine(err.Error()), "re-run ./bin/darkplum configure")
		return
	}
	if strings.TrimSpace(have) != strings.TrimSpace(want) {
		if _, err := r.kc.Run(trustedProxiesManifest(r.namespace, want), "apply", "-f", "-"); err != nil {
			r.problem("audit", "could not write the trusted-proxy list: "+firstLine(err.Error()), "re-run ./bin/darkplum configure")
			return
		}
		fmt.Fprintf(r.out, "%s restarting web to load the load balancer's addresses…\n", sDim("…"))
		if _, err := r.kc.NS(nil, "rollout", "restart", "deployment/darkplum-web"); err != nil {
			r.problem("audit", "could not restart web: "+firstLine(err.Error()), "re-run ./bin/darkplum configure")
			return
		}
		if _, err := r.kc.NS(nil, "rollout", "status", "deployment/darkplum-web", "--timeout=180s"); err != nil {
			r.problem("audit", "web did not come back after the restart: "+firstLine(err.Error()), "see which pod is stuck: ./bin/darkplum status")
			return
		}
	}
	r.pass("audit", "client addresses recorded (trusts only the load balancer at "+strings.Join(ips, ", ")+")")
}

// untrustProxies removes the trust list and the web-ingress policy left from
// an address that no longer exists, and restarts web so nginx forgets it.
func (r *configureRun) untrustProxies() {
	for _, kind := range []string{"configmap/" + trustedProxiesConfigMap, "networkpolicy/" + webIngressPolicy} {
		if _, err := r.kc.NS(nil, "delete", kind, "--ignore-not-found"); err != nil {
			r.problem("audit", "could not remove "+kind+": "+firstLine(err.Error()), "re-run ./bin/darkplum configure")
			return
		}
	}
	if _, err := r.kc.NS(nil, "rollout", "restart", "deployment/darkplum-web"); err != nil {
		r.problem("audit", "could not restart web: "+firstLine(err.Error()), "re-run ./bin/darkplum configure")
		return
	}
	printCheckTo(r.out, checkResult{name: "audit", status: checkWarn, detail: "the private address is gone: removed the old load balancer trust list and web-ingress policy",
		hint: "client addresses are not recorded until the address is deployed again"})
}

// probeHTTPS asks the team's address for /health from this machine.
func probeHTTPS(rawURL string) error {
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	resp, err := c.Get(rawURL + "/health")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("answered %d", resp.StatusCode)
	}
	return nil
}

// ssoRedirectCheck warns when SSO sends people back somewhere other than the
// team's address (the IdP would then return them to the wrong place).
func (r *configureRun) ssoRedirectCheck() {
	if !r.sso || r.web.hostname == "" {
		return
	}
	enc, err := r.kc.NS(nil, "get", "secret", "darkplum-auth", "-o", "jsonpath={.data.OIDC_REDIRECT_URL}")
	if err != nil {
		return
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		return
	}
	want := r.web.url() + "/auth/callback"
	if u, perr := url.Parse(string(b)); perr == nil && strings.EqualFold(u.Hostname(), r.web.hostname) && u.Scheme == "https" {
		return
	}
	printCheckTo(r.out, checkResult{name: "sso", status: checkWarn, detail: "redirect URL is " + string(b) + ", not on " + r.web.url(),
		hint: "./bin/darkplum sso configure --redirect-url " + want + " (and add it at your identity provider)"})
}

// teamLines is the finish screen's part about the team's address.
func (r *configureRun) teamLines() {
	w := r.out
	if r.web.hostname == "" {
		fmt.Fprintf(w, "  %s\n", sDim("No team address yet: re-run ./bin/darkplum init to add a private HTTPS address, then deploy."))
		return
	}
	fmt.Fprintf(w, "  Your team: %s\n", sCmd(r.web.url()))
	if err := r.probe(r.web.url()); err != nil {
		fmt.Fprintf(w, "    %s %s\n", sWarn("!"), sDim("not reachable from this machine — expected unless you are on a network in "+strings.Join(r.web.allowed, ", ")))
	} else {
		fmt.Fprintf(w, "    %s %s\n", sSuccess("✓"), sDim("reachable from this machine"))
	}
	fmt.Fprintf(w, "  %s\n", sDim("An admin adds teammates in Admin > Users & Keys (name, email, password); there is no invite email."))
}
