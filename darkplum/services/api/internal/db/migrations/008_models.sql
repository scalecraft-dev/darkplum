CREATE TABLE IF NOT EXISTS models (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id     TEXT        NOT NULL UNIQUE,
    provider     TEXT        NOT NULL DEFAULT 'vllm',
    display_name TEXT,
    enabled      BOOLEAN     NOT NULL DEFAULT false,
    discovered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_models_enabled  ON models (enabled) WHERE enabled = true;
CREATE INDEX IF NOT EXISTS idx_models_provider ON models (provider);
