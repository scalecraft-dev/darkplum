package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
	"github.com/scalecraft/darkplum/api/internal/billing"
	"github.com/scalecraft/darkplum/api/internal/kube"
)

const billingSentinel = "sk-ant-admin01-SENTINELBILLINGKEY0123456789"

type fakeBillingSecrets struct {
	mu        sync.Mutex
	available bool
	setErr    error
	data      map[string]string // secret/name -> value
}

func (f *fakeBillingSecrets) Available() bool { return f.available }
func (f *fakeBillingSecrets) SetNamedSecretKey(_ context.Context, secret, name string, v []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.data[secret+"/"+name] = string(v)
	return nil
}
func (f *fakeBillingSecrets) RemoveNamedSecretKey(_ context.Context, secret, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, secret+"/"+name)
	return nil
}

type billingFixture struct {
	pool    *pgxpool.Pool
	r       *gin.Engine
	secrets *fakeBillingSecrets
	vendor  *httptest.Server
	status  *int // forced status for usage endpoint
	mu      sync.Mutex
	svc     *billing.Service
}

func newBillingFixture(t *testing.T, secure bool) *billingFixture {
	t.Helper()
	pool := setupTestDB(t)
	f := &billingFixture{pool: pool, secrets: &fakeBillingSecrets{available: true, data: map[string]string{}}}
	forced := 0
	f.status = &forced
	f.vendor = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		st := *f.status
		f.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			fmt.Fprintf(w, `{"error":"echo %s"}`, billingSentinel) // hostile vendor
			return
		}
		today := time.Now().UTC().Format("2006-01-02")
		switch r.URL.Path {
		case "/v1/organizations/usage_report/messages":
			fmt.Fprintf(w, `{"data":[{"starting_at":"%sT00:00:00Z","results":[{"uncached_input_tokens":10,"cache_read_input_tokens":2,"cache_creation_input_tokens":3,"output_tokens":4,"model":"claude-s"}]}],"has_more":false}`, today)
		case "/v1/organizations/cost_report":
			fmt.Fprintf(w, `{"data":[{"starting_at":"%sT00:00:00Z","results":[{"amount":"7.25","description":"d","model":"claude-s"}]}],"has_more":false}`, today)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.vendor.Close)
	f.svc = billing.NewService(&billing.Store{DB: pool}, billing.KeyReader{Dir: t.TempDir()},
		map[string]billing.Vendor{
			billing.Anthropic: billing.NewAnthropicClient(f.vendor.URL, nil),
			billing.OpenAI:    billing.NewOpenAIClient(f.vendor.URL, nil),
		}, nil, nil)

	var adminID string
	if err := pool.QueryRow(context.Background(), `INSERT INTO users (email, name, role) VALUES ('admin@x.io','Admin','admin') RETURNING id::text`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	h := NewBillingHandler(pool, activity.NewRecorder(pool), f.svc, f.secrets, secure)
	gin.SetMode(gin.TestMode)
	f.r = gin.New()
	f.r.Use(func(c *gin.Context) { c.Set("user_id", adminID); c.Next() })
	f.r.POST("/api/v1/providers/:vendor/billing-key/verify", h.PostVerify)
	f.r.PUT("/api/v1/providers/:vendor/billing-key", h.PutKey)
	f.r.DELETE("/api/v1/providers/:vendor/billing-key", h.DeleteKey)
	f.r.GET("/api/v1/usage/reconciliation", h.GetReconciliation)
	f.r.POST("/api/v1/usage/reconciliation/refresh", h.PostRefresh)
	return f
}

func (f *billingFixture) force(code int) { f.mu.Lock(); *f.status = code; f.mu.Unlock() }

