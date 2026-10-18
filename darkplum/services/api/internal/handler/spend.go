package handler

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/api/internal/pricing"
)

// spendGroup describes one group_by dimension: the SQL key/label expressions
// and the joins they need. Key is always non-NULL text (” = none).
type spendGroup struct {
	key, label, join string
	noneLabel        string
}

var spendGroups = map[string]spendGroup{
	"user": {
		key:       "COALESCE(ie.user_id::text, '')",
		label:     "COALESCE(NULLIF(u.name, ''), u.email, '')",
		join:      "LEFT JOIN users u ON u.id = ie.user_id",
		noneLabel: "(unknown)",
	},
	"api_key": {
		key:       "COALESCE(ie.api_key_id::text, '')",
		label:     "COALESCE(k.name, '')",
		join:      "LEFT JOIN api_keys k ON k.id = ie.api_key_id",
		noneLabel: "(none)",
	},
	"model":         {key: "ie.model", label: "ie.model", noneLabel: "(none)"},
	"provider":      {key: "ie.provider", label: "ie.provider", noneLabel: "(none)"},
	"backend_class": {key: "COALESCE(ie.backend_class, 'unrecorded')", label: "COALESCE(ie.backend_class, 'unrecorded')", noneLabel: "unrecorded"},
	// alias: what the caller asked for when it differs from the resolved model.
	"alias": {key: "COALESCE(NULLIF(ie.requested_model, ie.model), '')", label: "COALESCE(NULLIF(ie.requested_model, ie.model), '')", noneLabel: "(direct)"},
	"route": {key: "COALESCE(ie.route_name, '')", label: "COALESCE(ie.route_name, '')", noneLabel: "(none)"},
	"day":   {key: "to_char(date_trunc('day', ie.created_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD')", label: "to_char(date_trunc('day', ie.created_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD')", noneLabel: "(none)"},
}

type spendAgg struct {
	key, label                           string
	requests, prompt, completion, cr, cc int64
	unpriced, billable                   int64
	cost                                 *big.Rat // sum over priced groups
	pricedAny                            bool
	// provider is the single provider seen in this group ("" when none or
	// mixed); the reconciliation uses it to attribute model rows to a vendor.
	provider string
	mixed    bool
}

func (a *spendAgg) cents() *string {
	if a.billable > 0 && !a.pricedAny {
		return nil
	}
	s := pricing.FormatCents(a.cost)
	return &s
}

