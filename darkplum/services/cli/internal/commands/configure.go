package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
)

// ConfigureCmd is `darkplum configure`: the guided first run after make
// deploy. It reaches the running install through a private tunnel to the web
// service, creates the first admin the same way the web first-run screen does,
// enables models, sends one test chat, and leaves the tunnel open for the
// browser. Each step checks what is already done, so it is safe to re-run.
type ConfigureCmd struct {
	Yes         bool          `help:"Take every default: enables every ready model for everyone who can sign in, and waits for a loading self-hosted model when nothing else is ready. Still asks for the admin email and password; a password is never read from a flag, the environment or a pipe."`
	Wait        bool          `help:"Wait for the self-hosted model to finish loading, then enable it and chat with it once."`
	WaitTimeout time.Duration `name:"wait-timeout" help:"How long to wait for the self-hosted model." default:"40m"`
	Tfvars      string        `help:"Install settings written by darkplum init (default: terraform/aws/terraform.tfvars)." default:""`
}

// configureEnv is everything configure touches outside its flags, so tests
// swap the cluster lookup, kubectl, the tunnel, the clock and the prompter.
type configureEnv struct {
	out         io.Writer
	prompt      Prompter
	interactive bool // ask questions that have a default (false under --yes)
	namespace   string
	resolve     func() (clusterTarget, checkResult, error)
	newKube     func(t clusterTarget, namespace string) *kube.Client
	newFwd      func(t clusterTarget) Forwarder
	httpc       *http.Client
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error
	hold        bool // keep the tunnel open on the finish screen until Ctrl-C

	web       webAddress                                 // from the install settings
	tfOutputs func() (map[string]json.RawMessage, error) // `terraform output -json`, read-only
	probe     func(url string) error                     // HTTPS reachability from this machine
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run executes `darkplum configure`.
func (c *ConfigureCmd) Run(g *Globals) error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return errors.New("configure asks for the admin password, so it needs a terminal (stdin is not one)")
	}
	path := c.Tfvars
	if path == "" {
		path = defaultTFVars
	}
	var (
		mu  sync.Mutex
		fwd Forwarder
	)
	env := &configureEnv{
		out:         os.Stdout,
		prompt:      newTermPrompter(),
		interactive: !c.Yes,
		namespace:   g.Namespace,
		resolve: func() (clusterTarget, checkResult, error) {
			return installCluster(path, runWithStderr)
		},
		newKube: func(t clusterTarget, ns string) *kube.Client { return t.kube(ns) },
		newFwd: func(t clusterTarget) Forwarder {
			mu.Lock()
			defer mu.Unlock()
			fwd = newKubectlForwarder(t, g.Namespace)
			return fwd
		},
		httpc: &http.Client{Timeout: 3 * time.Minute},
		now:   time.Now,
		sleep: sleepCtx,
		hold:  true,
		probe: probeHTTPS,
	}
	if tf, err := readTFVars(path); err == nil {
		env.web = webAddressFrom(tf)
		env.tfOutputs = func() (map[string]json.RawMessage, error) { return terraformOutputs(path, tf.Profile) }
	}

	// Ctrl-C (or closing the terminal: SIGHUP) cancels the run, which closes the tunnel on its way out. A
	// prompt blocked reading the terminal cannot see the cancel, so if the
	// run has not ended shortly after, stop the tunnel, put the terminal back
	// (a password prompt turns echo off) and exit.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	termState, _ := term.GetState(fd)
	finished := make(chan struct{})
	go func() {
		select {
		case <-sigs:
		case <-finished:
			return
		}
		cancel()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			mu.Lock()
			if fwd != nil {
				fwd.Close()
			}
			mu.Unlock()
			if termState != nil {
				_ = term.Restore(fd, termState)
			}
			fmt.Println()
			os.Exit(130)
		}
	}()
	err := c.execute(ctx, env)
	close(finished)
	return err
}

// configureRun is one run's state.
type configureRun struct {
	*configureEnv
	cmd   *ConfigureCmd
	t     clusterTarget
	kc    *kube.Client
	fwd   Forwarder
	api   *apiClient
	email string // who to sign in as, for the finish screen
	sso   bool

	reconnected bool
	problems    int    // ✗ rows that did not stop the run
	note        string // shown on the finish screen
}

