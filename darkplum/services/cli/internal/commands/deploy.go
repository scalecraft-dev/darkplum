package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/scalecraft-dev/darkplum/cli/internal/bundle"
	"github.com/scalecraft-dev/darkplum/cli/internal/iamcheck"
	"github.com/scalecraft-dev/darkplum/cli/internal/install"
	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
	"github.com/scalecraft-dev/darkplum/cli/internal/manifests"
	"github.com/scalecraft-dev/darkplum/cli/internal/mirror"
	"github.com/scalecraft-dev/darkplum/cli/internal/release"
	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
	"github.com/scalecraft-dev/darkplum/cli/internal/verify"
)

// DeployCmd is `darkplum deploy`: infrastructure, then the platform, in one
// command, and the upgrade path too. It is resumable by observation: every step
// looks at what is already there (Terraform's plan, the mirrored tags, the
// Secrets, the rollouts) and does only what is missing, so after any failure the
// answer is to run it again.
//
// The order is the point:
//
//	preflight (read-only) -> verify the release's signatures -> terraform
//	-> outputs -> cluster -> mirror -> network policies -> secrets -> api ->
//	gateway -> web -> install record -> GPU model -> Layer 1 self-test
//
// Nothing is written anywhere until the images verify, so a bad release costs
// seconds. Policies are applied before any workload starts.
type DeployCmd struct {
	MinSession  int  `name:"min-session" help:"Stop unless the AWS session outlasts this many minutes. Default: 90 with a self-hosted GPU model, 30 without." default:"0"`
	ConfirmPlan bool `name:"confirm-plan" help:"Approve a Terraform plan that deletes or replaces resources. Without it deploy shows the list and asks; it never approves a delete on its own, and there is no --yes that would."`

	GPUWait time.Duration `name:"gpu-wait" hidden:"" help:"How long to wait for a GPU to become allocatable before reporting the platform healthy without it." default:"15m"`

	ReleaseImages  string `name:"release-images" hidden:"" help:"Development builds only: a release.json to deploy (its images are still verified)."`
	UnsignedImages string `name:"unsigned-images" hidden:"" help:"Development builds only: gateway=REPO@sha256:...,api=...,web=... Images are mirrored without any signature check and the install is marked image-trust: unsigned."`
}

// platformNS is where the platform's objects live; the manifests name it.
const platformNS = "darkplum-system"

const deploySteps = 13

// exitError is a command that did its work but wants a specific exit status.
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

// ExitCode reports an exit status a command asked for.
func ExitCode(err error) (int, bool) {
	var e exitError
	if errors.As(err, &e) {
		return e.code, true
	}
	return 0, false
}

// exitDegraded is the status of a deploy whose platform is healthy but whose
// self-hosted model has no GPU to run on.
const exitDegraded = 3

// Run executes `darkplum deploy`.
func (c *DeployCmd) Run(g *Globals) error {
	if g.Namespace != platformNS {
		return fmt.Errorf("deploy installs into %s; --namespace %q is not supported here", platformNS, g.Namespace)
	}
	in, rec, err := openInstall(g.Install)
	if err != nil {
		printCheckTo(os.Stdout, noInstallRow(err))
		return reportedError{"no install to deploy"}
	}
	m, err := release.Current()
	if err != nil {
		return err
	}
	tfv, err := readTFVars(in.TFVars())
	if err != nil {
		printCheckTo(os.Stdout, checkResult{name: "install settings", status: checkFail, detail: firstLine(err.Error()),
			hint: "run `darkplum init` to write them"})
		return reportedError{"no install settings"}
	}

	logf, logPath, err := openDeployLog(in, time.Now())
	if err != nil {
		return err
	}
	defer logf.Close()
	e := &deployEnv{
		out: newLockedWriter(io.MultiWriter(os.Stdout, logf)), logPath: logPath,
		in: in, rec: rec, tfv: tfv, manifest: m, flags: *c,
		now: time.Now, sleep: sleepCtx, heartbeat: 30 * time.Second, poll: 10 * time.Second,
		aws:         runWithStderr,
		interactive: term.IsTerminal(int(os.Stdin.Fd())),
		confirm: func(q string) bool {
			return strings.EqualFold(promptLine(q), "yes")
		},
	}
	e.useRealSeams()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	done := make(chan struct{})
	defer close(done)
	go func() {
		n := 0
		for {
			select {
			case <-sigs:
				n++
				if n == 1 {
					fmt.Fprintln(e.out, "\n"+sWarn("! interrupted: stopping Terraform cleanly (it releases its state lock). Ctrl-C again to quit now."))
					cancel()
					continue
				}
				fmt.Fprintln(e.out, "\n"+sFail("✗ quit while Terraform may still hold the state lock.")+
					"\n  If the next run reports a lock, it prints the lock id: `terraform force-unlock <id>` clears it (only when nothing else is running).")
				os.Exit(130)
			case <-done:
				return
			}
		}
	}()
	return e.run(ctx)
}

func openDeployLog(in install.Install, now time.Time) (*os.File, string, error) {
	return openCommandLog(in, "deploy", now)
}

// openCommandLog creates <workspace>/logs/<command>-<time>.log (0600): the full
// output of one run, kept for support and for the next run's diagnosis.
func openCommandLog(in install.Install, command string, now time.Time) (*os.File, string, error) {
	if err := os.MkdirAll(in.LogsDir(), 0o700); err != nil {
		return nil, "", err
	}
	p := filepath.Join(in.LogsDir(), command+"-"+now.UTC().Format("20060102T150405Z")+".log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, "", err
	}
	return f, p, nil
}

// deployEnv is everything deploy touches, so tests replace the cloud, Terraform,
// the registry and the clock.
type deployEnv struct {
	out     io.Writer
	logPath string
	in      install.Install
	rec     install.Record
	tfv     tfvarsInfo
	// manifest is this binary's release manifest.
	manifest release.Manifest
	flags    DeployCmd

	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
	heartbeat time.Duration // how often apply reports it is still running
	poll      time.Duration // how often to look at a cluster that is changing

	// mutated is set when the run first changes anything, so a failure before then
	// can say nothing was touched.
	mutated bool

	interactive bool
	confirm     func(question string) bool

	aws       cmdRunner
	checks    func(minSession int) []checkResult
	sessionOK func(minMinutes int) checkResult
	openTF    func() (tfRunner, string, error)
	cluster   func() (clusterTarget, checkResult, error)
	kube      func(t clusterTarget) *kube.Client
	resolve   func(ctx context.Context) (verify.Verified, error)
	platforms func(ctx context.Context, img release.Image) ([]string, error)
	mirror    func(ctx context.Context, v verify.Verified, repos map[string]string, region string) ([]mirror.Mirrored, error)
	iam       func(ctx context.Context, o iamcheck.Outputs) (iamcheck.Report, error)
}

