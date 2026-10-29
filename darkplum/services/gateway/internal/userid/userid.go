// Package userid pseudonymises the OpenAI `user` field for backends that must
// not receive it raw (Anthropic metadata.user_id, Bedrock requestMetadata).
//
// The value sent is "<key id>:<hex HMAC-SHA256(secret, user)>". The secret is a
// per-organisation Kubernetes Secret (darkplum-user-hmac) in the customer's
// namespace, generated at install and mounted into the gateway as files, the
// same way vendor keys are. It never lives
// in Postgres or the control plane, is never read from an env var, and is never
// logged. Errors name files, never their content. The same user under the same secret always yields the same value, so
// the vendor can still tell one end user from another; without the secret the
// value cannot be reversed or linked to a person.
//
// When the secret is missing, empty or too short the field is omitted:
// Pseudonym reports ok=false and the raw value is never substituted. The first
// such failure is logged at WARNING (once, until the secret becomes usable
// again). Phase B of #3 records the omission in the dropped-parameter header
// and audit column; until then it is visible only in that log line.
package userid

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// KeyFile and KeyIDFile are the Secret's data keys, i.e. the file names
	// under the mount directory.
	KeyFile   = "user-hmac-key"
	KeyIDFile = "user-hmac-key-id"

	// DefaultKeyID is the key id used when the Secret has no user-hmac-key-id.
	DefaultKeyID = "k1"

	// minKeyLen is the shortest secret accepted: anything shorter is a
	// placeholder, not a key.
	minKeyLen = 16
)

// keyIDPattern bounds the key id to something that cannot break the
// "<id>:<hex>" shape or Bedrock's requestMetadata value charset.
var keyIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,15}$`)

// recheckInterval bounds how often the files are touched; between rechecks
// the last good (or failed) read is reused.
const recheckInterval = 3 * time.Second

// Pseudonymizer derives user pseudonyms from the mounted secret. Safe for
// concurrent use. A nil *Pseudonymizer reports ok=false for every user.
//
// The key and its id are two files, and a rotation replaces both. They are
// read as a pair that must be stable (see readPair), so an HMAC is never
// labelled with an id that belongs to a different key.
type Pseudonymizer struct {
	dir string
	ttl time.Duration

	mu        sync.Mutex
	lastCheck time.Time
	secret    string
	id        string
	err       error
	warned    bool
}

// New reads the secret from dir (files KeyFile and KeyIDFile). An empty dir
// means no secret is mounted. Nothing is read until the first Pseudonym call,
// and the files are re-read as the kubelet refreshes them, so rotating the
// Secret needs no gateway restart.
func New(dir string) *Pseudonymizer {
	return &Pseudonymizer{dir: dir, ttl: recheckInterval}
}

// Pseudonym returns the pseudonym for user, or ok=false when the field must be
// omitted (empty user, or no usable secret).
func (p *Pseudonymizer) Pseudonym(user string) (string, bool) {
	if user == "" {
		return "", false
	}
	secret, id, err := p.material()
	if err != nil {
		p.warnOnce(err)
		return "", false
	}
	p.recovered()

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(user))
	return id + ":" + hex.EncodeToString(mac.Sum(nil)), true
}

var nilWarn sync.Once

var (
	errNotMounted = errors.New("no user-hmac secret is mounted")
	errKeyShort   = errors.New("user-hmac-key is shorter than the minimum")
	errKeyIDBad   = errors.New("user-hmac-key-id is not a short lowercase identifier")
	errUnstable   = errors.New("user-hmac files kept changing while being read")
)

func (p *Pseudonymizer) material() (secret, id string, err error) {
	if p == nil || p.dir == "" {
		return "", "", errNotMounted
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.lastCheck.IsZero() && time.Since(p.lastCheck) < p.ttl {
		return p.secret, p.id, p.err
	}
	p.lastCheck = time.Now()
	p.secret, p.id, p.err = p.readPair()
	return p.secret, p.id, p.err
}

// readPair reads key and id as one version. kubelet writes each version of a
// Secret into its own timestamped directory and swaps the dir/..data symlink to
// it atomically; the files inside a version directory never change. So both
// files are read from the directory ..data names, and the read is kept only if
// ..data still names it afterwards (a version directory can be deleted after a
// swap). Comparing two reads of dir/<file> instead is not enough: an A→B→A
// flip can produce the same mismatched pair twice.
//
// A mount without ..data (plain files, e.g. a hand-made dev directory) has no
// versions to pin; it is read twice and accepted when both reads agree.
func (p *Pseudonymizer) readPair() (secret, id string, err error) {
	dataLink := filepath.Join(p.dir, "..data")
	for range 5 {
		target, lerr := os.Readlink(dataLink)
		if lerr != nil {
			if !errors.Is(lerr, os.ErrNotExist) {
				return "", "", fmt.Errorf("reading ..data: %w", lerr)
			}
			k1, i1, e1 := p.readOnce(p.dir)
			if e1 != nil {
				return "", "", e1
			}
			k2, i2, e2 := p.readOnce(p.dir)
			if e2 != nil {
				return "", "", e2
			}
			if k1 == k2 && i1 == i2 {
				return k1, i1, nil
			}
			continue
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(p.dir, target)
		}
		k, i, e := p.readOnce(target)
		after, aerr := os.Readlink(dataLink)
		if aerr == nil && !filepath.IsAbs(after) {
			after = filepath.Join(p.dir, after)
		}
		if aerr == nil && after == target {
			if e != nil {
				return "", "", e
			}
			return k, i, nil
		}
	}
	return "", "", errUnstable
}

func (p *Pseudonymizer) readOnce(dir string) (secret, id string, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, KeyFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", errNotMounted
		}
		return "", "", fmt.Errorf("reading %s: %w", KeyFile, err)
	}
	secret = strings.TrimSpace(string(raw))
	if len(secret) < minKeyLen {
		return "", "", errKeyShort
	}
	idRaw, err := os.ReadFile(filepath.Join(dir, KeyIDFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		id = DefaultKeyID
	case err != nil:
		return "", "", fmt.Errorf("reading %s: %w", KeyIDFile, err)
	default:
		id = strings.TrimSpace(string(idRaw))
	}
	if !keyIDPattern.MatchString(id) {
		return "", "", errKeyIDBad
	}
	return secret, id, nil
}

// warnOnce logs the first failure of a run of failures. The error text never
// contains secret content (credsource errors carry the path only).
func (p *Pseudonymizer) warnOnce(cause error) {
	if p == nil {
		nilWarn.Do(func() {
			slog.Warn("userid: no user-hmac secret is configured; the OpenAI `user` field is omitted on Anthropic and Bedrock requests (never sent raw)")
		})
		return
	}
	p.mu.Lock()
	first := !p.warned
	p.warned = true
	p.mu.Unlock()
	if first {
		slog.Warn("userid: user-hmac secret unavailable; the OpenAI `user` field is omitted on Anthropic and Bedrock requests until it is installed (never sent raw)",
			"cause", cause.Error())
	}
}

func (p *Pseudonymizer) recovered() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.warned = false
	p.mu.Unlock()
}
