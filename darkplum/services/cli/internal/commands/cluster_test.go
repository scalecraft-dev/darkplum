package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

const (
	testEndpoint = "https://ABC123.gr7.us-east-2.eks.amazonaws.com"
	testCA       = "Q0VSVElGSUNBVEU=" // base64("CERTIFICATE")
	testARN      = "arn:aws:eks:us-east-2:123456789012:cluster/dp-trial"
)

// installDir writes install settings and returns the tfvars path.
func installDir(t *testing.T, tfvars string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "terraform", "aws")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "terraform.tfvars")
	if err := os.WriteFile(p, []byte(tfvars), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// testInstall creates a workspace in a private DARKPLUM_HOME for the install the
// tests below use (dp-trial, profile acme, account 123456789012). mut edits the
// record before it is saved.
func testInstall(t *testing.T, mut func(*install.Record)) install.Install {
	t.Helper()
	t.Setenv("DARKPLUM_HOME", t.TempDir())
	home, err := install.DefaultHome()
	if err != nil {
		t.Fatal(err)
	}
	rec := install.Record{Name: "dp-trial", ClusterName: "dp-trial", Region: "us-east-2", Profile: "acme", AccountIDs: []string{"123456789012"},
		Backend: install.Backend{Type: "s3", Bucket: "darkplum-tfstate-123456789012-us-east-2-a1b2c3", Key: "darkplum/aws/dp-trial/terraform.tfstate", Region: "us-east-2", Encrypt: true, UseLockfile: true}}
	if mut != nil {
		mut(&rec)
	}
	in, err := install.Open(home, "dp-trial")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Save(rec); err != nil {
		t.Fatal(err)
	}
	return in
}

const pinnedTFVars = "aws_profile = \"acme\"\naws_region = \"us-east-2\"\ncluster_name = \"dp-trial\"\nallowed_account_ids = [\"123456789012\"]\n"

// fakeAWS answers describe-cluster and get-caller-identity, recording argv.
type fakeAWS struct {
	arn, caller string
	status      string
	noEndpoint  bool
	describeErr error
	calls       [][]string
}

func (f *fakeAWS) run(_ string, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "eks describe-cluster"):
		if f.describeErr != nil {
			return "", f.describeErr
		}
		ep, ca := testEndpoint, testCA
		if f.noEndpoint {
			ep, ca = "", ""
		}
		b, _ := json.Marshal(map[string]string{"endpoint": ep, "ca": ca, "arn": f.arn, "status": f.status})
		return string(b), nil
	case strings.Contains(joined, "sts get-caller-identity"):
		return f.caller + "\n", nil
	}
	return "", errors.New("unexpected aws " + joined)
}

