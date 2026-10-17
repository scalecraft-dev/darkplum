// Package pricing turns inference_events token counts into estimated USD
// cost from a versioned price book (model_prices). Costs are list-price
// estimates computed at query time, so a corrected price applies
// retroactively; nothing here is stored on the event rows.
package pricing

import (
	"fmt"
	"math/big"
	"path"
	"sort"
	"strings"
	"time"
)

// Providers is the closed set model_prices.provider accepts (mirrors the
// table's CHECK constraint).
var Providers = []string{"anthropic", "openai", "bedrock", "vllm", "lmstudio", "mock"}

// ValidProvider reports whether p is an accepted provider.
func ValidProvider(p string) bool {
	for _, v := range Providers {
		if v == p {
			return true
		}
	}
	return false
}

// ValidatePattern checks that pattern is a well-formed path.Match glob.
func ValidatePattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("model_pattern is required")
	}
	if _, err := path.Match(pattern, ""); err != nil {
		return fmt.Errorf("model_pattern is not a valid glob: %v", err)
	}
	return nil
}

// Price is one row of model_prices. Rates are USD per 1M tokens.
type Price struct {
	ID            string
	Provider      string
	ModelPattern  string
	Region        *string
	Input         *big.Rat
	Output        *big.Rat
	CacheRead     *big.Rat // nil = bill cache reads at the input rate
	CacheWrite    *big.Rat // nil = bill cache creation at the input rate
	EffectiveFrom time.Time
	EffectiveTo   *time.Time
	Source        string
	Notes         *string
}

// Usage is the token counts of one event (or an aggregate of events that
// share a price). Uncached is the already-clamped uncached prompt portion,
// so aggregation stays exact: callers clamp per event, then sum.
type Usage struct {
	Uncached    int64
	CacheRead   int64
	CacheCreate int64
	Completion  int64
}

// UncachedPrompt is prompt - cacheRead - cacheCreation, never below 0.
func UncachedPrompt(prompt, cacheRead, cacheCreate int64) int64 {
	n := prompt - cacheRead - cacheCreate
	if n < 0 {
		return 0
	}
	return n
}

// literalPrefixLen is the length of the pattern before its first wildcard
// (or escape) character -- the specificity measure.
func literalPrefixLen(pattern string) int {
	if i := strings.IndexAny(pattern, `*?[\`); i >= 0 {
		return i
	}
	return len(pattern)
}

// effective reports effective_from <= t < coalesce(effective_to, infinity).
func (p *Price) effective(t time.Time) bool {
	if p.EffectiveFrom.After(t) {
		return false
	}
	return p.EffectiveTo == nil || t.Before(*p.EffectiveTo)
}

// better reports whether a is more specific than b: longer literal prefix,
// then region-specific over NULL, then later effective_from, then ID (so the
// choice is deterministic).
func better(a, b *Price) bool {
	if la, lb := literalPrefixLen(a.ModelPattern), literalPrefixLen(b.ModelPattern); la != lb {
		return la > lb
	}
	if (a.Region != nil) != (b.Region != nil) {
		return a.Region != nil
	}
	if !a.EffectiveFrom.Equal(b.EffectiveFrom) {
		return a.EffectiveFrom.After(b.EffectiveFrom)
	}
	return a.ID < b.ID
}

// GlobalRegion is the price-row region for Bedrock "global." inference
// profiles. inference_events.region is the AWS region the request actually
// went to and is never "global", so the model id decides which table applies.
const GlobalRegion = "global"

// regionEligible: a "global." model id is priced only by rows whose region is
// "global" (whatever region the request ran in); any other model by rows with
// region == the event region or NULL, never by "global" rows.
func regionEligible(rowRegion *string, model string, eventRegion *string) bool {
	if strings.HasPrefix(model, "global.") {
		return rowRegion != nil && *rowRegion == GlobalRegion
	}
	if rowRegion == nil {
		return true
	}
	if *rowRegion == GlobalRegion {
		return false
	}
	return eventRegion != nil && *eventRegion == *rowRegion
}

// Match returns the most specific price effective at t for the event's
// provider/model/region, or nil when none applies (the event is "unpriced").
func Match(prices []Price, provider, model string, region *string, t time.Time) *Price {
	var best *Price
	for i := range prices {
		p := &prices[i]
		if p.Provider != provider || !p.effective(t) {
			continue
		}
		if !regionEligible(p.Region, model, region) {
			continue
		}
		if ok, err := path.Match(p.ModelPattern, model); err != nil || !ok {
			continue
		}
		if best == nil || better(p, best) {
			best = p
		}
	}
	return best
}

var (
	oneMillionCentsDivisor = big.NewRat(10000, 1) // /1e6 tokens * 100 cents
)

// CostCents is the exact cost in cents of u under p.
func (p *Price) CostCents(u Usage) *big.Rat {
	cacheRead, cacheWrite := p.CacheRead, p.CacheWrite
	if cacheRead == nil {
		cacheRead = p.Input
	}
	if cacheWrite == nil {
		cacheWrite = p.Input
	}
	sum := new(big.Rat)
	add := func(tokens int64, rate *big.Rat) {
		sum.Add(sum, new(big.Rat).Mul(big.NewRat(tokens, 1), rate))
	}
	add(u.Uncached, p.Input)
	add(u.CacheRead, cacheRead)
	add(u.CacheCreate, cacheWrite)
	add(u.Completion, p.Output)
	return sum.Quo(sum, oneMillionCentsDivisor)
}

// FormatCents renders cents as a decimal string with 4 decimal places.
func FormatCents(r *big.Rat) string { return r.FloatString(4) }

// ParseRate parses a non-negative decimal rate.
func ParseRate(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok || strings.Contains(s, "/") {
		return nil, fmt.Errorf("%q is not a number", s)
	}
	if r.Sign() < 0 {
		return nil, fmt.Errorf("must be non-negative")
	}
	if r.Cmp(big.NewRat(1000000, 1)) >= 0 {
		return nil, fmt.Errorf("must be less than 1000000")
	}
	return r, nil
}

// FormatRate renders a rate with up to 6 decimals, trailing zeros trimmed.
func FormatRate(r *big.Rat) string {
	s := r.FloatString(6)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	return s
}

func sortTimes(ts []time.Time) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
}
