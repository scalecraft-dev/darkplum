-- Estimated cost attribution from Darkplum's own records.
--
-- cache_read_tokens / cache_creation_tokens: prompt-cache token counts the
-- gateway records per request. NULL = the backend reported no usage at all;
-- 0 = usage reported without cache fields. Added on the partitioned parent
-- with no default, so it is a catalog-only change (no table rewrite).
ALTER TABLE inference_events ADD COLUMN IF NOT EXISTS cache_read_tokens INT;
ALTER TABLE inference_events ADD COLUMN IF NOT EXISTS cache_creation_tokens INT;

-- model_prices is a versioned price book (USD per 1M tokens). Rows are never
-- mutated to change a price: a correction closes the old row (effective_to)
-- and inserts a new one, so history stays reproducible. Cost is computed at
-- query time from these rows, never stored on inference_events.
-- model_pattern is a path.Match glob (same semantics as the gateway's model
-- filters); region NULL means "any region".
CREATE TABLE IF NOT EXISTS model_prices (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider             TEXT NOT NULL CHECK (provider IN ('anthropic','openai','bedrock','vllm','lmstudio','mock')),
    model_pattern        TEXT NOT NULL,
    region               TEXT,
    input_per_mtok       NUMERIC(12,6) NOT NULL,
    output_per_mtok      NUMERIC(12,6) NOT NULL,
    cache_read_per_mtok  NUMERIC(12,6),
    cache_write_per_mtok NUMERIC(12,6),
    effective_from       TIMESTAMPTZ NOT NULL DEFAULT now(),
    effective_to         TIMESTAMPTZ,
    source               TEXT NOT NULL CHECK (source IN ('seed','admin')),
    notes                TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by           UUID
);

CREATE INDEX IF NOT EXISTS idx_model_prices_provider_from ON model_prices (provider, effective_from);
