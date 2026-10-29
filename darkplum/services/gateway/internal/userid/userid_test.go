package userid

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

const email = "alice@example.com"

func mount(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

var shape = regexp.MustCompile(`^k1:[0-9a-f]{64}$`)

func TestPseudonym_DeterministicPerSecretAndPrefixed(t *testing.T) {
	p := New(mount(t, map[string]string{KeyFile: "secret-one-secret-one\n"}))
	a, ok := p.Pseudonym(email)
	if !ok || !shape.MatchString(a) {
		t.Fatalf("Pseudonym = %q ok=%v, want k1:<64 hex>", a, ok)
	}
	b, _ := p.Pseudonym(email)
	if a != b {
		t.Error("same user and secret gave different values")
	}
	other, _ := p.Pseudonym("bob@example.com")
	if other == a {
		t.Error("different users collided")
	}

	// Independently computed HMAC-SHA256 over the trimmed secret.
	mac := hmac.New(sha256.New, []byte("secret-one-secret-one"))
	mac.Write([]byte(email))
	if want := "k1:" + hex.EncodeToString(mac.Sum(nil)); a != want {
		t.Errorf("Pseudonym = %s, want %s", a, want)
	}
	if strings.Contains(a, "alice") || strings.Contains(a, "example") {
		t.Errorf("pseudonym leaks the input: %s", a)
	}
}

func TestPseudonym_DiffersAcrossSecrets(t *testing.T) {
	a, _ := New(mount(t, map[string]string{KeyFile: "secret-one-secret-one"})).Pseudonym(email)
	b, _ := New(mount(t, map[string]string{KeyFile: "secret-two-secret-two"})).Pseudonym(email)
	if a == "" || a == b {
		t.Errorf("two orgs' secrets produced %q and %q", a, b)
	}
}

func TestPseudonym_KeyIDComesFromTheSecret(t *testing.T) {
	p := New(mount(t, map[string]string{KeyFile: "secret-one-secret-one", KeyIDFile: "k2\n"}))
	got, ok := p.Pseudonym(email)
	if !ok || !strings.HasPrefix(got, "k2:") {
		t.Errorf("Pseudonym = %q, want k2: prefix", got)
	}
}

func TestPseudonym_OmittedNeverRaw(t *testing.T) {
	cases := map[string]*Pseudonymizer{
		"nil":            nil,
		"no dir":         New(""),
		"dir missing":    New(filepath.Join(t.TempDir(), "absent")),
		"key missing":    New(mount(t, nil)),
		"key empty":      New(mount(t, map[string]string{KeyFile: "  \n"})),
		"key too short":  New(mount(t, map[string]string{KeyFile: "short"})),
		"key id invalid": New(mount(t, map[string]string{KeyFile: "secret-one-secret-one", KeyIDFile: "K 1:"})),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := p.Pseudonym(email); ok || got != "" {
				t.Errorf("Pseudonym = %q ok=%v, want omitted", got, ok)
			}
		})
	}
	if got, ok := New(mount(t, map[string]string{KeyFile: "secret-one-secret-one"})).Pseudonym(""); ok || got != "" {
		t.Errorf("empty user: %q ok=%v, want omitted", got, ok)
	}
}

func TestWarning_OncePerOutageAndNeverContainsSecretOrUser(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	p := New(mount(t, map[string]string{KeyFile: "short"}))
	for range 5 {
		p.Pseudonym(email)
	}
	out := buf.String()
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Errorf("warnings = %d, want 1:\n%s", n, out)
	}
	if strings.Contains(out, email) {
		t.Errorf("log line leaks input: %s", out)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// kubelet mounts a Secret as dir/<file> -> dir/..data/<file>, and a rotation
// repoints ..data atomically. Flip it continuously while pseudonymising: every
// value must be a (key, id) pair that really existed, never a new key under
// the old id or the reverse.
func TestPseudonym_RotationNeverMislabelsTheKey(t *testing.T) {
	dir := t.TempDir()
	versions := map[string][2]string{"v1": {"secret-one-secret-one", "k1"}, "v2": {"secret-two-secret-two", "k2"}}
	for v, kv := range versions {
		if err := os.MkdirAll(filepath.Join(dir, v), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, v, KeyFile), []byte(kv[0]), 0o600)
		_ = os.WriteFile(filepath.Join(dir, v, KeyIDFile), []byte(kv[1]), 0o600)
	}
	point := func(v string) {
		tmp := filepath.Join(dir, "..data_tmp")
		_ = os.Remove(tmp)
		if err := os.Symlink(v, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	point("v1")
	for _, f := range []string{KeyFile, KeyIDFile} {
		if err := os.Symlink(filepath.Join("..data", f), filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	valid := map[string]bool{}
	for _, kv := range versions {
		mac := hmac.New(sha256.New, []byte(kv[0]))
		mac.Write([]byte(email))
		valid[kv[1]+":"+hex.EncodeToString(mac.Sum(nil))] = true
	}

	p := New(dir)
	p.ttl = 0 // re-read on every call
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				point([]string{"v1", "v2"}[i%2])
			}
		}
	}()
	seen := map[string]bool{}
	for range 3000 {
		got, ok := p.Pseudonym(email)
		if !ok {
			continue // an unstable read is omitted, never mislabelled
		}
		if !valid[got] {
			close(stop)
			<-done
			t.Fatalf("mislabelled pseudonym %s", got)
		}
		seen[got[:2]] = true
	}
	close(stop)
	<-done
	if len(seen) == 0 {
		t.Error("never produced a pseudonym")
	}
}