// useRealSeams wires the production implementations of everything a test replaces.
func (e *deployEnv) useRealSeams() {
	e.checks = e.realChecks
	e.sessionOK = func(min int) checkResult { return checkAWSSession(e.rec.Profile, min) }
	e.openTF = func() (tfRunner, string, error) { return openTF(e.in, e.rec) }
	e.cluster = func() (clusterTarget, checkResult, error) { return installCluster(e.in, e.aws) }
	e.kube = func(t clusterTarget) *kube.Client { return t.kube(platformNS) }
	e.resolve = e.realResolve
	e.platforms = func(ctx context.Context, img release.Image) ([]string, error) {
		return mirror.Platforms(ctx, img, mirror.Options{})
	}
	e.mirror = func(ctx context.Context, v verify.Verified, repos map[string]string, region string) ([]mirror.Mirrored, error) {
		auth, err := mirror.ECRAuth(func(args ...string) (string, error) { return e.aws("aws", args...) }, e.rec.Profile, region)
		if err != nil {
			return nil, err
		}
		return mirror.Release(ctx, v, repos, auth, mirror.Options{})
	}
	e.iam = func(ctx context.Context, o iamcheck.Outputs) (iamcheck.Report, error) {
		return iamcheck.Run(ctx, func(args ...string) (string, error) {
			return e.aws("aws", runner.ProfileArgs(e.rec.Profile, args...)...)
		}, o)
	}
}

// lockedWriter serialises writes: the apply heartbeat and Terraform's own output
// reach the same terminal and log from different goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newLockedWriter(w io.Writer) *lockedWriter { return &lockedWriter{w: w} }

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (e *deployEnv) printf(format string, a ...any) { fmt.Fprintf(e.out, format, a...) }

func (e *deployEnv) step(n int, title string) {
	fmt.Fprintf(e.out, "\n%s\n", sHeader(fmt.Sprintf("[%d/%d] %s", n, deploySteps, title)))
}

func (e *deployEnv) row(r checkResult) { printCheckTo(e.out, r) }

// stop prints a failed row and ends the run with the "already shown" error.
func (e *deployEnv) stop(r checkResult, msg string) error {
	e.row(r)
	return reportedError{msg}
}

func (e *deployEnv) minSession() int {
	if e.flags.MinSession > 0 {
		return e.flags.MinSession
	}
	if e.tfv.GPUDesired == 0 {
		return 30
	}
	return 90
}

// realChecks are the doctor's checks, run against the install's own profile and
// pinned account (the install record, not the ambient credentials).
func (e *deployEnv) realChecks(minSession int) []checkResult {
	p := e.rec.Profile
	checks := []checkResult{
		checkTool("terraform"), checkTool(runner.Kubectl), checkTool("aws"),
		checkAWSCreds(p), checkAWSSession(p, minSession), checkAccountMatch(p, e.rec.AccountIDs),
	}
	if e.tfv.GPUDesired > 0 {
		gpuType := defaultGPUInstance
		if e.tfv.GPUType != "" {
			gpuType = e.tfv.GPUType
		}
		checks = append(checks, checkAWSGPUQuota(e.rec.Region, p, gpuType))
		if len(e.tfv.AZs) > 0 {
			checks = append(checks, checkOfferings(newAWSCloud(p, e.aws), e.rec.Region, e.tfv.AZs, []string{defaultCPUInstance, gpuType}))
		}
	}
	return checks
}

