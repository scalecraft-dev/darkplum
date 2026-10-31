package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
)

const lockedBucket = "darkplum-tfstate-" + testAccount + "-us-east-2-a1b2c3"

// liveInstall is a workspace with a record, as a deployed install has.
func liveInstall(t *testing.T) (dir string, in install.Install, rec install.Record) {
	t.Helper()
	dir = t.TempDir()
	in, err := install.OpenDir(dir, "darkplum")
	if err != nil {
		t.Fatal(err)
	}
	rec = install.Record{Name: "darkplum", ClusterName: "darkplum", Region: "us-east-2", AccountIDs: []string{testAccount},
		Created: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Bundle: "v1-abc",
		Backend: install.Backend{Type: "s3", Bucket: lockedBucket, Key: "darkplum/terraform.tfstate", Region: "us-east-2", Encrypt: true, UseLockfile: true}}
	if err := in.Save(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "terraform.tfvars"), []byte(bedrockInstallTF), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, in, rec
}

func sameRecord(t *testing.T, in install.Install, want install.Record) {
	t.Helper()
	got, err := in.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.ClusterName != want.ClusterName || got.Region != want.Region || strings.Join(got.AccountIDs, ",") != strings.Join(want.AccountIDs, ",") || got.Backend.Bucket != want.Backend.Bucket || got.Backend.Key != want.Backend.Key || got.Backend.Region != want.Backend.Region {
		t.Errorf("the install's identity changed:\n got %+v\nwant %+v", got, want)
	}
}

func TestLockedFields(t *testing.T) {
	old := install.Record{ClusterName: "c", Region: "us-east-2", AccountIDs: []string{"111111111111"},
		Backend: install.Backend{Bucket: "b", Key: "k", Region: "us-east-2"}}
	ok := &settings{cluster: "c", region: "us-east-2", account: "111111111111"}
	spec := backendSpec{Bucket: "b", Key: "k", Region: "us-east-2"}
	if err := lockedFields(old, ok, spec); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*settings, *backendSpec){
		"cluster":        func(s *settings, _ *backendSpec) { s.cluster = "d" },
		"region":         func(s *settings, _ *backendSpec) { s.region = "eu-west-1" },
		"account":        func(s *settings, _ *backendSpec) { s.account = "222222222222" },
		"bucket":         func(_ *settings, b *backendSpec) { b.Bucket = "other" },
		"key":            func(_ *settings, b *backendSpec) { b.Key = "other" },
		"backend region": func(_ *settings, b *backendSpec) { b.Region = "eu-west-1" },
	} {
		s, sp := *ok, spec
		mut(&s, &sp)
		if lockedFields(old, &s, sp) == nil {
			t.Errorf("a change of the %s was accepted", name)
		}
	}
}

// Path 1: Start over, interactively, asking for another region.
func TestExistingInstallRegionCannotChangeOnStartOver(t *testing.T) {
	dir, in, rec := liveInstall(t)
	c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1, Region: "eu-west-1", Mode: "bedrock", Domain: "acme.com"}
	cloud := newFakeCloud()
	env, out := testEnv(t, cloud, nil, "2\n\n\n\n")
	err := c.execute(env)
	if err == nil || !IsReported(err) || !strings.Contains(out.String(), "cannot move") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	sameRecord(t, in, rec)
	if cloud.createCalls != 0 || readFile(t, filepath.Join(dir, "terraform.tfvars")) != bedrockInstallTF {
		t.Error("something was created or written")
	}
}

