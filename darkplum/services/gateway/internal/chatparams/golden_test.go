package chatparams

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// docs/chat-params.json is the golden copy of the registry (pattern: the
// route inventory, cmd/route_inventory_test.go). A row added to or changed in
// the registry without the golden, or a golden row with no registry row,
// fails here. Regenerate with UPDATE_GOLDEN=1 go test ./internal/chatparams.

func goldenPath() string {
	// internal/chatparams -> gateway -> services -> darkplum -> docs
	return filepath.Join("..", "..", "..", "..", "docs", "chat-params.json")
}

func TestRegistryMatchesGolden(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "" {
		b, err := MarshalDocument()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath(), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(goldenPath())
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var golden Doc
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}

	key := func(r Row) string { return r.Param + " | " + r.When }
	reg := map[string]Row{}
	for _, r := range Rows() {
		if _, dup := reg[key(r)]; dup {
			t.Errorf("registry has two rows for %s", key(r))
		}
		reg[key(r)] = r
	}
	gold := map[string]Row{}
	for _, r := range golden.Rows {
		gold[key(r)] = r
	}
	for k, r := range reg {
		g, ok := gold[k]
		if !ok {
			t.Errorf("registry row %q is not in docs/chat-params.json", k)
			continue
		}
		if !reflect.DeepEqual(r, g) {
			t.Errorf("row %q differs from the golden:\n registry %+v\n golden   %+v", k, r, g)
		}
	}
	for k := range gold {
		if _, ok := reg[k]; !ok {
			t.Errorf("docs/chat-params.json row %q has no registry row", k)
		}
	}
	if !reflect.DeepEqual(golden.Backends, Backends) || !reflect.DeepEqual(golden.Dispositions, Dispositions) {
		t.Error("golden backends/dispositions differ from the registry")
	}
	if golden.UnknownKeys != UnknownKeys {
		t.Errorf("golden unknown_keys = %+v, registry %+v", golden.UnknownKeys, UnknownKeys)
	}
	if golden.Excluded.Mock != MockExcluded || golden.Excluded.Mock == "" {
		t.Error("golden must state the mock exclusion exactly as the registry does")
	}
	// Byte-for-byte too, so the file is the registry's own rendering and a
	// hand edit that parses the same still shows up.
	want, _ := MarshalDocument()
	if string(raw) != string(want) {
		t.Error("docs/chat-params.json is not byte-identical to the registry's rendering; run UPDATE_GOLDEN=1 go test ./internal/chatparams")
	}
}

// Shape rules the rest of the code relies on.
func TestRegistryShape(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Rows() {
		k := r.Param + "|" + r.When
		if seen[k] {
			t.Errorf("duplicate row %s", k)
		}
		seen[k] = true
		for _, b := range Backends {
			d, ok := r.Backends[b]
			if !ok {
				t.Errorf("%s has no %s cell", k, b)
				continue
			}
			s := r.Strict[b]
			want := d
			if d == DropRecorded {
				want = ContractRefuse
			}
			if s != want {
				t.Errorf("%s strict %s = %s, want %s", k, b, s, want)
			}
			if d == ContractRefuse && r.Reason == "" && r.When != "non-text-content" {
				// A refusal needs its sentence.
				t.Errorf("%s refuses on %s but has no reason", k, b)
			}
		}
	}
	if _, exact := BackendOfKind("mock"); exact {
		t.Error("mock must not have a registry column")
	}
}