func (e *deployEnv) run(ctx context.Context) (err error) {
	m := e.manifest
	e.printf("%s\n", sHeader("▸ deploy · "+e.rec.ClusterName+" · "+m.Version+" ("+m.Channel+")"))
	e.printf("%s\n", sDim("log: "+e.logPath))
	defer func() {
		if _, exit := ExitCode(err); err != nil && !exit {
			if !e.mutated {
				e.printf("\n%s\n", sDim("Nothing was changed. Full log: "+e.logPath))
			} else {
				e.printf("\n%s\n", sDim("Re-run `darkplum deploy`: completed steps are skipped and it continues from where this stopped. Full log: "+e.logPath))
			}
		}
	}()

	// ---- 1. preflight: read-only ------------------------------------------------
	e.step(1, "Preflight (nothing is changed)")
	if r := e.devFlagsRefusal(); r != nil {
		return e.stop(*r, "dev-only flags refused")
	}
	failed := 0
	for _, r := range e.checks(e.minSession()) {
		e.row(r)
		if r.status == checkFail {
			failed++
		}
	}
	if failed > 0 {
		return reportedError{fmt.Sprintf("%d preflight check(s) failed; nothing was changed", failed)}
	}
	if err := e.inspectLive(); err != nil {
		return err
	}

	// ---- 2. verify the images before anything is written ------------------------
	e.step(2, "Verify the release's images")
	imgs, err := e.resolve(ctx)
	if err != nil {
		return err
	}

	// ---- 3. terraform init and the audit log group ------------------------------
	e.step(3, "Terraform init")
	tf, note, err := e.openTF()
	if err != nil {
		return e.stop(checkResult{name: "terraform", status: checkFail, detail: firstLine(err.Error()),
			hint: "the shipped Terraform could not be prepared in the install's workspace"}, "cannot prepare terraform")
	}
	if note != "" {
		e.printf("%s\n", sWarn("! "+note))
	}
	if err := tf.Init(); err != nil {
		return e.stop(checkResult{name: "terraform init", status: checkFail, detail: firstLine(err.Error()),
			hint: e.loginHint(err.Error())}, "terraform init failed")
	}
	if err := e.adoptLogGroup(tf); err != nil {
		return e.stop(checkResult{name: "audit log group", status: checkFail, detail: firstLine(err.Error()),
			hint: "nothing was applied; fix the above and re-run"}, "cannot check the audit log group")
	}

	// ---- 4. plan, confirm, apply ------------------------------------------------
	e.step(4, "Terraform plan and apply")
	if err := e.planAndApply(ctx, tf); err != nil {
		return err
	}

	// ---- 5. outputs, read once, after apply --------------------------------------
	e.step(5, "Read the infrastructure's outputs")
	raw, err := tf.Capture("output", "-json")
	if err != nil {
		return e.stop(checkResult{name: "terraform output", status: checkFail, detail: firstLine(err.Error())}, "cannot read the outputs")
	}
	out, err := parseOutputs(raw)
	if err != nil {
		return e.stop(checkResult{name: "terraform output", status: checkFail, detail: firstLine(err.Error()),
			hint: "the apply finished but its outputs are incomplete; this is a bug in the shipped Terraform, not something to patch around"}, "incomplete outputs")
	}
	if out.ClusterName != e.rec.ClusterName || out.Region != e.rec.Region {
		return e.stop(checkResult{name: "terraform output", status: checkFail,
			detail: fmt.Sprintf("the state describes cluster %s in %s, the install is %s in %s", out.ClusterName, out.Region, e.rec.ClusterName, e.rec.Region),
			hint:   "the state backend does not belong to this install; check backend.hcl"}, "state and install disagree")
	}
	e.row(checkResult{name: "outputs", status: checkPass, detail: fmt.Sprintf("cluster %s · %d GPU node(s)", out.ClusterName, out.GPUDesired)})

	// ---- 6. cluster ----------------------------------------------------------------
	e.step(6, "Connect to the cluster")
	t, row, err := e.cluster()
	if err != nil {
		return e.stop(row, "cannot reach the cluster")
	}
	e.row(row)
	kc := e.kube(t)
	nodes, err := e.waitNodes(ctx, kc)
	if err != nil {
		return e.stop(checkResult{name: "nodes", status: checkFail, detail: firstLine(err.Error()),
			hint: "no node became Ready; check the node group in the EKS console (instance quota, subnets), then re-run"}, "no Ready node")
	}
	e.row(checkResult{name: "nodes", status: checkPass, detail: fmt.Sprintf("%d Ready (%s)", len(nodes), strings.Join(archSet(nodes), ", "))})
	if err := e.checkArch(ctx, imgs, nodes); err != nil {
		return err
	}

	// ---- 7. mirror -------------------------------------------------------------------
	e.step(7, "Copy the verified images into your registry")
	if r := e.sessionOK(20); r.status == checkFail {
		return e.stop(r, "the AWS session is about to expire")
	}
	mirrored, err := e.mirror(ctx, imgs, out.Repos, out.Region)
	if err != nil {
		return e.stop(checkResult{name: "mirror", status: checkFail, detail: firstLine(err.Error()),
			hint: e.loginHint(err.Error()) + " (copies already made are kept)"}, "cannot mirror the images")
	}
	images := map[string]string{}
	for _, mi := range mirrored {
		images[mi.Service] = mi.Ref
		d := "already there"
		if mi.Copied {
			d = "copied"
		}
		e.row(checkResult{name: mi.Service, status: checkPass, detail: mi.Ref + " · " + d})
	}

	// ---- 8-10. platform ----------------------------------------------------------------
	if r := e.sessionOK(10); r.status == checkFail {
		return e.stop(r, "the AWS session is about to expire")
	}
	fsys, err := tf.K8s()
	if err != nil {
		return e.stop(checkResult{name: "manifests", status: checkFail, detail: firstLine(err.Error())}, "cannot read the bundle")
	}
	upstreams, err := carriedUpstreams(kc)
	if err != nil {
		return e.stop(checkResult{name: "gateway models", status: checkFail, detail: firstLine(err.Error()),
			hint: "the live gateway's model list could not be read; applying without it would drop the models added with `darkplum models`"}, "cannot read the live gateway")
	}
	phases, err := manifests.Platform(fsys, manifests.Inputs{
		Images: images, BedrockRegion: out.InferenceRegion, BedrockModelFilter: out.BedrockModelFilter,
		VLLMUpstreams: upstreams, ServiceCIDR: out.ServiceCIDR, VPCCIDR: out.VPCCIDR,
	})
	if err != nil {
		return e.stop(checkResult{name: "manifests", status: checkFail, detail: firstLine(err.Error())}, "the platform manifests do not render")
	}
	if err := e.applyPlatform(ctx, kc, phases, out, images); err != nil {
		return err
	}

	// ---- 11. install record ------------------------------------------------------------
	e.step(12, "Record the install")
	if err := e.recordInstall(kc, imgs, images); err != nil {
		return e.stop(checkResult{name: "darkplum-install", status: checkFail, detail: firstLine(err.Error())}, "cannot write the install record")
	}
	e.row(checkResult{name: "darkplum-install", status: checkPass, detail: m.Version + " · image-trust " + trustWord(imgs)})

	// ---- 12. GPU model (never blocks the platform) and the Layer 1 self-test --------------
	e.step(13, "Self-hosted model and the Layer 1 self-test")
	degraded := false
	if out.GPUDesired == 0 {
		e.row(checkResult{name: "self-hosted model", status: checkPass, detail: "gpu_desired_size = 0: Bedrock models only (raise it in `darkplum init`, then deploy)"})
	} else if err := e.deployGPU(ctx, kc, fsys, out); err != nil {
		degraded = true
		e.row(checkResult{name: "self-hosted model", status: checkWarn, detail: firstLine(err.Error()),
			hint: "the platform is healthy and Bedrock models work; fix the GPU and run `darkplum deploy` again"})
	}
	iamFailed := e.layer1(ctx, out)
	if iamFailed {
		return reportedError{"the gateway role does not behave as configured"}
	}

	e.printf("\n%s\n", sSuccess("✓ Darkplum is deployed."))
	if degraded {
		e.printf("%s\n", sWarn("! Degraded: the platform is healthy, the self-hosted model is not running (no GPU). Exit status 3."))
		e.printf("\nNext: darkplum configure\n")
		return exitError{exitDegraded, "platform healthy, no GPU for the self-hosted model"}
	}
	if out.GPUDesired > 0 {
		e.printf("%s\n", sDim("The self-hosted model is loading (15-25 minutes); configure shows its progress."))
	} else {
		e.printf("%s\n", sDim("Bedrock-only install. Add self-hosted models later: `darkplum init` (Self-hosted GPU), then `darkplum deploy`."))
	}
	e.printf("\nNext: darkplum configure\n")
	return nil
}

// loginHint is the remedy for an expired session when the failure looks like one.
func (e *deployEnv) loginHint(msg string) string {
	l := strings.ToLower(msg)
	if strings.Contains(l, "expired") || strings.Contains(l, "sso") || strings.Contains(l, "unauthorized") || strings.Contains(l, "credentials") {
		login := "aws sso login"
		if e.rec.Profile != "" {
			login += " --profile " + e.rec.Profile
		}
		return "your AWS session may have expired: run `" + login + "`, then re-run"
	}
	return "fix the above and re-run"
}

func trustWord(v verify.Verified) string {
	if v.Unsigned() {
		return "unsigned"
	}
	return "verified"
}

// ---- dev-only flags ---------------------------------------------------------

