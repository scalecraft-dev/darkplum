-- Request parameters the gateway accepted but did not pass on as sent (GitHub
-- #3). A parameter a backend cannot honour is either refused (400
-- unsupported_parameter) or, by default, dropped and recorded; the record is
-- the X-Darkplum-Dropped-Params response header and this column.
--
--   inference_events.dropped_params
--       The entries of that header: a bare parameter name for a dropped
--       parameter ("seed", "stream_options.include_obfuscation"), name=action
--       for an adjustment ("max_tokens=raised"). Names and actions only, never
--       the values the caller sent; note that an unknown request key's NAME
--       is chosen by the caller and may itself carry data (it stays in the
--       customer's account). NULL when nothing was dropped.
--
--   principal_routing_policies.params_mode
--       The organisation-wide switch, on the org row only. NULL and 'lenient'
--       mean the same thing: drop and record. 'strict' turns every drop into a
--       400 unsupported_parameter. A contract refusal (n>1, tools on a backend
--       that cannot take them, ...) is a 400 in both modes. The gateway reads
--       this column from the org row of the policy snapshot and treats a
--       database without it as lenient.
--
--   param_drop_notices
--       Durable dedupe for the admin audit event written the first time a
--       given principal has a given parameter dropped (activity_events,
--       event_type 'inference.param_dropped'). The gateway inserts here with
--       ON CONFLICT DO NOTHING and writes the event only when the insert
--       happened, so replicas and restarts never repeat it. principal_id is
--       not a foreign key: a revoked key keeps its history.

ALTER TABLE inference_events ADD COLUMN IF NOT EXISTS dropped_params TEXT[];

ALTER TABLE principal_routing_policies ADD COLUMN IF NOT EXISTS params_mode TEXT;

ALTER TABLE principal_routing_policies DROP CONSTRAINT IF EXISTS principal_routing_policies_params_mode_check;
ALTER TABLE principal_routing_policies ADD CONSTRAINT principal_routing_policies_params_mode_check
    CHECK (params_mode IS NULL OR params_mode IN ('lenient', 'strict'));

-- Only the org row carries it: a user or key cannot opt itself out of (or
-- into) the organisation's choice.
ALTER TABLE principal_routing_policies DROP CONSTRAINT IF EXISTS principal_routing_policies_params_mode_org_only;
ALTER TABLE principal_routing_policies ADD CONSTRAINT principal_routing_policies_params_mode_org_only
    CHECK (principal_type = 'org' OR params_mode IS NULL);

CREATE TABLE IF NOT EXISTS param_drop_notices (
    principal_kind TEXT NOT NULL CHECK (principal_kind IN ('api_key', 'user', 'service_key')),
    principal_id   UUID NOT NULL,
    param          TEXT NOT NULL,
    first_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_kind, principal_id, param)
);