// fail shows a ✗ row and returns an error main will not print again.
func (r *configureRun) fail(name, detail, hint, msg string) error {
	printCheckTo(r.out, checkResult{name: name, status: checkFail, detail: detail, hint: hint})
	return reportedError{msg}
}

// problem shows a ✗ row and carries on; the run then exits non-zero.
func (r *configureRun) problem(name, detail, hint string) {
	printCheckTo(r.out, checkResult{name: name, status: checkFail, detail: detail, hint: hint})
	r.problems++
}

func (r *configureRun) pass(name, detail string) {
	printCheckTo(r.out, checkResult{name: name, status: checkPass, detail: detail})
}

func (c *ConfigureCmd) execute(ctx context.Context, env *configureEnv) error {
	w := env.out
	fmt.Fprintf(w, "%s\n\n", sHeader("▸ configure"))

	t, row, err := env.resolve()
	printCheckTo(w, row)
	if err != nil {
		return reportedError{"cannot reach the install's cluster"}
	}

	r := &configureRun{configureEnv: env, cmd: c, t: t, kc: env.newKube(t, env.namespace)}
	if err := r.waitServices(ctx); err != nil {
		return err
	}
	// Before the tunnel: it may restart web.
	r.trustProxies()

	r.fwd = env.newFwd(t)
	defer r.fwd.Close()
	base, err := r.fwd.Open(ctx)
	if err != nil {
		return r.fail("connection", firstLine(err.Error()), "check the services with ./bin/darkplum status, then re-run", "could not open a connection to the web service")
	}
	r.api = &apiClient{fwd: r.fwd, http: env.httpc, base: base, cookies: map[string]string{}, reconnect: r.reconnect}
	// Registered after the Close above, so it runs first: end the CLI's own
	// session while the tunnel is still up. Errors do not matter here.
	defer r.api.logout()

	cfg, err := r.waitAPI(ctx)
	if err != nil {
		return err
	}
	r.pass("connection", "private, from this machine only")

	signedIn, err := r.admin(ctx, cfg)
	if err != nil {
		return err
	}
	r.ssoRedirectCheck()
	if signedIn {
		if err := r.modelsAndChat(ctx); err != nil {
			return err
		}
	}
	return r.finish(ctx)
}

// reconnect reopens a tunnel that died (the web pod restarted), once per run.
func (r *configureRun) reconnect(ctx context.Context) (string, error) {
	if r.reconnected {
		return "", errors.New("it closed again after reconnecting")
	}
	r.reconnected = true
	printCheckTo(r.out, checkResult{name: "connection", status: checkWarn, detail: "dropped (the web service may have restarted); reconnecting"})
	return r.fwd.Open(ctx)
}

// waitServices waits until api, web and gateway each have every replica ready.
func (r *configureRun) waitServices(ctx context.Context) error {
	const timeout = 5 * time.Minute
	start := r.now()
	lastSaid := start
	for {
		out, err := r.kc.NS(nil, "get", "deploy", "darkplum-api", "darkplum-web", "darkplum-gateway",
			"-o", `jsonpath={range .items[*]}{.metadata.name}={.status.readyReplicas}/{.spec.replicas}{"\n"}{end}`)
		if err != nil {
			if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "not found") {
				return r.fail("services", "darkplum is not deployed on this cluster", "deploy the install first", "darkplum is not deployed")
			}
			return r.fail("services", firstLine(err.Error()), "check your AWS session, then re-run", "cannot read the services")
		}
		var waiting []string
		seen := 0
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			name, counts, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			seen++
			ready, want, _ := strings.Cut(counts, "/")
			if want == "" || want == "0" || ready != want {
				waiting = append(waiting, strings.TrimPrefix(name, "darkplum-"))
			}
		}
		if seen == 3 && len(waiting) == 0 {
			r.pass("services", "api, web and gateway ready")
			return nil
		}
		if seen < 3 {
			return r.fail("services", "darkplum is only partly deployed", "finish the deploy, then re-run", "darkplum is only partly deployed")
		}
		if r.now().Sub(start) >= timeout {
			return r.fail("services", "not ready after 5 min: "+strings.Join(waiting, ", "), "see which pod is stuck: ./bin/darkplum status", "services not ready")
		}
		if r.now().Sub(lastSaid) >= 30*time.Second || lastSaid == start {
			fmt.Fprintf(r.out, "%s waiting for %s to be ready…\n", sDim("…"), strings.Join(waiting, ", "))
			lastSaid = r.now()
		}
		if err := r.sleep(ctx, 5*time.Second); err != nil {
			return reportedError{"stopped"}
		}
	}
}