// devFlagsRefusal enforces that --release-images and --unsigned-images exist only
// on development builds, and that a development build says which images it means.
func (e *deployEnv) devFlagsRefusal() *checkResult {
	f := e.flags
	dev := f.ReleaseImages != "" || f.UnsignedImages != ""
	switch e.manifest.Channel {
	case release.Dev:
		switch {
		case f.ReleaseImages != "" && f.UnsignedImages != "":
			return &checkResult{name: "images", status: checkFail, detail: "--release-images and --unsigned-images are exclusive"}
		case !dev:
			return &checkResult{name: "images", status: checkFail, detail: "this is a development build: it carries no images",
				hint: "use a release binary, or pass --release-images <release.json> / --unsigned-images gateway=REF,api=REF,web=REF"}
		}
	default:
		if dev {
			return &checkResult{name: "images", status: checkFail,
				detail: "--release-images and --unsigned-images are for development builds; this is a " + e.manifest.Channel + " build",
				hint:   "a release binary deploys exactly the images it was built with, after verifying their signatures"}
		}
	}
	return nil
}

var unsignedRefRe = regexp.MustCompile(`^([^\s@=,]+)@(sha256:[0-9a-f]{64})$`)

// realResolve decides which images are deployed and proves what can be proved
// about them: a release or release candidate's three images are verified against
// the signing identity in its manifest, each under its own run; a development
// build's hand-given images are marked unsigned.
func (e *deployEnv) realResolve(ctx context.Context) (verify.Verified, error) {
	m := e.manifest
	f := e.flags
	if m.Channel == release.Dev {
		if f.UnsignedImages != "" {
			dm := release.Manifest{Schema: 1, Channel: release.Dev, Version: "dev", Images: map[string]release.Image{}}
			for _, kv := range strings.Split(f.UnsignedImages, ",") {
				svc, ref, ok := strings.Cut(kv, "=")
				sub := unsignedRefRe.FindStringSubmatch(ref)
				if !ok || sub == nil || !isService(svc) {
					return verify.Verified{}, e.stop(checkResult{name: "images", status: checkFail, detail: fmt.Sprintf("--unsigned-images entry %q", kv),
						hint: "each is service=repository@sha256:<64 hex>, for gateway, api and web"}, "bad --unsigned-images")
				}
				dm.Images[svc] = release.Image{Repository: sub[1], Digest: sub[2], RunAttempt: "1"}
			}
			if len(dm.Images) != len(release.Services) {
				return verify.Verified{}, e.stop(checkResult{name: "images", status: checkFail, detail: "--unsigned-images needs gateway, api and web"}, "bad --unsigned-images")
			}
			v, err := verify.Unverified(dm)
			if err != nil {
				return verify.Verified{}, err
			}
			e.row(checkResult{name: "images", status: checkWarn, detail: "UNSIGNED: development build, nothing was verified",
				hint: "the install is marked image-trust: unsigned"})
			return v, nil
		}
		raw, err := os.ReadFile(f.ReleaseImages)
		if err != nil {
			return verify.Verified{}, e.stop(checkResult{name: "images", status: checkFail, detail: firstLine(err.Error())}, "cannot read --release-images")
		}
		if m, err = release.Parse(raw); err != nil {
			return verify.Verified{}, e.stop(checkResult{name: "images", status: checkFail, detail: firstLine(err.Error())}, "bad --release-images")
		}
	}
	dir, err := verify.DefaultCacheDir()
	if err != nil {
		return verify.Verified{}, err
	}
	v, err := verify.NewLive(dir)
	if err != nil {
		return verify.Verified{}, e.stop(checkResult{name: "trust root", status: checkFail, detail: firstLine(err.Error()),
			hint: "needs https://tuf-repo-cdn.sigstore.dev; check the network and retry"}, "cannot establish the sigstore trust root")
	}
	vr, err := v.VerifyRelease(ctx, verify.RegistryFetcher(), m)
	bad := 0
	for _, svc := range release.Services {
		if res, ok := vr.Results[svc]; ok {
			e.row(checkResult{name: svc, status: checkPass, detail: m.Images[svc].Ref() + " · " + res.RunInvocationURI})
			continue
		}
		bad++
		e.row(checkResult{name: svc, status: checkFail, detail: m.Images[svc].Ref(),
			hint: "no signature from the release workflow's identity; do not deploy this image"})
	}
	if err != nil || bad > 0 {
		if err != nil {
			e.printf("%s\n", sDim(err.Error()))
		}
		return verify.Verified{}, reportedError{fmt.Sprintf("%d of %d images failed verification; nothing was changed", bad, len(release.Services))}
	}
	return vr, nil
}

func isService(s string) bool {
	for _, x := range release.Services {
		if x == s {
			return true
		}
	}
	return false
}

// ---- live cluster inspection ------------------------------------------------------

const lbSchemeAnnotation = "service.beta.kubernetes.io/aws-load-balancer-scheme"

// inspectLive looks at an existing install's cluster before anything is changed.
// A cluster that does not exist yet is a first install. A cluster that exists
// but cannot be validated (another account's credentials, no access) stops the
// run: deploy does not guess what is running.
//
// Two states it will not deploy over, because the new release would silently
// change them:
//   - an internet-facing (or any LoadBalancer) darkplum-web Service, left by the
//     old expose-web: the shipped Service is cluster-internal and the team's
//     address is a private load balancer, so applying it would take the public
//     address down without warning;
//   - a semantic router installed by helm, which this release does not manage.
func (e *deployEnv) inspectLive() error {
	t, row, err := e.cluster()
	if errors.Is(err, errClusterNotFound) {
		e.row(checkResult{name: "cluster", status: checkPass, detail: "none yet: a first install"})
		return nil
	}
	if err != nil {
		return e.stop(row, "cannot inspect the existing cluster; nothing was changed")
	}
	e.row(row)
	kc := e.kube(t)

	svc, err := kc.Run(nil, "-n", platformNS, "get", "svc", "darkplum-web", "-o", "json")
	switch {
	case err == nil:
		if ex := webExposure(svc); ex != "" {
			return e.stop(checkResult{name: "web exposure", status: checkFail, detail: ex,
				hint: "this release serves the web UI on a private load balancer and keeps the darkplum-web Service cluster-internal. " +
					"Deploying would remove the address above without warning. Nothing was changed. To move to private: " +
					"`eval \"$(darkplum kubeconfig)\"`, then `kubectl -n darkplum-system patch svc darkplum-web --type=merge -p '{\"spec\":{\"type\":\"ClusterIP\"}}'` " +
					"and re-run deploy. To keep the public address, stay on the release that created it."}, "internet-facing web service")
		}
	case kubeNotFound(err, "services", "darkplum-web"):
	default:
		return e.stop(checkResult{name: "web exposure", status: checkFail, detail: firstLine(err.Error()),
			hint: "cannot tell how the web UI is exposed; nothing was changed"}, "cannot inspect the web service")
	}

	sec, err := kc.Run(nil, "-n", platformNS, "get", "secret", "-l", "owner=helm,name=semantic-router", "-o", "name")
	if err != nil {
		return e.stop(checkResult{name: "semantic router", status: checkFail, detail: firstLine(err.Error()),
			hint: "cannot tell whether a helm-installed router exists; nothing was changed"}, "cannot inspect helm releases")
	}
	if strings.TrimSpace(sec) != "" {
		return e.stop(checkResult{name: "semantic router", status: checkFail,
			detail: "installed by helm (release semantic-router)",
			hint: "this release does not manage the router: it has no helm, and no way yet to take the release over without losing any edits you made to its values. " +
				"Nothing was changed. Stay on the release that installed it, or remove it first (`helm uninstall semantic-router -n darkplum-system` with your own cluster access); routed aliases then fall back to their default route."},
			"helm-installed semantic router")
	}
	return nil
}

