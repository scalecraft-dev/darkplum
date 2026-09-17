-- Admin/auth activity log. Low volume, retained indefinitely.
CREATE TABLE IF NOT EXISTS activity_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type  TEXT NOT NULL,
    actor_id    UUID REFERENCES users(id) ON DELETE SET NULL,
    target_type TEXT,
    target_id   UUID,
    detail      JSONB,
    ip_address  INET,
    request_id  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_activity_events_actor   ON activity_events (actor_id, created_at DESC);
CREATE INDEX idx_activity_events_type    ON activity_events (event_type, created_at DESC);
CREATE INDEX idx_activity_events_created ON activity_events (created_at DESC);

-- Inference usage log. Partitioned by month.
CREATE TABLE IF NOT EXISTS inference_events (
    id                UUID NOT NULL DEFAULT gen_random_uuid(),
    request_id        TEXT NOT NULL,
    api_key_id        UUID,
    user_id           UUID,
    model             TEXT NOT NULL,
    status_code       INT NOT NULL,
    latency_ms        INT NOT NULL,
    prompt_tokens     INT,
    completion_tokens INT,
    stream            BOOLEAN NOT NULL DEFAULT false,
    ip_address        INET,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE inference_events_2026_05 PARTITION OF inference_events
    FOR VALUES FROM ('2026-05-01') TO ('2026-06-01');
CREATE TABLE inference_events_2026_06 PARTITION OF inference_events
    FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');
CREATE TABLE inference_events_2026_07 PARTITION OF inference_events
    FOR VALUES FROM ('2026-07-01') TO ('2026-08-01');
CREATE TABLE inference_events_2026_08 PARTITION OF inference_events
    FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');
CREATE TABLE inference_events_2026_09 PARTITION OF inference_events
    FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE inference_events_2026_10 PARTITION OF inference_events
    FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');

-- Default partition catches rows outside the explicitly defined ranges.
-- This prevents INSERT failures when named partitions haven't been created yet
-- for a future month. New monthly partitions should be created proactively;
-- when they are, matching rows are automatically moved out of the default.
CREATE TABLE inference_events_default PARTITION OF inference_events DEFAULT;

CREATE INDEX idx_inference_events_user    ON inference_events (user_id, created_at DESC);
CREATE INDEX idx_inference_events_key     ON inference_events (api_key_id, created_at DESC);
CREATE INDEX idx_inference_events_created ON inference_events (created_at DESC);
CREATE INDEX idx_inference_events_request ON inference_events (request_id);
