package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
	"github.com/scalecraft-dev/darkplum/cli/internal/release"
)

// StatusCmd is `darkplum status`.
type StatusCmd struct {
	Context string `name:"context" help:"Local kind context (kind-*) to read, for a cluster Darkplum did not install. An install is found from its settings. A loopback address can still be an ssh tunnel to a remote cluster: the flag cannot tell, so do not point it at one." default:""`
}

// Run executes `darkplum status`. Like every kube command it establishes and
// validates the install's cluster first, and refuses rather than read
// whatever cluster the current context points at.
func (s *StatusCmd) Run(g *Globals) error {
	c, target, err := installKubeTarget(os.Stdout, g.Namespace, s.Context, g.Install)
	if err != nil {
		return err
	}
	e := &statusEnv{out: os.Stdout, kc: c, ns: g.Namespace, httpc: &http.Client{Timeout: 10 * time.Second}}
	if target != nil {
		e.fwd = func() Forwarder { return newKubectlForwarder(*target, g.Namespace) }
	}
	if m, err := release.Current(); err == nil {
		e.proof.version = m.Version
		// Where the images are comes from the install's own record (what deploy
		// mirrored), not from a guess at the registry: an adopted install may have
		// no single account, and the registry host is whatever deploy wrote.
		// What they must be, the digest, is this build's.
		e.build = &m
	}
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
	// proof is what this build can confirm about the images; recorded is the
	// release version the install's own record says it runs (read by release()).
	proof    imageProof
	build    *release.Manifest // this build's manifest; nil for a dev build
	recorded string
	// fwd opens a tunnel to the web service (nil for a local --context, which has
	// none); httpc reads through it. Tests replace both.
	fwd   func() Forwarder
	httpc *http.Client
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
	e.library()

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
// names a real issuer rather than being empty or deploy's placeholder.
func oidcConfigured(b64 string) bool {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return false
	}
	v := strings.TrimSpace(string(raw))
	return v != "" && v != "unconfigured"
}

// libraryStatus is GET /api/v1/admin/library/status.
type libraryStatus struct {
	// Enabled is false on an install without a bucket (the Library is in
	// Postgres alone); nil on an api that predates the field, which always
	// had one.
	Enabled             *bool   `json:"enabled"`
	Reachable           bool    `json:"reachable"`
	Bucket              string  `json:"bucket"`
	QueueDepth          int     `json:"queue_depth"`
	Failing             int     `json:"failing"`
	OldestQueuedSeconds *int    `json:"oldest_queued_seconds"`
	LastReconcileAt     *string `json:"last_reconcile_at"`
	ForeignObjects      int     `json:"foreign_objects"`
}

// library shows how the Library's bucket is doing, as the api sees it. It is
// informational: nothing here fails status (the bucket is never on the chat path).
// The route is an admin one and status holds no admin session, so an api that wants
// one is said to, rather than guessed at.
func (e *statusEnv) library() {
	fmt.Fprintf(e.out, "\n%s\n", sHeader("▸ Library"))
	if e.fwd == nil || e.httpc == nil {
		fmt.Fprintln(e.out, "  not read (a local --context has no tunnel to the api)")
		return
	}
	f := e.fwd()
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base, err := f.Open(ctx)
	if err != nil {
		fmt.Fprintf(e.out, "  %s could not be read: %s\n", sWarn("!"), firstLine(err.Error()))
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/admin/library/status", nil)
	if err != nil {
		fmt.Fprintf(e.out, "  %s could not be read: %s\n", sWarn("!"), firstLine(err.Error()))
		return
	}
	req.Header.Set("Accept", "application/json")
	resp, err := e.httpc.Do(req)
	if err != nil {
		fmt.Fprintf(e.out, "  %s could not be read: %s\n", sWarn("!"), firstLine(err.Error()))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		fmt.Fprintln(e.out, "  library: not available on this api")
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		fmt.Fprintf(e.out, "  %s library status needs an admin session: see Admin in the web UI\n", sWarn("!"))
	case resp.StatusCode != http.StatusOK:
		fmt.Fprintf(e.out, "  %s could not be read: the api answered %d\n", sWarn("!"), resp.StatusCode)
	default:
		var s libraryStatus
		if json.Unmarshal(body, &s) != nil {
			fmt.Fprintf(e.out, "  %s could not be read: unexpected answer\n", sWarn("!"))
			return
		}
		e.printLibrary(s)
	}
}

func (e *statusEnv) printLibrary(s libraryStatus) {
	if s.Enabled != nil && !*s.Enabled {
		fmt.Fprintf(e.out, "  %s Postgres %s\n", sSuccess("✓"), sDim("no bucket (library_bucket_enabled = true in terraform.tfvars adds one)"))
		return
	}
	mark, state := sSuccess("✓"), "reachable"
	if !s.Reachable {
		mark, state = sWarn("!"), "NOT reachable"
	}
	fmt.Fprintf(e.out, "  %s %s %s\n", mark, s.Bucket, sDim(state))
	oldest := "-"
	if s.OldestQueuedSeconds != nil {
		oldest = (time.Duration(*s.OldestQueuedSeconds) * time.Second).String()
	}
	reconciled := "never"
	if s.LastReconcileAt != nil && *s.LastReconcileAt != "" {
		reconciled = *s.LastReconcileAt
	}
	fmt.Fprintf(e.out, "    %s\n", sDim(fmt.Sprintf("queue %d (oldest %s) · failing %d · foreign objects %d · last reconcile %s",
		s.QueueDepth, oldest, s.Failing, s.ForeignObjects, reconciled)))
	if s.Failing > 0 {
		fmt.Fprintf(e.out, "    %s\n", sWarn(fmt.Sprintf("! %d item(s) keep failing to sync; they are listed by id in the api log", s.Failing)))
	}
}
