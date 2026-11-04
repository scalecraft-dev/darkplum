package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

// runContract is the behaviour every Store must share. It runs against
// MemStore always and against the real S3 store behind the s3live build tag.
// prefix isolates one run's keys (the live bucket is shared).
func runContract(t *testing.T, s Store, prefix string) {
	ctx := context.Background()
	k := func(n string) string { return prefix + n }

	t.Run("put get head roundtrip with metadata", func(t *testing.T) {
		body := []byte("---\nname: x\n---\n\nhello\n")
		if err := s.Put(ctx, k("a/x.skill.md"), body, Meta{"darkplum-item-id": "id-1", "darkplum-sha256": "abc"}); err != nil {
			t.Fatal(err)
		}
		got, info, err := s.Get(ctx, k("a/x.skill.md"))
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("get = %q, %v", got, err)
		}
		if info.Meta["darkplum-item-id"] != "id-1" || info.Meta["darkplum-sha256"] != "abc" {
			t.Errorf("meta = %v", info.Meta)
		}
		h, err := s.Head(ctx, k("a/x.skill.md"))
		if err != nil || h.Size != int64(len(body)) || h.Meta["darkplum-item-id"] != "id-1" {
			t.Errorf("head = %+v, %v", h, err)
		}
	})

	t.Run("missing is ErrNotFound", func(t *testing.T) {
		if _, err := s.Head(ctx, k("nope")); !errors.Is(err, ErrNotFound) {
			t.Errorf("head: %v", err)
		}
		if _, _, err := s.Get(ctx, k("nope")); !errors.Is(err, ErrNotFound) {
			t.Errorf("get: %v", err)
		}
	})

	t.Run("put replaces", func(t *testing.T) {
		_ = s.Put(ctx, k("r"), []byte("one"), nil)
		_ = s.Put(ctx, k("r"), []byte("two"), nil)
		if b, _, _ := s.Get(ctx, k("r")); string(b) != "two" {
			t.Errorf("got %q", b)
		}
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		_ = s.Put(ctx, k("d"), []byte("x"), nil)
		if err := s.Delete(ctx, k("d")); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, k("d")); err != nil {
			t.Errorf("second delete: %v", err)
		}
		if err := s.Delete(ctx, k("never")); err != nil {
			t.Errorf("delete missing: %v", err)
		}
		if _, err := s.Head(ctx, k("d")); !errors.Is(err, ErrNotFound) {
			t.Errorf("still there: %v", err)
		}
	})

	t.Run("zero byte folder marker", func(t *testing.T) {
		if err := s.Put(ctx, k("folder/"), nil, nil); err != nil {
			t.Fatal(err)
		}
		h, err := s.Head(ctx, k("folder/"))
		if err != nil || h.Size != 0 {
			t.Errorf("marker = %+v, %v", h, err)
		}
	})

	t.Run("purge removes every version", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			_ = s.Put(ctx, k("p"), []byte(fmt.Sprint(i)), nil)
		}
		_ = s.Delete(ctx, k("p"))
		_ = s.Put(ctx, k("p-sibling"), []byte("keep"), nil)
		if err := s.PurgeVersions(ctx, k("p")); err != nil {
			t.Fatal(err)
		}
		if err := s.PurgeVersions(ctx, k("p")); err != nil {
			t.Errorf("second purge: %v", err)
		}
		if _, err := s.Head(ctx, k("p")); !errors.Is(err, ErrNotFound) {
			t.Errorf("p after purge: %v", err)
		}
		// A key that merely shares the prefix is untouched.
		if b, _, err := s.Get(ctx, k("p-sibling")); err != nil || string(b) != "keep" {
			t.Errorf("sibling = %q, %v", b, err)
		}
	})

	t.Run("list pages in key order", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			_ = s.Put(ctx, k(fmt.Sprintf("l/%d.md", i)), []byte("x"), nil)
		}
		var seen []string
		cursor := ""
		for pages := 0; pages < 10; pages++ {
			objs, next, err := s.List(ctx, k("l/"), cursor, 2)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range objs {
				seen = append(seen, o.Key)
			}
			if next == "" {
				break
			}
			cursor = next
		}
		if len(seen) != 5 || seen[0] != k("l/0.md") || seen[4] != k("l/4.md") {
			t.Errorf("seen = %v", seen)
		}
	})

	t.Run("URI and bucket", func(t *testing.T) {
		if s.Bucket() == "" || s.URI("x") == "" {
			t.Error("bucket/URI empty")
		}
	})
}

func TestMemStoreContract(t *testing.T) {
	runContract(t, NewMemStore(), "t/")
}

func TestMemStoreFaultInjection(t *testing.T) {
	m := NewMemStore()
	boom := errors.New("boom")
	m.FailNext("put", boom, 2)
	for i := 0; i < 2; i++ {
		if err := m.Put(context.Background(), "k", []byte("x"), nil); !errors.Is(err, boom) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if err := m.Put(context.Background(), "k", []byte("x"), nil); err != nil {
		t.Errorf("third put: %v", err)
	}
	m.FailAlways("get", boom)
	if _, _, err := m.Get(context.Background(), "k"); !errors.Is(err, boom) {
		t.Errorf("get: %v", err)
	}
	m.ClearFaults()
	if _, _, err := m.Get(context.Background(), "k"); err != nil {
		t.Errorf("after clear: %v", err)
	}
}

func TestParseS3URL(t *testing.T) {
	for raw, want := range map[string][2]string{
		"s3://b/library/v1/": {"b", "library/v1/"},
		"s3://b/library/v1":  {"b", "library/v1/"},
		"s3://b":             {"b", ""},
		"s3://b/":            {"b", ""},
	} {
		b, p, err := ParseS3URL(raw)
		if err != nil || b != want[0] || p != want[1] {
			t.Errorf("%s -> %q %q %v", raw, b, p, err)
		}
	}
	for _, bad := range []string{"", "gs://b/x/", "http://b/x", "s3:///x", "s3://u:p@b/x/", "s3://b/../x", "s3://b/x?y=1"} {
		if _, _, err := ParseS3URL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestErrorHasNoKeyOrMessage(t *testing.T) {
	e := &Error{Op: "put", Code: "AccessDenied", Status: 403}
	if e.Error() != "objectstore put failed: AccessDenied" || Code(e) != "AccessDenied" {
		t.Errorf("%q", e.Error())
	}
}