// webExposure describes a darkplum-web Service that is published outside the
// cluster, or "" when it is cluster-internal.
func webExposure(svcJSON string) string {
	var s struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Type string `json:"type"`
		} `json:"spec"`
		Status struct {
			LoadBalancer struct {
				Ingress []struct{ Hostname, IP string } `json:"ingress"`
			} `json:"loadBalancer"`
		} `json:"status"`
	}
	if err := jsonUnmarshal(svcJSON, &s); err != nil {
		return "the darkplum-web Service could not be read"
	}
	scheme := s.Metadata.Annotations[lbSchemeAnnotation]
	if s.Spec.Type != "LoadBalancer" && scheme != "internet-facing" {
		return ""
	}
	host := ""
	if len(s.Status.LoadBalancer.Ingress) > 0 {
		host = s.Status.LoadBalancer.Ingress[0].Hostname
		if host == "" {
			host = s.Status.LoadBalancer.Ingress[0].IP
		}
	}
	if scheme == "" {
		scheme = "unspecified scheme"
	}
	d := "darkplum-web is type " + s.Spec.Type + " (" + scheme + ")"
	if host != "" {
		d += " at " + host
	}
	return d
}

// ---- terraform ---------------------------------------------------------------------

const logGroupAddr = "module.eks.module.eks.aws_cloudwatch_log_group.this[0]"

// adoptLogGroup brings the EKS control-plane log group into the state when it
// exists but is not managed: destroy keeps it on purpose (it holds the Kubernetes
// API audit logs), and a reinstall would otherwise fail creating it. Every status
// is checked; a failing read is an error, never "not there".
func (e *deployEnv) adoptLogGroup(tf tfRunner) error {
	state, err := stateList(tf)
	if err != nil {
		return err
	}
	lg := "/aws/eks/" + e.rec.ClusterName + "/cluster"
	if state[logGroupAddr] {
		e.row(checkResult{name: "audit log group", status: checkPass, detail: lg + " (in the state)"})
		return nil
	}
	out, err := e.aws("aws", runner.ProfileArgs(e.rec.Profile, "logs", "describe-log-groups", "--log-group-name-prefix", lg,
		"--region", e.rec.Region, "--query", "length(logGroups[?logGroupName=='"+lg+"'])", "--output", "text")...)
	if err != nil {
		return fmt.Errorf("cannot look for the log group %s: %w", lg, err)
	}
	if strings.TrimSpace(out) != "1" {
		e.row(checkResult{name: "audit log group", status: checkPass, detail: "none to adopt"})
		return nil
	}
	e.mutated = true
	if _, err := tf.Capture("import", "-input=false", "-var-file="+tf.VarFile(), logGroupAddr, lg); err != nil {
		return fmt.Errorf("cannot adopt the log group %s kept from a previous install: %w", lg, err)
	}
	e.row(checkResult{name: "audit log group", status: checkPass, detail: "adopted " + lg + " (kept from a previous install)"})
	return nil
}

// tail keeps the last few KB of what Terraform printed, to recognise an expired
// session in it after the fact.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 16<<10 {
		t.buf = t.buf[len(t.buf)-8<<10:]
	}
	return len(p), nil
}

func (t *tail) String() string { t.mu.Lock(); defer t.mu.Unlock(); return string(t.buf) }

func (e *deployEnv) planAndApply(ctx context.Context, tf tfRunner) error {
	planPath := filepath.Join(e.in.Dir(), "plan-"+e.now().UTC().Format("20060102T150405Z")+".tfplan")
	defer os.Remove(planPath)
	vf := "-var-file=" + tf.VarFile()

	e.printf("%s\n", sInfo("→ terraform plan"))
	if err := e.streamTF(ctx, tf, 4, "plan", "plan", "-input=false", "-out="+planPath, vf); err != nil {
		return err
	}
	js, err := tf.Capture("show", "-json", planPath)
	if err != nil {
		return e.stop(checkResult{name: "terraform show", status: checkFail, detail: firstLine(err.Error())}, "cannot read the plan")
	}
	dangerous, err := destructiveChanges(js)
	if err != nil {
		return e.stop(checkResult{name: "plan", status: checkFail, detail: err.Error()}, "cannot read the plan")
	}
	if len(dangerous) > 0 {
		e.printf("\n%s\n", sWarn(fmt.Sprintf("! This plan deletes or replaces %d resource(s):", len(dangerous))))
		for _, d := range dangerous {
			what := "delete"
			if d.Replace {
				what = "replace"
			}
			e.printf("    %-8s %s\n", what, d.Address)
		}
		switch {
		case e.flags.ConfirmPlan:
			e.printf("%s\n", sDim("approved by --confirm-plan"))
		case e.interactive && e.confirm("\nType 'yes' to apply this plan, anything else cancels: "):
		default:
			return e.stop(checkResult{name: "plan", status: checkFail, detail: "deletes or replaces resources and was not approved",
				hint: "review the list above; re-run with --confirm-plan (or answer 'yes' at the prompt) to approve it. Nothing was applied"}, "plan not approved")
		}
	}

	e.mutated = true
	e.printf("\n%s\n", sInfo("→ terraform apply (a new install takes 15-20 minutes)"))
	if err := e.streamTF(ctx, tf, 4, "apply", "apply", "-input=false", planPath); err != nil {
		return err
	}
	// The workspace remembers which release last applied; a downgrade guard reads it.
	rec := e.rec
	rec.Bundle = e.manifest.Version
	if err := e.in.Save(rec); err != nil {
		e.row(checkResult{name: "install record", status: checkWarn, detail: firstLine(err.Error())})
	}
	e.row(checkResult{name: "infrastructure", status: checkPass, detail: "applied"})
	return nil
}

