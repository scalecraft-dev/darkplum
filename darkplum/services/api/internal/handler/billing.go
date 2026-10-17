// Vendor usage/cost reconciliation: a separate BILLING key per vendor
// (Anthropic Admin API key, OpenAI org admin key; Bedrock uses the api's AWS
// credential chain), install/verify/remove routes mirroring providers.go's
// hygiene, a refresh trigger, and the estimate-vs-reported comparison.
//
//	POST   /api/v1/providers/:vendor/billing-key/verify
//	PUT    /api/v1/providers/:vendor/billing-key
//	DELETE /api/v1/providers/:vendor/billing-key
//	POST   /api/v1/usage/reconciliation/refresh
//	GET    /api/v1/usage/reconciliation
//
// The key is never logged, echoed, wrapped into an error, or stored in
// Postgres: it goes to the cluster Secret darkplum-billing-keys only.
package handler

import (
	"context"
	"log/slog"
	"math/big"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
	"github.com/scalecraft/darkplum/api/internal/billing"
	"github.com/scalecraft/darkplum/api/internal/kube"
	"github.com/scalecraft/darkplum/api/internal/pricing"
)

// BillingHandler serves the billing-key and reconciliation routes.
type BillingHandler struct {
	db            *pgxpool.Pool
	activity      *activity.Recorder
	svc           *billing.Service
	kube          kube.BillingSecrets
	secureCookies bool
	locks         map[string]*sync.Mutex
}

func NewBillingHandler(pool *pgxpool.Pool, rec *activity.Recorder, svc *billing.Service, secrets kube.BillingSecrets, secureCookies bool) *BillingHandler {
	return &BillingHandler{db: pool, activity: rec, svc: svc, kube: secrets, secureCookies: secureCookies,
		locks: map[string]*sync.Mutex{vendorAnthropic: {}, vendorOpenAI: {}}}
}

func (h *BillingHandler) vendorParam(c *gin.Context) (string, bool) {
	v := c.Param("vendor")
	if !validVendor(v) { // bedrock has no key
		errJSON(c, http.StatusNotFound, "unknown_vendor", "unknown vendor")
		return "", false
	}
	return v, true
}

func (h *BillingHandler) secureTransport(c *gin.Context) bool {
	if !keyInstallTransportOK(h.secureCookies, c.Request.Host) {
		errJSON(c, http.StatusForbidden, "insecure_transport", insecureTransportMessage)
		return false
	}
	return true
}

type billingKeyRequest struct {
	Key string `json:"key"`
}

// readKey binds and validates the {key} body (format before any network call).
func (h *BillingHandler) readKey(c *gin.Context) (string, bool) {
	var req billingKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errJSON(c, http.StatusBadRequest, "invalid_request", invalidJSONMessage)
		return "", false
	}
	if !validKeyFormat(req.Key) {
		errJSON(c, http.StatusBadRequest, "invalid_key_format", "key must be 20-512 characters of letters, digits, underscore, or hyphen")
		return "", false
	}
	return req.Key, true
}

// verifyKey checks the key against the vendor's usage endpoint; on failure
// it writes the closed 422 and reports ok=false.
func (h *BillingHandler) verifyKey(c *gin.Context, vendor, key string) (billing.Sample, bool) {
	client := h.svc.Vendors[vendor]
	if client == nil {
		errJSON(c, http.StatusServiceUnavailable, "unavailable", "usage reconciliation is not enabled")
		return billing.Sample{}, false
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	sample, err := client.Verify(ctx, key, h.svc.Now())
	if err != nil {
		e := billing.AsError(err)
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": apiErrorBody{
			Code: e.Kind, Message: billing.MessageFor(vendor, e), VendorHTTPStatus: e.HTTPStatus}})
		return billing.Sample{}, false
	}
	return sample, true
}

