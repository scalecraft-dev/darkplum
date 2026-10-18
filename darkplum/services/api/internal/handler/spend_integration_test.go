package handler

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
	"github.com/scalecraft-dev/darkplum/api/internal/pricing"
)

type spendFixture struct {
	pool       *pgxpool.Pool
	r          *gin.Engine
	alice, bob string
	keyA, keyB string
	adminID    string
}

func newSpendFixture(t *testing.T) *spendFixture {
	t.Helper()
	pool := setupTestDB(t)
	ctx := context.Background()
	f := &spendFixture{pool: pool}
	mustScan := func(dst *string, q string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, q, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustScan(&f.alice, `INSERT INTO users (email, name, role) VALUES ('alice@x.io','Alice','user') RETURNING id::text`)
	mustScan(&f.bob, `INSERT INTO users (email, name, role) VALUES ('bob@x.io','Bob','user') RETURNING id::text`)
	mustScan(&f.adminID, `INSERT INTO users (email, name, role) VALUES ('admin@x.io','Admin','admin') RETURNING id::text`)
	mustScan(&f.keyA, `INSERT INTO api_keys (user_id, name, prefix, key_hash) VALUES ($1,'alice-key','dp_aaaaaaaa','h1') RETURNING id::text`, f.alice)
	mustScan(&f.keyB, `INSERT INTO api_keys (user_id, name, prefix, key_hash) VALUES ($1,'bob-key','dp_bbbbbbbb','h2') RETURNING id::text`, f.bob)

	// Price book: anthropic claude-*; openai gpt-4* repriced on 2026-03-01.
	px := `INSERT INTO model_prices (provider, model_pattern, input_per_mtok, output_per_mtok, cache_read_per_mtok, cache_write_per_mtok, effective_from, effective_to, source)
	       VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'admin')`
	epoch := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	cut := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, a := range [][]any{
		{"anthropic", "claude-*", 3, 15, 0.3, 3.75, epoch, nil},
		{"openai", "gpt-4*", 10, 30, nil, nil, epoch, cut},
		{"openai", "gpt-4*", 20, 60, nil, nil, cut, nil},
	} {
		if _, err := pool.Exec(ctx, px, a...); err != nil {
			t.Fatalf("insert price: %v", err)
		}
	}

	ev := `INSERT INTO inference_events (request_id, user_id, api_key_id, provider, model, requested_model, route_name, backend_class,
	         status_code, denial_code, latency_ms, prompt_tokens, completion_tokens, cache_read_tokens, cache_creation_tokens, created_at)
	       VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,5,$11,$12,$13,$14,$15)`
	d := func(m time.Month, day int) time.Time { return time.Date(2026, m, day, 12, 0, 0, 0, time.UTC) }
	for _, a := range [][]any{
		{"e1", f.alice, f.keyA, "anthropic", "claude-s", "smart", "code", "external_api", 200, nil, 1000000, 100000, 400000, 100000, d(2, 10)},
		{"e2", f.alice, f.keyA, "openai", "gpt-4o", nil, nil, "external_api", 200, nil, 100000, 10000, nil, nil, d(2, 11)},
		{"e3", f.bob, f.keyB, "openai", "gpt-4o", nil, nil, "external_api", 200, nil, 100000, 10000, nil, nil, d(3, 5)},
		{"e4", f.bob, f.keyB, "vllm", "llama", nil, nil, "self_hosted", 200, nil, 50, 5, nil, nil, d(3, 5)},
		{"e5", f.bob, f.keyB, "anthropic", "claude-s", nil, nil, "external_api", 403, "model_not_allowed", nil, nil, nil, nil, d(3, 6)},
	} {
		if _, err := pool.Exec(ctx, ev, a...); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}

	gin.SetMode(gin.TestMode)
	f.r = gin.New()
	f.r.Use(func(c *gin.Context) { c.Set("user_id", f.adminID); c.Next() })
	ah := NewActivityHandler(pool)
	ph := NewPricingHandler(pool, activity.NewRecorder(pool))
	f.r.GET("/api/v1/usage", ah.ListUsage)
	f.r.GET("/api/v1/usage/export", ah.ExportUsage)
	f.r.GET("/api/v1/usage/spend", ah.UsageSpend)
	f.r.GET("/api/v1/model-prices", ph.ListPrices)
	f.r.PUT("/api/v1/model-prices", ph.PutPrice)
	f.r.DELETE("/api/v1/model-prices/:id", ph.DeletePrice)
	return f
}

func (f *spendFixture) do(method, url, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

type spendResp struct {
	Totals struct {
		Requests, PromptTokens, CompletionTokens, CacheReadTokens, CacheCreationTokens, UnpricedRequests int64
		CostCents                                                                                        *string `json:"cost_cents"`
	} `json:"totals"`
	Rows []struct {
		Key, Label                               string
		Requests, UnpricedRequests, PromptTokens int64
		CostCents                                *string `json:"cost_cents"`
	} `json:"rows"`
	PricedCoverage float64 `json:"priced_coverage"`
	Currency       string
	Basis          string
}

func (f *spendFixture) spend(t *testing.T, q string) spendResp {
	t.Helper()
	w := f.do(http.MethodGet, "/api/v1/usage/spend?"+q, "")
	if w.Code != 200 {
		t.Fatalf("spend %s: %d %s", q, w.Code, w.Body.String())
	}
	raw := map[string]json.RawMessage{}
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	var out spendResp
	// Field names in the contract are snake_case; decode via a lenient map.
	var generic struct {
		Totals map[string]any   `json:"totals"`
		Rows   []map[string]any `json:"rows"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &generic)
	toI := func(m map[string]any, k string) int64 { v, _ := m[k].(float64); return int64(v) }
	toS := func(m map[string]any, k string) *string {
		if v, ok := m[k].(string); ok {
			return &v
		}
		return nil
	}
	out.Totals.Requests = toI(generic.Totals, "requests")
	out.Totals.PromptTokens = toI(generic.Totals, "prompt_tokens")
	out.Totals.CompletionTokens = toI(generic.Totals, "completion_tokens")
	out.Totals.CacheReadTokens = toI(generic.Totals, "cache_read_tokens")
	out.Totals.CacheCreationTokens = toI(generic.Totals, "cache_creation_tokens")
	out.Totals.UnpricedRequests = toI(generic.Totals, "unpriced_requests")
	out.Totals.CostCents = toS(generic.Totals, "cost_cents")
	for _, m := range generic.Rows {
		out.Rows = append(out.Rows, struct {
			Key, Label                               string
			Requests, UnpricedRequests, PromptTokens int64
			CostCents                                *string `json:"cost_cents"`
		}{*toS(m, "key"), *toS(m, "label"), toI(m, "requests"), toI(m, "unpriced_requests"), toI(m, "prompt_tokens"), toS(m, "cost_cents")})
	}
	var cov struct {
		PricedCoverage  float64 `json:"priced_coverage"`
		Currency, Basis string
	}
	_ = json.Unmarshal(w.Body.Bytes(), &cov)
	out.PricedCoverage, out.Currency, out.Basis = cov.PricedCoverage, cov.Currency, cov.Basis
	return out
}

func eqS(p *string, want string) bool { return p != nil && *p == want }

func TestSpend_TotalsAndEnvelope(t *testing.T) {
	f := newSpendFixture(t)
	r := f.spend(t, "group_by=model")
	tt := r.Totals
	if tt.Requests != 5 || tt.PromptTokens != 1200050 || tt.CompletionTokens != 120005 || tt.CacheReadTokens != 400000 || tt.CacheCreationTokens != 100000 {
		t.Fatalf("totals %+v", tt)
	}
	// e1 349.5 + e2 130 (old gpt-4 price) + e3 260 (new price); e4 unpriced; e5 refused = 0.
	if !eqS(tt.CostCents, "739.5000") || tt.UnpricedRequests != 1 {
		t.Fatalf("cost %v unpriced %d", tt.CostCents, tt.UnpricedRequests)
	}
	if r.PricedCoverage != 0.75 || r.Currency != "USD" || r.Basis != "list_price_estimate" {
		t.Fatalf("envelope %+v", r)
	}
}

func TestSpend_GroupBy(t *testing.T) {
	f := newSpendFixture(t)
	type want struct {
		reqs int64
		cost string // "" = null
	}
	cases := map[string]map[string]want{
		"user":          {f.alice: {2, "479.5000"}, f.bob: {3, "260.0000"}},
		"api_key":       {f.keyA: {2, "479.5000"}, f.keyB: {3, "260.0000"}},
		"model":         {"claude-s": {2, "349.5000"}, "gpt-4o": {2, "390.0000"}, "llama": {1, ""}},
		"provider":      {"anthropic": {2, "349.5000"}, "openai": {2, "390.0000"}, "vllm": {1, ""}},
		"backend_class": {"external_api": {4, "739.5000"}, "self_hosted": {1, ""}},
		"alias":         {"smart": {1, "349.5000"}, "": {4, "390.0000"}},
		"route":         {"code": {1, "349.5000"}, "": {4, "390.0000"}},
		"day":           {"2026-02-10": {1, "349.5000"}, "2026-02-11": {1, "130.0000"}, "2026-03-05": {2, "260.0000"}, "2026-03-06": {1, "0.0000"}},
	}
	for gb, exp := range cases {
		t.Run(gb, func(t *testing.T) {
			r := f.spend(t, "group_by="+gb)
			if len(r.Rows) != len(exp) {
				t.Fatalf("rows %d want %d: %+v", len(r.Rows), len(exp), r.Rows)
			}
			for _, row := range r.Rows {
				w, ok := exp[row.Key]
				if !ok {
					t.Fatalf("unexpected key %q", row.Key)
				}
				if row.Requests != w.reqs {
					t.Fatalf("key %q requests %d want %d", row.Key, row.Requests, w.reqs)
				}
				if w.cost == "" && row.CostCents != nil || w.cost != "" && !eqS(row.CostCents, w.cost) {
					t.Fatalf("key %q cost %v want %q", row.Key, row.CostCents, w.cost)
				}
			}
		})
	}
	r := f.spend(t, "group_by=user")
	if r.Rows[0].Label != "Alice" || r.Rows[1].Label != "Bob" || r.Rows[1].UnpricedRequests != 1 {
		t.Fatalf("labels/order %+v", r.Rows)
	}
	if r := f.spend(t, "group_by=api_key"); r.Rows[0].Label != "alice-key" {
		t.Fatalf("api key label %+v", r.Rows)
	}
	d := f.spend(t, "group_by=day")
	if d.Rows[0].Key != "2026-02-10" || d.Rows[3].Key != "2026-03-06" {
		t.Fatalf("day order %+v", d.Rows)
	}
}

func TestSpend_FiltersLimitAndValidation(t *testing.T) {
	f := newSpendFixture(t)
	if r := f.spend(t, "group_by=model&since=2026-03-01T00:00:00Z"); r.Totals.Requests != 3 || !eqS(r.Totals.CostCents, "260.0000") {
		t.Fatalf("since: %+v", r.Totals)
	}
	if r := f.spend(t, "group_by=model&until=2026-02-28T00:00:00Z"); r.Totals.Requests != 2 {
		t.Fatalf("until: %+v", r.Totals)
	}
	if r := f.spend(t, "group_by=model&user_id="+f.alice); r.Totals.Requests != 2 {
		t.Fatalf("user filter: %+v", r.Totals)
	}
	if r := f.spend(t, "group_by=model&denial_code=any"); r.Totals.Requests != 1 || !eqS(r.Totals.CostCents, "0.0000") || r.PricedCoverage != 1 {
		t.Fatalf("refused only: %+v cov=%v", r.Totals, r.PricedCoverage)
	}
	if r := f.spend(t, "group_by=model&limit=1"); len(r.Rows) != 1 || r.Totals.Requests != 5 {
		t.Fatalf("limit must trim rows but not totals: %+v", r)
	}
	if r := f.spend(t, "group_by=model&limit=99999"); len(r.Rows) != 3 {
		t.Fatalf("limit clamp: %+v", r)
	}
	for _, q := range []string{"", "group_by=bogus", "group_by=model&limit=0", "group_by=model&since=nope", "group_by=model&user_id=x"} {
		if w := f.do(http.MethodGet, "/api/v1/usage/spend?"+q, ""); w.Code != 400 {
			t.Fatalf("%q: want 400 got %d", q, w.Code)
		}
	}
	// A retroactive price correction changes the estimate (query-time cost).
	if _, err := f.pool.Exec(context.Background(), `UPDATE model_prices SET effective_to = NULL, effective_from = '2000-01-01' WHERE model_pattern = 'gpt-4*' AND input_per_mtok = 20`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM model_prices WHERE model_pattern = 'gpt-4*' AND input_per_mtok = 10`); err != nil {
		t.Fatal(err)
	}
	if r := f.spend(t, "group_by=model"); !eqS(r.Totals.CostCents, "869.5000") { // e2 now 260
		t.Fatalf("retroactive: %v", r.Totals.CostCents)
	}
}

func TestListUsageAndExport_CostColumns(t *testing.T) {
	f := newSpendFixture(t)
	w := f.do(http.MethodGet, "/api/v1/usage?limit=50", "")
	var resp struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Events) != 5 {
		t.Fatalf("list: %v %s", err, w.Body.String())
	}
	got := map[string]any{}
	for _, e := range resp.Events {
		if _, ok := e["cost_cents"]; !ok {
			t.Fatalf("cost_cents key must always be present: %v", e)
		}
		if _, ok := e["cache_read_tokens"]; !ok {
			t.Fatalf("cache_read_tokens key must always be present")
		}
		got[e["model"].(string)+"/"+e["request_id"].(string)] = e["cost_cents"]
	}
	if got["claude-s/e1"] != "349.5000" || got["gpt-4o/e2"] != "130.0000" || got["gpt-4o/e3"] != "260.0000" || got["llama/e4"] != nil || got["claude-s/e5"] != "0.0000" {
		t.Fatalf("event costs %v", got)
	}

	w = f.do(http.MethodGet, "/api/v1/usage/export", "")
	recs, err := csv.NewReader(w.Body).ReadAll()
	if err != nil || len(recs) != 6 {
		t.Fatalf("csv: %v %d", err, len(recs))
	}
	h := recs[0]
	n := len(h)
	if h[n-3] != "cache_read_tokens" || h[n-2] != "cache_creation_tokens" || h[n-1] != "cost_cents" {
		t.Fatalf("csv header %v", h)
	}
	for _, rec := range recs[1:] {
		if rec[1] == "e1" && (rec[n-3] != "400000" || rec[n-2] != "100000" || rec[n-1] != "349.5000") {
			t.Fatalf("e1 row %v", rec)
		}
		if rec[1] == "e4" && rec[n-1] != "" {
			t.Fatalf("unpriced csv cost must be blank: %v", rec)
		}
	}
}

func TestModelPrices_CRUDAndAudit(t *testing.T) {
	f := newSpendFixture(t)
	ctx := context.Background()

	w := f.do(http.MethodPut, "/api/v1/model-prices", `{"provider":"vllm","model_pattern":"llama*","input_per_mtok":0.2,"output_per_mtok":"0.4","effective_from":"2026-01-01T00:00:00Z","notes":"amortised"}`)
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Row map[string]any `json:"row"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := created.Row["id"].(string)
	if created.Row["source"] != "admin" || created.Row["input_per_mtok"].(float64) != 0.2 || created.Row["effective_to"] != nil || created.Row["cache_read_per_mtok"] != nil {
		t.Fatalf("created row %v", created.Row)
	}
	// Now the previously unpriced e4 is priced.
	if r := f.spend(t, "group_by=model"); r.Totals.UnpricedRequests != 0 {
		t.Fatalf("admin row must price llama: %+v", r.Totals)
	}

	// Replace: closes the old row, inserts a new one.
	w = f.do(http.MethodPut, "/api/v1/model-prices", `{"id":"`+id+`","provider":"vllm","model_pattern":"llama*","input_per_mtok":0.5,"output_per_mtok":1}`)
	if w.Code != 200 {
		t.Fatalf("replace: %d %s", w.Code, w.Body.String())
	}
	var repl struct {
		Row map[string]any `json:"row"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &repl)
	newID := repl.Row["id"].(string)
	if newID == id {
		t.Fatal("replace must insert a new row")
	}
	var closed, openFrom *time.Time
	var oldIn string
	if err := f.pool.QueryRow(ctx, `SELECT effective_to, input_per_mtok::text FROM model_prices WHERE id=$1`, id).Scan(&closed, &oldIn); err != nil || closed == nil || oldIn != "0.200000" {
		t.Fatalf("old row must be closed and unmutated: %v %v %v", closed, oldIn, err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT effective_from FROM model_prices WHERE id=$1`, newID).Scan(&openFrom); err != nil || !openFrom.Equal(*closed) {
		t.Fatalf("new row starts when old closed: %v %v %v", openFrom, closed, err)
	}
	// Replacing a retired row conflicts; unknown id is 404.
	if w := f.do(http.MethodPut, "/api/v1/model-prices", `{"id":"`+id+`","provider":"vllm","model_pattern":"l*","input_per_mtok":1,"output_per_mtok":1}`); w.Code != 409 {
		t.Fatalf("retired replace: %d", w.Code)
	}
	if w := f.do(http.MethodPut, "/api/v1/model-prices", `{"id":"00000000-0000-0000-0000-000000000000","provider":"vllm","model_pattern":"l*","input_per_mtok":1,"output_per_mtok":1}`); w.Code != 404 {
		t.Fatalf("unknown id: %d", w.Code)
	}

	// Delete retires; seed rows retire the same way.
	var seedID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model_prices (provider, model_pattern, input_per_mtok, output_per_mtok, source) VALUES ('openai','o-*',1,1,'seed') RETURNING id::text`).Scan(&seedID); err != nil {
		t.Fatal(err)
	}
	for _, rid := range []string{newID, seedID} {
		if w := f.do(http.MethodDelete, "/api/v1/model-prices/"+rid, ""); w.Code != 200 {
			t.Fatalf("delete %s: %d %s", rid, w.Code, w.Body.String())
		}
		if w := f.do(http.MethodDelete, "/api/v1/model-prices/"+rid, ""); w.Code != 409 {
			t.Fatalf("double delete: %d", w.Code)
		}
	}
	if w := f.do(http.MethodDelete, "/api/v1/model-prices/not-a-uuid", ""); w.Code != 400 {
		t.Fatalf("bad id: %d", w.Code)
	}
	if w := f.do(http.MethodDelete, "/api/v1/model-prices/00000000-0000-0000-0000-000000000000", ""); w.Code != 404 {
		t.Fatalf("missing: %d", w.Code)
	}

	// Audit: created, replaced (before+after), retired x2, all by the admin.
	rows, err := f.pool.Query(ctx, `SELECT detail->>'action', jsonb_typeof(detail->'before') = 'object', jsonb_typeof(detail->'after') = 'object', actor_id::text
		FROM activity_events WHERE event_type='model_price.changed' ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var a string
		var hasB, hasA bool
		var actor *string
		if err := rows.Scan(&a, &hasB, &hasA, &actor); err != nil {
			t.Fatal(err)
		}
		if actor == nil || *actor != f.adminID || !hasA || (a == "created") == hasB {
			t.Fatalf("audit row %s before=%v after=%v actor=%v", a, hasB, hasA, actor)
		}
		actions = append(actions, a)
	}
	if strings.Join(actions, ",") != "created,replaced,retired,retired" {
		t.Fatalf("audit actions %v", actions)
	}

	// List.
	w = f.do(http.MethodGet, "/api/v1/model-prices", "")
	var list struct {
		Rows []map[string]any `json:"rows"`
		Seed any              `json:"seed_generated_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Rows) != 3+2+1 {
		t.Fatalf("list: %v rows=%d", err, len(list.Rows))
	}
}

func TestModelPrices_Validation(t *testing.T) {
	f := newSpendFixture(t)
	for name, body := range map[string]string{
		"bad provider":  `{"provider":"gemini","model_pattern":"x","input_per_mtok":1,"output_per_mtok":1}`,
		"bad glob":      `{"provider":"openai","model_pattern":"[","input_per_mtok":1,"output_per_mtok":1}`,
		"empty glob":    `{"provider":"openai","model_pattern":"","input_per_mtok":1,"output_per_mtok":1}`,
		"negative":      `{"provider":"openai","model_pattern":"x","input_per_mtok":-1,"output_per_mtok":1}`,
		"negative out":  `{"provider":"openai","model_pattern":"x","input_per_mtok":1,"output_per_mtok":-0.1}`,
		"negative cr":   `{"provider":"openai","model_pattern":"x","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":-1}`,
		"missing rates": `{"provider":"openai","model_pattern":"x"}`,
		"non-numeric":   `{"provider":"openai","model_pattern":"x","input_per_mtok":"abc","output_per_mtok":1}`,
		"bad id":        `{"id":"zzz","provider":"openai","model_pattern":"x","input_per_mtok":1,"output_per_mtok":1}`,
		"not json":      `nope`,
	} {
		if w := f.do(http.MethodPut, "/api/v1/model-prices", body); w.Code != 400 {
			t.Errorf("%s: want 400 got %d (%s)", name, w.Code, w.Body.String())
		}
	}
	var n int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM activity_events WHERE event_type='model_price.changed'`).Scan(&n)
	if n != 0 {
		t.Fatalf("rejected requests must not audit, got %d", n)
	}
}

func TestSeed_IdempotentAndPreservesAdminRows(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	data := []byte(`{"generated_at":"2026-01-01","sources":["x"],"entries":[
		{"provider":"anthropic","model_pattern":"claude-*","input_per_mtok":3,"output_per_mtok":15,"cache_read_per_mtok":0.3},
		{"provider":"bedrock","model_pattern":"anthropic.*","region":"us-east-1","input_per_mtok":3,"output_per_mtok":15,"notes":"n"}]}`)
	if _, err := pool.Exec(ctx, `INSERT INTO model_prices (provider, model_pattern, input_per_mtok, output_per_mtok, source) VALUES ('mock','m',9,9,'admin')`); err != nil {
		t.Fatal(err)
	}
	n, err := pricing.SeedFrom(ctx, pool, data)
	if err != nil || n != 2 {
		t.Fatalf("first seed: %d %v", n, err)
	}
	if n, err := pricing.SeedFrom(ctx, pool, data); err != nil || n != 0 {
		t.Fatalf("second seed must be a no-op: %d %v", n, err)
	}
	// Retiring seed rows does not trigger a re-seed.
	if _, err := pool.Exec(ctx, `UPDATE model_prices SET effective_to = now() WHERE source = 'seed'`); err != nil {
		t.Fatal(err)
	}
	if n, _ := pricing.SeedFrom(ctx, pool, data); n != 0 {
		t.Fatalf("retired seed rows must not re-seed: %d", n)
	}
	var seeds, admins int
	_ = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE source='seed'), count(*) FILTER (WHERE source='admin') FROM model_prices`).Scan(&seeds, &admins)
	if seeds != 2 || admins != 1 {
		t.Fatalf("seed=%d admin=%d", seeds, admins)
	}
	var region *string
	var notes *string
	var cr *string
	if err := pool.QueryRow(ctx, `SELECT region, notes FROM model_prices WHERE provider='bedrock'`).Scan(&region, &notes); err != nil || region == nil || *region != "us-east-1" || notes == nil {
		t.Fatalf("bedrock row: %v %v %v", region, notes, err)
	}
	if err := pool.QueryRow(ctx, `SELECT cache_read_per_mtok::text FROM model_prices WHERE provider='anthropic'`).Scan(&cr); err != nil || cr == nil || *cr != "0.300000" {
		t.Fatalf("cache_read: %v %v", cr, err)
	}
	// Invalid seed is rejected whole (nothing inserted).
	pool2 := setupTestDB(t)
	if _, err := pricing.SeedFrom(ctx, pool2, []byte(`{"entries":[{"provider":"nope","model_pattern":"x","input_per_mtok":1,"output_per_mtok":1}]}`)); err == nil {
		t.Fatal("invalid seed must error")
	}
	// The embedded file seeds cleanly.
	if _, err := pricing.Seed(ctx, pool2); err != nil {
		t.Fatalf("embedded seed: %v", err)
	}
}

