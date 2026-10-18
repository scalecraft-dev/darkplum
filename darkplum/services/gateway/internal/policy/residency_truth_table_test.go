package policy

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

// residencyTruthTablePath is the canonical, cross-service artifact naming
// every (residency, class) -> permitted cell of the residency ladder, plus
// the residency rank order — see guilfoyle's M3 review finding: the gateway
// (this file), the api (models_logic_test.go's TestResidencyPermitsClass),
// and services/web/scripts/check-policy.mts all read this SAME file rather
// than maintaining three hand-typed copies of the same truth table, so
// editing the ladder in one implementation without the other two fails CI.
const residencyTruthTablePath = "testdata/residency_truth_table.json"

// unknownToken is the literal value residencyTruthTable uses in a cell's
// "residency" or "class" field to mean "any value not in residency_rank /
// known_classes" — the wire contract's "unknown -> refused/strictest" rule
// applies identically regardless of which unrecognised string actually
// appears, so the golden only needs to record the ONE unknown-shaped cell
// per row/column rather than an infinite set of examples.
const unknownToken = "<unknown>"

// exampleUnknownResidency / exampleUnknownClass are concrete unrecognised
// values used to exercise the unknownToken cells against the live
// ResidencyPermits function — any other unrecognised string would classify
// identically (see isKnownResidency/classResidencyCeiling).
const (
	exampleUnknownResidency = "not-a-real-residency"
	exampleUnknownClass     = "some_future_class"
)

type residencyTruthTableCell struct {
	Residency string `json:"residency"`
	Class     string `json:"class"`
	Permitted bool   `json:"permitted"`
}

// residencyTruthTable is the JSON shape of residencyTruthTablePath. Field
// names are part of the cross-service contract — the api and web consumers
// decode this exact shape.
type residencyTruthTable struct {
	// ResidencyRank is the residency ladder from loosest to strictest —
	// residencyByRank verbatim (see effective.go): any(0) <
	// cloud_account_only(1) < self_hosted_only(2). A residency value NOT in
	// this list is treated as the strictest rank (see residencyRankOf).
	ResidencyRank []string `json:"residency_rank"`
	// KnownClasses are the backend classes ResidencyPermits recognises. A
	// class NOT in this list is refused under every residency, including
	// "any" — see classResidencyCeiling's doc comment.
	KnownClasses []string `json:"known_classes"`
	// UnknownToken is the literal string a cell uses for "any value not in
	// ResidencyRank/KnownClasses" — see this file's doc comment.
	UnknownToken string                    `json:"unknown_token"`
	Cells        []residencyTruthTableCell `json:"cells"`
}

// buildResidencyTruthTable generates the truth table from the CURRENT code —
// residencyByRank for the rank order, and a live ResidencyPermits(residency,
// class) call for every cell — so TestResidencyTruthTable_MatchesGolden can
// fail the moment the golden file diverges from what the code actually does,
// in either direction.
func buildResidencyTruthTable() residencyTruthTable {
	residencies := append(append([]string{}, residencyByRank...), exampleUnknownResidency)
	knownClasses := []string{routing.ClassSelfHosted, routing.ClassCloudManaged, routing.ClassExternalAPI}
	classes := append(append([]string{}, knownClasses...), exampleUnknownClass)

	cells := make([]residencyTruthTableCell, 0, len(residencies)*len(classes))
	for _, r := range residencies {
		rLabel := r
		if r == exampleUnknownResidency {
			rLabel = unknownToken
		}
		for _, c := range classes {
			cLabel := c
			if c == exampleUnknownClass {
				cLabel = unknownToken
			}
			cells = append(cells, residencyTruthTableCell{
				Residency: rLabel,
				Class:     cLabel,
				Permitted: ResidencyPermits(r, c),
			})
		}
	}

	return residencyTruthTable{
		ResidencyRank: append([]string{}, residencyByRank...),
		KnownClasses:  knownClasses,
		UnknownToken:  unknownToken,
		Cells:         cells,
	}
}

// updateResidencyGolden regenerates testdata/residency_truth_table.json from
// the current code. Run with:
//
//	go test ./internal/policy/... -run TestResidencyTruthTable_MatchesGolden -update
//
// ONLY when a deliberate ResidencyPermits/rank-order change means the golden
// itself must move — never to silently paper over an unintended regression.
var updateResidencyGolden = flag.Bool("update", false, "rewrite "+residencyTruthTablePath+" from the current code")

// TestResidencyTruthTable_MatchesGolden is the single source of truth
// (guilfoyle M3) for the residency x class permission ladder: it asserts
// policy.ResidencyPermits against testdata/residency_truth_table.json AND
// fails if that file is not EXACTLY what the code currently generates —
// mirroring how docs/api-routes.json is diffed against the live route table
// elsewhere in this repo (cmd/route_inventory_test.go in the api), except
// here the golden is regenerable in-place via -update rather than hand-typed.
func TestResidencyTruthTable_MatchesGolden(t *testing.T) {
	generated := buildResidencyTruthTable()

	if *updateResidencyGolden {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		// SetEscapeHTML(false): the default HTML-escaping turns "<unknown>"
		// into "<unknown>", which is valid JSON but needlessly
		// unreadable/unmaintainable for a file three services hand-diff and
		// occasionally hand-edit.
		enc.SetEscapeHTML(false)
		if err := enc.Encode(generated); err != nil {
			t.Fatalf("marshal generated truth table: %v", err)
		}
		if err := os.WriteFile(residencyTruthTablePath, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write %s: %v", residencyTruthTablePath, err)
		}
		t.Logf("rewrote %s from the current code", residencyTruthTablePath)
		return
	}

	data, err := os.ReadFile(residencyTruthTablePath)
	if err != nil {
		t.Fatalf("read %s: %v (run with -update to generate it)", residencyTruthTablePath, err)
	}
	var want residencyTruthTable
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("decode %s: %v", residencyTruthTablePath, err)
	}

	if !reflect.DeepEqual(want.ResidencyRank, generated.ResidencyRank) {
		t.Errorf("%s residency_rank = %v, want %v (code truth) — run with -update to regenerate",
			residencyTruthTablePath, want.ResidencyRank, generated.ResidencyRank)
	}
	if !reflect.DeepEqual(want.KnownClasses, generated.KnownClasses) {
		t.Errorf("%s known_classes = %v, want %v (code truth) — run with -update to regenerate",
			residencyTruthTablePath, want.KnownClasses, generated.KnownClasses)
	}
	if want.UnknownToken != generated.UnknownToken {
		t.Errorf("%s unknown_token = %q, want %q", residencyTruthTablePath, want.UnknownToken, generated.UnknownToken)
	}
	if !reflect.DeepEqual(want.Cells, generated.Cells) {
		t.Errorf("%s cells are stale relative to policy.ResidencyPermits — got %+v, want %+v (run with -update to regenerate)",
			residencyTruthTablePath, want.Cells, generated.Cells)
	}

	// Belt-and-suspenders: also assert every golden cell directly against a
	// FRESH ResidencyPermits call (not just against the same-run `generated`
	// struct above), so a golden that was hand-edited to match a bug still
	// fails this test.
	for _, cell := range want.Cells {
		residency := cell.Residency
		if residency == unknownToken {
			residency = exampleUnknownResidency
		}
		class := cell.Class
		if class == unknownToken {
			class = exampleUnknownClass
		}
		if got := ResidencyPermits(residency, class); got != cell.Permitted {
			t.Errorf("ResidencyPermits(%q, %q) = %v, want %v (per %s)", residency, class, got, cell.Permitted, residencyTruthTablePath)
		}
	}
}