// waitAPI reads /auth/config through the tunnel. Right after a deploy nginx
// can answer 502/503 for a moment; those are retried for up to two minutes.
func (r *configureRun) waitAPI(ctx context.Context) (authConfig, error) {
	deadline := r.now().Add(2 * time.Minute)
	unknown := 0
	var last string
	for {
		resp, err := r.api.do(ctx, http.MethodGet, "/auth/config", nil, true)
		switch {
		case errors.Is(err, errForwardLost):
			return authConfig{}, r.fail("connection", firstLine(err.Error()), "re-run ./bin/darkplum configure", "connection lost")
		case err != nil:
			last = firstLine(err.Error())
		case resp.status == http.StatusOK:
			var cfg authConfig
			if jerr := json.Unmarshal(resp.body, &cfg); jerr != nil {
				return cfg, r.fail("api", "unexpected answer from /auth/config", "check the services with ./bin/darkplum status", "unexpected api answer")
			}
			if cfg.OIDC.Enabled || cfg.SetupRequired != nil {
				return cfg, nil
			}
			// The api omits setup_required when it cannot count users.
			unknown++
			last = "the api could not read its database"
			if unknown >= 4 {
				return cfg, r.fail("api", last, "check the api and its database: ./bin/darkplum status", "api database unavailable")
			}
		case resp.status == http.StatusBadGateway || resp.status == http.StatusServiceUnavailable || resp.status == http.StatusGatewayTimeout:
			last = fmt.Sprintf("the api is not answering yet (%d)", resp.status)
		default:
			msg, _ := resp.errMessage()
			return authConfig{}, r.fail("api", fmt.Sprintf("%d %s", resp.status, msg), "check the services with ./bin/darkplum status", "unexpected api answer")
		}
		if r.now().After(deadline) {
			return authConfig{}, r.fail("api", last, "check the services with ./bin/darkplum status", "api not answering")
		}
		if err := r.sleep(ctx, 3*time.Second); err != nil {
			return authConfig{}, reportedError{"stopped"}
		}
	}
}

// admin creates the first admin when there is none, else signs in. It
// returns whether there is a session to enable models with.
func (r *configureRun) admin(ctx context.Context, cfg authConfig) (bool, error) {
	if cfg.OIDC.Enabled {
		r.sso = true
		printCheckTo(r.out, checkResult{name: "first admin", status: checkWarn, detail: "SSO is on: admins sign in through your identity provider",
			hint: "enable models under Admin > Models after signing in"})
		ok := false
		if r.interactive {
			var err error
			if ok, err = r.prompt.Confirm("Sign in with a local password admin to enable models here?", false); err != nil {
				return false, err
			}
		}
		if !ok {
			return false, nil
		}
		return r.login(ctx)
	}
	if *cfg.SetupRequired {
		return r.createAdmin(ctx)
	}
	r.pass("first admin", "already set up")
	fmt.Fprintf(r.out, "  %s\n", sDim("Sign in to enable models and send a test chat."))
	return r.login(ctx)
}

func validateEmail(s string) error {
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || !strings.Contains(domain, ".") || strings.ContainsAny(s, " \t") || strings.HasSuffix(domain, ".") {
		return errors.New("enter an email address, like you@example.com")
	}
	return nil
}

func validateName(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("enter a name")
	}
	return nil
}

// nameFromEmail suggests a display name: jane.doe@x → Jane Doe.
func nameFromEmail(email string) string {
	local, _, _ := strings.Cut(email, "@")
	local, _, _ = strings.Cut(local, "+")
	words := strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '_' || r == '-' })
	for i, w := range words {
		r := []rune(w)
		words[i] = strings.ToUpper(string(r[:1])) + string(r[1:])
	}
	return strings.Join(words, " ")
}