// PostVerify handles POST /api/v1/providers/:vendor/billing-key/verify.
func (h *BillingHandler) PostVerify(c *gin.Context) {
	vendor, ok := h.vendorParam(c)
	if !ok {
		return
	}
	key, ok := h.readKey(c)
	if !ok || !h.secureTransport(c) {
		return
	}
	sample, ok := h.verifyKey(c, vendor, key)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"state": "ok", "sample": sample})
}

func (h *BillingHandler) lock(c *gin.Context, vendor string) (func(), bool) {
	mu := h.locks[vendor]
	if !mu.TryLock() {
		vendorBusyJSON(c)
		return nil, false
	}
	return mu.Unlock, true
}

func (h *BillingHandler) actor(c *gin.Context) *string {
	if uid := c.GetString("user_id"); uid != "" {
		return &uid
	}
	return nil
}

// PutKey handles PUT /api/v1/providers/:vendor/billing-key: verify, store in
// the Secret, audit, then pull in the background with the verified key (the
// mounted file lags the Secret by up to a kubelet sync).
func (h *BillingHandler) PutKey(c *gin.Context) {
	vendor, ok := h.vendorParam(c)
	if !ok {
		return
	}
	key, ok := h.readKey(c)
	if !ok || !h.secureTransport(c) {
		return
	}
	if !h.kube.Available() {
		kubeErrorJSON(c, kube.ErrNotInCluster)
		return
	}
	release, ok := h.lock(c, vendor)
	if !ok {
		return
	}
	defer release()
	ctx := c.Request.Context()

	if _, ok := h.verifyKey(c, vendor, key); !ok {
		return
	}
	wasConfigured := false
	_ = h.db.QueryRow(ctx, `SELECT configured FROM vendor_billing_status WHERE vendor = $1`, vendor).Scan(&wasConfigured)

	if err := h.kube.SetNamedSecretKey(ctx, kube.BillingKeysSecret, billing.KeyFile(vendor), []byte(key)); err != nil {
		kubeErrorJSON(c, err)
		return
	}
	h.svc.MarkInstalled(vendor)
	last4 := billing.Last4(key)
	if err := h.svc.Store.SetStatus(ctx, vendor, true, billing.StatePulling, nil, &last4, false); err != nil {
		slog.ErrorContext(ctx, "billing key: status write failed", "vendor", vendor)
	}

	evtType := activity.ProviderBillingKeyInstalled
	if wasConfigured {
		evtType = activity.ProviderBillingKeyRotated
	}
	evt := activity.FromContext(c, evtType)
	evt.TargetType = activity.StrPtr("vendor_billing")
	evt.Detail = map[string]any{"vendor": vendor, "key_last4": last4}
	h.activity.Record(ctx, evt)

	go h.svc.PullVendor(context.Background(), vendor, key)

	c.JSON(http.StatusOK, gin.H{"state": "installed"})
}

// DeleteKey handles DELETE /api/v1/providers/:vendor/billing-key. Already
// pulled report rows are kept.
func (h *BillingHandler) DeleteKey(c *gin.Context) {
	vendor, ok := h.vendorParam(c)
	if !ok {
		return
	}
	if !h.kube.Available() {
		kubeErrorJSON(c, kube.ErrNotInCluster)
		return
	}
	release, ok := h.lock(c, vendor)
	if !ok {
		return
	}
	defer release()
	ctx := c.Request.Context()

	if err := h.kube.RemoveNamedSecretKey(ctx, kube.BillingKeysSecret, billing.KeyFile(vendor)); err != nil {
		kubeErrorJSON(c, err)
		return
	}
	h.svc.MarkRemoved(vendor)
	if err := h.svc.Store.SetStatus(ctx, vendor, false, billing.StateNotConfigure, nil, nil, false); err != nil {
		slog.ErrorContext(ctx, "billing key: status write failed", "vendor", vendor)
	}
	evt := activity.FromContext(c, activity.ProviderBillingKeyRemoved)
	evt.TargetType = activity.StrPtr("vendor_billing")
	evt.Detail = map[string]any{"vendor": vendor}
	h.activity.Record(ctx, evt)

	c.JSON(http.StatusOK, gin.H{"state": billing.StateNotConfigure})
}

