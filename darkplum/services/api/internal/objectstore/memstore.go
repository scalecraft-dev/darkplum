package objectstore

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemStore is an in-memory Store with a version history and fault
// injection, used by unit tests and the handler suite.
type MemStore struct {
	mu      sync.Mutex
	objs    map[string]memObj
	history map[string][]string // key -> "put"/"marker" per version, newest last
	fail    map[string][]error  // op -> queued errors (consumed one per call)
	failAll map[string]error    // op -> error on every call
	failKey func(op, key string) error
	calls   map[string]int
	now     func() time.Time
}

type memObj struct {
	body []byte
	meta Meta
	at   time.Time
}

func NewMemStore() *MemStore {
	return &MemStore{
		objs: map[string]memObj{}, history: map[string][]string{},
		fail: map[string][]error{}, failAll: map[string]error{}, calls: map[string]int{},
		now: time.Now,
	}
}

// FailNext makes the next n calls of op ("put","get","head","delete","purge","list") return err.
func (m *MemStore) FailNext(op string, err error, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := 0; i < n; i++ {
		m.fail[op] = append(m.fail[op], err)
	}
}

// FailAlways makes every call of op return err until ClearFaults.
func (m *MemStore) FailAlways(op string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failAll[op] = err
}

// FailWhen installs a predicate consulted on every call.
func (m *MemStore) FailWhen(f func(op, key string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failKey = f
}

func (m *MemStore) ClearFaults() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail, m.failAll, m.failKey = map[string][]error{}, map[string]error{}, nil
}

// Calls reports how many times op was invoked.
func (m *MemStore) Calls(op string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[op]
}

func (m *MemStore) check(op, key string) error {
	m.calls[op]++
	if q := m.fail[op]; len(q) > 0 {
		m.fail[op] = q[1:]
		return q[0]
	}
	if e := m.failAll[op]; e != nil {
		return e
	}
	if m.failKey != nil {
		return m.failKey(op, key)
	}
	return nil
}

// Keys returns every current key, sorted.
func (m *MemStore) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ks := make([]string, 0, len(m.objs))
	for k := range m.objs {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Body returns an object's bytes without counting as a call.
func (m *MemStore) Body(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	return append([]byte(nil), o.body...), ok
}

// Versions is the number of retained versions + delete markers of key.
func (m *MemStore) Versions(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.history[key])
}

// Seed writes an object as if a person had uploaded it (no metadata unless given).
func (m *MemStore) Seed(key string, body []byte, meta Meta) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.put(key, body, meta)
}

func (m *MemStore) put(key string, body []byte, meta Meta) {
	lm := Meta{}
	for k, v := range meta {
		lm[strings.ToLower(k)] = v
	}
	m.objs[key] = memObj{body: append([]byte(nil), body...), meta: lm, at: m.now()}
	m.history[key] = append(m.history[key], "put")
}

func (m *MemStore) Put(_ context.Context, key string, body []byte, meta Meta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("put", key); err != nil {
		return err
	}
	m.put(key, body, meta)
	return nil
}

func (m *MemStore) info(key string, o memObj, withMeta bool) Info {
	sum := md5.Sum(o.body)
	i := Info{Key: key, Size: int64(len(o.body)), LastModified: o.at, ETag: hex.EncodeToString(sum[:])}
	if withMeta {
		i.Meta = Meta{}
		for k, v := range o.meta {
			i.Meta[k] = v
		}
	}
	return i
}

func (m *MemStore) Head(_ context.Context, key string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("head", key); err != nil {
		return Info{}, err
	}
	o, ok := m.objs[key]
	if !ok {
		return Info{}, ErrNotFound
	}
	return m.info(key, o, true), nil
}

func (m *MemStore) Get(_ context.Context, key string) ([]byte, Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("get", key); err != nil {
		return nil, Info{}, err
	}
	o, ok := m.objs[key]
	if !ok {
		return nil, Info{}, ErrNotFound
	}
	if int64(len(o.body)) > MaxObjectBytes {
		return nil, Info{}, ErrTooLarge
	}
	return append([]byte(nil), o.body...), m.info(key, o, true), nil
}

func (m *MemStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("delete", key); err != nil {
		return err
	}
	if _, ok := m.objs[key]; ok {
		delete(m.objs, key)
		m.history[key] = append(m.history[key], "marker")
	}
	return nil
}

func (m *MemStore) PurgeVersions(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("purge", key); err != nil {
		return err
	}
	delete(m.objs, key)
	delete(m.history, key)
	return nil
}

func (m *MemStore) List(_ context.Context, prefix, cursor string, limit int) ([]Info, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("list", prefix); err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var keys []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) && k > cursor {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	next := ""
	if len(keys) > limit {
		keys = keys[:limit]
		next = keys[len(keys)-1]
	}
	out := make([]Info, 0, len(keys))
	for _, k := range keys {
		out = append(out, m.info(k, m.objs[k], false))
	}
	return out, next, nil
}

func (m *MemStore) Bucket() string        { return "mem-bucket" }
func (m *MemStore) URI(key string) string { return "s3://mem-bucket/library/v1/" + key }