func (f *billingFixture) do(method, url, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

func (f *billingFixture) events(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT event_type || ' ' || COALESCE(detail::text,'') FROM activity_events WHERE event_type LIKE 'provider.billing_key_%' ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func TestBillingKey_VerifyShapeAndClassification(t *testing.T) {
	logs := captureLogs(t)
	f := newBillingFixture(t, true)
	body := `{"key":"` + billingSentinel + `"}`
	w := f.do("POST", "/api/v1/providers/anthropic/billing-key/verify", body)
	var ok struct {
		State  string `json:"state"`
		Sample struct {
			Day     string `json:"day"`
			Buckets int    `json:"buckets"`
		} `json:"sample"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ok) != nil || ok.State != "ok" || ok.Sample.Buckets != 1 || len(ok.Sample.Day) != 10 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(f.secrets.data) != 0 {
		t.Fatal("verify must store nothing")
	}

	for _, tc := range []struct {
		status, http int
		code         string
	}{{401, 422, "auth_failed"}, {403, 422, "wrong_key_type"}, {500, 422, "vendor_error"}} {
		f.force(tc.status)
		w := f.do("POST", "/api/v1/providers/anthropic/billing-key/verify", body)
		var e struct {
			Error struct {
				Code             string `json:"code"`
				Message          string `json:"message"`
				VendorHTTPStatus int    `json:"vendor_http_status"`
			} `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != tc.http || e.Error.Code != tc.code || e.Error.VendorHTTPStatus != tc.status || e.Error.Message == "" {
			t.Fatalf("%d: %d %s", tc.status, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "SENTINEL") {
			t.Fatalf("key echoed: %s", w.Body.String())
		}
	}
	// inference-key prefix + 401 -> wrong_key_type with the closed text
	f.force(401)
	w = f.do("POST", "/api/v1/providers/anthropic/billing-key/verify", `{"key":"sk-ant-api03-`+strings.Repeat("q", 30)+`"}`)
	var ik struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &ik)
	if ik.Error.Code != "wrong_key_type" || ik.Error.Message != "This looks like an inference key; the usage reports need an Admin API key (sk-ant-admin01-…) created under Settings > Admin keys." {
		t.Fatalf("%s", w.Body.String())
	}
	if strings.Contains(logs.String(), "SENTINEL") {
		t.Fatal("key logged")
	}

	if w := f.do("POST", "/api/v1/providers/anthropic/billing-key/verify", `{"key":"short"}`); w.Code != 400 {
		t.Fatalf("format: %d", w.Code)
	}
	if w := f.do("POST", "/api/v1/providers/bedrock/billing-key/verify", body); w.Code != 404 {
		t.Fatalf("bedrock has no key: %d", w.Code)
	}
	if w := f.do("POST", "/api/v1/providers/anthropic/billing-key/verify", `not json`); w.Code != 400 || strings.Contains(w.Body.String(), "'") {
		t.Fatalf("bad json: %d %s", w.Code, w.Body.String())
	}
}

func TestBillingKey_InsecureTransportRefused(t *testing.T) {
	f := newBillingFixture(t, false)
	req := httptest.NewRequest("PUT", "http://darkplum.example.com/api/v1/providers/anthropic/billing-key", strings.NewReader(`{"key":"`+billingSentinel+`"}`))
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "insecure_transport") || len(f.secrets.data) != 0 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestBillingKey_InstallRotateRemove(t *testing.T) {
	logs := captureLogs(t)
	f := newBillingFixture(t, true)
	body := `{"key":"` + billingSentinel + `"}`

	// failed verify stores nothing
	f.force(403)
	if w := f.do("PUT", "/api/v1/providers/anthropic/billing-key", body); w.Code != 422 || len(f.secrets.data) != 0 || len(f.events(t)) != 0 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	f.force(0)

	// kube unavailable -> 503, nothing stored
	f.secrets.available = false
	if w := f.do("PUT", "/api/v1/providers/anthropic/billing-key", body); w.Code != 503 || !strings.Contains(w.Body.String(), "not_in_cluster") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	f.secrets.available = true
	f.secrets.setErr = kube.ErrForbidden
	if w := f.do("PUT", "/api/v1/providers/anthropic/billing-key", body); w.Code != 503 || !strings.Contains(w.Body.String(), `"forbidden"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	f.secrets.setErr = nil

	w := f.do("PUT", "/api/v1/providers/anthropic/billing-key", body)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"state":"installed"}` {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if f.secrets.data["darkplum-billing-keys/anthropic-billing-key"] != billingSentinel {
		t.Fatalf("secret not written: %v", f.secrets.data)
	}
	// the kicked pull used the verified key and stored rows
	waitDB(t, f.pool, `SELECT count(*) FROM vendor_cost_reports WHERE vendor='anthropic'`, 1)
	waitDB(t, f.pool, `SELECT count(*) FROM vendor_billing_status WHERE vendor='anthropic' AND state='ok' AND configured AND key_last4='6789'`, 1)

	// rotate
	body2 := `{"key":"sk-ant-admin01-ROTATEDKEY000000000000WXYZ"}`
	if w := f.do("PUT", "/api/v1/providers/anthropic/billing-key", body2); w.Code != 200 {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	// DELETE with no body
	w = f.do("DELETE", "/api/v1/providers/anthropic/billing-key", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"state":"not_configured"}` || len(f.secrets.data) != 0 {
		t.Fatalf("%d %s %v", w.Code, w.Body.String(), f.secrets.data)
	}
	waitDB(t, f.pool, `SELECT count(*) FROM vendor_billing_status WHERE vendor='anthropic' AND NOT configured AND state='not_configured' AND key_last4 IS NULL`, 1)

	ev := f.events(t)
	if len(ev) != 3 || !strings.HasPrefix(ev[0], "provider.billing_key_installed") || !strings.Contains(ev[0], `"key_last4": "6789"`) && !strings.Contains(ev[0], `"key_last4":"6789"`) ||
		!strings.HasPrefix(ev[1], "provider.billing_key_rotated") || !strings.HasPrefix(ev[2], "provider.billing_key_removed") {
		t.Fatalf("audit %v", ev)
	}
	// the key appears nowhere durable: not in audit, status, report tables, logs, or any response
	var leaks int
	_ = f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM activity_events WHERE detail::text LIKE '%SENTINEL%' OR detail::text LIKE '%ROTATEDKEY%')
		+ (SELECT count(*) FROM vendor_billing_status WHERE key_last4 LIKE '%SENTINEL%')`).Scan(&leaks)
	if leaks != 0 || strings.Contains(logs.String(), "SENTINEL") || strings.Contains(logs.String(), "ROTATEDKEY") {
		t.Fatalf("key leaked: db=%d logs=%s", leaks, logs.String())
	}
	if w := f.do("POST", "/api/v1/usage/reconciliation/refresh", ""); w.Code != 202 {
		t.Fatalf("refresh %d", w.Code)
	}
}

func waitDB(t *testing.T, pool *pgxpool.Pool, q string, want int) {
	t.Helper()
	var n int
	for i := 0; i < 300; i++ {
		_ = pool.QueryRow(context.Background(), q).Scan(&n)
		if n == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s = %d, want %d", q, n, want)
}

func TestReconciliation_JoinsEstimatesAndReports(t *testing.T) {
	f := newBillingFixture(t, true)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	epoch := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	px := `INSERT INTO model_prices (provider, model_pattern, input_per_mtok, output_per_mtok, effective_from, source) VALUES ($1,$2,$3,$4,$5,'admin')`
	exec(px, "anthropic", "claude-*", 3, 15, epoch)
	exec(px, "bedrock", "*anthropic.claude-haiku-4-5*", 1, 5, epoch)
	exec(px, "openai", "gpt-4*", 10, 30, epoch)
	ev := `INSERT INTO inference_events (request_id, provider, model, status_code, latency_ms, prompt_tokens, completion_tokens, created_at) VALUES ($1,$2,$3,200,5,$4,$5,$6)`
	d := func(day int) time.Time { return time.Date(2026, 10, day, 12, 0, 0, 0, time.UTC) }
	// claude-s: 1M in (300c) + 1M out (1500c) on 10-01 = 1800c; openai gpt-4o 100k in = 100c on 10-02;
	// bedrock haiku 1M in = 100c on 10-02; vllm is not a billable vendor and must be excluded.
	exec(ev, "a", "anthropic", "claude-s", 1000000, 1000000, d(1))
	exec(ev, "b", "openai", "gpt-4o", 100000, 0, d(2))
	exec(ev, "c", "bedrock", "us.anthropic.claude-haiku-4-5-20251001-v1:0", 1000000, 0, d(2))
	exec(ev, "d", "vllm", "llama", 5, 5, d(2))
	exec(ev, "e", "anthropic", "claude-unpriced-by-nothing", 10, 10, d(3)) // priced by claude-*, fine
	exec(`INSERT INTO vendor_cost_reports (vendor, day, description, model, cost_cents) VALUES
		('anthropic','2026-10-01','Sonnet in','claude-s',1750.5),
		('anthropic','2026-10-01','Web search',NULL,10),
		('openai','2026-10-02','gpt-4o, input','gpt-4o',120),
		('bedrock','2026-10-02','USE1-ClaudeHaiku45-input-tokens',NULL,95.5),
		('bedrock','2026-10-02','USE1-Mystery-input-tokens',NULL,4)`)
	exec(`INSERT INTO vendor_usage_reports (vendor, day, model, api_key_id, uncached_input, cache_read, cache_creation, output) VALUES
		('anthropic','2026-10-01','claude-s','k1',900000,50000,50000,1000000),
		('anthropic','2026-10-01','claude-s','k2',1,1,1,1)`)
	exec(`UPDATE vendor_billing_status SET configured=true, state='ok', key_last4='abcd', last_pulled_at=now() WHERE vendor='anthropic'`)

	type row struct {
		Key, Label     string
		Vendor         *string
		EstimatedCents *string        `json:"estimated_cents"`
		ReportedCents  *string        `json:"reported_cents"`
		DeltaCents     *string        `json:"delta_cents"`
		ReportedTokens map[string]int `json:"reported_tokens"`
	}
	get := func(q string) (rows []row, vendors []map[string]any) {
		t.Helper()
		w := f.do("GET", "/api/v1/usage/reconciliation?"+q, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body.String())
		}
		var out struct {
			Vendors  []map[string]any `json:"vendors"`
			Rows     []row            `json:"rows"`
			Currency string           `json:"currency"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Currency != "USD" {
			t.Fatalf("decode: %v", err)
		}
		return out.Rows, out.Vendors
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	win := "since=2026-09-20T00:00:00Z&until=2026-10-10T00:00:00Z"

	rows, vendors := get("group_by=day&" + win)
	if len(vendors) != 3 || vendors[0]["vendor"] != "anthropic" || vendors[0]["configured"] != true || vendors[0]["state"] != "ok" || vendors[0]["key_last4"] != "abcd" || vendors[2]["state"] != "not_configured" {
		t.Fatalf("vendors %v", vendors)
	}
	if len(rows) != 3 || rows[0].Key != "2026-10-01" || rows[1].Key != "2026-10-02" || rows[2].Key != "2026-10-03" {
		t.Fatalf("day rows %+v", rows)
	}
	// 10-01: est 1800, reported 1760.5, delta -39.5; tokens summed over both usage rows
	if str(rows[0].EstimatedCents) != "1800.0000" || str(rows[0].ReportedCents) != "1760.5000" || str(rows[0].DeltaCents) != "-39.5000" ||
		rows[0].ReportedTokens["input"] != 900001+50001 || rows[0].ReportedTokens["cached"] != 50001 || rows[0].ReportedTokens["output"] != 1000001 {
		t.Fatalf("10-01 %+v", rows[0])
	}
	// 10-02: est 100 (openai) + 100 (bedrock) = 200, vllm excluded; reported 120+95.5+4 = 219.5
	if str(rows[1].EstimatedCents) != "200.0000" || str(rows[1].ReportedCents) != "219.5000" || str(rows[1].DeltaCents) != "19.5000" || rows[1].ReportedTokens != nil || rows[1].Vendor != nil {
		t.Fatalf("10-02 %+v", rows[1])
	}
	// 10-03: estimate only -> reported/delta null
	if rows[2].ReportedCents != nil || rows[2].DeltaCents != nil || rows[2].EstimatedCents == nil {
		t.Fatalf("10-03 %+v", rows[2])
	}

	rows, _ = get("group_by=vendor&" + win)
	byKey := map[string]row{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	if len(rows) != 3 || str(byKey["openai"].DeltaCents) != "20.0000" || str(byKey["openai"].EstimatedCents) != "100.0000" || byKey["vllm"].Key != "" {
		t.Fatalf("vendor rows %+v", rows)
	}
	if v := byKey["anthropic"].Vendor; v == nil || *v != "anthropic" {
		t.Fatal("vendor field")
	}

	rows, _ = get("group_by=model&" + win)
	byKey = map[string]row{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	if r := byKey["claude-s"]; str(r.EstimatedCents) != "1800.0000" || str(r.ReportedCents) != "1750.5000" || str(r.DeltaCents) != "-49.5000" {
		t.Fatalf("claude-s %+v", r)
	}
	// anthropic report row without a model keys by its description
	if r := byKey["Web search"]; str(r.ReportedCents) != "10.0000" || r.EstimatedCents != nil {
		t.Fatalf("web search %+v", r)
	}
	// bedrock usage type maps onto the model id the gateway recorded
	if r := byKey["us.anthropic.claude-haiku-4-5-20251001-v1:0"]; str(r.EstimatedCents) != "100.0000" || str(r.ReportedCents) != "95.5000" || str(r.DeltaCents) != "-4.5000" {
		t.Fatalf("bedrock mapped %+v", r)
	}
	if r := byKey["USE1-Mystery-input-tokens"]; !strings.Contains(r.Label, "unmapped") || str(r.ReportedCents) != "4.0000" || r.EstimatedCents != nil {
		t.Fatalf("bedrock unmapped %+v", r)
	}

	// window filter: 10-02 only
	rows, _ = get("group_by=day&since=2026-10-02T00:00:00Z&until=2026-10-02T23:00:00Z")
	if len(rows) != 1 || rows[0].Key != "2026-10-02" {
		t.Fatalf("window %+v", rows)
	}
	if w := f.do("GET", "/api/v1/usage/reconciliation?group_by=user", ""); w.Code != 400 {
		t.Fatalf("bad group_by %d", w.Code)
	}
	if w := f.do("GET", "/api/v1/usage/reconciliation?since=yesterday", ""); w.Code != 400 {
		t.Fatalf("bad since %d", w.Code)
	}

	// providers[].billing summary source
	s := billingSummaries(ctx, f.pool)
	if !s["anthropic"].Configured || s["anthropic"].State != "ok" || s["openai"].Configured {
		t.Fatalf("summaries %+v", s)
	}
}