// Path 2: --yes --force never bypasses it.
func TestExistingInstallForceDoesNotBypassTheLock(t *testing.T) {
	t.Run("another region", func(t *testing.T) {
		dir, in, rec := liveInstall(t)
		cloud := newFakeCloud()
		env, _ := testEnv(t, cloud, nil, "-")
		err := yesCmd(dir, func(c *InitCmd) { c.Force, c.Region = true, "eu-west-1" }).execute(env)
		if err == nil || !IsReported(err) {
			t.Fatalf("err = %v", err)
		}
		sameRecord(t, in, rec)
		if cloud.createCalls != 0 {
			t.Error("a state backend was created")
		}
	})
	t.Run("another account", func(t *testing.T) {
		dir, in, rec := liveInstall(t)
		cloud := newFakeCloud()
		cloud.id.Account = "999999999999"
		env, out := testEnv(t, cloud, nil, "-")
		err := yesCmd(dir, func(c *InitCmd) { c.Force = true }).execute(env)
		if err == nil || !strings.Contains(out.String(), "cannot move to another account") {
			t.Fatalf("err = %v\n%s", err, out)
		}
		sameRecord(t, in, rec)
		if cloud.createCalls != 0 {
			t.Error("a state backend was created")
		}
	})
	t.Run("the install's region is used when none is given", func(t *testing.T) {
		dir, in, rec := liveInstall(t)
		cloud := newFakeCloud()
		cloud.region = "eu-west-1" // the profile's default region must not win
		env, out := testEnv(t, cloud, nil, "-")
		if err := yesCmd(dir, func(c *InitCmd) { c.Force = true }).execute(env); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		sameRecord(t, in, rec)
		if got, _ := in.Load(); got.Bundle != "v1-abc" || !got.Created.Equal(rec.Created) {
			t.Errorf("history lost: %+v", got)
		}
		if !strings.Contains(readFile(t, filepath.Join(dir, "terraform.tfvars")), `aws_region = "us-east-2"`) {
			t.Error("settings moved to another region")
		}
	})
}

// Path 3: the record writer itself.
func TestSaveRecordRefusesToChangeAnInstall(t *testing.T) {
	dir, in, rec := liveInstall(t)
	c := &InitCmd{}
	good := backendSpec{Bucket: rec.Backend.Bucket, Key: rec.Backend.Key, Region: rec.Backend.Region, KMSKeyARN: "arn:aws:kms:us-east-2:" + testAccount + ":key/x"}
	s := &settings{cluster: "darkplum", region: "us-east-2", account: testAccount}
	if err := c.saveRecord(dir, s, good, time.Now()); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(*settings, *backendSpec){
		"region":  func(s *settings, _ *backendSpec) { s.region = "eu-west-1" },
		"account": func(s *settings, _ *backendSpec) { s.account = "999999999999" },
		"bucket":  func(_ *settings, b *backendSpec) { b.Bucket = "darkplum-tfstate-" + testAccount + "-us-east-2-zzzzzz" },
	} {
		s2, b2 := *s, good
		f(&s2, &b2)
		if err := c.saveRecord(dir, &s2, b2, time.Now()); err == nil {
			t.Errorf("%s change saved", name)
		}
	}
	sameRecord(t, in, rec)
}

// planState's collision rename must never fire on the install's own resources.
func TestReRunNeverTreatsTheInstallsOwnResourcesAsACollision(t *testing.T) {
	dir, in, rec := liveInstall(t)
	cloud := newFakeCloud()
	// Everything that makes a fresh install rename itself exists, because it is ours.
	cloud.collisions = []checkResult{{name: "eks cluster", status: checkFail, detail: "darkplum exists"}}
	cloud.objects = map[string]int{lockedBucket + "/" + rec.Backend.Key: 40}
	cloud.buckets = []stateBucket{{Name: lockedBucket}}
	// Start over, interactively. Any rename prompt would eat the next answers.
	c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1, Mode: "bedrock", Domain: "acme.com", Force: false}
	env, out := testEnv(t, cloud, nil, "2\n\n\n\n\n")
	err := c.execute(env)
	if strings.Contains(out.String(), "Cluster name") || strings.Contains(out.String(), "already keeps state here") {
		t.Fatalf("a rename was offered on the install's own resources:\n%s", out)
	}
	if err != nil && !errors.Is(err, errAborted) {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, r := range cloud.reads {
		if r == lockedBucket+"/"+rec.Backend.Key {
			t.Error("the install's own state was read as if it were another install's")
		}
	}
	sameRecord(t, in, rec)
}

// An adopted install can record several accounts. Start over cannot tell it is
// the same install then, and the refusal says that, not "account X, not Y".
func TestLockedFieldsMultiAccountSaysWhy(t *testing.T) {
	old := install.Record{ClusterName: "c", Region: "us-east-2", AccountIDs: []string{"111111111111", "222222222222"},
		Backend: install.Backend{Bucket: "b", Key: "k", Region: "us-east-2"}}
	s := &settings{cluster: "c", region: "us-east-2", account: "111111111111"}
	err := lockedFields(old, s, backendSpec{Bucket: "b", Key: "k", Region: "us-east-2"})
	if err == nil || !strings.Contains(err.Error(), "records 2 AWS accounts (111111111111, 222222222222)") || !strings.Contains(err.Error(), "Start over answers for exactly one") {
		t.Errorf("err = %v", err)
	}
}
