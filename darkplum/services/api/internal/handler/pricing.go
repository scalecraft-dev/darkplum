package handler

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
	"github.com/scalecraft/darkplum/api/internal/pricing"
)

// PricingHandler serves the admin price book (model_prices).
type PricingHandler struct {
	db       *pgxpool.Pool
	activity *activity.Recorder
}

// NewPricingHandler returns a PricingHandler.
func NewPricingHandler(db *pgxpool.Pool, rec *activity.Recorder) *PricingHandler {
	return &PricingHandler{db: db, activity: rec}
}

// modelPriceJSON is the wire shape of one price row. Rates are JSON numbers
// (USD per 1M tokens); timestamps RFC3339.
type modelPriceJSON struct {
	ID            string       `json:"id"`
	Provider      string       `json:"provider"`
	ModelPattern  string       `json:"model_pattern"`
	Region        *string      `json:"region"`
	InputPerMtok  json.Number  `json:"input_per_mtok"`
	OutputPerMtok json.Number  `json:"output_per_mtok"`
	CacheRead     *json.Number `json:"cache_read_per_mtok"`
	CacheWrite    *json.Number `json:"cache_write_per_mtok"`
	EffectiveFrom time.Time    `json:"effective_from"`
	EffectiveTo   *time.Time   `json:"effective_to"`
	Source        string       `json:"source"`
	Notes         *string      `json:"notes"`
}

func ratNumPtr(r *big.Rat) *json.Number {
	if r == nil {
		return nil
	}
	n := json.Number(pricing.FormatRate(r))
	return &n
}

func priceToJSON(p pricing.Price) modelPriceJSON {
	return modelPriceJSON{
		ID: p.ID, Provider: p.Provider, ModelPattern: p.ModelPattern, Region: p.Region,
		InputPerMtok: json.Number(pricing.FormatRate(p.Input)), OutputPerMtok: json.Number(pricing.FormatRate(p.Output)),
		CacheRead: ratNumPtr(p.CacheRead), CacheWrite: ratNumPtr(p.CacheWrite),
		EffectiveFrom: p.EffectiveFrom, EffectiveTo: p.EffectiveTo, Source: p.Source, Notes: p.Notes,
	}
}

// ListPrices handles GET /api/v1/model-prices.
func (h *PricingHandler) ListPrices(c *gin.Context) {
	prices, err := pricing.LoadAll(c.Request.Context(), h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list model prices failed"})
		return
	}
	rows := make([]modelPriceJSON, 0, len(prices))
	for _, p := range prices {
		rows = append(rows, priceToJSON(p))
	}
	c.JSON(http.StatusOK, gin.H{"rows": rows, "seed_generated_at": pricing.SeedGeneratedAt()})
}

type putPriceBody struct {
	ID            *string      `json:"id"`
	Provider      string       `json:"provider"`
	ModelPattern  string       `json:"model_pattern"`
	Region        *string      `json:"region"`
	InputPerMtok  *json.Number `json:"input_per_mtok"`
	OutputPerMtok *json.Number `json:"output_per_mtok"`
	CacheRead     *json.Number `json:"cache_read_per_mtok"`
	CacheWrite    *json.Number `json:"cache_write_per_mtok"`
	EffectiveFrom *time.Time   `json:"effective_from"`
	Notes         *string      `json:"notes"`
}

func parseOptRate(n *json.Number) (*big.Rat, error) {
	if n == nil {
		return nil, nil
	}
	return pricing.ParseRate(n.String())
}

