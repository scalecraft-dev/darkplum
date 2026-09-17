-- Why a request was refused before it reached a model backend. Set by the
-- gateway for policy denials (model_not_found, model_not_enabled,
-- model_residency_violation, policy_unavailable); NULL for dispatched requests.
-- Together with model (resolved) and requested_model (alias), this makes a
-- refusal auditable from the table alone, not just from gateway logs.
ALTER TABLE inference_events
    ADD COLUMN IF NOT EXISTS denial_code TEXT;