// streamTerraform runs one long Terraform command with its output on out, a
// heartbeat while it is quiet, and a 45 minute cap. It returns the tail of what
// Terraform printed (to recognise an expired session or a lock) and why its context ended
// (nil, canceled or deadline exceeded), so the caller can tell a cancel or a
// timeout from a failure.
func streamTerraform(ctx context.Context, tf tfRunner, out io.Writer, now func() time.Time, heartbeat time.Duration, tag string, args ...string) (tl *tail, ctxErr error, err error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	start := now()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				el := now().Sub(start).Round(time.Second)
				fmt.Fprintf(out, "%s\n", sDim(fmt.Sprintf("  %s %dm%02ds", tag, int(el.Minutes()), int(el.Seconds())%60)))
			case <-stop:
				return
			}
		}
	}()
	tl = &tail{}
	err = tf.Stream(ctx, io.MultiWriter(out, tl), args...)
	close(stop)
	wg.Wait()
	return tl, ctx.Err(), err
}

// streamTF is streamTerraform for deploy, turning a failure into the row that
// says what to do about it.
func (e *deployEnv) streamTF(ctx context.Context, tf tfRunner, stepNo int, label string, args ...string) error {
	tl, ctxErr, err := streamTerraform(ctx, tf, e.out, e.now, e.heartbeat, fmt.Sprintf("[%d/%d] terraform %s", stepNo, deploySteps, label), args...)
	if err == nil {
		return nil
	}
	hint := "see the output above; the full log is " + e.logPath
	switch {
	case errors.Is(ctxErr, context.Canceled):
		hint = "interrupted; Terraform stopped cleanly. Re-run `darkplum deploy` to continue"
	case errors.Is(ctxErr, context.DeadlineExceeded):
		hint = "stopped after 45 minutes. Re-run `darkplum deploy` to continue"
	case strings.Contains(tl.String(), "Error acquiring the state lock"):
		hint = "another Terraform run holds the state lock (its id is printed above). If you are sure nothing else is running: `terraform force-unlock <id>`"
	default:
		if h := e.loginHint(tl.String()); !strings.HasPrefix(h, "fix the above") {
			hint = h
		}
	}
	return e.stop(checkResult{name: "terraform " + label, status: checkFail, detail: firstLine(err.Error()), hint: hint}, "terraform "+label+" failed")
}

// ---- cluster -------------------------------------------------------------------------

type nodeInfo struct {
	Name, OS, Arch string
}