// PostRefresh handles POST /api/v1/usage/reconciliation/refresh.
func (h *BillingHandler) PostRefresh(c *gin.Context) {
	h.svc.Kick()
	c.JSON(http.StatusAccepted, gin.H{"state": "pulling"})
}

// billingSummary is the providers[].billing block of the admin models list.
type billingSummary struct {
	Configured   bool       `json:"configured"`
	State        string     `json:"state"`
	LastPulledAt *time.Time `json:"last_pulled_at"`
}

func billingSummaries(ctx context.Context, pool *pgxpool.Pool) map[string]billingSummary {
	out := map[string]billingSummary{}
	sts, err := (&billing.Store{DB: pool}).Statuses(ctx)
	if err != nil {
		slog.WarnContext(ctx, "list admin models: read billing status failed")
		return out
	}
	for _, s := range sts {
		out[s.Vendor] = billingSummary{s.Configured, s.State, s.LastPulledAt}
	}
	return out
}

type reconTokens struct {
	Input  int64 `json:"input"`
	Cached int64 `json:"cached"`
	Output int64 `json:"output"`
}

type reconRow struct {
	Key            string       `json:"key"`
	Label          string       `json:"label"`
	Vendor         *string      `json:"vendor"`
	EstimatedCents *string      `json:"estimated_cents"`
	ReportedCents  *string      `json:"reported_cents"`
	ReportedTokens *reconTokens `json:"reported_tokens"`
	DeltaCents     *string      `json:"delta_cents"`

	est      *spendAgg
	rep      *big.Rat
	vendor   string
	sortCost *big.Rat
}