// PutPrice handles PUT /api/v1/model-prices. Without id it inserts a new
// admin row; with id it closes that row (effective_to = now) and inserts a
// replacement, never mutating history.
func (h *PricingHandler) PutPrice(c *gin.Context) {
	var body putPriceBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}
	bad := func(msg string) { c.JSON(http.StatusBadRequest, gin.H{"error": msg}) }

	if !pricing.ValidProvider(body.Provider) {
		bad("provider must be one of anthropic, openai, bedrock, vllm, lmstudio, mock")
		return
	}
	if err := pricing.ValidatePattern(body.ModelPattern); err != nil {
		bad(err.Error())
		return
	}
	if body.Region != nil && *body.Region == "" {
		body.Region = nil
	}
	if body.InputPerMtok == nil || body.OutputPerMtok == nil {
		bad("input_per_mtok and output_per_mtok are required")
		return
	}
	in, err := pricing.ParseRate(body.InputPerMtok.String())
	if err != nil {
		bad("input_per_mtok " + err.Error())
		return
	}
	out, err := pricing.ParseRate(body.OutputPerMtok.String())
	if err != nil {
		bad("output_per_mtok " + err.Error())
		return
	}
	cr, err := parseOptRate(body.CacheRead)
	if err != nil {
		bad("cache_read_per_mtok " + err.Error())
		return
	}
	cw, err := parseOptRate(body.CacheWrite)
	if err != nil {
		bad("cache_write_per_mtok " + err.Error())
		return
	}
	if body.ID != nil && !isValidUUID(*body.ID) {
		bad("invalid id")
		return
	}

	ctx := c.Request.Context()
	tx, err := h.db.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save model price failed"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var before *modelPriceJSON
	action := "created"
	if body.ID != nil {
		old, err := pricing.ScanPrice(tx.QueryRow(ctx,
			`SELECT `+pricing.Columns+` FROM model_prices WHERE id = $1 FOR UPDATE`, *body.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model price not found"})
			return
		} else if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "save model price failed"})
			return
		}
		if old.EffectiveTo != nil && !old.EffectiveTo.After(time.Now()) {
			c.JSON(http.StatusConflict, gin.H{"error": "model price is already retired"})
			return
		}
		if _, err := tx.Exec(ctx, `UPDATE model_prices SET effective_to = now() WHERE id = $1`, *body.ID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "save model price failed"})
			return
		}
		old2, err := pricing.ScanPrice(tx.QueryRow(ctx, `SELECT `+pricing.Columns+` FROM model_prices WHERE id = $1`, *body.ID))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "save model price failed"})
			return
		}
		j := priceToJSON(old2)
		before = &j
		action = "replaced"
	}

	var actor *string
	if s := c.GetString("user_id"); s != "" {
		actor = &s
	}
	// A replacement starts exactly when the old row closed (now()), so there is
	// no gap or overlap; a plain create may be backdated explicitly.
	from := body.EffectiveFrom
	if body.ID != nil {
		from = nil
	}
	created, err := pricing.ScanPrice(tx.QueryRow(ctx, `
		INSERT INTO model_prices
			(provider, model_pattern, region, input_per_mtok, output_per_mtok,
			 cache_read_per_mtok, cache_write_per_mtok, effective_from, source, notes, updated_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,COALESCE($8, now()),'admin',$9,$10)
		RETURNING `+pricing.Columns,
		body.Provider, body.ModelPattern, body.Region, pricing.FormatRate(in), pricing.FormatRate(out),
		ratStrPtr(cr), ratStrPtr(cw), from, body.Notes, actor))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save model price failed"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save model price failed"})
		return
	}

	after := priceToJSON(created)
	h.audit(c, ctx, action, created.ID, before, &after)
	c.JSON(http.StatusOK, gin.H{"row": after})
}

func ratStrPtr(r *big.Rat) *string {
	if r == nil {
		return nil
	}
	s := pricing.FormatRate(r)
	return &s
}

// DeletePrice handles DELETE /api/v1/model-prices/:id: retires a row by
// setting effective_to = now (history is kept; seed rows retire the same way).
func (h *PricingHandler) DeletePrice(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	ctx := c.Request.Context()
	tx, err := h.db.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "retire model price failed"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	old, err := pricing.ScanPrice(tx.QueryRow(ctx, `SELECT `+pricing.Columns+` FROM model_prices WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "model price not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "retire model price failed"})
		return
	}
	if old.EffectiveTo != nil && !old.EffectiveTo.After(time.Now()) {
		c.JSON(http.StatusConflict, gin.H{"error": "model price is already retired"})
		return
	}
	closed, err := pricing.ScanPrice(tx.QueryRow(ctx,
		`UPDATE model_prices SET effective_to = now() WHERE id = $1 RETURNING `+pricing.Columns, id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "retire model price failed"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "retire model price failed"})
		return
	}
	b, a := priceToJSON(old), priceToJSON(closed)
	h.audit(c, ctx, "retired", id, &b, &a)
	c.JSON(http.StatusOK, gin.H{"row": a})
}

func (h *PricingHandler) audit(c *gin.Context, ctx context.Context, action, id string, before, after *modelPriceJSON) {
	evt := activity.FromContext(c, activity.ModelPriceChanged)
	evt.TargetType = activity.StrPtr("model_price")
	evt.TargetID = &id
	evt.Detail = map[string]any{"action": action, "before": before, "after": after}
	h.activity.Record(ctx, evt)
}
