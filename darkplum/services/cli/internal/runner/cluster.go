package runner

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Context is the one context in an install's private kubeconfig.
const Context = "darkplum"

// ErrClusterNotFound means the claim names a cluster that does not exist (never
// created, or already destroyed).
var ErrClusterNotFound = errors.New("cluster not found")

// ErrClusterNotReady means the cluster exists but cannot be used yet: it is still
// being created or updated into existence (no endpoint or certificate, or a status
// other than ACTIVE or UPDATING). It is not a refusal to be obeyed: a deploy that
// was interrupted is exactly in this state and must be able to continue.
var ErrClusterNotReady = errors.New("cluster not ready")

// NotReadyError is the typed form of ErrClusterNotReady (errors.Is matches both).
// Status is the EKS cluster status as AWS reported it (CREATING, FAILED, ...);
// callers decide on it, never on the display text. It is empty-valued only when
// AWS reported none.
type NotReadyError struct {
	Cluster string
	Status  string
	Why     string
}

func (e *NotReadyError) Error() string {
	return fmt.Sprintf("%s: %s %s", ErrClusterNotReady, e.Cluster, e.Why)
}

func (e *NotReadyError) Is(target error) bool { return target == ErrClusterNotReady }

// Claim is what the install says its cluster is. Nothing in it is trusted until
// Validate has checked it against AWS.
type Claim struct {
	Name, Region, Profile string
	// AccountIDs are the accounts the install is pinned to.
	AccountIDs []string
}

// AWSFunc runs the aws CLI (arguments only; the program is fixed) and returns
// trimmed stdout. Tests supply a fake; production uses AWS.
type AWSFunc func(args ...string) (string, error)

// AWS is the real thing: the operator's own aws CLI under their credentials.
func AWS(args ...string) (string, error) { return Tool("aws", args...) }

// ProfileArgs prefixes `--profile <p>` when a profile is set, so every check
// runs as the account the install uses, not whatever the ambient default
// credential chain resolves to.
func ProfileArgs(profile string, args ...string) []string {
	if profile != "" {
		return append([]string{"--profile", profile}, args...)
	}
	return args
}

// Refusal is why Validate did not hand out a handle. Detail says what is wrong,
// Hint what to do. Cause wraps ErrClusterNotFound where that applies.
type Refusal struct {
	Detail, Hint string
	Cause        error
}

func (r *Refusal) Error() string {
	if r.Cause != nil {
		return r.Cause.Error()
	}
	return r.Detail
}

func (r *Refusal) Unwrap() error { return r.Cause }

type cluster struct {
	name, region, profile, account string
	endpoint, ca                   string
	// kubeconfig is the file this handle wrote, and sum its SHA-256: both are set
	// only by WriteKubeconfig, and Exec re-reads the file against sum on every call.
	kubeconfig string
	sum        [32]byte
}

// ValidatedCluster is a cluster that passed Validate. Its fields are unexported
// and there is no constructor other than Validate, so holding one is proof the
// checks ran. The zero value is usable only to be refused.
type ValidatedCluster struct{ c *cluster }

// Valid reports whether this is a real handle.
func (v ValidatedCluster) Valid() bool { return v.c != nil }

func (v ValidatedCluster) Name() string   { return v.get(func(c *cluster) string { return c.name }) }
func (v ValidatedCluster) Region() string { return v.get(func(c *cluster) string { return c.region }) }
func (v ValidatedCluster) Profile() string {
	return v.get(func(c *cluster) string { return c.profile })
}
func (v ValidatedCluster) Account() string {
	return v.get(func(c *cluster) string { return c.account })
}
func (v ValidatedCluster) Endpoint() string {
	return v.get(func(c *cluster) string { return c.endpoint })
}

// Kubeconfig is the private kubeconfig path this handle wrote ("" until
// WriteKubeconfig).
func (v ValidatedCluster) Kubeconfig() string {
	return v.get(func(c *cluster) string { return c.kubeconfig })
}

func (v ValidatedCluster) get(f func(*cluster) string) string {
	if v.c == nil {
		return ""
	}
	return f(v.c)
}

