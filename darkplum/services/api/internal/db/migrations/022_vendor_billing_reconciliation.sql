-- Vendor-reported usage and cost, pulled with a separate billing (admin-scope)
-- key per vendor, for reconciling Darkplum's own estimates (inference_events x
-- model_prices) against what the vendor says it billed. The billing keys
-- themselves are NEVER stored here (they live only in the cluster Secret
-- darkplum-billing-keys); key_last4 is the audit-grade fingerprint.

-- Token usage per UTC day. workspace_id / api_key_id are the vendor's ids
-- (Anthropic workspace_id, OpenAI project_id); NULL when ungrouped.
CREATE TABLE IF NOT EXISTS vendor_usage_reports (
    vendor         TEXT        NOT NULL CHECK (vendor IN ('anthropic','openai','bedrock')),
    day            DATE        NOT NULL,
    model          TEXT        NOT NULL,
    workspace_id   TEXT,
    api_key_id     TEXT,
    uncached_input BIGINT      NOT NULL DEFAULT 0,
    cache_read     BIGINT      NOT NULL DEFAULT 0,
    cache_creation BIGINT      NOT NULL DEFAULT 0,
    output         BIGINT      NOT NULL DEFAULT 0,
    pulled_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_vendor_usage_reports
    ON vendor_usage_reports (vendor, day, model, COALESCE(workspace_id, ''), COALESCE(api_key_id, ''));

-- Billed cost per UTC day, in cents (decimal). For bedrock, description is the
-- Cost Explorer usage type and model stays NULL (mapped to a model id at
-- query time, see billing.MapBedrockUsageType).
CREATE TABLE IF NOT EXISTS vendor_cost_reports (
    vendor       TEXT          NOT NULL CHECK (vendor IN ('anthropic','openai','bedrock')),
    day          DATE          NOT NULL,
    description  TEXT          NOT NULL,
    model        TEXT,
    workspace_id TEXT,
    cost_cents   NUMERIC(14,4) NOT NULL,
    pulled_at    TIMESTAMPTZ   NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_vendor_cost_reports
    ON vendor_cost_reports (vendor, day, description, COALESCE(workspace_id, ''));

CREATE TABLE IF NOT EXISTS vendor_billing_status (
    vendor          TEXT PRIMARY KEY CHECK (vendor IN ('anthropic','openai','bedrock')),
    configured      BOOLEAN     NOT NULL DEFAULT false,
    state           TEXT        NOT NULL DEFAULT 'not_configured'
                    CHECK (state IN ('not_configured','ok','auth_failed','wrong_key_type','unreachable','pulling')),
    last_pulled_at  TIMESTAMPTZ,
    last_error_kind TEXT,
    key_last4       TEXT,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO vendor_billing_status (vendor) VALUES ('anthropic'), ('openai'), ('bedrock')
ON CONFLICT (vendor) DO NOTHING;
