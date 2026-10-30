package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCertCovers(t *testing.T) {
	cases := []struct {
		names []string
		host  string
		want  bool
	}{
		{[]string{"darkplum.acme.com"}, "darkplum.acme.com", true},
		{[]string{"DarkPlum.Acme.com."}, "darkplum.acme.com", true},
		{[]string{"*.acme.com"}, "darkplum.acme.com", true},
		{[]string{"*.ACME.com."}, "Darkplum.acme.com.", true},
		{[]string{"*.acme.com"}, "acme.com", false},            // no apex
		{[]string{"*.acme.com"}, "a.darkplum.acme.com", false}, // one label only
		{[]string{"*.darkplum.acme.com"}, "darkplum.acme.com", false},
		{[]string{"acme.com", "*.eng.acme.com"}, "darkplum.acme.com", false},
		{[]string{"acme.com", "*.acme.com"}, "darkplum.acme.com", true},
	}
	for _, c := range cases {
		if got := certCovers(c.names, c.host); got != c.want {
			t.Errorf("certCovers(%v, %q) = %v, want %v", c.names, c.host, got, c.want)
		}
	}
}

func TestDelegated(t *testing.T) {
	want := []string{"ns-1.awsdns-01.org", "ns-2.awsdns-02.co.uk"}
	if !delegated([]string{"NS-2.awsdns-02.co.uk.", "ns-1.awsdns-01.org."}, want) {
		t.Error("case and trailing dots must not matter")
	}
	if delegated([]string{"ns-1.awsdns-01.org"}, want) || delegated(nil, want) {
		t.Error("every name server must be visible")
	}
}

func readTFVarsFile(t *testing.T, dir string) string {
	t.Helper()
	return readFile(t, filepath.Join(dir, "terraform.tfvars"))
}