// WriteKubeconfig renders the install's private kubeconfig from the endpoint and
// certificate Validate read from AWS, writes it atomically to path (0600, its
// directory 0700), and returns the handle pinned to it. The caller cannot supply
// the contents: kubectl through this handle can only ever reach the cluster that
// was validated. The handle remembers the file's hash and refuses to run kubectl
// if the file has changed since (it is re-read on every call).
func (v ValidatedCluster) WriteKubeconfig(path string) (ValidatedCluster, error) {
	if v.c == nil {
		return v, errors.New("runner: no validated cluster")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return v, err
	}
	b := renderKubeconfig(v.c)
	if err := writePrivateFile(abs, b); err != nil {
		return v, err
	}
	c := *v.c
	c.kubeconfig, c.sum = abs, sha256.Sum256(b)
	return ValidatedCluster{&c}, nil
}

// Env is what kubectl needs: the install's AWS profile for the exec credential
// (`aws eks get-token`) and KUBECONFIG, so plugins agree with --kubeconfig.
func (v ValidatedCluster) Env() []string {
	if v.c == nil {
		return nil
	}
	var env []string
	if v.c.profile != "" {
		env = append(env, "AWS_PROFILE="+v.c.profile)
	}
	if v.c.kubeconfig != "" {
		env = append(env, "KUBECONFIG="+v.c.kubeconfig)
	}
	return env
}

// Flags are the selectors every kubectl invocation through this handle carries.
// Exec adds them itself; they are exposed for the port-forward and for display.
func (v ValidatedCluster) Flags() []string {
	if v.c == nil || v.c.kubeconfig == "" {
		return nil
	}
	return []string{"--kubeconfig", v.c.kubeconfig, "--context", Context}
}

// Exec runs kubectl against this cluster and nothing else. It prepends the
// pinned selectors itself and refuses any argument that would select a different
// cluster, credentials or identity; without a pinned kubeconfig, or if the file
// no longer matches what the handle wrote, it runs nothing.
func (v ValidatedCluster) Exec() Exec {
	if v.c == nil || v.c.kubeconfig == "" {
		return noCluster{}
	}
	c := v.c
	return kubectlExec{
		prefix: v.Flags(),
		env:    v.Env(),
		check: func() error {
			b, err := os.ReadFile(c.kubeconfig)
			if err != nil {
				return fmt.Errorf("runner: the install's kubeconfig is unreadable: %w", err)
			}
			if sha256.Sum256(b) != c.sum {
				return errors.New("runner: the install's kubeconfig changed since it was written; refusing to run kubectl against it")
			}
			return nil
		},
	}
}