type spendRowJSON struct {
	Key                 string  `json:"key"`
	Label               string  `json:"label"`
	Requests            int64   `json:"requests"`
	PromptTokens        int64   `json:"prompt_tokens"`
	CompletionTokens    int64   `json:"completion_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CostCents           *string `json:"cost_cents"`
	UnpricedRequests    int64   `json:"unpriced_requests"`
}

func (a *spendAgg) json() spendRowJSON {
	return spendRowJSON{a.key, a.label, a.requests, a.prompt, a.completion, a.cr, a.cc, a.cents(), a.unpriced}
}

// UsageSpend handles GET /api/v1/usage/spend.
// Query params: group_by (required), user_id, api_key_id, model, purpose,
// denial_code, backend_class, since, until, limit (default 50, max 500).
// Cost is a list-price estimate computed at query time from model_prices.
func (h *ActivityHandler) UsageSpend(c *gin.Context) {
	var (
		grp   spendGroup
		ok    bool
		time2 string // "day"|"hour" for the two-dimensional <bucket>,backend_class form
	)
	if g := c.Query("group_by"); g == "day,backend_class" || g == "hour,backend_class" {
		time2 = strings.TrimSuffix(g, ",backend_class")
		grp, ok = spendTimeClassGroup(time2), true
	} else {
		grp, ok = spendGroups[g]
	}
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "group_by must be one of user, api_key, model, provider, backend_class, alias, route, day, day,backend_class, hour,backend_class"})
		return
	}
	limit, maxLimit := 50, 500
	if time2 != "" {
		// limit counts time buckets (not rows): the newest N, chronological.
		limit, maxLimit = 31, 744
		if time2 == "hour" {
			limit = 24
		}
	}
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
			return
		}
		limit = min(n, maxLimit)
	}

	args := []any{}
	where := "WHERE 1=1"
	argN := 1
	if v := c.Query("user_id"); v != "" {
		if !isValidUUID(v) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
			return
		}
		where += fmt.Sprintf(" AND ie.user_id = $%d", argN)
		args = append(args, v)
		argN++
	}
	if v := c.Query("api_key_id"); v != "" {
		if !isValidUUID(v) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid api_key_id"})
			return
		}
		where += fmt.Sprintf(" AND ie.api_key_id = $%d", argN)
		args = append(args, v)
		argN++
	}
	if v := c.Query("model"); v != "" {
		where += fmt.Sprintf(" AND ie.model = $%d", argN)
		args = append(args, v)
		argN++
	}
	purpose := c.Query("purpose")
	if purpose == "" {
		purpose = "inference"
	}
	if !validUsagePurposeFilter(purpose) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "purpose must be one of inference, routing, routing_dryrun, all"})
		return
	}
	if purpose != "all" {
		where += fmt.Sprintf(" AND ie.purpose = $%d", argN)
		args = append(args, purpose)
		argN++
	}
	var filterErr error
	if where, args, argN, filterErr = appendDenialCodeFilter(c, where, args, argN); filterErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": filterErr.Error()})
		return
	}
	if where, args, argN, filterErr = appendBackendClassFilter(c, where, args, argN); filterErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": filterErr.Error()})
		return
	}
	if v := c.Query("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since must be RFC3339"})
			return
		}
		where += fmt.Sprintf(" AND ie.created_at >= $%d", argN)
		args = append(args, t)
		argN++
	}
	if v := c.Query("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "until must be RFC3339"})
			return
		}
		where += fmt.Sprintf(" AND ie.created_at <= $%d", argN)
		args = append(args, t)
		argN++
	}

	byKey, total, err := aggregateSpend(c.Request.Context(), h.db, grp, where, args)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if time2 != "" {
		h.writeTimeClassSpend(c, byKey, total, limit)
		return
	}

	list := make([]*spendAgg, 0, len(byKey))
	for _, a := range byKey {
		list = append(list, a)
	}
	if c.Query("group_by") == "day" {
		sort.Slice(list, func(i, j int) bool { return list[i].key > list[j].key })
	} else {
		sort.Slice(list, func(i, j int) bool {
			a, b := list[i], list[j]
			ac, bc := a.cents() != nil, b.cents() != nil
			if ac != bc {
				return ac
			}
			if ac {
				if d := a.cost.Cmp(b.cost); d != 0 {
					return d > 0
				}
			}
			if a.requests != b.requests {
				return a.requests > b.requests
			}
			return a.key < b.key
		})
	}
	if len(list) > limit {
		list = list[:limit]
	}
	if c.Query("group_by") == "day" { // newest N days, rendered chronologically
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	out := make([]spendRowJSON, 0, len(list))
	for _, a := range list {
		out = append(out, a.json())
	}

	coverage := 1.0
	if total.billable > 0 {
		coverage = float64(total.billable-total.unpriced) / float64(total.billable)
	}
	c.JSON(http.StatusOK, gin.H{
		"totals": gin.H{
			"requests": total.requests, "prompt_tokens": total.prompt, "completion_tokens": total.completion,
			"cache_read_tokens": total.cr, "cache_creation_tokens": total.cc,
			"cost_cents": total.cents(), "unpriced_requests": total.unpriced,
		},
		"rows":            out,
		"priced_coverage": coverage,
		"currency":        "USD",
		"basis":           "list_price_estimate",
	})
}

// aggregateSpend is the Layer-1 estimate engine: events matching where/args
// (which must already bind $1..$n; the price-boundary array is appended here)
// are grouped by grp and priced. Shared by UsageSpend and the vendor
// reconciliation so both estimate identically. Error text is a fixed
// string safe to return to the client.
func aggregateSpend(ctx context.Context, db *pgxpool.Pool, grp spendGroup, where string, args []any) (map[string]*spendAgg, *spendAgg, error) {
	argN := len(args) + 1
	prices, err := pricing.LoadAll(ctx, db)
	if err != nil {
		return nil, nil, errors.New("load model prices failed")
	}
	bounds := pricing.Boundaries(prices)
	args = append(args[:len(args):len(args)], bounds)

	// Events are aggregated per (group, provider, model, region, price epoch,
	// refused). width_bucket against every price boundary splits the range
	// into intervals in which the effective price set is constant, so
	// pricing one aggregate is exact (cost is linear in tokens; the
	// uncached-prompt clamp is applied per event inside the SUM).
	q := fmt.Sprintf(`
		SELECT %s, %s, ie.provider, ie.model, ie.region,
			width_bucket(ie.created_at, $%d::timestamptz[]) AS bk,
			(ie.denial_code IS NOT NULL) AS refused,
			COUNT(*)::bigint,
			COALESCE(SUM(ie.prompt_tokens), 0)::bigint,
			COALESCE(SUM(ie.completion_tokens), 0)::bigint,
			COALESCE(SUM(ie.cache_read_tokens), 0)::bigint,
			COALESCE(SUM(ie.cache_creation_tokens), 0)::bigint,
			COALESCE(SUM(GREATEST(COALESCE(ie.prompt_tokens,0) - COALESCE(ie.cache_read_tokens,0) - COALESCE(ie.cache_creation_tokens,0), 0)), 0)::bigint
		FROM inference_events ie
		%s
		%s
		GROUP BY 1, 2, 3, 4, 5, 6, 7
	`, grp.key, grp.label, argN, grp.join, where)

	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return nil, nil, errors.New("spend query failed")
	}
	defer rows.Close()

	byKey := map[string]*spendAgg{}
	total := &spendAgg{cost: new(big.Rat)}
	for rows.Next() {
		var (
			key, label, provider, model string
			region                      *string
			bk                          int
			refused                     bool
			n, prompt, comp, cr, cc, un int64
		)
		if err := rows.Scan(&key, &label, &provider, &model, &region, &bk, &refused, &n, &prompt, &comp, &cr, &cc, &un); err != nil {
			return nil, nil, errors.New("scan spend failed")
		}
		a := byKey[key]
		if a == nil {
			if label == "" {
				label = grp.noneLabel
			}
			a = &spendAgg{key: key, label: label, cost: new(big.Rat)}
			byKey[key] = a
		}
		switch {
		case a.provider == "":
			a.provider = provider
		case a.provider != provider:
			a.mixed = true
		}
		for _, t := range []*spendAgg{a, total} {
			t.requests += n
			t.prompt += prompt
			t.completion += comp
			t.cr += cr
			t.cc += cc
		}
		if refused {
			continue // refused requests count as requests but cost 0
		}
		a.billable += n
		total.billable += n
		at := time.Time{}
		if bk > 0 {
			at = bounds[bk-1]
		}
		p := pricing.Match(prices, provider, model, region, at)
		if p == nil {
			a.unpriced += n
			total.unpriced += n
			continue
		}
		cost := p.CostCents(pricing.Usage{Uncached: un, CacheRead: cr, CacheCreate: cc, Completion: comp})
		a.cost.Add(a.cost, cost)
		total.cost.Add(total.cost, cost)
		a.pricedAny = true
		total.pricedAny = true
	}
	if err := rows.Err(); err != nil {
		return nil, nil, errors.New("spend rows error")
	}
	return byKey, total, nil
}

// spendTimeClassGroup is the two-dimensional group: an RFC3339 UTC bucket
// (day or hour) joined to the backend class with '|' (neither part can
// contain '|'); writeTimeClassSpend splits it back apart.
func spendTimeClassGroup(unit string) spendGroup {
	bucket := fmt.Sprintf(`to_char(date_trunc('%s', ie.created_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`, unit)
	k := bucket + ` || '|' || COALESCE(ie.backend_class, 'unrecorded')`
	return spendGroup{key: k, label: k, noneLabel: "(none)"}
}

type spendTimeClassRowJSON struct {
	spendRowJSON
	BackendClass string `json:"backend_class"`
}

// writeTimeClassSpend renders group_by=day|hour,backend_class: rows for the
// newest `limit` buckets, chronological, one row per (bucket, class).
func (h *ActivityHandler) writeTimeClassSpend(c *gin.Context, byKey map[string]*spendAgg, total *spendAgg, limit int) {
	type row struct {
		bucket, class string
		a             *spendAgg
	}
	rows := make([]row, 0, len(byKey))
	buckets := map[string]struct{}{}
	for k, a := range byKey {
		i := strings.LastIndex(k, "|")
		rows = append(rows, row{k[:i], k[i+1:], a})
		buckets[k[:i]] = struct{}{}
	}
	if len(buckets) > limit {
		keys := make([]string, 0, len(buckets))
		for b := range buckets {
			keys = append(keys, b)
		}
		sort.Strings(keys)
		keep := map[string]struct{}{}
		for _, b := range keys[len(keys)-limit:] {
			keep[b] = struct{}{}
		}
		kept := rows[:0]
		for _, r := range rows {
			if _, ok := keep[r.bucket]; ok {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].bucket != rows[j].bucket {
			return rows[i].bucket < rows[j].bucket
		}
		return rows[i].class < rows[j].class
	})
	out := make([]spendTimeClassRowJSON, 0, len(rows))
	for _, r := range rows {
		j := r.a.json()
		j.Key, j.Label = r.bucket, r.bucket
		out = append(out, spendTimeClassRowJSON{j, r.class})
	}
	coverage := 1.0
	if total.billable > 0 {
		coverage = float64(total.billable-total.unpriced) / float64(total.billable)
	}
	c.JSON(http.StatusOK, gin.H{
		"totals": gin.H{
			"requests": total.requests, "prompt_tokens": total.prompt, "completion_tokens": total.completion,
			"cache_read_tokens": total.cr, "cache_creation_tokens": total.cc,
			"cost_cents": total.cents(), "unpriced_requests": total.unpriced,
		},
		"rows":            out,
		"priced_coverage": coverage,
		"currency":        "USD",
		"basis":           "list_price_estimate",
	})
}