func TestInitWebAddressPaths(t *testing.T) {
	t.Run("parent zone in this account: Terraform creates and delegates the zone", func(t *testing.T) {
		dir := t.TempDir()
		cloud := newFakeCloud()
		env, out := testEnv(t, cloud, nil, "-")
		if err := yesCmd(dir, func(c *InitCmd) { c.AllowedCIDRs = "10.0.0.0/16, 10.8.0.0/16" }).execute(env); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		tf := readTFVarsFile(t, dir)
		for _, want := range []string{`web_domain = "acme.com"`, `web_allowed_cidrs = ["10.0.0.0/16", "10.8.0.0/16"]`} {
			if !strings.Contains(tf, want) {
				t.Errorf("tfvars lacks %s:\n%s", want, tf)
			}
		}
		if strings.Contains(tf, "web_zone_id") || strings.Contains(tf, "web_hostname") || cloud.zoneCreates != 0 {
			t.Errorf("same-account parent: no zone id, no custom hostname, nothing created by init:\n%s", tf)
		}
		if !strings.Contains(out.String(), "Your team's address after deploy: https://darkplum.acme.com") {
			t.Errorf("summary lacks the address:\n%s", out)
		}
	})

	t.Run("DNS elsewhere: init creates the zone, shows NS records, waits, then writes", func(t *testing.T) {
		dir := t.TempDir()
		cloud := newFakeCloud()
		cloud.hzones = map[string]hostedZone{}
		env, out := testEnv(t, cloud, nil, "-")
		calls := 0
		env.lookupNS = func(string) ([]string, error) {
			calls++
			if calls < 3 {
				return nil, os.ErrNotExist
			}
			return cloud.hzones["darkplum.acme.com"].NameServers, nil
		}
		if err := yesCmd(dir, nil).execute(env); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !strings.Contains(readTFVarsFile(t, dir), `web_zone_id = "ZNEWZONE123"`) {
			t.Errorf("tfvars lacks the created zone:\n%s", readTFVarsFile(t, dir))
		}
		for _, want := range []string{"darkplum.acme.com  NS  ns-1.awsdns-01.org", "✓ delegation"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("delegation never appears: nothing written, re-run resumes the same zone", func(t *testing.T) {
		dir := t.TempDir()
		cloud := newFakeCloud()
		cloud.hzones = map[string]hostedZone{}
		clock := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
		env, out := testEnv(t, cloud, nil, "-")
		env.now = func() time.Time { return clock }
		env.sleep = func(d time.Duration) { clock = clock.Add(d) }
		err := yesCmd(dir, nil).execute(env)
		if err == nil || !IsReported(err) || !strings.Contains(out.String(), "re-run ./bin/darkplum init: it continues from here") {
			t.Fatalf("want a reported timeout, got %v\n%s", err, out)
		}
		if _, serr := os.Stat(filepath.Join(dir, "terraform.tfvars")); serr == nil {
			t.Error("tfvars written before the delegation resolved")
		}
		env2, out2 := testEnv(t, cloud, nil, "-")
		env2.lookupNS = func(string) ([]string, error) { return cloud.hzones["darkplum.acme.com"].NameServers, nil }
		if err := yesCmd(dir, nil).execute(env2); err != nil {
			t.Fatalf("%v\n%s", err, out2)
		}
		if len(cloud.hzones) != 1 || !strings.Contains(readTFVarsFile(t, dir), `web_zone_id = "ZNEWZONE123"`) {
			t.Errorf("re-run must reuse the zone: %v", cloud.hzones)
		}
	})

	t.Run("hostname already has records in the parent zone", func(t *testing.T) {
		cloud := newFakeCloud()
		cloud.records = map[string][]string{"ZPARENT123 darkplum.acme.com": {"CNAME"}}
		env, out := testEnv(t, cloud, nil, "-")
		err := yesCmd(t.TempDir(), nil).execute(env)
		if err == nil || !strings.Contains(out.String(), "darkplum.acme.com already has CNAME records") {
			t.Fatalf("want refusal, got %v\n%s", err, out)
		}
	})

	t.Run("supplied certificate", func(t *testing.T) {
		arn := "arn:aws:acm:us-east-2:" + testAccount + ":certificate/0b8f2a3c-1d2e-4f5a-8b9c-0d1e2f3a4b5c"
		good := certInfo{Status: "ISSUED", NotAfter: time.Now().Add(200 * 24 * time.Hour), Names: []string{"*.acme.com"}}
		for name, tc := range map[string]struct {
			ci   certInfo
			arn  string
			want string
		}{
			"covers":        {good, arn, ""},
			"wrong name":    {certInfo{Status: "ISSUED", NotAfter: good.NotAfter, Names: []string{"acme.com"}}, arn, "covers acme.com, not darkplum.acme.com"},
			"expiring":      {certInfo{Status: "ISSUED", NotAfter: time.Now().Add(5 * 24 * time.Hour), Names: good.Names}, arn, "less than 30 days"},
			"not issued":    {certInfo{Status: "PENDING_VALIDATION", NotAfter: good.NotAfter, Names: good.Names}, arn, "not ISSUED"},
			"other account": {good, strings.Replace(arn, testAccount, "999999999999", 1), "belongs to account 999999999999"},
		} {
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				cloud := newFakeCloud()
				cloud.cert = tc.ci
				env, out := testEnv(t, cloud, nil, "-")
				env.now = time.Now
				err := yesCmd(dir, func(c *InitCmd) { c.CertificateARN = tc.arn }).execute(env)
				if tc.want == "" {
					if err != nil || !strings.Contains(readTFVarsFile(t, dir), "web_certificate_arn = \""+arn+"\"") {
						t.Fatalf("%v\n%s", err, out)
					}
					return
				}
				if err == nil || !strings.Contains(out.String(), tc.want) {
					t.Fatalf("want %q, got %v\n%s", tc.want, err, out)
				}
			})
		}
	})

	t.Run("--yes needs --domain", func(t *testing.T) {
		env, out := testEnv(t, newFakeCloud(), nil, "-")
		err := yesCmd(t.TempDir(), func(c *InitCmd) { c.Domain = "" }).execute(env)
		if err == nil || !strings.Contains(out.String(), "pass --domain") {
			t.Fatalf("want a --domain refusal, got %v\n%s", err, out)
		}
	})

	t.Run("dry run creates no zone", func(t *testing.T) {
		cloud := newFakeCloud()
		cloud.hzones = map[string]hostedZone{}
		env, out := testEnv(t, cloud, nil, "-")
		if err := yesCmd(t.TempDir(), func(c *InitCmd) { c.DryRun = true }).execute(env); err != nil {
			t.Fatal(err)
		}
		if cloud.zoneCreates != 0 || !strings.Contains(out.String(), "would create the public hosted zone darkplum.acme.com") {
			t.Errorf("zone creates %d\n%s", cloud.zoneCreates, out)
		}
	})
}

func TestInitAddsWebAddressToExistingInstall(t *testing.T) {
	dir := t.TempDir()
	orig := "# hand-kept comment\naws_profile = \"acme\"\nallowed_account_ids = [\"" + testAccount + "\"]\naws_region = \"us-east-2\"\nvpc_cidr = \"10.20.0.0/16\"\ngpu_desired_size = 0\n"
	path := filepath.Join(dir, "terraform.tfvars")
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	cloud := newFakeCloud()
	// Add one (1), domain, networks (default = the install's VPC), confirm.
	env, out := testEnv(t, cloud, nil, "1\nAcme.com.\n\n\n")
	c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1}
	if err := c.execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := readFile(t, path)
	if !strings.HasPrefix(got, orig) {
		t.Errorf("existing lines must be kept verbatim:\n%s", got)
	}
	for _, want := range []string{`web_domain = "acme.com"`, `web_allowed_cidrs = ["10.20.0.0/16"]`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s:\n%s", want, got)
		}
	}
	if readFile(t, path+".bak") != orig {
		t.Error("a .bak copy of the previous settings must be kept")
	}
	for _, notAsked := range []string{"Region", "How should Darkplum run models", "Terraform keep its state"} {
		if strings.Contains(out.String(), notAsked) {
			t.Errorf("the merge path must ask only about the address, asked %q:\n%s", notAsked, out)
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("tfvars mode %v", fi.Mode().Perm())
	}
}

