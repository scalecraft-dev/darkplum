-- Per-principal routing policy: org -> user -> api_key. Same policy shape at
-- every level (residency, allowed_models, default_model); a NULL column
-- always means "inherit the next level up" (org's NULL residency means
-- "any", since there is nothing above org to inherit from). The gateway
-- (internal/policy) is the single place that merges a principal's chain into
-- an "effective" policy -- this table only stores what an admin explicitly
-- set at each level, never a computed/merged value.
--
-- One row per principal, enforced by three partial unique indexes rather than
-- a single natural key, because the three principal kinds don't share a
-- column: an org row has neither user_id nor api_key_id, so a plain UNIQUE
-- constraint on (user_id, api_key_id) would let every org-kind row collide on
-- (NULL, NULL) -- Postgres treats NULLs as distinct for uniqueness, which
-- would silently allow more than one 'org' row. The partial indexes below key
-- off principal_type instead, matching the intent directly.
--
-- Service keys (service_keys table) never get a row here -- there is no
-- api_key_id to reference (service_keys and api_keys are separate tables) and
-- no per-principal narrowing makes sense for the credential the api itself
-- uses to call the gateway.
CREATE TABLE principal_routing_policies (
  principal_type TEXT NOT NULL CHECK (principal_type IN ('org','user','api_key')),
  user_id        UUID REFERENCES users(id)    ON DELETE CASCADE,
  api_key_id     UUID REFERENCES api_keys(id) ON DELETE CASCADE,
  residency      TEXT CHECK (residency = 'self_hosted_only'),
  allowed_models TEXT[],
  default_model  TEXT,
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_by     UUID,
  CHECK ( (principal_type='org'     AND user_id IS NULL     AND api_key_id IS NULL)
       OR (principal_type='user'    AND user_id IS NOT NULL AND api_key_id IS NULL)
       OR (principal_type='api_key' AND api_key_id IS NOT NULL AND user_id IS NULL) )
);
CREATE UNIQUE INDEX prp_org_singleton ON principal_routing_policies (principal_type) WHERE principal_type = 'org';
CREATE UNIQUE INDEX prp_user_unique   ON principal_routing_policies (user_id)    WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX prp_key_unique    ON principal_routing_policies (api_key_id) WHERE api_key_id IS NOT NULL;

-- Seed the org row from routing_policy (migration 010) so the org's current
-- residency choice survives the cutover: 'any' has no column value at this
-- level (NULL = inherit-from-nothing = any), so only 'self_hosted_only'
-- carries over explicitly.
INSERT INTO principal_routing_policies (principal_type, residency, updated_at, updated_by)
  SELECT 'org', CASE WHEN residency = 'self_hosted_only' THEN 'self_hosted_only' END, updated_at, updated_by FROM routing_policy;

-- Rollout safety: keep routing_policy.residency mirrored from the org row
-- until every gateway replica has rolled onto reading
-- principal_routing_policies directly (dropping routing_policy is the
-- eventual cleanup once that's safe -- migration 016 does NOT do this; it
-- widens the residency ladder to a third value and teaches this trigger to
-- downgrade that value rather than mirror it verbatim, since an old gateway
-- has no concept of it -- see that migration's comment). Without this
-- mirror, a gateway pod still on the old snapshot loader would silently stop
-- seeing residency changes made through the new org policy endpoint during a
-- rolling deploy.
CREATE OR REPLACE FUNCTION prp_mirror_org_residency() RETURNS trigger AS $$
BEGIN
  IF NEW.principal_type = 'org' THEN
    UPDATE routing_policy SET residency = COALESCE(NEW.residency, 'any'), updated_at = now(), updated_by = NEW.updated_by WHERE id = true;
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER prp_mirror_org AFTER INSERT OR UPDATE ON principal_routing_policies FOR EACH ROW EXECUTE FUNCTION prp_mirror_org_residency();
