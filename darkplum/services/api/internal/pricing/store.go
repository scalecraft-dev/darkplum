package pricing

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed seed/prices.json
var seedJSON []byte

// seedFile is the schema of seed/prices.json.
type seedFile struct {
	GeneratedAt string      `json:"generated_at"`
	Sources     []string    `json:"sources"`
	Entries     []seedEntry `json:"entries"`
}

type seedEntry struct {
	Provider          string   `json:"provider"`
	ModelPattern      string   `json:"model_pattern"`
	Region            *string  `json:"region,omitempty"`
	InputPerMtok      *float64 `json:"input_per_mtok"`
	OutputPerMtok     *float64 `json:"output_per_mtok"`
	CacheReadPerMtok  *float64 `json:"cache_read_per_mtok,omitempty"`
	CacheWritePerMtok *float64 `json:"cache_write_per_mtok,omitempty"`
	Notes             *string  `json:"notes,omitempty"`
}

// seedEffectiveFrom backdates seed rows so list prices also price events that
// predate the install of this feature (estimates are computed at query time).
var seedEffectiveFrom = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

func parseSeed(data []byte) (*seedFile, error) {
	var f seedFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("decode seed prices: %w", err)
	}
	for i, e := range f.Entries {
		if !ValidProvider(e.Provider) {
			return nil, fmt.Errorf("seed entry %d: invalid provider %q", i, e.Provider)
		}
		if err := ValidatePattern(e.ModelPattern); err != nil {
			return nil, fmt.Errorf("seed entry %d: %w", i, err)
		}
		if e.InputPerMtok == nil || e.OutputPerMtok == nil {
			return nil, fmt.Errorf("seed entry %d (%s/%s): input_per_mtok and output_per_mtok are required", i, e.Provider, e.ModelPattern)
		}
		for _, v := range []*float64{e.InputPerMtok, e.OutputPerMtok, e.CacheReadPerMtok, e.CacheWritePerMtok} {
			if v != nil && (*v < 0 || *v >= 1000000) {
				return nil, fmt.Errorf("seed entry %d (%s/%s): rate out of range", i, e.Provider, e.ModelPattern)
			}
		}
	}
	return &f, nil
}

// SeedGeneratedAt returns the embedded seed file's generated_at, or nil when
// the file carries none (placeholder).
func SeedGeneratedAt() *string {
	f, err := parseSeed(seedJSON)
	if err != nil || f.GeneratedAt == "" {
		return nil
	}
	return &f.GeneratedAt
}

// Seed inserts the embedded list prices with source='seed' when model_prices
// has no seed rows yet. Idempotent and replica-safe (advisory lock); never
// touches admin rows; a later seed version is deliberately not re-applied.
// Returns the number of rows inserted.
func Seed(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	return SeedFrom(ctx, pool, seedJSON)
}

// SeedFrom is Seed with explicit JSON (tests).
func SeedFrom(ctx context.Context, pool *pgxpool.Pool, data []byte) (int, error) {
	f, err := parseSeed(data)
	if err != nil {
		return 0, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin seed tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('model_prices_seed'))`); err != nil {
		return 0, fmt.Errorf("seed lock: %w", err)
	}
	var have bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_prices WHERE source = 'seed')`).Scan(&have); err != nil {
		return 0, fmt.Errorf("check seed rows: %w", err)
	}
	if have || len(f.Entries) == 0 {
		return 0, nil
	}
	for _, e := range f.Entries {
		if _, err := tx.Exec(ctx, `
			INSERT INTO model_prices
				(provider, model_pattern, region, input_per_mtok, output_per_mtok,
				 cache_read_per_mtok, cache_write_per_mtok, effective_from, source, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'seed',$9)`,
			e.Provider, e.ModelPattern, e.Region, *e.InputPerMtok, *e.OutputPerMtok,
			e.CacheReadPerMtok, e.CacheWritePerMtok, seedEffectiveFrom, e.Notes,
		); err != nil {
			return 0, fmt.Errorf("insert seed price %s/%s: %w", e.Provider, e.ModelPattern, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit seed: %w", err)
	}
	return len(f.Entries), nil
}

// Columns is the SELECT list scanRow expects (numerics as text so they parse
// exactly into big.Rat).
const Columns = `id::text, provider, model_pattern, region,
	input_per_mtok::text, output_per_mtok::text, cache_read_per_mtok::text, cache_write_per_mtok::text,
	effective_from, effective_to, source, notes`

// ScanPrice scans one row selected with Columns.
func ScanPrice(row pgx.Row) (Price, error) {
	var (
		p       Price
		in, out string
		cr, cw  *string
	)
	if err := row.Scan(&p.ID, &p.Provider, &p.ModelPattern, &p.Region, &in, &out, &cr, &cw,
		&p.EffectiveFrom, &p.EffectiveTo, &p.Source, &p.Notes); err != nil {
		return Price{}, err
	}
	var ok bool
	if p.Input, ok = new(big.Rat).SetString(in); !ok {
		return Price{}, fmt.Errorf("bad input_per_mtok %q", in)
	}
	if p.Output, ok = new(big.Rat).SetString(out); !ok {
		return Price{}, fmt.Errorf("bad output_per_mtok %q", out)
	}
	if cr != nil {
		if p.CacheRead, ok = new(big.Rat).SetString(*cr); !ok {
			return Price{}, fmt.Errorf("bad cache_read_per_mtok %q", *cr)
		}
	}
	if cw != nil {
		if p.CacheWrite, ok = new(big.Rat).SetString(*cw); !ok {
			return Price{}, fmt.Errorf("bad cache_write_per_mtok %q", *cw)
		}
	}
	return p, nil
}

// Querier is satisfied by *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// LoadAll reads the whole price book (small by construction) so a request
// can price any number of events from one in-memory snapshot.
func LoadAll(ctx context.Context, q Querier) ([]Price, error) {
	rows, err := q.Query(ctx, `SELECT `+Columns+` FROM model_prices ORDER BY provider, effective_from, id`)
	if err != nil {
		return nil, fmt.Errorf("load model_prices: %w", err)
	}
	defer rows.Close()
	var out []Price
	for rows.Next() {
		p, err := ScanPrice(rows)
		if err != nil {
			return nil, fmt.Errorf("scan model_prices: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Boundaries returns the sorted, distinct instants at which the effective
// price set can change; between two adjacent boundaries every event shares
// the same price, so aggregating by bucket stays exact.
func Boundaries(prices []Price) []time.Time {
	seen := map[int64]time.Time{}
	for _, p := range prices {
		seen[p.EffectiveFrom.UnixMicro()] = p.EffectiveFrom
		if p.EffectiveTo != nil {
			seen[p.EffectiveTo.UnixMicro()] = *p.EffectiveTo
		}
	}
	out := make([]time.Time, 0, len(seen))
	for _, t := range seen {
		out = append(out, t)
	}
	sortTimes(out)
	return out
}
