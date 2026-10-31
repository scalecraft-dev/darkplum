package runner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/scalecraft-dev/darkplum/cli/internal/bundle"
)

// H2: whose credentials these are is decided before the cluster is looked at, so
// credentials for another account can never learn whether the cluster exists.
func TestValidateChecksTheCallerBeforeDescribingTheCluster(t *testing.T) {
	for name, tc := range map[string]struct {
		caller      string
		describeErr error
	}{
		"wrong account, cluster missing":   {"999999999999", errors.New("ResourceNotFoundException")},
		"wrong account, cluster present":   {"999999999999", nil},
		"wrong account, describe denied":   {"999999999999", errors.New("AccessDenied")},
		"unreadable credentials, missing":  {"", errors.New("ResourceNotFoundException")},
		"malformed account, cluster there": {"not-an-account", nil},
	} {
		t.Run(name, func(t *testing.T) {
			a := goodAWS()
			a.caller, a.describeErr = tc.caller, tc.describeErr
			_, err := Validate(claim(), a.run)
			if err == nil {
				t.Fatal("validated")
			}
			if errors.Is(err, ErrClusterNotFound) {
				t.Errorf("wrong-account credentials were told the cluster does not exist: %v", err)
			}
			for _, c := range a.calls {
				if strings.Contains(strings.Join(c, " "), "describe-cluster") {
					t.Errorf("describe-cluster was called for credentials that do not belong to the install: %v", a.calls)
				}
			}
		})
	}
	// and with the right account a missing cluster is still reported as missing
	a := goodAWS()
	a.describeErr = errors.New("An error occurred (ResourceNotFoundException)")
	if _, err := Validate(claim(), a.run); !errors.Is(err, ErrClusterNotFound) {
		t.Errorf("pinned-account credentials: %v", err)
	}
}

func written(t *testing.T) (ValidatedCluster, string) {
	t.Helper()
	v, err := Validate(claim(), goodAWS().run)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ws", "kubeconfig")
	h, err := v.WriteKubeconfig(p)
	if err != nil {
		t.Fatal(err)
	}
	return h, p
}

