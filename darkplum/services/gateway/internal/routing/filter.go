package routing

import (
	"fmt"
	"path"
)

// ApplyGlobFilter returns the subset of ids that match at least one pattern
// in patterns, using path.Match semantics (e.g. "us.anthropic.*"). An empty
// patterns slice means "no filter": ids is returned unchanged. A malformed
// glob returns an error naming the bad pattern rather than silently matching
// nothing — config.Load already validates the filter env var at startup, so
// this only fires if that guard is ever bypassed. envVarName is named in the
// returned error only (e.g. "BEDROCK_MODEL_FILTER", "ANTHROPIC_MODEL_FILTER",
// "OPENAI_MODEL_FILTER") so every source using this shared helper produces an
// error that names the actual knob an operator would fix, not a hardcoded
// one.
func ApplyGlobFilter(ids []string, patterns []string, envVarName string) ([]string, error) {
	if len(patterns) == 0 {
		return ids, nil
	}

	out := make([]string, 0, len(ids))
	for _, id := range ids {
		matched, err := matchesAnyGlob(id, patterns, envVarName)
		if err != nil {
			return nil, err
		}
		if matched {
			out = append(out, id)
		}
	}
	return out, nil
}

func matchesAnyGlob(id string, patterns []string, envVarName string) (bool, error) {
	for _, p := range patterns {
		ok, err := path.Match(p, id)
		if err != nil {
			return false, fmt.Errorf("routing: invalid %s pattern %q: %w", envVarName, p, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