// secret reads a password, turning the no-terminal refusal into a ✗ row.
func (r *configureRun) secret(label string) (string, error) {
	v, err := r.prompt.AskSecret(label)
	if errors.Is(err, errNoTerminal) {
		return "", r.fail("password", "needs a terminal", "run ./bin/darkplum configure in a terminal; a password is never read from a pipe, flag or the environment", "no terminal for the password")
	}
	return v, err
}

func (r *configureRun) newPassword() (string, error) {
	for {
		pw, err := r.secret("Password (8+ characters)")
		if err != nil {
			return "", err
		}
		if utf8.RuneCountInString(pw) < 8 {
			fmt.Fprintf(r.out, "  %s password must be at least 8 characters\n", sFail("✗"))
			continue
		}
		again, err := r.secret("Password again")
		if err != nil {
			return "", err
		}
		if again != pw {
			fmt.Fprintf(r.out, "  %s the passwords do not match, try again\n", sFail("✗"))
			continue
		}
		return pw, nil
	}
}

func (r *configureRun) createAdmin(ctx context.Context) (bool, error) {
	fmt.Fprintf(r.out, "  %s\n", sDim("No admin yet. Create the first one; it can manage users, models and settings."))
	email, err := r.prompt.Ask("Admin email", "", validateEmail)
	if err != nil {
		return false, err
	}
	name := nameFromEmail(email)
	if r.interactive || name == "" {
		if name, err = r.prompt.Ask("Name", name, validateName); err != nil {
			return false, err
		}
	}
	pw, err := r.newPassword()
	if err != nil {
		return false, err
	}
	resp, err := r.api.do(ctx, http.MethodPost, "/auth/setup",
		map[string]string{"email": email, "name": name, "password": pw}, false)
	if err != nil {
		return false, r.fail("first admin", "the connection dropped while creating it", "re-run ./bin/darkplum configure; it checks whether the admin was created", "connection lost during setup")
	}
	switch resp.status {
	case http.StatusCreated, http.StatusOK:
		r.email = email
		r.pass("first admin", email+" created")
		return true, nil
	case http.StatusConflict:
		return false, r.fail("first admin", "someone else created the first admin moments ago",
			"nothing was changed; sign in with that account, or ask whoever created it to add you", "first admin already claimed")
	default:
		msg, _ := resp.errMessage()
		return false, r.fail("first admin", fmt.Sprintf("refused (%d): %s", resp.status, msg), "re-run ./bin/darkplum configure", "first admin not created")
	}
}

// login signs in with a local password. The api rate-limits sign-ins, so a
// wrong password is re-asked at most twice, by the person, never automatically.
func (r *configureRun) login(ctx context.Context) (bool, error) {
	email, err := r.prompt.Ask("Admin email", "", validateEmail)
	if err != nil {
		return false, err
	}
	const attempts = 3
	for i := 1; ; i++ {
		pw, err := r.secret("Password")
		if err != nil {
			return false, err
		}
		resp, err := r.api.do(ctx, http.MethodPost, "/auth/login", map[string]string{"email": email, "password": pw}, false)
		if err != nil {
			return false, r.fail("sign in", firstLine(err.Error()), "re-run ./bin/darkplum configure", "sign-in failed")
		}
		switch resp.status {
		case http.StatusOK:
			r.email = email
			r.pass("sign in", email)
			return true, nil
		case http.StatusUnauthorized:
			if i < attempts {
				fmt.Fprintf(r.out, "  %s email or password is wrong, try again\n", sFail("✗"))
				continue
			}
			return false, r.fail("sign in", "email or password is wrong",
				"locked out? ./bin/darkplum admin create adds another admin", "sign-in failed")
		case http.StatusTooManyRequests:
			return false, r.fail("sign in", "too many sign-in attempts", "wait a minute, then re-run ./bin/darkplum configure", "sign-in rate limited")
		default:
			msg, _ := resp.errMessage()
			return false, r.fail("sign in", fmt.Sprintf("refused (%d): %s", resp.status, msg), "re-run ./bin/darkplum configure", "sign-in failed")
		}
	}
}

