CREATE TABLE IF NOT EXISTS service_keys (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    prefix     TEXT NOT NULL,
    key_hash   TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_service_keys_hash ON service_keys(key_hash) WHERE revoked_at IS NULL;
