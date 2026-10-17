-- Gateway-reported per-model availability. Bedrock refuses some models
-- (e.g. Claude Fable 5/5.1) unless the AWS account's data-retention mode is
-- aws_review or higher; the gateway reports that on each Bedrock entry of
-- GET /v1/models as "availability": {"state": "ok"|"needs_aws_opt_in"|
-- "unknown", "reason": "data_retention", "account_mode", "required_mode"}.
--
-- availability_state is the extracted state (queryable); availability_detail
-- is the whole object, passed through verbatim to the web. Both are gateway
-- FACTS, so SyncModels overwrites them on every sync (NULL when the gateway
-- omits the key, i.e. every non-Bedrock model) -- unlike enabled/display_name,
-- which are admin settings that survive a resync.
--
-- ADD COLUMN IF NOT EXISTS keeps this re-runnable by hand (see 018).
ALTER TABLE models
    ADD COLUMN IF NOT EXISTS availability_state TEXT NULL,
    ADD COLUMN IF NOT EXISTS availability_detail JSONB NULL;
