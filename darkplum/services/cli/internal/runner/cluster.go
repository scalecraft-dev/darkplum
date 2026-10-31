package runner

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Context is the one context in an install's private kubeconfig.
const Context = "darkplum"

// ErrClusterNotFound means the claim names a cluster that does not exist (never
// created, or already destroyed).
var ErrClusterNotFound = errors.New("cluster not found")

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
	kubeconfig                     string
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

// CA is the cluster's base64 certificate authority data.
func (v ValidatedCluster) CA() string { return v.get(func(c *cluster) string { return c.ca }) }

// Kubeconfig is the private kubeconfig path this handle is pinned to ("" until
// WithKubeconfig).
func (v ValidatedCluster) Kubeconfig() string {
	return v.get(func(c *cluster) string { return c.kubeconfig })
}

func (v ValidatedCluster) get(f func(*cluster) string) string {
	if v.c == nil {
		return ""
	}
	return f(v.c)
}

// WithKubeconfig pins the handle to the install's private kubeconfig file (one
// cluster, one exec user, context "darkplum"). kubectl is not runnable through
// a handle until it is.
func (v ValidatedCluster) WithKubeconfig(path string) ValidatedCluster {
	if v.c == nil {
		return v
	}
	c := *v.c
	c.kubeconfig = path
	return ValidatedCluster{&c}
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
func (v ValidatedCluster) Flags() []string {
	if v.c == nil || v.c.kubeconfig == "" {
		return nil
	}
	return []string{"--kubeconfig", v.c.kubeconfig, "--context", Context}
}

// Exec runs kubectl against this cluster and nothing else. Without a pinned
// kubeconfig it runs nothing.
func (v ValidatedCluster) Exec() Exec {
	if v.c == nil || v.c.kubeconfig == "" {
		return noCluster{}
	}
	return kubectlExec{env: v.Env()}
}

var accountRe = regexp.MustCompile(`^[0-9]{12}$`)

// Validate establishes that the claimed cluster is the one the install means and
// that the caller may act on it. It refuses unless the cluster exists, has an
// endpoint and certificate, its ARN names the claim's region and name, and both
// the cluster's account and the caller's credentials are an account the install
// is pinned to. It reads AWS; it changes nothing.
func Validate(cl Claim, aws AWSFunc) (ValidatedCluster, error) {
	refuse := func(detail, hint string, cause error) (ValidatedCluster, error) {
		return ValidatedCluster{}, &Refusal{Detail: detail, Hint: hint, Cause: cause}
	}
	if cl.Region == "" {
		return refuse("install settings name no region", "re-run ./bin/darkplum init", nil)
	}
	if len(cl.AccountIDs) == 0 {
		return refuse("install settings pin no AWS account", "re-run ./bin/darkplum init so the install is pinned to one account", nil)
	}
	login := "aws sso login"
	if cl.Profile != "" {
		login += " --profile " + cl.Profile
	}

	out, err := aws(ProfileArgs(cl.Profile, "eks", "describe-cluster", "--name", cl.Name, "--region", cl.Region,
		"--query", "cluster.{endpoint:endpoint,ca:certificateAuthority.data,arn:arn}", "--output", "json")...)
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
	}
	if json.Unmarshal([]byte(out), &dc) != nil {
		return refuse("unexpected answer describing cluster "+cl.Name, "check your AWS CLI version (2.x)", nil)
	}
	ca, cerr := base64.StdEncoding.DecodeString(dc.CA)
	if !strings.HasPrefix(dc.Endpoint, "https://") || cerr != nil || len(ca) == 0 {
		return refuse("cluster "+cl.Name+" has no usable endpoint or certificate yet", "wait for the deploy to finish, then re-run", nil)
	}
	arnAccount, ok := clusterARNAccount(dc.ARN, cl.Region, cl.Name)
	if !ok {
		return refuse("cluster ARN "+dc.ARN+" does not match "+cl.Name+" in "+cl.Region, "re-run ./bin/darkplum init", nil)
	}

	caller, err := aws(ProfileArgs(cl.Profile, "sts", "get-caller-identity", "--region", cl.Region,
		"--query", "Account", "--output", "text")...)
	caller = strings.TrimSpace(caller)
	if err != nil || caller == "" {
		return refuse("cannot read who the AWS credentials belong to", "check your AWS session (`"+login+"`)", nil)
	}
	if caller != arnAccount {
		return refuse(fmt.Sprintf("credentials are for account %s, the cluster is in %s", caller, arnAccount),
			"use the profile for the install's account (re-run ./bin/darkplum init to change it)", nil)
	}
	pinned := false
	for _, a := range cl.AccountIDs {
		pinned = pinned || a == arnAccount
	}
	if !pinned {
		return refuse(fmt.Sprintf("cluster %s is in account %s, not the install's account %s", cl.Name, arnAccount, strings.Join(cl.AccountIDs, ",")),
			"use the profile for the install's account, or re-run ./bin/darkplum init", nil)
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

// LocalKind is a kind cluster on this machine: a context named kind-<name> in the
// operator's own kubeconfig whose API server is a loopback address. It is the
// only way to point the CLI at a cluster Darkplum did not install, it cannot
// reach AWS, and it is a different type from ValidatedCluster so it can never
// be passed where an install's cluster is required.
type LocalKind struct{ context string }

var kindContextRe = regexp.MustCompile(`^kind-[a-z0-9][a-z0-9.-]*$`)

// Context is the kubeconfig context name.
func (k LocalKind) Context() string { return k.context }

// Flags are the selectors every kubectl invocation through this handle carries.
func (k LocalKind) Flags() []string {
	if k.context == "" {
		return nil
	}
	return []string{"--context", k.context}
}

// Exec runs kubectl against the kind context and nothing else.
func (k LocalKind) Exec() Exec {
	if k.context == "" {
		return noCluster{}
	}
	return kubectlExec{}
}

// NewLocalKind accepts only a kind-* context whose server is on this machine.
func NewLocalKind(context string) (LocalKind, error) {
	return newLocalKind(context, func(ctx string) (string, error) {
		out, err := kubectlExec{}.Run(nil, "kubectl", "--context", ctx, "config", "view", "--minify",
			"-o", "jsonpath={.clusters[0].cluster.server}")
		return strings.TrimSpace(string(out)), err
	})
}

func newLocalKind(context string, serverOf func(string) (string, error)) (LocalKind, error) {
	if !kindContextRe.MatchString(context) {
		return LocalKind{}, fmt.Errorf("--context %q refused: only local kind clusters (contexts named kind-*) can be used without an install; "+
			"a Darkplum install's cluster is found from its settings", context)
	}
	server, err := serverOf(context)
	if err != nil {
		return LocalKind{}, fmt.Errorf("cannot read context %s: %w", context, err)
	}
	if !loopbackServer(server) {
		return LocalKind{}, fmt.Errorf("--context %q refused: its API server %q is not on this machine", context, server)
	}
	return LocalKind{context: context}, nil
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