func archSet(nodes []nodeInfo) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range nodes {
		k := n.OS + "/" + n.Arch
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// readyNodes lists the nodes whose Ready condition is True.
func readyNodes(kc *kube.Client, selector string) ([]nodeInfo, error) {
	args := []string{"get", "nodes", "-o", "json"}
	if selector != "" {
		args = append(args, "-l", selector)
	}
	out, err := kc.Run(nil, args...)
	if err != nil {
		return nil, err
	}
	var l struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Status   struct {
				Conditions  []struct{ Type, Status string }                `json:"conditions"`
				NodeInfo    struct{ OperatingSystem, Architecture string } `json:"nodeInfo"`
				Allocatable map[string]string                              `json:"allocatable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := jsonUnmarshal(out, &l); err != nil {
		return nil, errors.New("unexpected answer listing nodes")
	}
	var nodes []nodeInfo
	for _, it := range l.Items {
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				nodes = append(nodes, nodeInfo{Name: it.Metadata.Name, OS: it.Status.NodeInfo.OperatingSystem, Arch: it.Status.NodeInfo.Architecture})
			}
		}
	}
	return nodes, nil
}

// waitNodes waits (10 minutes) for the API to answer and at least one node to be
// Ready. A fresh cluster's API is reachable minutes before its first node joins.
func (e *deployEnv) waitNodes(ctx context.Context, kc *kube.Client) ([]nodeInfo, error) {
	deadline := e.now().Add(10 * time.Minute)
	var last error
	for {
		nodes, err := readyNodes(kc, "")
		if err == nil && len(nodes) > 0 {
			return nodes, nil
		}
		last = err
		if err == nil {
			last = errors.New("no node is Ready yet")
		}
		if e.now().After(deadline) {
			return nil, fmt.Errorf("after 10 minutes: %w", last)
		}
		e.printf("%s\n", sDim("  waiting for a Ready node: "+firstLine(last.Error())))
		if err := e.sleep(ctx, e.poll); err != nil {
			return nil, err
		}
	}
}

// checkArch refuses to go on when a node cannot run an image: the old flow
// built for the build machine's architecture and the pods died with
// "exec format error" after everything else had been applied.
func (e *deployEnv) checkArch(ctx context.Context, v verify.Verified, nodes []nodeInfo) error {
	m := v.Manifest()
	for _, svc := range release.Services {
		have, err := e.platforms(ctx, m.Images[svc])
		if err != nil {
			return e.stop(checkResult{name: "node architecture", status: checkFail, detail: firstLine(err.Error()),
				hint: "cannot read the image's platforms; nothing was applied to the cluster"}, "cannot check image platforms")
		}
		ok := map[string]bool{}
		for _, p := range have {
			ok[p] = true
		}
		for _, n := range nodes {
			if !ok[n.OS+"/"+n.Arch] {
				return e.stop(checkResult{name: "node architecture", status: checkFail,
					detail: fmt.Sprintf("node %s is %s/%s; image %s provides %s", n.Name, n.OS, n.Arch, svc, strings.Join(have, ", ")),
					hint:   "the pods would fail with 'exec format error'. Nothing was applied to the cluster"}, "an image does not run on the nodes")
			}
		}
	}
	e.row(checkResult{name: "node architecture", status: checkPass, detail: strings.Join(archSet(nodes), ", ") + " is provided by every image"})
	return nil
}

// carriedUpstreams reads VLLM_UPSTREAMS from the live gateway: `darkplum models`
// owns that value, and a redeploy must not drop the models added since. No
// gateway yet is "". Any other failure is an error: guessing "none" would drop them.
func carriedUpstreams(kc *kube.Client) (string, error) {
	out, err := kc.Run(nil, "-n", platformNS, "get", "deploy", "darkplum-gateway", "-o", "json")
	if err != nil {
		if kubeNotFound(err, "deployments", "darkplum-gateway") {
			return "", nil
		}
		return "", err
	}
	var o manifests.Object
	if err := jsonUnmarshal(out, &o); err != nil {
		return "", errors.New("unexpected answer reading the gateway")
	}
	v, _ := manifests.EnvValue(o, "VLLM_UPSTREAMS")
	return v, nil
}

// ---- platform --------------------------------------------------------------------------

var rolloutTimeout = map[string]string{"darkplum-api": "300s", "darkplum-gateway": "300s", "darkplum-web": "180s"}

func (e *deployEnv) applyPlatform(ctx context.Context, kc *kube.Client, phases []manifests.Phase, out deployOutputs, images map[string]string) error {
	for _, p := range phases {
		switch p.Name {
		case manifests.PhaseAPI:
			e.step(9, "Deploy the api (it owns the database schema)")
			// The Secrets the workloads read: between the identities and the first workload.
			if err := e.secrets(kc, out); err != nil {
				return err
			}
		case "namespaces":
			e.step(8, "Apply namespaces, network policies and identities")
		case manifests.PhaseGateway:
			e.step(10, "Deploy the gateway")
		case manifests.PhaseWeb:
			e.step(11, "Deploy the web UI")
		}
		list, err := manifests.JSONList(p.Objects)
		if err != nil {
			return err
		}
		if _, err := kc.Run(list, "apply", "-f", "-"); err != nil {
			return e.stop(checkResult{name: p.Name, status: checkFail, detail: firstLine(err.Error()),
				hint: e.loginHint(err.Error())}, "kubectl apply failed")
		}
		e.row(checkResult{name: p.Name, status: checkPass, detail: fmt.Sprintf("%d object(s) applied", len(p.Objects))})
		for _, d := range p.Rollouts {
			if err := e.rollout(ctx, kc, d, images); err != nil {
				return err
			}
		}
	}
	return nil
}

// secrets writes the three Secrets the workloads read, by the rules in secrets.go.
func (e *deployEnv) secrets(kc *kube.Client, out deployOutputs) error {
	if err := applyPostgresSecret(kc, platformNS, out.DatabaseURL); err != nil {
		return e.stop(checkResult{name: postgresSecretName, status: checkFail, detail: firstLine(err.Error())}, "cannot write the database secret")
	}
	e.row(checkResult{name: postgresSecretName, status: checkPass, detail: "from the database_url output"})
	for _, s := range []struct {
		name string
		fn   func() (secretOutcome, error)
	}{
		{authSecretName, func() (secretOutcome, error) { return ensureAuthSecret(kc, platformNS, nil) }},
		{userHMACSecretName, func() (secretOutcome, error) { return ensureUserHMAC(kc, platformNS) }},
	} {
		res, err := s.fn()
		if err != nil {
			return e.stop(checkResult{name: s.name, status: checkFail, detail: firstLine(err.Error()),
				hint: "nothing was overwritten; fix kubectl access and re-run"}, "cannot write "+s.name)
		}
		d := "kept"
		switch {
		case res.Created:
			d = "created"
		case len(res.Added) > 0:
			d = "kept; added " + strings.Join(res.Added, ", ")
		}
		e.row(checkResult{name: s.name, status: checkPass, detail: d})
	}
	return nil
}

// rollout waits for one Deployment, then checks that the pods running are on the
// digest that was mirrored: a rollout can finish on the old image when a tag moved.
func (e *deployEnv) rollout(ctx context.Context, kc *kube.Client, deployment string, images map[string]string) error {
	if _, err := kc.Run(nil, "-n", platformNS, "rollout", "status", "deployment/"+deployment, "--timeout="+rolloutTimeout[deployment]); err != nil {
		e.diagnose(kc, deployment)
		return e.stop(checkResult{name: deployment, status: checkFail, detail: firstLine(err.Error()),
			hint: "the pods did not become ready; their state and logs are above. Nothing else was applied"}, deployment+" did not roll out")
	}
	svc := strings.TrimPrefix(deployment, "darkplum-")
	if err := e.checkRunningImage(kc, deployment, images[svc]); err != nil {
		return e.stop(checkResult{name: deployment, status: checkFail, detail: firstLine(err.Error()),
			hint: "the running pods are not on the image that was mirrored"}, deployment+" runs the wrong image")
	}
	e.row(checkResult{name: deployment, status: checkPass, detail: "rolled out · running " + digestOf(images[svc])})
	return nil
}

func digestOf(ref string) string {
	i := strings.Index(ref, "@")
	if i < 0 {
		return ref
	}
	d := ref[i+1:]
	if len(d) > 19 {
		d = d[:19]
	}
	return d
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
func jsonMarshal(v any) ([]byte, error)   { return json.Marshal(v) }

// checkRunningImage requires every pod of the Deployment that is not terminating
// to run want's digest, as the container runtime reports it.
func (e *deployEnv) checkRunningImage(kc *kube.Client, deployment, want string) error {
	digest := want[strings.Index(want, "@")+1:]
	out, err := kc.Run(nil, "-n", platformNS, "get", "pods", "-l", "app.kubernetes.io/name="+deployment, "-o", "json")
	if err != nil {
		return err
	}
	var l struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				DeletionTimestamp string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Spec struct {
				Containers []struct{ Name, Image string } `json:"containers"`
			} `json:"spec"`
			Status struct {
				ContainerStatuses []struct {
					Name    string `json:"name"`
					ImageID string `json:"imageID"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := jsonUnmarshal(out, &l); err != nil {
		return errors.New("unexpected answer listing pods")
	}
	seen := 0
	for _, p := range l.Items {
		if p.Metadata.DeletionTimestamp != "" {
			continue
		}
		for _, c := range p.Spec.Containers {
			if c.Image != want {
				continue
			}
			seen++
			got := ""
			for _, s := range p.Status.ContainerStatuses {
				if s.Name == c.Name {
					got = s.ImageID
				}
			}
			if !strings.Contains(got, digest) {
				return fmt.Errorf("pod %s runs image id %q, not %s", p.Metadata.Name, got, digest)
			}
		}
	}
	if seen == 0 {
		return fmt.Errorf("no running pod of %s uses %s", deployment, want)
	}
	return nil
}

// diagnose prints what a failed rollout looks like from the inside: the pods and
// their events, and the previous container's last log lines. Best effort.
func (e *deployEnv) diagnose(kc *kube.Client, deployment string) {
	sel := "app.kubernetes.io/name=" + deployment
	for _, q := range [][]string{
		{"-n", platformNS, "get", "pods", "-l", sel, "-o", "wide"},
		{"-n", platformNS, "get", "events", "--field-selector", "type=Warning", "--sort-by=.lastTimestamp"},
		{"-n", platformNS, "logs", "deployment/" + deployment, "--tail=30", "--all-containers", "--previous"},
		{"-n", platformNS, "logs", "deployment/" + deployment, "--tail=30", "--all-containers"},
	} {
		out, err := kc.Run(nil, q...)
		if err != nil {
			e.printf("%s\n", sDim("  ("+firstLine(err.Error())+")"))
			continue
		}
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if l != "" {
				e.printf("      %s\n", sDim("│ "+l))
			}
		}
	}
}

// recordInstall writes the darkplum-install ConfigMap: which release, which
// digests, and whether they were verified. status and the upgrade guard read it.
func (e *deployEnv) recordInstall(kc *kube.Client, v verify.Verified, images map[string]string) error {
	m := v.Manifest()
	b, err := bundle.Embedded()
	if err != nil {
		return err
	}
	data := map[string]string{
		"version": e.manifest.Version, "channel": e.manifest.Channel, "commit": e.manifest.Commit,
		"bundle": b.Hash()[:12], "image-trust": trustWord(v), "time": e.now().UTC().Format(time.RFC3339),
		"images-source-version": m.Version,
	}
	for _, svc := range release.Services {
		data["image-"+svc] = images[svc]
	}
	cm := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "darkplum-install", "namespace": platformNS,
			"labels": map[string]string{"app.kubernetes.io/part-of": "darkplum", "app.kubernetes.io/managed-by": "darkplum-deploy"}},
		"data": data,
	}
	body, err := jsonMarshal(cm)
	if err != nil {
		return err
	}
	if _, err = kc.Run(body, "apply", "-f", "-"); err != nil {
		return err
	}
	// The same facts on the Deployments, where `status` reads image trust. Metadata
	// only: it does not roll the pods.
	trust := trustWord(v)
	for _, d := range []string{"darkplum-api", "darkplum-gateway", "darkplum-web"} {
		if _, err := kc.Run(nil, "-n", platformNS, "annotate", "deployment/"+d, "--overwrite",
			"darkplum.ai/image-trust="+trust, "darkplum.ai/release="+e.manifest.Version,
			"darkplum.ai/release-channel="+e.manifest.Channel, "darkplum.ai/source-commit="+e.manifest.Commit); err != nil {
			return err
		}
	}
	return nil
}