// renderKubeconfig is the private kubeconfig: endpoint, CA and an exec stanza
// (no token is ever stored). JSON is valid YAML, and escapes every value.
func renderKubeconfig(c *cluster) []byte {
	args := ProfileArgs(c.profile, "--region", c.region, "eks", "get-token", "--cluster-name", c.name, "--output", "json")
	var env []map[string]string
	if c.profile != "" {
		env = []map[string]string{{"name": "AWS_PROFILE", "value": c.profile}}
	}
	cfg := map[string]any{
		"apiVersion":      "v1",
		"kind":            "Config",
		"current-context": Context,
		"clusters": []any{map[string]any{"name": Context, "cluster": map[string]any{
			"server": c.endpoint, "certificate-authority-data": c.ca}}},
		"users": []any{map[string]any{"name": Context, "user": map[string]any{"exec": map[string]any{
			"apiVersion": "client.authentication.k8s.io/v1beta1", "command": "aws", "args": args,
			"env": env, "interactiveMode": "Never"}}}},
		"contexts": []any{map[string]any{"name": Context, "context": map[string]any{
			"cluster": Context, "user": Context}}},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return append(b, '\n')
}

// writePrivateFile writes b to path (0600, directory 0700) by renaming a
// temporary file over it, so a concurrent reader never sees half a file.
func writePrivateFile(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".kubeconfig-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op after the rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

var accountRe = regexp.MustCompile(`^[0-9]{12}$`)

// Validate establishes that the claimed cluster is the one the install means and
// that the caller may act on it. The order matters: who the credentials belong to
// is checked FIRST and must be an account the install is pinned to, and only then
// is the cluster described, so credentials for another account never learn
// whether the cluster exists. Then the cluster must exist, have an endpoint and
// certificate, and its ARN must name the claim's region and name and the same
// account. It reads AWS; it changes nothing.
func Validate(cl Claim, aws AWSFunc) (ValidatedCluster, error) {
	refuse := func(detail, hint string, cause error) (ValidatedCluster, error) {
		return ValidatedCluster{}, &Refusal{Detail: detail, Hint: hint, Cause: cause}
	}
	if cl.Region == "" {
		return refuse("install settings name no region", "re-run darkplum init", nil)
	}
	if len(cl.AccountIDs) == 0 {
		return refuse("install settings pin no AWS account", "re-run darkplum init so the install is pinned to one account", nil)
	}
	login := "aws sso login"
	if cl.Profile != "" {
		login += " --profile " + cl.Profile
	}

	// 1. Whose credentials are these? An account the install is pinned to, or stop.
	caller, err := aws(ProfileArgs(cl.Profile, "sts", "get-caller-identity", "--region", cl.Region,
		"--query", "Account", "--output", "text")...)
	caller = strings.TrimSpace(caller)
	if err != nil || !accountRe.MatchString(caller) {
		return refuse("cannot read who the AWS credentials belong to", "check your AWS session (`"+login+"`)", nil)
	}
	pinned := false
	for _, a := range cl.AccountIDs {
		pinned = pinned || a == caller
	}
	if !pinned {
		return refuse(fmt.Sprintf("credentials are for account %s, not the install's account %s", caller, strings.Join(cl.AccountIDs, ",")),
			"use the profile for the install's account (re-run darkplum init to change it)", nil)
	}

	// 2. Only now look at the cluster.
	out, err := aws(ProfileArgs(cl.Profile, "eks", "describe-cluster", "--name", cl.Name, "--region", cl.Region,
		"--query", "cluster.{endpoint:endpoint,ca:certificateAuthority.data,arn:arn,status:status}", "--output", "json")...)
	if err != nil {
		if strings.Contains(err.Error(), "ResourceNotFoundException") {
			return refuse("cluster "+cl.Name+" does not exist in "+cl.Region, "deploy the install first",
				fmt.Errorf("%w: %s in %s", ErrClusterNotFound, cl.Name, cl.Region))
		}
		return refuse("cannot read cluster "+cl.Name+" in "+cl.Region+": "+firstLine(err.Error()),
			"check your AWS session (`"+login+"`) and that the deploy finished", nil)
	}
	var dc struct {
		Endpoint string `json:"endpoint"`
		CA       string `json:"ca"`
		ARN      string `json:"arn"`
		Status   string `json:"status"`
	}
	if json.Unmarshal([]byte(out), &dc) != nil {
		return refuse("unexpected answer describing cluster "+cl.Name, "check your AWS CLI version (2.x)", nil)
	}
	ca, cerr := base64.StdEncoding.DecodeString(dc.CA)
	arnAccount, ok := clusterARNAccount(dc.ARN, cl.Region, cl.Name)
	if !ok {
		return refuse("cluster ARN "+dc.ARN+" does not match "+cl.Name+" in "+cl.Region, "re-run darkplum init", nil)
	}
	if arnAccount != caller {
		return refuse(fmt.Sprintf("credentials are for account %s, the cluster is in %s", caller, arnAccount),
			"use the profile for the install's account (re-run darkplum init to change it)", nil)
	}
	// Only after the account is established: a cluster that is still being created
	// in ANOTHER account is a refusal, not "not ready".
	notReady := func(why string) (ValidatedCluster, error) {
		return refuse("cluster "+cl.Name+" "+why, "wait for the deploy to finish, then re-run",
			&NotReadyError{Cluster: cl.Name, Status: dc.Status, Why: why})
	}
	if dc.Status != "" && dc.Status != "ACTIVE" && dc.Status != "UPDATING" {
		return notReady("is " + dc.Status)
	}
	if !strings.HasPrefix(dc.Endpoint, "https://") || cerr != nil || len(ca) == 0 {
		return notReady("has no usable endpoint or certificate yet")
	}
	return ValidatedCluster{&cluster{name: cl.Name, region: cl.Region, profile: cl.Profile, account: arnAccount,
		endpoint: dc.Endpoint, ca: dc.CA}}, nil
}

// clusterARNAccount checks arn:<partition>:eks:<region>:<account>:cluster/<name>
// and returns the account.
func clusterARNAccount(arn, region, name string) (string, bool) {
	f := strings.SplitN(arn, ":", 6)
	if len(f) != 6 || f[0] != "arn" || !strings.HasPrefix(f[1], "aws") || f[2] != "eks" ||
		f[3] != region || !accountRe.MatchString(f[4]) || f[5] != "cluster/"+name {
		return "", false
	}
	return f[4], true
}

// LocalKind is a kind cluster on this machine: a context named kind-<name> in a
// kubeconfig file of the operator's whose API server is a loopback address. It is
// the only way to point the CLI at a cluster Darkplum did not install, it cannot
// reach AWS, and it is a different type from ValidatedCluster so it can never be
// passed where an install's cluster is required.
//
// The check pins the file it found the context in: kubectl is run with
// --kubeconfig <that file> and KUBECONFIG set to it, so a different file earlier
// on $KUBECONFIG cannot redefine the context afterwards.
//
// Residual risk, by design not closed: a loopback address can be a tunnel. A
// context named kind-x whose server is 127.0.0.1:PORT, with PORT forwarded to a
// remote API server (ssh -L), looks local and is not. The kind- prefix and the
// loopback check make mistakes unlikely, not malice impossible; this flag is for
// the operator's own machine, and an install's cluster never goes through it.
type LocalKind struct{ context, file string }

var kindContextRe = regexp.MustCompile(`^kind-[a-z0-9][a-z0-9.-]*$`)

// Context is the kubeconfig context name.
func (k LocalKind) Context() string { return k.context }

// File is the kubeconfig file the context was found in.
func (k LocalKind) File() string { return k.file }

// Flags are the selectors every kubectl invocation through this handle carries.
func (k LocalKind) Flags() []string {
	if k.context == "" {
		return nil
	}
	return []string{"--kubeconfig", k.file, "--context", k.context}
}

// Exec runs kubectl against the kind context and nothing else.
func (k LocalKind) Exec() Exec {
	if k.context == "" {
		return noCluster{}
	}
	return kubectlExec{prefix: k.Flags(), env: []string{"KUBECONFIG=" + k.file}}
}

// NewLocalKind accepts only a kind-* context whose server is on this machine.
func NewLocalKind(context string) (LocalKind, error) {
	return newLocalKind(context, kubeconfigCandidates(), func(file, ctx string) (string, error) {
		out, err := kubectlExec{}.Run(nil, Kubectl, "--kubeconfig", file, "--context", ctx, "config", "view", "--minify",
			"-o", "jsonpath={.clusters[0].cluster.server}")
		return strings.TrimSpace(string(out)), err
	})
}

// kubeconfigCandidates are the files kubectl would merge, in its order.
func kubeconfigCandidates() []string {
	var files []string
	if env := os.Getenv("KUBECONFIG"); env != "" {
		for _, f := range filepath.SplitList(env) {
			if f != "" {
				files = append(files, f)
			}
		}
		return files
	}
	if h, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(h, ".kube", "config"))
	}
	return files
}

func newLocalKind(context string, files []string, serverOf func(file, ctx string) (string, error)) (LocalKind, error) {
	if !kindContextRe.MatchString(context) {
		return LocalKind{}, fmt.Errorf("--context %q refused: only local kind clusters (contexts named kind-*) can be used without an install; "+
			"a Darkplum install's cluster is found from its settings", context)
	}
	var lastErr error
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		server, err := serverOf(f, context)
		if err != nil {
			lastErr = err // the context is not defined in this file
			continue
		}
		if !loopbackServer(server) {
			return LocalKind{}, fmt.Errorf("--context %q refused: its API server %q is not on this machine", context, server)
		}
		abs, err := filepath.Abs(f)
		if err != nil {
			return LocalKind{}, err
		}
		return LocalKind{context: context, file: abs}, nil
	}
	if lastErr != nil {
		return LocalKind{}, fmt.Errorf("cannot read context %s: %w", context, lastErr)
	}
	return LocalKind{}, fmt.Errorf("context %s is not defined in any kubeconfig file (looked at %s)", context, strings.Join(files, ", "))
}

func loopbackServer(server string) bool {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