// vllm reads the self-hosted model's pod. A read error counts as "no pod":
// configure then carries on with whatever the api lists.
func (r *configureRun) vllm() vllmState {
	out, err := r.kc.Run(nil, "-n", inferenceNamespace, "get", "pods", "-l", vllmSelector, "-o", "json")
	if err != nil {
		return vllmState{}
	}
	return vllmStateFromPods([]byte(out))
}

// modelsAndChat enables models and sends the test chat.
func (r *configureRun) modelsAndChat(ctx context.Context) error {
	ms, err := r.api.models(ctx)
	if err != nil {
		r.problem("models", firstLine(err.Error()), "enable models under Admin > Models in the web UI")
		return nil
	}
	vs := r.vllm()
	ms, err = r.enableModels(ctx, ms, vs)
	if err != nil {
		return err
	}

	m, ok := chatModel(ms)
	if !ok {
		if !vs.present || vs.broken {
			return nil // already shown as a ✗ row
		}
		wait := r.cmd.Wait
		if !wait && r.interactive {
			if wait, err = r.prompt.Confirm("Nothing is ready to chat with yet. Wait for the self-hosted model? (Ctrl-C stops waiting)", true); err != nil {
				return err
			}
		} else if !wait {
			wait = true // the default answer, under --yes
		}
		if !wait {
			r.note = "The self-hosted model is still loading. Re-run ./bin/darkplum configure once it is ready to enable it."
			return nil
		}
		if m, err = r.waitSelfHosted(ctx); err != nil {
			return err
		}
		return r.chat(ctx, m)
	}
	if err := r.chat(ctx, m); err != nil {
		return err
	}
	if r.cmd.Wait && vs.present && !vs.broken && !hasReadySelfHosted(ms) {
		sm, err := r.waitSelfHosted(ctx)
		if err != nil {
			return err
		}
		return r.chat(ctx, sm)
	}
	return nil
}

func hasReadySelfHosted(ms []adminModel) bool {
	for _, m := range ms {
		if m.selfHosted() && m.ready() && m.Enabled {
			return true
		}
	}
	return false
}