func TestInstallClusterWritesPrivateKubeconfig(t *testing.T) {
	// The operator's own kubeconfigs, including a context named "darkplum"
	// that points elsewhere: never read, never written.
	home := t.TempDir()
	t.Setenv("HOME", home)
	userCfg := []byte("apiVersion: v1\nkind: Config\ncurrent-context: darkplum\nclusters:\n- name: darkplum\n  cluster:\n    server: https://prod.example.com\n")
	if err := os.MkdirAll(filepath.Join(home, ".kube"), 0o700); err != nil {
		t.Fatal(err)
	}
	homeCfg := filepath.Join(home, ".kube", "config")
	envCfg := filepath.Join(home, "other-kubeconfig")
	for _, p := range []string{homeCfg, envCfg} {
		if err := os.WriteFile(p, userCfg, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KUBECONFIG", envCfg)

	in := testInstall(t, nil)
	aws := &fakeAWS{arn: testARN, caller: "123456789012"}
	tg, row, err := installCluster(in, aws.run)
	if err != nil {
		t.Fatalf("%v %+v", err, row)
	}
	want := in.Kubeconfig()
	if tg.Kubeconfig != want || tg.Context != "darkplum" || tg.Account != "123456789012" {
		t.Errorf("target = %+v", tg)
	}
	for _, c := range aws.calls {
		j := strings.Join(c, " ")
		if !strings.HasPrefix(j, "--profile acme ") || !strings.Contains(j, "--region us-east-2") {
			t.Errorf("aws call without the install's profile and region: %s", j)
		}
	}

	st, _ := os.Stat(want)
	if st == nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig mode = %v, want 0600", st)
	}
	if dst, _ := os.Stat(filepath.Dir(want)); dst.Mode().Perm() != 0o700 {
		t.Errorf("kubeconfig dir mode = %v, want 0700", dst.Mode().Perm())
	}
	raw, _ := os.ReadFile(want)
	if strings.Contains(string(raw), "insecure-skip-tls-verify") || strings.Contains(string(raw), "token") && !strings.Contains(string(raw), "get-token") {
		t.Errorf("kubeconfig must hold no token and never skip TLS:\n%s", raw)
	}
	var cfg struct {
		Current  string `json:"current-context"`
		Clusters []struct {
			Name    string `json:"name"`
			Cluster struct {
				Server string `json:"server"`
				CA     string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User struct {
				Exec struct {
					Command, InteractiveMode string
					Args                     []string
					Env                      []map[string]string
				} `json:"exec"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Current != "darkplum" || len(cfg.Clusters) != 1 || cfg.Clusters[0].Cluster.Server != testEndpoint || cfg.Clusters[0].Cluster.CA != testCA {
		t.Errorf("cluster stanza = %+v", cfg)
	}
	ex := cfg.Users[0].User.Exec
	wantArgs := []string{"--profile", "acme", "--region", "us-east-2", "eks", "get-token", "--cluster-name", "dp-trial", "--output", "json"}
	if ex.Command != "aws" || ex.InteractiveMode != "Never" || !reflect.DeepEqual(ex.Args, wantArgs) ||
		len(ex.Env) != 1 || ex.Env[0]["name"] != "AWS_PROFILE" || ex.Env[0]["value"] != "acme" {
		t.Errorf("exec stanza = %+v", ex)
	}

	// A real kubectl invocation (a fake binary that reports what it got)
	// selects the private file by flag and by KUBECONFIG.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\necho \"$KUBECONFIG|$AWS_PROFILE|$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := tg.kube("darkplum-system").NS(nil, "get", "pods")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != want+"|acme|--kubeconfig "+want+" --context darkplum -n darkplum-system get pods" {
		t.Errorf("kubectl saw %q", got)
	}

	for _, p := range []string{homeCfg, envCfg} {
		if b, _ := os.ReadFile(p); !bytes.Equal(b, userCfg) {
			t.Errorf("%s changed", p)
		}
	}
}

func TestInstallClusterRefusals(t *testing.T) {
	cases := []struct {
		name     string
		mut      func(*install.Record)
		aws      *fakeAWS
		detail   string
		notFound bool
	}{
		{"cluster in an account the install does not pin", nil,
			&fakeAWS{arn: "arn:aws:eks:us-east-2:999999999999:cluster/dp-trial", caller: "999999999999"}, "not the install's account", false},
		{"credentials for another account", nil,
			&fakeAWS{arn: testARN, caller: "999999999999"}, "credentials are for account 999999999999", false},
		{"cluster missing", nil,
			&fakeAWS{caller: "123456789012", describeErr: errors.New("exit status 254: An error occurred (ResourceNotFoundException) when calling the DescribeCluster operation")},
			"does not exist in us-east-2", true},
		// wrong-account credentials must not learn whether the cluster exists
		{"cluster missing but credentials for another account", nil,
			&fakeAWS{caller: "999999999999", describeErr: errors.New("ResourceNotFoundException")},
			"credentials are for account 999999999999", false},
		{"ARN for another cluster", nil,
			&fakeAWS{arn: "arn:aws:eks:us-east-2:123456789012:cluster/other", caller: "123456789012"}, "does not match dp-trial", false},
		{"expired session", nil,
			&fakeAWS{caller: "123456789012", describeErr: errors.New("ExpiredToken")}, "cannot read cluster", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := testInstall(t, tc.mut)
			stale := in.Kubeconfig()
			_ = os.MkdirAll(filepath.Dir(stale), 0o700)
			_ = os.WriteFile(stale, []byte("stale"), 0o600)
			_, row, err := installCluster(in, tc.aws.run)
			if err == nil || row.status != checkFail || !strings.Contains(row.detail, tc.detail) {
				t.Fatalf("want refusal %q, got %v %+v", tc.detail, err, row)
			}
			if errors.Is(err, errClusterNotFound) != tc.notFound {
				t.Errorf("errClusterNotFound = %v, want %v", errors.Is(err, errClusterNotFound), tc.notFound)
			}
			if _, serr := os.Stat(stale); !os.IsNotExist(serr) {
				t.Error("a refusal must remove the previous kubeconfig")
			}
		})
	}
	t.Run("a record that does not validate", func(t *testing.T) {
		in := testInstall(t, nil)
		if err := os.WriteFile(in.RecordPath(), []byte(`{"schema":1,"name":"dp-trial","cluster_name":"dp-trial","region":"us-east-2","account_ids":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, row, err := installCluster(in, (&fakeAWS{arn: testARN, caller: "123456789012"}).run)
		if err == nil || row.status != checkFail || !strings.Contains(row.detail, "install record unreadable") {
			t.Fatalf("got %v %+v", err, row)
		}
	})
	t.Run("no install", func(t *testing.T) {
		t.Setenv("DARKPLUM_HOME", t.TempDir())
		home, _ := install.DefaultHome()
		ghost, _ := install.Open(home, "ghost")
		t.Chdir(t.TempDir())
		_, row, err := installCluster(ghost, (&fakeAWS{}).run)
		if !errors.Is(err, errNoSettings) || strings.Contains(row.detail+row.hint, "tfvars") || !strings.Contains(row.hint, "darkplum init") {
			t.Fatalf("want errNoSettings pointing at init, without file names, got %v %+v", err, row)
		}
	})
	t.Run("a Makefile-era root here points at adopt", func(t *testing.T) {
		t.Setenv("DARKPLUM_HOME", t.TempDir())
		home, _ := install.DefaultHome()
		ghost, _ := install.Open(home, "ghost")
		cwd := t.TempDir()
		if err := os.MkdirAll(filepath.Join(cwd, "terraform", "aws"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cwd, "terraform", "aws", "backend.tf"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(cwd)
		_, row, err := installCluster(ghost, (&fakeAWS{}).run)
		if !errors.Is(err, errNoSettings) || !strings.Contains(row.hint, "darkplum init --adopt") {
			t.Fatalf("got %v %+v", err, row)
		}
	})
}

func TestKubeconfigCommandPrintsExport(t *testing.T) {
	in := testInstall(t, nil)
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  *describe-cluster*) echo '{"endpoint":"` + testEndpoint + `","ca":"` + testCA + `","arn":"` + testARN + `"}' ;;
  *get-caller-identity*) echo 123456789012 ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	capture := func(c *KubeconfigCmd) (string, error) {
		r, w, _ := os.Pipe()
		old := os.Stdout
		os.Stdout = w
		err := c.Run(&Globals{})
		w.Close()
		os.Stdout = old
		b, _ := io.ReadAll(r)
		return string(b), err
	}
	out, err := capture(&KubeconfigCmd{})
	want := "export KUBECONFIG='" + in.Kubeconfig() + "'\n"
	if err != nil || out != want {
		t.Errorf("stdout = %q, %v; want %q", out, err, want)
	}
	if out, err := capture(&KubeconfigCmd{Write: true}); err != nil || out != "" {
		t.Errorf("--write must be silent: %q %v", out, err)
	}
}

// The target carries a validated handle only on success; kubectl is reachable
// through nothing else.
func TestInstallClusterHandsOutAValidatedHandleOnlyOnSuccess(t *testing.T) {
	in := testInstall(t, nil)
	tg, _, err := installCluster(in, (&fakeAWS{arn: testARN, caller: "123456789012"}).run)
	if err != nil {
		t.Fatal(err)
	}
	if !tg.h.Valid() || tg.h.Kubeconfig() != tg.Kubeconfig || tg.h.Account() != "123456789012" {
		t.Errorf("handle = %+v for target %+v", tg.h, tg)
	}
	// a kubeconfig someone edited after it was written is never used
	if err := os.WriteFile(tg.Kubeconfig, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.kube("darkplum-system").NS(nil, "get", "pods"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("kubectl ran against an edited kubeconfig: %v", err)
	}

	// credentials for another account: no target, no handle, no private kubeconfig
	bad, _, err := installCluster(in, (&fakeAWS{arn: testARN, caller: "999999999999"}).run)
	if err == nil || bad.h.Valid() {
		t.Fatalf("credentials for another account produced a handle: %+v %v", bad, err)
	}
	if _, err := bad.kube("ns").NS(nil, "get", "pods"); err == nil {
		t.Error("a refused target ran kubectl")
	}
	if _, serr := os.Stat(tg.Kubeconfig); serr == nil {
		t.Error("a refused validation left the private kubeconfig behind")
	}
}

// --context is the escape hatch for local kind clusters only; an install's
// cluster, or any cluster that is not on this machine, is never reached with it.
func TestContextEscapeIsRestrictedToLocalKind(t *testing.T) {
	for _, ctx := range []string{"prod", "darkplum", "arn:aws:eks:us-east-2:123456789012:cluster/dp-trial", "kind-prod-but-remote"} {
		t.Run(ctx, func(t *testing.T) {
			old := newLocalKind
			defer func() { newLocalKind = old }()
			if ctx == "kind-prod-but-remote" { // named like kind, server is not local
				newLocalKind = func(c string) (runner.LocalKind, error) {
					return runner.LocalKind{}, errors.New("kind: its API server is not on this machine")
				}
			}
			var buf bytes.Buffer
			k, err := installKube(&buf, "darkplum-system", ctx, "")
			if err == nil || k != nil {
				t.Fatalf("--context %q was accepted: %v", ctx, err)
			}
			if !strings.Contains(buf.String(), "kind") {
				t.Errorf("refusal does not explain the kind restriction: %q", buf.String())
			}
		})
	}
}
