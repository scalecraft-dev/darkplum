package billing

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/api/internal/pricing"
)

// Store persists vendor reports and the per-vendor status row.
type Store struct{ DB *pgxpool.Pool }

// Status is one vendor_billing_status row.
type Status struct {
	Vendor        string     `json:"vendor"`
	Configured    bool       `json:"configured"`
	State         string     `json:"state"`
	LastPulledAt  *time.Time `json:"last_pulled_at"`
	LastErrorKind *string    `json:"-"`
	KeyLast4      *string    `json:"key_last4"`
}

// Statuses returns the three vendor rows (seeded by migration 022).
func (s *Store) Statuses(ctx context.Context) ([]Status, error) {
	rows, err := s.DB.Query(ctx, `SELECT vendor, configured, state, last_pulled_at, last_error_kind, key_last4
		FROM vendor_billing_status ORDER BY CASE vendor WHEN 'anthropic' THEN 1 WHEN 'openai' THEN 2 ELSE 3 END`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Status
	for rows.Next() {
		var st Status
		if err := rows.Scan(&st.Vendor, &st.Configured, &st.State, &st.LastPulledAt, &st.LastErrorKind, &st.KeyLast4); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// Configured reports the stored configured flag (owned by install/remove).
func (s *Store) Configured(ctx context.Context, vendor string) (bool, error) {
	var c bool
	err := s.DB.QueryRow(ctx, `SELECT configured FROM vendor_billing_status WHERE vendor = $1`, vendor).Scan(&c)
	return c, err
}

// SetInstalled is the install handler's write: configured, fingerprint, and
// state pulling until the first pull finishes.
func (s *Store) SetInstalled(ctx context.Context, vendor, last4 string) error {
	_, err := s.DB.Exec(ctx, `UPDATE vendor_billing_status SET configured = true, key_last4 = $2,
		state = 'pulling', last_error_kind = NULL, updated_at = now() WHERE vendor = $1`, vendor, last4)
	return err
}

// SetRemoved is the remove handler's write.
func (s *Store) SetRemoved(ctx context.Context, vendor string) error {
	_, err := s.DB.Exec(ctx, `UPDATE vendor_billing_status SET configured = false, key_last4 = NULL,
		state = 'not_configured', last_error_kind = NULL, updated_at = now() WHERE vendor = $1`, vendor)
	return err
}

// SetPullState is the poller's only write: state, last_error_kind and
// (pulled=true) last_pulled_at. It never touches configured or key_last4.
func (s *Store) SetPullState(ctx context.Context, vendor, state string, errKind *string, pulled bool) error {
	_, err := s.DB.Exec(ctx, `UPDATE vendor_billing_status SET state = $2, last_error_kind = $3,
		last_pulled_at = CASE WHEN $4::boolean THEN now() ELSE last_pulled_at END, updated_at = now()
		WHERE vendor = $1`, vendor, state, errKind, pulled)
	return err
}

// SetFileKeyConfigured records a key found only as a mounted file (installed
// out of band, no handler ran). It only ever sets configured=true.
func (s *Store) SetFileKeyConfigured(ctx context.Context, vendor, last4 string) error {
	_, err := s.DB.Exec(ctx, `UPDATE vendor_billing_status SET configured = true, key_last4 = $2, updated_at = now()
		WHERE vendor = $1`, vendor, last4)
	return err
}

// SetBedrockConfigured: Bedrock has no install handler (AWS credential
// chain), so its configured flag is the one the poller owns.
func (s *Store) SetBedrockConfigured(ctx context.Context, configured bool) error {
	_, err := s.DB.Exec(ctx, `UPDATE vendor_billing_status SET configured = $1, updated_at = now() WHERE vendor = 'bedrock'`, configured)
	return err
}

// Upsert stores a pull idempotently: re-running the same pull rewrites the
// same rows (unique on the NULL-safe key) and bumps pulled_at.
func (s *Store) Upsert(ctx context.Context, vendor string, usage []UsageRow, costs []CostRow) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	b := &pgx.Batch{}
	for _, r := range usage {
		b.Queue(`INSERT INTO vendor_usage_reports
			(vendor, day, model, workspace_id, api_key_id, uncached_input, cache_read, cache_creation, output, pulled_at)
			VALUES ($1, $2::date, $3, $4, $5, $6, $7, $8, $9, now())
			ON CONFLICT (vendor, day, model, COALESCE(workspace_id, ''), COALESCE(api_key_id, '')) DO UPDATE SET
				uncached_input = EXCLUDED.uncached_input, cache_read = EXCLUDED.cache_read,
				cache_creation = EXCLUDED.cache_creation, output = EXCLUDED.output, pulled_at = now()`,
			vendor, r.Day, r.Model, r.WorkspaceID, r.APIKeyID, r.UncachedInput, r.CacheRead, r.CacheCreation, r.OutputToken)
	}
	for _, r := range costs {
		b.Queue(`INSERT INTO vendor_cost_reports
			(vendor, day, description, model, workspace_id, cost_cents, pulled_at)
			VALUES ($1, $2::date, $3, $4, $5, $6::numeric, now())
			ON CONFLICT (vendor, day, description, COALESCE(workspace_id, '')) DO UPDATE SET
				model = EXCLUDED.model, cost_cents = EXCLUDED.cost_cents, pulled_at = now()`,
			vendor, r.Day, r.Description, r.Model, r.WorkspaceID, pricing.FormatCents(r.Cents))
	}
	br := tx.SendBatch(ctx, b)
	for i := 0; i < b.Len(); i++ {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return err
		}
	}
	if err := br.Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
