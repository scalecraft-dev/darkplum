package routing

import (
	"fmt"
	"path"
)

// ApplyGlobFilter returns the subset of ids that match at least one pattern
// in patterns, using path.Match semantics (e.g. "us.anthropic.*"). An empty
// patterns slice means "no filter": ids is returned unchanged. A malformed
// glob returns an error naming the bad pattern rather than silently matching
// nothing — config.Load already validates BEDROCK_MODEL_FILTER at startup,
// so this only fires if that guard is ever bypassed.
func ApplyGlobFilter(ids []string, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return ids, nil
	}

	out := make([]string, 0, len(ids))
	for _, id := range ids {
		matched, err := matchesAnyGlob(id, patterns)
		if err != nil {
			return nil, err
		}
		if matched {
			out = append(out, id)
		}
	}
	return out, nil
}

func matchesAnyGlob(id string, patterns []string) (bool, error) {
	for _, p := range patterns {
		ok, err := path.Match(p, id)
		if err != nil {
			return false, fmt.Errorf("routing: invalid BEDROCK_MODEL_FILTER pattern %q: %w", p, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