func TestSpend_TimeByBackendClass(t *testing.T) {
	f := newSpendFixture(t)
	type tcRow struct {
		Key, Label, BackendClass string
		Requests                 int64
		CostCents                *string
	}
	get := func(q string) []tcRow {
		t.Helper()
		w := f.do(http.MethodGet, "/api/v1/usage/spend?"+q, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body.String())
		}
		var raw struct {
			Rows []map[string]any `json:"rows"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		var out []tcRow
		for _, m := range raw.Rows {
			r := tcRow{Key: m["key"].(string), Label: m["label"].(string), BackendClass: m["backend_class"].(string), Requests: int64(m["requests"].(float64))}
			if s, ok := m["cost_cents"].(string); ok {
				r.CostCents = &s
			}
			out = append(out, r)
		}
		return out
	}
	rows := get("group_by=day,backend_class")
	want := []struct {
		key, class string
		reqs       int64
		cost       string
	}{
		{"2026-02-10T00:00:00Z", "external_api", 1, "349.5000"},
		{"2026-02-11T00:00:00Z", "external_api", 1, "130.0000"},
		{"2026-03-05T00:00:00Z", "external_api", 1, "260.0000"},
		{"2026-03-05T00:00:00Z", "self_hosted", 1, ""},
		{"2026-03-06T00:00:00Z", "external_api", 1, "0.0000"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows %+v", rows)
	}
	for i, w := range want {
		r := rows[i]
		if r.Key != w.key || r.Label != w.key || r.BackendClass != w.class || r.Requests != w.reqs {
			t.Fatalf("row %d = %+v want %+v", i, r, w)
		}
		if w.cost == "" && r.CostCents != nil || w.cost != "" && !eqS(r.CostCents, w.cost) {
			t.Fatalf("row %d cost %v want %q", i, r.CostCents, w.cost)
		}
	}
	// limit counts buckets: newest 2 days = 03-05 (two class rows) + 03-06.
	rows = get("group_by=day,backend_class&limit=2")
	if len(rows) != 3 || rows[0].Key != "2026-03-05T00:00:00Z" || rows[2].Key != "2026-03-06T00:00:00Z" {
		t.Fatalf("limit rows %+v", rows)
	}
	// hour view: both 2026-03-05 events fall in the 12:00 bucket.
	rows = get("group_by=hour,backend_class&since=2026-03-05T00:00:00Z&until=2026-03-05T23:00:00Z")
	if len(rows) != 2 || rows[0].Key != "2026-03-05T12:00:00Z" || rows[1].BackendClass != "self_hosted" {
		t.Fatalf("hour rows %+v", rows)
	}
	if w := f.do(http.MethodGet, "/api/v1/usage/spend?group_by=day,user", ""); w.Code != 400 {
		t.Fatalf("bad 2d group: %d", w.Code)
	}
	if w := f.do(http.MethodGet, "/api/v1/usage/spend?group_by=day,backend_class&limit=0", ""); w.Code != 400 {
		t.Fatalf("limit 0: %d", w.Code)
	}
}