// chatModel picks the model for the test chat: an enabled, ready one, a
// cloud-managed one first (it answers now; a self-hosted one may be loading).
func chatModel(ms []adminModel) (adminModel, bool) {
	var fallback *adminModel
	for i, m := range ms {
		if !m.Enabled || !m.ready() {
			continue
		}
		if !m.selfHosted() {
			return m, true
		}
		if fallback == nil {
			fallback = &ms[i]
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return adminModel{}, false
}

// enableModels shows what is enabled, asks which others to enable (every
// ready one by default) and enables them. It never disables anything. It
// returns the list with the new enabled flags.
func (r *configureRun) enableModels(ctx context.Context, ms []adminModel, vs vllmState) ([]adminModel, error) {
	var candidates []int
	for i, m := range ms {
		if m.Enabled {
			detail := m.kind() + " · enabled"
			if !m.ready() {
				detail += " · " + m.state()
			}
			r.pass(m.label(), detail)
			continue
		}
		candidates = append(candidates, i)
	}

	if vs.present && !hasListedReadySelfHosted(ms) {
		switch {
		case vs.broken:
			r.problem("self-hosted model", vs.phase, vs.hint)
		case vs.ready:
			printCheckTo(r.out, checkResult{name: "self-hosted model", status: checkWarn, detail: "loaded; the gateway lists it within a minute",
				hint: "re-run ./bin/darkplum configure in a minute to enable it, or pass --wait"})
		default:
			printCheckTo(r.out, checkResult{name: "self-hosted model", status: checkWarn, detail: vs.phase + " (15-25 min after deploy)",
				hint: "it needs enabling once it is ready: re-run ./bin/darkplum configure then, or pass --wait"})
		}
	}

	if len(ms) == 0 && !vs.present {
		r.problem("models", "none found yet", "Bedrock models appear a minute or two after the deploy; if they do not, check model access in the Bedrock console and ./bin/darkplum status, then re-run")
		return ms, nil
	}
	if len(candidates) == 0 {
		return ms, nil
	}

	opts := make([]Option, len(candidates))
	var defs []int
	for j, i := range candidates {
		m := ms[i]
		opts[j] = Option{Label: m.label(), Detail: m.kind() + " · " + m.state()}
		if m.ready() {
			defs = append(defs, j)
		}
	}
	chosen := defs
	if r.interactive {
		var err error
		if chosen, err = r.prompt.MultiSelect("Enable which models? Everyone who signs in can chat with an enabled model", opts, defs); err != nil {
			return ms, err
		}
	}
	for _, j := range chosen {
		i := candidates[j]
		if err := r.api.enable(ctx, ms[i].ID); err != nil {
			r.problem(ms[i].label(), "could not enable: "+firstLine(err.Error()), "enable it under Admin > Models in the web UI")
			continue
		}
		ms[i].Enabled = true
		r.pass(ms[i].label(), ms[i].kind()+" · enabled now")
	}
	return ms, nil
}

func hasListedReadySelfHosted(ms []adminModel) bool {
	for _, m := range ms {
		if m.selfHosted() && m.ready() {
			return true
		}
	}
	return false
}

// waitSelfHosted waits (bounded by --wait-timeout, Ctrl-C stops it) until the
// self-hosted model is loaded and listed, then enables it.
func (r *configureRun) waitSelfHosted(ctx context.Context) (adminModel, error) {
	timeout := r.cmd.WaitTimeout
	if timeout <= 0 {
		timeout = 40 * time.Minute
	}
	start := r.now()
	fmt.Fprintf(r.out, "%s waiting for the self-hosted model, up to %s %s\n", sDim("…"), minutes(timeout), sDim("(Ctrl-C stops waiting)"))
	last, lastSaid := "", start
	for {
		vs := r.vllm()
		phase := vs.phase
		if !vs.present {
			phase = "no model pod yet"
		}
		if vs.broken {
			return adminModel{}, r.fail("self-hosted model", vs.phase, vs.hint, "self-hosted model failed")
		}
		if vs.ready {
			ms, err := r.api.models(ctx)
			if err != nil {
				return adminModel{}, r.fail("models", firstLine(err.Error()), "enable it under Admin > Models in the web UI", "cannot list models")
			}
			for _, m := range ms {
				if !m.selfHosted() || !m.ready() {
					continue
				}
				if !m.Enabled {
					if err := r.api.enable(ctx, m.ID); err != nil {
						return m, r.fail(m.label(), "could not enable: "+firstLine(err.Error()), "enable it under Admin > Models in the web UI", "could not enable the self-hosted model")
					}
					m.Enabled = true
				}
				r.pass(m.label(), m.kind()+" · enabled")
				return m, nil
			}
			phase = "loaded; waiting for the gateway to list it"
		}
		elapsed := r.now().Sub(start)
		if phase != last || r.now().Sub(lastSaid) >= 2*time.Minute {
			fmt.Fprintf(r.out, "  %s self-hosted model: %s %s\n", sDim("…"), phase, sDim(fmt.Sprintf("(%d min)", int(elapsed.Minutes()))))
			last, lastSaid = phase, r.now()
		}
		if elapsed >= timeout {
			return adminModel{}, r.fail("self-hosted model", fmt.Sprintf("not ready after %s (%s)", minutes(timeout), phase),
				"follow it: "+kubectlHint("-n "+inferenceNamespace+" logs -f deploy/vllm-inference -c model-download"),
				"self-hosted model not ready")
		}
		if err := r.sleep(ctx, 15*time.Second); err != nil {
			fmt.Fprintf(r.out, "\n  %s\n", sDim("Stopped waiting. The model keeps loading; re-run ./bin/darkplum configure later."))
			return adminModel{}, reportedError{"stopped waiting"}
		}
	}
}

// chat sends one short chat through the same route as the web UI and names
// the component when it fails. A model enabled seconds ago can be refused
// until the gateway reloads its policy, so that refusal is retried briefly.
func (r *configureRun) chat(ctx context.Context, m adminModel) error {
	for try := 1; ; try++ {
		res, resp, err := r.api.chat(ctx, m.ModelID, r.now)
		if err != nil {
			r.problem("test chat", firstLine(err.Error()), "try a chat in the web UI; ./bin/darkplum status checks the services")
			return nil
		}
		if resp.status == http.StatusOK {
			r.pass("test chat", fmt.Sprintf("%s answered in %.1fs", m.label(), res.latency.Seconds()))
			if res.reply != "" {
				reply := res.reply
				if utf8.RuneCountInString(reply) > 160 {
					reply = string([]rune(reply)[:157]) + "..."
				}
				fmt.Fprintf(r.out, "      %s\n", sDim("“"+reply+"”"))
			}
			return nil
		}
		msg, code := resp.errMessage()
		if resp.status == http.StatusForbidden && code == "model_not_enabled" && try < 6 {
			if err := r.sleep(ctx, 3*time.Second); err != nil {
				return reportedError{"stopped"}
			}
			continue
		}
		detail, hint := chatFailure(m, resp.status, code, msg)
		r.problem("test chat", detail, hint)
		return nil
	}
}

// chatFailure says which component refused a test chat.
func chatFailure(m adminModel, status int, code, msg string) (detail, hint string) {
	switch {
	case code == "model_not_enabled":
		return "the gateway has not picked up the newly enabled model yet", "try a chat in the web UI in a minute"
	case status == http.StatusNotFound:
		return "the gateway does not list " + m.ModelID + " yet", "wait a minute, then re-run ./bin/darkplum configure"
	case m.selfHosted() && status >= 500:
		return fmt.Sprintf("the self-hosted model did not answer (%d): it may still be loading", status),
			"follow it: " + kubectlHint("-n "+inferenceNamespace+" logs -f deploy/vllm-inference -c vllm")
	case status == http.StatusUnprocessableEntity:
		return msg, "this model needs an AWS data-retention opt-in; choose another under Admin > Models"
	case m.Provider == "bedrock":
		return fmt.Sprintf("Bedrock refused or failed (%d): %s", status, msg),
			"check model access in the Bedrock console for this account and region; ./bin/darkplum status checks the services"
	case status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout:
		return fmt.Sprintf("the gateway did not answer (%d)", status), "check the services with ./bin/darkplum status"
	default:
		return fmt.Sprintf("refused (%d): %s", status, msg), "try a chat in the web UI; ./bin/darkplum status checks the services"
	}
}

// minutes renders a wait bound the way people say it: "40 min".
func minutes(d time.Duration) string {
	if d < time.Minute {
		return d.String()
	}
	return fmt.Sprintf("%d min", int(d.Minutes()))
}

func localURL(base string) string {
	return strings.Replace(base, "127.0.0.1", "localhost", 1)
}

func (r *configureRun) finish(ctx context.Context) error {
	w := r.out
	fmt.Fprintln(w)
	r.teamLines()
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Open darkplum: %s\n", sCmd(localURL(r.api.base)))
	switch {
	case r.email != "":
		fmt.Fprintf(w, "  Sign in as %s\n", sInfo(r.email))
	case r.sso:
		fmt.Fprintf(w, "  Sign in with SSO\n")
	}
	fmt.Fprintf(w, "  %s\n", sDim("Works while this window is open. Ctrl-C closes it."))
	if r.note != "" {
		fmt.Fprintf(w, "\n  %s\n", sDim(r.note))
	}

	if r.hold {
		if err := r.holdOpen(ctx); err != nil {
			return err
		}
	}
	if r.problems > 0 {
		return reportedError{fmt.Sprintf("%d step(s) need attention (✗ above)", r.problems)}
	}
	return nil
}

// holdOpen keeps the tunnel open until Ctrl-C, reopening it once if it dies.
func (r *configureRun) holdOpen(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(r.out)
			return nil
		case <-r.fwd.Done():
			if ctx.Err() != nil {
				fmt.Fprintln(r.out)
				return nil
			}
			base, err := r.reconnect(ctx)
			if err != nil {
				return r.fail("connection", "closed: "+firstLine(err.Error()),
					"re-run ./bin/darkplum configure to open it again (it skips the steps already done)", "connection closed")
			}
			r.api.base = base
			fmt.Fprintf(r.out, "  Open darkplum: %s %s\n", sCmd(localURL(base)), sDim("(new address)"))
		}
	}
}
