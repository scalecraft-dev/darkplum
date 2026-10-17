-- Anthropic API keys created at ORGANIZATION level (not inside a workspace)
-- are refused by Anthropic with HTTP 400 "This API key is not scoped to a
-- workspace, so this request must include the anthropic-workspace-id header
-- with the ID of the workspace to use." workspace_id is that id. The gateway
-- sends it as the anthropic-workspace-id header on every Anthropic request;
-- keys created inside a workspace need none (NULL).
--
-- It is NOT a secret (shown in the admin UI, written to the audit log), only
-- meaningful for the anthropic row, and rides on the SAME last-writer-wins
-- stamp as model_filter: filter_set_at governs both, so the DB row wins over
-- the ANTHROPIC_MODEL_FILTER_SET_AT / ANTHROPIC_WORKSPACE_ID env pair iff
-- filter_set_at >= the env stamp (see 017). The api validates
-- ^[A-Za-z0-9_-]{1,64}$ and stores NULL for "none".
--
-- ADD COLUMN IF NOT EXISTS keeps this re-runnable by hand (see 018).
ALTER TABLE vendor_provider_settings
    ADD COLUMN IF NOT EXISTS workspace_id TEXT NULL;
