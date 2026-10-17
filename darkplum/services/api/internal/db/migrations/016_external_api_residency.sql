-- Adds the third backend class (external_api: direct Anthropic/OpenAI vendor
-- calls, never entering the customer's cloud account) and the third residency
-- rung (cloud_account_only) it needs to be governable by. See PLAN v3.1's
-- "Wire contract": backend_class in (self_hosted, cloud_managed,
-- external_api); residency ladder any(0) < cloud_account_only(1) <
-- self_hosted_only(2), tighten-only.
--
-- Constraint names below were confirmed by querying pg_constraint against a
-- database that had run migrations 001-015 (they are Postgres's own
-- deterministic <table>_<column>_check names for the inline, unnamed CHECKs
-- those migrations wrote) -- not guessed.

-- 1. Widen the CHECK constraints that only knew about two classes/two
-- residency values. DROP ... IF EXISTS keeps this migration re-runnable by
-- hand against a schema that already has the new constraint (the tracked
-- migration runner only ever applies a given file once, but a DBA restoring
-- from a partial/manual run should not get a bare "constraint already
-- exists" error).
ALTER TABLE models DROP CONSTRAINT IF EXISTS models_backend_class_check;
ALTER TABLE models ADD CONSTRAINT models_backend_class_check
    CHECK (backend_class IN ('self_hosted', 'cloud_managed', 'external_api'));

-- routing_policy is the legacy (pre-015) mirror table -- see
-- prp_mirror_org_residency below for why it is never actually written
-- 'cloud_account_only' by this codebase. The CHECK is still widened to the
-- full ladder rather than left at two values: it documents the column's real
-- domain and does not depend on the trigger's current downgrade behaviour to
-- stay correct if that ever changes.
ALTER TABLE routing_policy DROP CONSTRAINT IF EXISTS routing_policy_residency_check;
ALTER TABLE routing_policy ADD CONSTRAINT routing_policy_residency_check
    CHECK (residency IN ('any', 'cloud_account_only', 'self_hosted_only'));

-- principal_routing_policies.residency: NULL still means "inherit" at every
-- level (and "any" specifically at the org level, which has nothing above it
-- to inherit from -- see orgResidencyString in routing.go); a non-NULL value
-- can now be 'cloud_account_only' as well as 'self_hosted_only', at org,
-- user, and api_key rows alike.
ALTER TABLE principal_routing_policies DROP CONSTRAINT IF EXISTS principal_routing_policies_residency_check;
ALTER TABLE principal_routing_policies ADD CONSTRAINT principal_routing_policies_residency_check
    CHECK (residency IN ('cloud_account_only', 'self_hosted_only'));

-- 2. The legacy mirror (migration 015) exists solely so a gateway pod still
-- running pre-principal-policy code during a rolling deploy keeps seeing
-- residency changes made through the new endpoint. Such a gateway's
-- Effective() only ever compares residency to 'self_hosted_only' (it has no
-- concept of external_api or of refusing it under cloud_account_only) and it
-- has no external-provider route table to enforce anything on in the first
-- place. Mirroring 'cloud_account_only' into that table verbatim would be
-- actively wrong: the old gateway would treat it as an unrecognized string,
-- which its own residency check treats as not-self_hosted_only, i.e. as
-- "any" -- so it would already permit everything. Writing the real literal
-- 'cloud_account_only' would additionally leak a three-tier concept into a
-- column/consumer that cannot reason about it, and would misinform a human
-- or script reading routing_policy directly during a rollout, expecting the
-- historical two-value vocabulary. Downgrading to 'any' keeps that read
-- truthful for what an old gateway can actually enforce: nothing beyond
-- self_hosted_only, so anything looser reads the same as unrestricted.
--
-- This stops being safe the day any code path relies on routing_policy
-- (rather than principal_routing_policies) as authoritative for a
-- cloud_account_only decision -- which should never happen post-rollout, and
-- is exactly the condition migration 015's rollout-safety comment describes
-- for dropping this table altogether once every gateway replica reads
-- principal_routing_policies directly.
CREATE OR REPLACE FUNCTION prp_mirror_org_residency() RETURNS trigger AS $$
BEGIN
  IF NEW.principal_type = 'org' THEN
    UPDATE routing_policy
    SET residency = CASE
          WHEN NEW.residency IS NULL THEN 'any'
          WHEN NEW.residency = 'cloud_account_only' THEN 'any'
          ELSE NEW.residency
        END,
        updated_at = now(),
        updated_by = NEW.updated_by
    WHERE id = true;
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

-- 3. Seed the org policy to cloud_account_only, on both a fresh install (no
-- org row yet -- shouldn't happen given migration 015 always creates one
-- from routing_policy's singleton, but the INSERT branch below costs nothing
-- and makes this migration correct even if that invariant is ever broken by
-- a future squash) and an existing install already at 'any' (residency IS
-- NULL).
--
-- Deliberately does NOT set updated_at/updated_by on the UPDATE branch: this
-- is an operator-invisible default change, not an admin action, and must not
-- appear as one in the org policy's own "last changed" field or in
-- affected-principals accounting that keys off updated_at.
--
-- Does NOT touch a org already at self_hosted_only: cloud_account_only is
-- LOOSER than self_hosted_only on the ladder (rank 1 < rank 2), so applying
-- it there would be a loosening, not the tightening this seed is meant to
-- be. The "WHERE ... residency IS NULL" on the DO UPDATE clause encodes
-- exactly that: only a row currently at 'any' (NULL) is moved, everything
-- else (self_hosted_only) is left alone by the conflict action doing
-- nothing.
--
-- This INSERT fires prp_mirror_org (AFTER INSERT OR UPDATE), which downgrades
-- the legacy mirror to 'any' per the trigger rewrite above -- exactly the
-- "old gateway never sees cloud_account_only" invariant PLAN v3.1 requires.
INSERT INTO principal_routing_policies (principal_type, residency, updated_at, updated_by)
VALUES ('org', 'cloud_account_only', now(), NULL)
ON CONFLICT (principal_type) WHERE principal_type = 'org' DO UPDATE
    SET residency = 'cloud_account_only'
    WHERE principal_routing_policies.residency IS NULL;

-- 4. inference_events.backend_class: nullable (a request refused before a
-- model resolved has no class to record -- see models_logic.go's
-- backendClass constants), added to the partitioned parent (which cascades
-- the column to every existing partition and the default partition
-- automatically) and validated in two steps so adding it never blocks
-- writes against the existing partitions while the CHECK is confirmed
-- against their (all-NULL, trivially valid) existing rows.
ALTER TABLE inference_events ADD COLUMN IF NOT EXISTS backend_class TEXT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'inference_events_backend_class_check'
          AND conrelid = 'inference_events'::regclass
    ) THEN
        ALTER TABLE inference_events
            ADD CONSTRAINT inference_events_backend_class_check
            CHECK (backend_class IN ('self_hosted', 'cloud_managed', 'external_api')) NOT VALID;
    END IF;
END $$;

ALTER TABLE inference_events VALIDATE CONSTRAINT inference_events_backend_class_check;
