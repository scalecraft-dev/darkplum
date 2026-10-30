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

const pinnedTFVars = "aws_profile = \"acme\"\naws_region = \"us-east-2\"\ncluster_name = \"dp-trial\"\nallowed_account_ids = [\"123456789012\"]\n"

// fakeAWS answers describe-cluster and get-caller-identity, recording argv.
type fakeAWS struct {
	arn, caller string
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
		b, _ := json.Marshal(map[string]string{"endpoint": testEndpoint, "ca": testCA, "arn": f.arn})
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

	tfvars := installDir(t, pinnedTFVars)
	aws := &fakeAWS{arn: testARN, caller: "123456789012"}
	tg, row, err := installCluster(tfvars, aws.run)
	if err != nil {
		t.Fatalf("%v %+v", err, row)
	}
	want := filepath.Join(filepath.Dir(tfvars), ".darkplum", "kubeconfig")
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
		name, tfvars string
		aws          *fakeAWS
		detail       string
		notFound     bool
	}{
		{"cluster in an account the install does not pin", pinnedTFVars,
			&fakeAWS{arn: "arn:aws:eks:us-east-2:999999999999:cluster/dp-trial", caller: "999999999999"}, "not the install's account", false},
		{"credentials for another account", pinnedTFVars,
			&fakeAWS{arn: testARN, caller: "999999999999"}, "credentials are for account 999999999999", false},
		{"cluster missing", pinnedTFVars,
			&fakeAWS{describeErr: errors.New("exit status 254: An error occurred (ResourceNotFoundException) when calling the DescribeCluster operation")},
			"does not exist in us-east-2", true},
		{"ARN for another cluster", pinnedTFVars,
			&fakeAWS{arn: "arn:aws:eks:us-east-2:123456789012:cluster/other", caller: "123456789012"}, "does not match dp-trial", false},
		{"expired session", pinnedTFVars,
			&fakeAWS{describeErr: errors.New("ExpiredToken")}, "cannot read cluster", false},
		{"no pinned account", "aws_region = \"us-east-2\"\n",
			&fakeAWS{arn: testARN, caller: "123456789012"}, "pin no AWS account", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tfvars := installDir(t, tc.tfvars)
			stale := filepath.Join(filepath.Dir(tfvars), ".darkplum", "kubeconfig")
			_ = os.MkdirAll(filepath.Dir(stale), 0o700)
			_ = os.WriteFile(stale, []byte("stale"), 0o600)
			_, row, err := installCluster(tfvars, tc.aws.run)
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
	t.Run("no install settings", func(t *testing.T) {
		_, row, err := installCluster(filepath.Join(t.TempDir(), "terraform.tfvars"), (&fakeAWS{}).run)
		if !errors.Is(err, errNoSettings) || strings.Contains(row.detail+row.hint, "tfvars") {
			t.Fatalf("want errNoSettings without file names, got %v %+v", err, row)
		}
	})
}

func TestKubeconfigCommandPrintsExport(t *testing.T) {
	tfvars := installDir(t, pinnedTFVars)
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
	out, err := capture(&KubeconfigCmd{Tfvars: tfvars})
	want := "export KUBECONFIG='" + filepath.Join(filepath.Dir(tfvars), ".darkplum", "kubeconfig") + "'\n"
	if err != nil || out != want {
		t.Errorf("stdout = %q, %v; want %q", out, err, want)
	}
	if out, err := capture(&KubeconfigCmd{Tfvars: tfvars, Write: true}); err != nil || out != "" {
		t.Errorf("--write must be silent: %q %v", out, err)
	}
}
