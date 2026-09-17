-- Model routing: backend class, aliases, org-wide residency policy, and the
-- requested-vs-resolved model split in the inference log.
--
-- The gateway reads models.enabled, model_aliases and routing_policy (cached
-- snapshot) to enforce routing policy on every request. The api owns writes.

-- Where a model runs. 'self_hosted' = inside the customer's cluster (vLLM, mock,
-- LM Studio in dev). 'cloud_managed' = a managed service in the customer's cloud
-- account (Bedrock). Set by model sync from the gateway's /v1/models response.
ALTER TABLE models
    ADD COLUMN IF NOT EXISTS backend_class TEXT NOT NULL DEFAULT 'self_hosted'
        CHECK (backend_class IN ('self_hosted', 'cloud_managed'));

UPDATE models SET backend_class = 'cloud_managed' WHERE provider = 'bedrock';

-- Stable names workloads address instead of model versions. An alias points at
-- exactly one model_id (never another alias). The target is not a foreign key:
-- model rows are deleted and re-created by discovery, and an alias whose target
-- has temporarily disappeared must survive (requests to it are refused).
CREATE TABLE IF NOT EXISTS model_aliases (
    alias           TEXT        PRIMARY KEY
        CHECK (alias ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
    target_model_id TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Org-wide routing policy. Singleton row. 'self_hosted_only' refuses every
-- request that would resolve to a cloud_managed model. This is NOT per-workspace
-- policy (no workspace concept exists yet); when workspaces arrive this column
-- moves onto the workspace row and this table becomes the org default.
CREATE TABLE IF NOT EXISTS routing_policy (
    id         BOOLEAN     PRIMARY KEY DEFAULT true CHECK (id),
    residency  TEXT        NOT NULL DEFAULT 'any'
        CHECK (residency IN ('any', 'self_hosted_only')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID
);

INSERT INTO routing_policy (id) VALUES (true) ON CONFLICT (id) DO NOTHING;

-- inference_events.model holds the RESOLVED model (what actually served the
-- request). requested_model holds what the caller asked for when it differs
-- (i.e. an alias), so the audit trail answers both questions.
ALTER TABLE inference_events
    ADD COLUMN IF NOT EXISTS requested_model TEXT;
