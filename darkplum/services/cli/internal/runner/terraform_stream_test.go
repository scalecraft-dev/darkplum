package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	err := TerraformStream(context.Background(), v, dirs, nil, &buf, "apply")
	if err == nil {
		t.Fatal("the exit status must reach the caller")
	}
	if !strings.Contains(buf.String(), "out-line") || !strings.Contains(buf.String(), "err-line") {
		t.Errorf("output = %q", buf.String())
	}
	if err := TerraformStream(context.Background(), bundle.Verified{}, dirs, nil, &buf, "apply"); err == nil {
		t.Error("streamed without a verified bundle")
	}
}

func TestTerraformStreamInterruptsOnCancel(t *testing.T) {
	// The fake exits 130 only when it receives SIGINT: a kill would not leave the marker.
	v, dirs := streamFixture(t, "#!/bin/sh\ntrap 'echo got-interrupt; exit 130' INT\necho started\nwhile true; do sleep 0.1; done\n")
	var buf lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- TerraformStream(ctx, v, dirs, nil, &buf, "apply") }()
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
