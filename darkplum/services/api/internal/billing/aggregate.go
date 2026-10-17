package billing

import "math/big"

func newRat100() *big.Rat { return big.NewRat(100, 1) }

func sv(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// AggregateUsage merges rows that share the table's unique key (a vendor can
// return the same group in several buckets/pages), so an upsert never hits
// the same row twice in one batch.
func AggregateUsage(in []UsageRow) []UsageRow {
	type k struct{ day, model, ws, key string }
	idx := map[k]int{}
	var out []UsageRow
	for _, r := range in {
		key := k{r.Day, r.Model, sv(r.WorkspaceID), sv(r.APIKeyID)}
		if i, ok := idx[key]; ok {
			out[i].UncachedInput += r.UncachedInput
			out[i].CacheRead += r.CacheRead
			out[i].CacheCreation += r.CacheCreation
			out[i].OutputToken += r.OutputToken
			continue
		}
		idx[key] = len(out)
		out = append(out, r)
	}
	return out
}

// AggregateCosts merges cost rows sharing (day, description, workspace).
func AggregateCosts(in []CostRow) []CostRow {
	type k struct{ day, desc, ws string }
	idx := map[k]int{}
	var out []CostRow
	for _, r := range in {
		key := k{r.Day, r.Description, sv(r.WorkspaceID)}
		if i, ok := idx[key]; ok {
			out[i].Cents = new(big.Rat).Add(out[i].Cents, r.Cents)
			if out[i].Model == nil {
				out[i].Model = r.Model
			}
			continue
		}
		idx[key] = len(out)
		out = append(out, r)
	}
	return out
}