// GetReconciliation handles GET /api/v1/usage/reconciliation.
//
// Estimates are the Layer-1 engine over inference_events for the three
// billable vendors (anthropic, openai, bedrock), ALL purposes (routing calls
// are billed too), in [since, until]. Reported figures are the vendor
// reports for the UTC days covering [since, until]. delta = reported -
// estimated, only when both exist. For tokens: input = uncached + cache
// creation, cached = cache reads (prompt total = input + cached).
func (h *BillingHandler) GetReconciliation(c *gin.Context) {
	gb := c.DefaultQuery("group_by", "day")
	if gb != "day" && gb != "model" && gb != "vendor" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "group_by must be one of day, model, vendor"})
		return
	}
	until := time.Now().UTC()
	since := until.AddDate(0, 0, -30)
	var err error
	if v := c.Query("since"); v != "" {
		if since, err = time.Parse(time.RFC3339, v); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since must be RFC3339"})
			return
		}
	}
	if v := c.Query("until"); v != "" {
		if until, err = time.Parse(time.RFC3339, v); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "until must be RFC3339"})
			return
		}
	}
	ctx := c.Request.Context()

	statuses, err := (&billing.Store{DB: h.db}).Statuses(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read billing status failed"})
		return
	}

	grp := spendGroups[gb]
	if gb == "vendor" {
		grp = spendGroups["provider"]
	}
	where := `WHERE ie.provider IN ('anthropic','openai','bedrock') AND ie.created_at >= $1 AND ie.created_at <= $2`
	est, _, err := aggregateSpend(ctx, h.db, grp, where, []any{since, until})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	rows := map[string]*reconRow{}
	get := func(key, label string) *reconRow {
		r := rows[key]
		if r == nil {
			r = &reconRow{Key: key, Label: label}
			rows[key] = r
		}
		return r
	}
	for k, a := range est {
		r := get(k, a.label)
		r.est = a
		if !a.mixed && a.provider != "" {
			r.vendor = a.provider
		}
	}

	var candidates []string
	if gb == "model" {
		cr, err := h.db.Query(ctx, `SELECT DISTINCT model FROM inference_events WHERE provider='bedrock' AND created_at >= $1 AND created_at <= $2`, since, until)
		if err == nil {
			for cr.Next() {
				var m string
				if cr.Scan(&m) == nil {
					candidates = append(candidates, m)
				}
			}
			cr.Close()
		}
	}

	cr, err := h.db.Query(ctx, `SELECT vendor, day::text, description, model, cost_cents::text
		FROM vendor_cost_reports WHERE day BETWEEN $1::date AND $2::date`, since.UTC(), until.UTC())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read vendor cost reports failed"})
		return
	}
	for cr.Next() {
		var vendor, day, desc, cents string
		var model *string
		if err := cr.Scan(&vendor, &day, &desc, &model, &cents); err != nil {
			cr.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan vendor cost reports failed"})
			return
		}
		amt, ok := new(big.Rat).SetString(cents)
		if !ok {
			continue
		}
		var key, label string
		switch gb {
		case "day":
			key, label = day, day
		case "vendor":
			key, label = vendor, vendor
		default:
			switch {
			case model != nil:
				key, label = *model, *model
			case vendor == billing.Bedrock:
				if m := billing.MapBedrockUsageType(desc, candidates); m != nil {
					key, label = *m, *m
				} else {
					key, label = desc, desc+" (Bedrock usage type, unmapped)"
				}
			default:
				key, label = desc, desc
			}
		}
		r := get(key, label)
		if r.rep == nil {
			r.rep = new(big.Rat)
		}
		r.rep.Add(r.rep, amt)
		if gb != "day" && r.vendor == "" {
			r.vendor = vendor
		}
	}
	cr.Close()

	tokens := map[string]*reconTokens{}
	ur, err := h.db.Query(ctx, `SELECT vendor, day::text, model, SUM(uncached_input)::bigint, SUM(cache_read)::bigint, SUM(cache_creation)::bigint, SUM(output)::bigint
		FROM vendor_usage_reports WHERE day BETWEEN $1::date AND $2::date GROUP BY vendor, day, model`, since.UTC(), until.UTC())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read vendor usage reports failed"})
		return
	}
	for ur.Next() {
		var vendor, day, model string
		var unc, cread, ccreate, out int64
		if err := ur.Scan(&vendor, &day, &model, &unc, &cread, &ccreate, &out); err != nil {
			ur.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan vendor usage reports failed"})
			return
		}
		key := map[string]string{"day": day, "vendor": vendor, "model": model}[gb]
		t := tokens[key]
		if t == nil {
			t = &reconTokens{}
			tokens[key] = t
		}
		t.Input += unc + ccreate
		t.Cached += cread
		t.Output += out
		if _, ok := rows[key]; !ok {
			r := get(key, key)
			if gb != "day" {
				r.vendor = vendor
			}
		}
	}
	ur.Close()

	out := make([]*reconRow, 0, len(rows))
	for k, r := range rows {
		if r.est != nil {
			r.EstimatedCents = r.est.cents()
		}
		if r.rep != nil {
			s := pricing.FormatCents(r.rep)
			r.ReportedCents = &s
		}
		if r.EstimatedCents != nil && r.ReportedCents != nil {
			ev, _ := new(big.Rat).SetString(*r.EstimatedCents)
			d := pricing.FormatCents(new(big.Rat).Sub(r.rep, ev))
			r.DeltaCents = &d
		}
		r.ReportedTokens = tokens[k]
		if r.vendor != "" {
			v := r.vendor
			r.Vendor = &v
		}
		r.sortCost = new(big.Rat)
		if r.rep != nil {
			r.sortCost.Set(r.rep)
		} else if r.est != nil && r.est.cost != nil {
			r.sortCost.Set(r.est.cost)
		}
		out = append(out, r)
	}
	if gb == "day" {
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	} else {
		sort.Slice(out, func(i, j int) bool {
			if d := out[i].sortCost.Cmp(out[j].sortCost); d != 0 {
				return d > 0
			}
			return out[i].Key < out[j].Key
		})
	}
	if len(out) > 1000 {
		out = out[:1000]
	}

	c.JSON(http.StatusOK, gin.H{"vendors": statuses, "rows": out, "currency": "USD"})
}
