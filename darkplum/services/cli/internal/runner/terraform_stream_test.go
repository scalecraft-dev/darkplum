package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/bundle"
)

func streamFixture(t *testing.T, script string) (bundle.Verified, TerraformDirs) {
	t.Helper()
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
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return v, TerraformDirs{Workspace: ws, Data: filepath.Join(ws, "tfdata", "k")}
}

func TestTerraformStreamWritesBothStreamsAsTheyArrive(t *testing.T) {
	v, dirs := streamFixture(t, "#!/bin/sh\necho out-line\necho err-line >&2\nexit 3\n")
	var buf bytes.Buffer
	err := TerraformStream(context.Background(), v, dirs, nil, &buf, StreamOptions{}, "apply")
	if err == nil {
		t.Fatal("the exit status must reach the caller")
	}
	if !strings.Contains(buf.String(), "out-line") || !strings.Contains(buf.String(), "err-line") {
		t.Errorf("output = %q", buf.String())
	}
	if err := TerraformStream(context.Background(), bundle.Verified{}, dirs, nil, &buf, StreamOptions{}, "apply"); err == nil {
		t.Error("streamed without a verified bundle")
	}
}

func TestTerraformStreamInterruptsOnCancel(t *testing.T) {
	// The fake exits 130 only when it receives SIGINT: a kill would not leave the marker.
	v, dirs := streamFixture(t, "#!/bin/sh\ntrap 'echo got-interrupt; exit 130' INT\necho started\nwhile true; do sleep 0.1; done\n")
	var buf lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- TerraformStream(ctx, v, dirs, nil, &buf, StreamOptions{}, "apply") }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "started") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled run reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("terraform was not stopped")
	}
	if !strings.Contains(buf.String(), "got-interrupt") {
		t.Errorf("terraform was killed, not interrupted: %q", buf.String())
	}
}

// lockedBuf is a buffer that can be read while the command is still writing.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Ctrl-C must reach Terraform exactly once: a second SIGINT makes it quit without
// releasing the state lock. The fake counts the SIGINTs it receives, and reports
// its process group against the CLI's.
func TestTerraformStreamDeliversExactlyOneInterruptInItsOwnProcessGroup(t *testing.T) {
	marks := filepath.Join(t.TempDir(), "marks")
	script := "#!/bin/sh\ntrap 'echo INT >> " + marks + "; sleep 1; echo exiting >> " + marks + "; exit 130' INT\n" +
		"echo \"pgid $(ps -o pgid= -p $$ | tr -d ' ')\" >> " + marks + "\necho started\nwhile true; do sleep 0.1; done\n"
	v, dirs := streamFixture(t, script)
	var buf lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- TerraformStream(ctx, v, dirs, nil, &buf, StreamOptions{}, "apply") }()
	waitFor(t, "start", func() bool { return strings.Contains(buf.String(), "started") })
	cancel()
	// Pretend the terminal's Ctrl-C also reached us again: a real second interrupt
	// is the caller's Force, not another signal from the runner.
	if err := <-done; err == nil {
		t.Error("an interrupted run reported success")
	}
	b, _ := os.ReadFile(marks)
	if n := strings.Count(string(b), "INT\n"); n != 1 {
		t.Errorf("terraform received %d SIGINTs, want exactly 1:\n%s", n, b)
	}
	if !strings.Contains(string(b), "exiting") {
		t.Errorf("terraform did not get to wind down:\n%s", b)
	}
	own := strconv.Itoa(syscall.Getpgrp())
	if strings.Contains(string(b), "pgid "+own+"\n") {
		t.Errorf("terraform shares the CLI's process group (%s), so the terminal's Ctrl-C would reach it too:\n%s", own, b)
	}
}

// An interrupted run is not killed on a timer: a create that Terraform cannot
// abandon (EKS, RDS) takes minutes, and killing it leaves the state out of step.
func TestTerraformStreamIsNotKilledWhileItWindsDown(t *testing.T) {
	script := "#!/bin/sh\ntrap 'echo winding-down; sleep 3; echo finished-cleanly; exit 130' INT\necho started\nwhile true; do sleep 0.1; done\n"
	v, dirs := streamFixture(t, script)
	var buf lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	var waits int
	var mu sync.Mutex
	done := make(chan error, 1)
	opts := StreamOptions{WaitingEvery: 500 * time.Millisecond, Waiting: func(time.Duration) { mu.Lock(); waits++; mu.Unlock() }}
	go func() { done <- TerraformStream(ctx, v, dirs, nil, &buf, opts, "apply") }()
	waitFor(t, "start", func() bool { return strings.Contains(buf.String(), "started") })
	cancel()
	<-done
	if !strings.Contains(buf.String(), "finished-cleanly") {
		t.Errorf("terraform was killed while winding down:\n%s", buf.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if waits < 2 {
		t.Errorf("the caller was told %d times that terraform is still winding down, want several", waits)
	}
}

func TestTerraformStreamForceKillsOnlyOnTheSecondRequest(t *testing.T) {
	script := "#!/bin/sh\ntrap 'echo ignoring' INT\necho started\nwhile true; do sleep 0.1; done\n"
	v, dirs := streamFixture(t, script)
	var buf lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	force := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- TerraformStream(ctx, v, dirs, nil, &buf, StreamOptions{Force: force}, "apply") }()
	waitFor(t, "start", func() bool { return strings.Contains(buf.String(), "started") })
	cancel()
	select {
	case <-done:
		t.Fatal("stopped on the first request although terraform ignored it")
	case <-time.After(1500 * time.Millisecond):
	}
	close(force)
	select {
	case err := <-done:
		if err == nil {
			t.Error("a killed run reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Force did not stop it")
	}
}

func TestTerraformStreamGraceIsABackstop(t *testing.T) {
	v, dirs := streamFixture(t, "#!/bin/sh\ntrap '' INT\necho started\nwhile true; do sleep 0.1; done\n")
	var buf lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- TerraformStream(ctx, v, dirs, nil, &buf, StreamOptions{Grace: 700 * time.Millisecond}, "apply")
	}()
	waitFor(t, "start", func() bool { return strings.Contains(buf.String(), "started") })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a run that never stops was not killed after the grace period")
	}
}