// webLines are the web_* lines of a tfvars file, in order.
func webLines(t *testing.T, tf string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(tf, "\n") {
		if strings.HasPrefix(l, "web_") {
			out = append(out, l)
		}
	}
	return out
}

func TestInitReRunKeepsTheWebAddress(t *testing.T) {
	arn := "arn:aws:acm:us-east-2:" + testAccount + ":certificate/0b8f2a3c-1d2e-4f5a-8b9c-0d1e2f3a4b5c"
	cases := map[string]struct {
		web   string
		setup func(*fakeCloud)
	}{
		// Terraform created and delegated the zone; its records exist now.
		"zone created by Terraform": {
			web: "web_domain = \"acme.com\"\nweb_allowed_cidrs = [\"10.0.0.0/16\", \"10.8.0.0/16\"]\n",
			setup: func(f *fakeCloud) {
				f.hzones["darkplum.acme.com"] = hostedZone{ID: "ZTFZONE1", Name: "darkplum.acme.com"}
				f.records = map[string][]string{"ZPARENT123 darkplum.acme.com": {"NS"}, "ZTFZONE1 darkplum.acme.com": {"A", "NS", "SOA"}}
			},
		},
		"supplied zone and certificate": {
			web: "web_domain = \"acme.com\"\nweb_hostname = \"chat.acme.com\"\nweb_zone_id = \"ZSUPPLIED1\"\nweb_certificate_arn = \"" + arn + "\"\nweb_allowed_cidrs = [\"192.168.4.0/24\"]\n",
			setup: func(f *fakeCloud) {
				f.hzones["acme.com"] = hostedZone{ID: "ZSUPPLIED1", Name: "acme.com"}
				f.records = map[string][]string{"ZSUPPLIED1 chat.acme.com": {"A"}}
				f.cert = certInfo{Status: "ISSUED", NotAfter: time.Now().Add(300 * 24 * time.Hour), Names: []string{"*.acme.com"}}
			},
		},
	}
	for name, tc := range cases {
		for _, mode := range []string{"--yes", "interactive"} {
			t.Run(name+" "+mode, func(t *testing.T) {
				dir := t.TempDir()
				prev := "aws_profile = \"acme\"\nallowed_account_ids = [\"" + testAccount + "\"]\naws_region = \"us-east-2\"\n" + tc.web
				if err := os.WriteFile(filepath.Join(dir, "terraform.tfvars"), []byte(prev), 0o600); err != nil {
					t.Fatal(err)
				}
				cloud := newFakeCloud()
				tc.setup(cloud)
				var err error
				var out string
				if mode == "--yes" {
					env, o := testEnv(t, cloud, nil, "-")
					env.now = time.Now
					err = yesCmd(dir, func(c *InitCmd) { c.Domain = ""; c.Force = true }).execute(env)
					out = o.String()
				} else {
					// Domain (Enter keeps it), networks (Enter keeps them), save.
					env, o := testEnv(t, cloud, nil, "\n\n\n")
					env.now = time.Now
					c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1, Profile: "acme", Region: "us-east-2", Mode: "bedrock", Backend: "local", Force: true}
					err = c.execute(env)
					out = o.String()
				}
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				got := webLines(t, readTFVarsFile(t, dir))
				want := webLines(t, prev)
				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("web_* lines changed on re-run:\n got %q\nwant %q", got, want)
				}
				if !strings.Contains(out, "(unchanged;") || cloud.zoneCreates != 0 {
					t.Errorf("the install's own name must be carried, not re-claimed:\n%s", out)
				}
				if !strings.Contains(out, "certificate transparency") {
					t.Error("the public-name notice must be shown on every path")
				}
			})
		}
	}
}
