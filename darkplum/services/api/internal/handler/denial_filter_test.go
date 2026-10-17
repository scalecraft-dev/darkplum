package handler

import (
	"encoding/json"
	"testing"
)

func TestValidDenialCodeFilter_ModelRequiresDataRetention(t *testing.T) {
	if !validDenialCodeFilter("model_requires_data_retention") {
		t.Fatal("model_requires_data_retention must be an accepted denial_code filter")
	}
	if validDenialCodeFilter("model_requires_something_else") {
		t.Fatal("unknown codes must still be rejected")
	}
}

func TestNormalizeAvailability(t *testing.T) {
	st, d := normalizeAvailability(json.RawMessage(`{"state":"ok","reason":"data_retention"}`))
	if st == nil || *st != "ok" || d == nil || *d != `{"state":"ok","reason":"data_retention"}` {
		t.Fatalf("object: %v %v", st, d)
	}
	for _, raw := range []string{``, `null`, `"x"`, `[]`, `{}`, `{"state":""}`, `{bad`} {
		if st, d := normalizeAvailability(json.RawMessage(raw)); st != nil || d != nil {
			t.Fatalf("%q must normalise to nil/nil, got %v %v", raw, st, d)
		}
	}
}