// ---- GPU model --------------------------------------------------------------------------

const inferenceNS = "darkplum-inference"

// deployGPU installs the NVIDIA device plugin, waits (bounded) for a GPU to be
// allocatable, then deploys the default vLLM model. It never waits for the model:
// loading takes 15-25 minutes and `configure` shows it. An error leaves the
// platform as it is; the caller reports degraded.
func (e *deployEnv) deployGPU(ctx context.Context, kc *kube.Client, fsys fs.FS, out deployOutputs) error {
	plan, err := manifests.GPU(fsys, manifests.GPUInputs{ModelName: out.ModelName, ModelRevision: out.ModelRevision, ServedModelName: out.ServedModel})
	if err != nil {
		return err
	}
	list, err := manifests.JSONList(plan.DevicePlugin)
	if err != nil {
		return err
	}
	if _, err := kc.Run(list, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("device plugin: %w", err)
	}
	e.row(checkResult{name: "nvidia device plugin", status: checkPass, detail: "applied"})

	deadline := e.now().Add(e.flags.GPUWait)
	for {
		nodes, err := readyNodes(kc, "darkplum.ai/node-type=gpu-inference")
		if err != nil {
			return fmt.Errorf("cannot list GPU nodes: %w", err)
		}
		if gpuAllocatable(kc) && len(nodes) > 0 {
			e.row(checkResult{name: "gpu", status: checkPass, detail: fmt.Sprintf("%d GPU node(s) with an allocatable GPU", len(nodes))})
			break
		}
		if e.now().After(deadline) {
			return fmt.Errorf("no GPU became allocatable within %s (check the GPU node joined: kubectl get nodes -l darkplum.ai/node-type=gpu-inference, and the device plugin pod on it)", e.flags.GPUWait)
		}
		e.printf("%s\n", sDim("  waiting for a GPU node to register and report nvidia.com/gpu ..."))
		if err := e.sleep(ctx, e.poll); err != nil {
			return err
		}
	}

	// clusterIP is immutable: a Service from an earlier release that has one must
	// be recreated as headless; one that already is headless is left alone.
	cur, err := kc.Run(nil, "-n", inferenceNS, "get", "svc", "vllm-inference", "-o", "jsonpath={.spec.clusterIP}")
	switch {
	case err == nil:
		if ip := strings.TrimSpace(cur); ip != "" && ip != "None" {
			e.printf("%s\n", sDim("  recreating Service vllm-inference as headless (was ClusterIP "+ip+")"))
			if _, err := kc.Run(nil, "-n", inferenceNS, "delete", "svc", "vllm-inference"); err != nil {
				return err
			}
		}
	case kubeNotFound(err, "services", "vllm-inference"):
	default:
		return err
	}
	wl, err := manifests.JSONList(plan.Workload)
	if err != nil {
		return err
	}
	if _, err := kc.Run(wl, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("vllm: %w", err)
	}
	e.row(checkResult{name: "vllm-inference", status: checkPass, detail: out.ModelName + " as " + out.ServedModel + " (loading; not waited for)"})
	return nil
}

// gpuAllocatable reports whether any GPU node advertises at least one nvidia.com/gpu.
func gpuAllocatable(kc *kube.Client) bool {
	out, err := kc.Run(nil, "get", "nodes", "-l", "darkplum.ai/node-type=gpu-inference", "-o", `jsonpath={range .items[*]}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}`)
	if err != nil {
		return false
	}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && l != "0" {
			return true
		}
	}
	return false
}

// layer1 runs the IAM self-test. A role that is wider or narrower than the install
// says is a failure; being unable to ask IAM is a warning.
func (e *deployEnv) layer1(ctx context.Context, out deployOutputs) (failed bool) {
	rep, err := e.iam(ctx, iamcheck.Outputs{GatewayRoleARN: out.GatewayRoleARN, InferenceRegion: out.InferenceRegion, BedrockInvokeARNs: out.InvokeARNs})
	var sim *iamcheck.SimulationError
	if errors.As(err, &sim) || (err != nil && len(rep.Checks) == 0) {
		e.row(checkResult{name: "layer 1 (IAM)", status: checkWarn, detail: firstLine(err.Error()),
			hint: "could not ask IAM what the gateway role may do (needs iam:SimulatePrincipalPolicy); the allowlist was not verified"})
		return false
	}
	for _, c := range rep.Checks {
		st := checkPass
		if !c.Pass {
			st = checkFail
		}
		e.row(checkResult{name: "layer 1 (IAM)", status: st, detail: fmt.Sprintf("%s: expected %s, got %s", c.Resource, c.Expect, c.Got)})
	}
	return !rep.Passed()
}