// H3: the runner writes the kubeconfig, from the validated endpoint and CA.
func TestRunnerWritesThePrivateKubeconfigItself(t *testing.T) {
	h, p := written(t)
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig: %v %v", st, err)
	}
	if d, _ := os.Stat(filepath.Dir(p)); d.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", d.Mode().Perm())
	}
	raw, _ := os.ReadFile(p)
	var cfg struct {
		Current  string `json:"current-context"`
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
				CA     string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User struct {
				Exec struct {
					Command string   `json:"command"`
					Args    []string `json:"args"`
				} `json:"exec"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	g := goodAWS()
	if cfg.Current != Context || len(cfg.Clusters) != 1 || cfg.Clusters[0].Cluster.Server != g.endpoint || cfg.Clusters[0].Cluster.CA != g.ca {
		t.Errorf("kubeconfig = %s", raw)
	}
	if cfg.Users[0].User.Exec.Command != "aws" || !strings.Contains(strings.Join(cfg.Users[0].User.Exec.Args, " "), "--profile acme --region us-east-2 eks get-token --cluster-name dp-trial") {
		t.Errorf("exec stanza = %+v", cfg.Users[0].User.Exec)
	}
	if strings.Contains(string(raw), "insecure-skip-tls-verify") || strings.Contains(string(raw), "\"token\"") {
		t.Errorf("kubeconfig holds a token or skips TLS:\n%s", raw)
	}
	if h.Kubeconfig() != p {
		t.Errorf("handle path %q", h.Kubeconfig())
	}
	// no way to pin a handle to a file the runner did not write
	var z ValidatedCluster
	if _, err := z.WriteKubeconfig(p); err == nil {
		t.Error("the zero handle wrote a kubeconfig")
	}
}

// H3: a kubeconfig edited after the handle wrote it is not used.
func TestExecRefusesAKubeconfigThatChanged(t *testing.T) {
	h, p := written(t)
	ex := h.Exec().(kubectlExec)
	if err := ex.check(); err != nil {
		t.Fatalf("fresh file: %v", err)
	}
	for name, tamper := range map[string]func(){
		"another cluster's server": func() {
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, []byte(strings.Replace(string(b), "https://ABC.eks.amazonaws.com", "https://evil.example.com", 1)), 0o600)
		},
		"deleted": func() { _ = os.Remove(p) },
		"empty":   func() { _ = os.WriteFile(p, nil, 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			tamper()
			if err := ex.check(); err == nil {
				t.Error("changed kubeconfig accepted")
			}
			if _, err := h.Exec().Run(nil, Kubectl, "get", "pods"); err == nil || !strings.Contains(err.Error(), "kubeconfig") {
				t.Errorf("kubectl ran against a changed kubeconfig: %v", err)
			}
			if _, err := h.PortForward("ns", "svc/x").Open(t.Context()); err == nil {
				t.Error("a tunnel opened against a changed kubeconfig")
			}
			h2, _ := Validate(claim(), goodAWS().run)
			_, _ = h2.WriteKubeconfig(p) // restore for the next case
			h, _ = h2.WriteKubeconfig(p)
			ex = h.Exec().(kubectlExec)
		})
	}
}

// H4: callers cannot select another cluster or identity, in any spelling.
func TestScreenArgsRefusesSelectors(t *testing.T) {
	bad := [][]string{
		{"--kubeconfig", "/x"}, {"--kubeconfig=/x"}, {"--context", "prod"}, {"--context=prod"},
		{"--cluster", "c"}, {"--cluster=c"}, {"--server", "https://x"}, {"--server=https://x"}, {"-s", "https://x"}, {"-shttps://x"},
		{"--user", "u"}, {"--user=u"}, {"--token", "t"}, {"--token=t"}, {"--as", "root"}, {"--as=root"},
		{"--as-group", "g"}, {"--as-uid=0"}, {"--certificate-authority=/x"}, {"--client-key", "/k"}, {"--client-certificate=/c"},
		{"--insecure-skip-tls-verify"}, {"--insecure-skip-tls-verify=true"}, {"--tls-server-name=x"}, {"--username=u"}, {"--password", "p"},
		{"get", "pods", "-n", "ns", "--context=prod"}, {"-n", "ns", "get", "pods", "--server", "https://x"},
	}
	for _, args := range bad {
		if err := screenArgs(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	good := [][]string{
		{"get", "pods"}, {"-n", "darkplum-system", "get", "svc", "darkplum-web", "-o", "json"}, {"apply", "-f", "-"},
		{"get", "pods", "--selector", "app=x", "-l", "a=b", "-o", "jsonpath={.items}"}, {"rollout", "status", "deploy/x", "--timeout=300s"},
		{"create", "secret", "generic", "x", "--from-literal=SERVER=1"},
		// after -- the words belong to the command a subcommand runs, not to kubectl
		{"exec", "pod", "--", "sh", "-c", "curl --server x; echo --context"},
	}
	for _, args := range good {
		if err := screenArgs(args); err != nil {
			t.Errorf("refused %v: %v", args, err)
		}
	}
	// a selector BEFORE -- is still refused even when one follows it
	if err := screenArgs([]string{"exec", "--context=prod", "pod", "--", "ls"}); err == nil {
		t.Error("selector before -- accepted")
	}
}

// H4: through a real handle, the exec puts the selectors first and refuses the
// caller's. A fake kubectl on PATH records argv.
func TestExecPrependsSelectorsAndRefusesOverrides(t *testing.T) {
	h, p := written(t)
	bin := t.TempDir()
	logf := filepath.Join(bin, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > '" + logf + "'\nprintf '%s\\n' \"KUBECONFIG=$KUBECONFIG AWS_PROFILE=$AWS_PROFILE\" >> '" + logf + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := h.Exec().Run(nil, Kubectl, "-n", "darkplum-system", "get", "pods"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(logf)
	want := "--kubeconfig " + p + " --context darkplum -n darkplum-system get pods\nKUBECONFIG=" + p + " AWS_PROFILE=acme\n"
	if string(got) != want {
		t.Errorf("argv/env = %q\nwant %q", got, want)
	}
	_ = os.Remove(logf)
	for _, args := range [][]string{{"get", "pods", "--context", "prod"}, {"--kubeconfig=/other", "get", "pods"}, {"get", "pods", "-s", "https://x"}} {
		if _, err := h.Exec().Run(nil, Kubectl, args...); err == nil {
			t.Errorf("accepted %v", args)
		}
		if _, err := os.Stat(logf); err == nil {
			t.Errorf("kubectl was started for %v", args)
		}
	}
	// handles from different installs never share a kubeconfig path or selector set
	if _, err := h.Exec().Run(nil, "aws", "x"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("handle ran aws: %v", err)
	}
}

func TestHandleIsInertUntilWritten(t *testing.T) {
	v, _ := Validate(claim(), goodAWS().run)
	if v.Flags() != nil || v.Kubeconfig() != "" {
		t.Error("an unwritten handle has selectors")
	}
	if _, err := v.Exec().Run(nil, Kubectl, "get", "pods"); err == nil {
		t.Error("kubectl ran before the kubeconfig was written")
	}
	var z ValidatedCluster
	if z.Valid() || z.Flags() != nil || z.Env() != nil || z.Name() != "" {
		t.Error("zero handle is not inert")
	}
	if _, err := z.Exec().Run(nil, Kubectl, "version"); err == nil {
		t.Error("zero handle ran kubectl")
	}
	if _, err := z.PortForward("ns", "svc/x").Open(t.Context()); err == nil {
		t.Error("zero handle opened a tunnel")
	}
}

func TestHandleExecRunsOnlyKubectl(t *testing.T) {
	h, _ := written(t)
	for _, name := range []string{"aws", "terraform", "helm", "sh", "/usr/bin/kubectl", "kubectl ", ""} {
		if _, err := h.Exec().Run(nil, name, "x"); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("handle ran %q: %v", name, err)
		}
	}
}

func fakeServer(m map[string]string) func(file, ctx string) (string, error) {
	return func(file, ctx string) (string, error) {
		if s, ok := m[file+"|"+ctx]; ok {
			return s, nil
		}
		return "", errors.New("context was not found")
	}
}

func TestLocalKindOnlyAdmitsLoopbackKindContexts(t *testing.T) {
	dir := t.TempDir()
	f1, f2 := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, f := range []string{f1, f2} {
		_ = os.WriteFile(f, []byte("x"), 0o600)
	}
	for _, ok := range []struct{ ctx, server string }{
		{"kind-darkplum", "https://127.0.0.1:52000"},
		{"kind-dev", "https://localhost:6443"},
		{"kind-a.b", "https://[::1]:6443"},
	} {
		k, err := newLocalKind(ok.ctx, []string{f1}, fakeServer(map[string]string{f1 + "|" + ok.ctx: ok.server}))
		if err != nil || k.Context() != ok.ctx || k.File() != f1 {
			t.Errorf("%s %s refused: %v", ok.ctx, ok.server, err)
		}
		if strings.Join(k.Flags(), " ") != "--kubeconfig "+f1+" --context "+ok.ctx {
			t.Errorf("flags = %v", k.Flags())
		}
	}
	for _, bad := range []struct{ ctx, server string }{
		{"darkplum", "https://127.0.0.1:1"}, {"prod", "https://127.0.0.1:1"}, {"arn:aws:eks:us-east-2:1:cluster/x", "https://127.0.0.1:1"},
		{"kind-", "https://127.0.0.1:1"}, {"Kind-x", "https://127.0.0.1:1"}, {"kind-x y", "https://127.0.0.1:1"},
		{"kind-prod", "https://ABC.gr7.us-east-2.eks.amazonaws.com"}, // named like kind, points at a real cluster
		{"kind-prod", "https://10.0.0.5:6443"}, {"kind-prod", "http://127.0.0.1:8080"}, {"kind-prod", "https://127.0.0.1.evil.com:443"},
		{"kind-prod", ""},
	} {
		if k, err := newLocalKind(bad.ctx, []string{f1}, fakeServer(map[string]string{f1 + "|" + bad.ctx: bad.server})); err == nil || k.Context() != "" {
			t.Errorf("%q -> %q admitted", bad.ctx, bad.server)
		}
	}
	// the context must be defined in one of the files; the file that defines it is the one pinned
	k, err := newLocalKind("kind-x", []string{f1, f2}, fakeServer(map[string]string{f2 + "|kind-x": "https://127.0.0.1:1"}))
	if err != nil || k.File() != f2 {
		t.Errorf("pinned %q, %v: the file that defines the context must be pinned", k.File(), err)
	}
	if _, err := newLocalKind("kind-x", []string{f1, f2}, fakeServer(nil)); err == nil {
		t.Error("a context defined nowhere was admitted")
	}
	if _, err := newLocalKind("kind-x", []string{filepath.Join(dir, "missing")}, fakeServer(nil)); err == nil {
		t.Error("a missing kubeconfig file was admitted")
	}
	// an earlier file that redefines the context as remote wins, as it does for kubectl
	if _, err := newLocalKind("kind-x", []string{f1, f2}, fakeServer(map[string]string{f1 + "|kind-x": "https://prod.example.com", f2 + "|kind-x": "https://127.0.0.1:1"})); err == nil {
		t.Error("a remote definition earlier on KUBECONFIG was ignored")
	}
	// exec through a LocalKind pins the same file and context, and refuses overrides
	lk := LocalKind{context: "kind-x", file: f2}
	ex := lk.Exec().(kubectlExec)
	if strings.Join(ex.prefix, " ") != "--kubeconfig "+f2+" --context kind-x" || strings.Join(ex.env, " ") != "KUBECONFIG="+f2 {
		t.Errorf("exec = %+v", ex)
	}
	if _, err := lk.Exec().Run(nil, Kubectl, "get", "pods", "--context=prod"); err == nil {
		t.Error("LocalKind accepted a context override")
	}
	if _, err := (LocalKind{}).Exec().Run(nil, Kubectl, "get", "pods"); err == nil {
		t.Error("zero LocalKind ran kubectl")
	}
}

func TestKubeconfigCandidatesFollowKubectlsOrder(t *testing.T) {
	t.Setenv("KUBECONFIG", "/a:/b::/c")
	if got := strings.Join(kubeconfigCandidates(), ","); got != "/a,/b,/c" {
		t.Errorf("candidates = %s", got)
	}
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", "/home/op")
	if got := strings.Join(kubeconfigCandidates(), ","); got != "/home/op/.kube/config" {
		t.Errorf("candidates = %s", got)
	}
}

// Terraform runs only from a verified bundle, in the workspace, with nothing
// written into the bundle.
func TestTerraformRequiresAVerifiedBundleAndKeepsOutputsOutOfIt(t *testing.T) {
	b, err := bundle.New(fstest.MapFS{"terraform/aws/main.tf": {Data: []byte("# main")}, "k8s/base/kustomization.yaml": {Data: []byte("k")}})
	if err != nil {
		t.Fatal(err)
	}
	bdir := filepath.Join(t.TempDir(), "bundle", "k")
	if err := b.Extract(bdir); err != nil {
		t.Fatal(err)
	}
	v, err := bundle.Verify(bdir, b.Hash())
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	dirs := TerraformDirs{Workspace: ws, Data: filepath.Join(ws, "tfdata", "k"), PluginCache: filepath.Join(ws, "plugins"),
		BackendHCL: filepath.Join(ws, "backend.hcl")}

	if _, _, err := Terraform(bundle.Verified{}, dirs, nil, "version"); err == nil {
		t.Error("terraform ran without a verified bundle")
	}
	for _, a := range []string{"-chdir=/elsewhere", "-chdir"} {
		if _, _, err := Terraform(v, dirs, nil, a, "version"); err == nil {
			t.Errorf("caller-supplied %s accepted", a)
		}
	}
	inside := dirs
	inside.Data = filepath.Join(v.Dir(), "tfdata")
	if _, _, err := Terraform(v, inside, nil, "version"); err == nil {
		t.Error("a data directory inside the bundle was accepted")
	}
	inside = dirs
	inside.Workspace = v.Dir()
	if _, _, err := Terraform(v, inside, nil, "version"); err == nil {
		t.Error("a working directory inside the bundle was accepted")
	}
	rel := dirs
	rel.Data = "tfdata"
	if _, _, err := Terraform(v, rel, nil, "version"); err == nil {
		t.Error("a relative data directory was accepted")
	}
	if _, err := TerraformInit(v, TerraformDirs{Workspace: ws, Data: dirs.Data, BackendHCL: "backend.hcl"}, "", nil); err == nil {
		t.Error("a relative backend.hcl was accepted")
	}

	// A fake terraform records argv, cwd and env: -chdir is the bundle, cwd the
	// workspace, TF_DATA_DIR the workspace's, init is -lockfile=readonly with the
	// backend config, and the bundle is untouched afterwards.
	bin := t.TempDir()
	logf := filepath.Join(bin, "log")
	script := "#!/bin/sh\n{ echo \"ARGS $*\"; echo \"CWD $(pwd)\"; echo \"DATA $TF_DATA_DIR\"; echo \"CACHE $TF_PLUGIN_CACHE_DIR\"; echo \"AUTO $TF_IN_AUTOMATION $TF_INPUT\"; } > '" + logf + "'\nmkdir -p \"$TF_DATA_DIR\" && echo x > \"$TF_DATA_DIR/written\"\n"
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := TerraformInit(v, dirs, "acme", []string{"AWS_PROFILE=acme"}); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(logf)
	resolved := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
	for _, want := range []string{
		"ARGS -chdir=" + v.TerraformDir() + " init -input=false -lockfile=readonly -backend-config=" + dirs.BackendHCL + " -backend-config=profile=acme",
		"CWD " + resolved(ws),
		"DATA " + dirs.Data,
		"CACHE " + dirs.PluginCache,
		"AUTO 1 0",
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("terraform saw:\n%s\nlacks %q", log, want)
		}
	}
	if _, err := bundle.Verify(v.Dir(), b.Hash()); err != nil {
		t.Errorf("the bundle changed during a terraform run: %v", err)
	}
}
