package handler

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// services/provider-parity.json is shared with the CLI (services/cli/internal/vendor),
// which applies the same bare-wildcard and filter-stamp rules to `darkplum keys set`.
// The two are separate Go modules, so this file is what keeps them identical.
func TestProviderParityVectors(t *testing.T) {
	b, err := os.ReadFile("../../../provider-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		BareWildcard []struct {
			Pattern string `json:"pattern"`
			Want    bool   `json:"want"`
		} `json:"bare_wildcard"`
		NextStamp []struct {
			Now  string `json:"now"`
			Old  string `json:"old"`
			Want string `json:"want"`
		} `json:"next_stamp"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.BareWildcard) == 0 || len(v.NextStamp) == 0 {
		t.Fatal("parity vectors are empty")
	}
	for _, c := range v.BareWildcard {
		if got := bareWildcardPattern(c.Pattern); got != c.Want {
			t.Errorf("bareWildcardPattern(%q) = %v, want %v", c.Pattern, got, c.Want)
		}
	}
	for _, c := range v.NextStamp {
		now, _ := time.Parse(time.RFC3339, c.Now)
		var old *time.Time
		if t0, err := time.Parse(time.RFC3339, c.Old); err == nil {
			old = &t0
		}
		got := nextFilterSetAt(now, old, nil).UTC().Format(time.RFC3339)
		if got != c.Want {
			t.Errorf("nextFilterSetAt(%s, %q) = %s, want %s", c.Now, c.Old, got, c.Want)
		}
	}
}
