-- Routed aliases: an alias can now resolve to one of several target models
-- via a per-request classifier, instead of always pointing at exactly one
-- model. "kind" distinguishes the two shapes on the same model_aliases row
-- rather than splitting into a separate table, so every existing alias (all
-- of them 'static' after this migration) keeps working with zero data
-- migration.
--
-- pinned_model (on conversations, below) and inference_events.purpose /
-- route_name / router_reason / router_latency_ms exist so the gateway's
-- routing decision is both replayable (the webchat can stick to the model a
-- routed alias resolved to on turn 1) and auditable (a classifier call is its
-- own inference_events row, never folded into the request it decided).
ALTER TABLE model_aliases ADD COLUMN kind TEXT NOT NULL DEFAULT 'static' CHECK (kind IN ('static','routed'));

-- A routed alias has no single target_model_id -- the target is decided per
-- request by the classifier -- so the column that used to be NOT NULL for
-- every alias is now only required for kind='static' (enforced below by
-- alias_kind_shape, not by a plain NOT NULL).
ALTER TABLE model_aliases ALTER COLUMN target_model_id DROP NOT NULL;

-- default_route is the route a routed alias falls back to whenever the
-- classifier can't confidently pick one (no keyword match, an invalid/
-- unavailable classifier model, a timeout, ...) -- see router_reason's
-- 'rules:default' / 'fallback:*' values. It must name a real row in
-- model_alias_routes; that FK-like invariant can't be a plain DB constraint
-- (routes are written as a separate statement in the same transaction as the
-- alias, and Postgres has no deferred cross-table CHECK), so it is enforced
-- by the api at PUT time inside the same pg_advisory_xact_lock transaction
-- that writes both the alias and its routes.
ALTER TABLE model_aliases ADD COLUMN default_route TEXT;

-- classifier_kind = 'rules' picks a route via ordered, explainable keyword
-- matching (no model call, no second audit row). classifier_kind =
-- 'semantic_router' delegates to the in-cluster vLLM Semantic Router (a CPU
-- classification service, not an LLM call — see the gateway's
-- SEMANTIC_ROUTER_URL), also with no second audit row (router_reason and
-- latency land on the main row). classifier_kind = 'model' calls
-- classifier_model_id (a self_hosted model, checked per request by the
-- gateway) and records a second inference_events row (purpose='routing') for
-- that call. The api stays ignorant of whether the gateway actually has
-- SEMANTIC_ROUTER_URL configured — an unconfigured or unreachable semantic
-- router degrades to default_route at request time
-- (fallback:classifier_invalid), it is never a PUT-time validation error.
ALTER TABLE model_aliases ADD COLUMN classifier_kind TEXT CHECK (classifier_kind IN ('rules','semantic_router','model'));
ALTER TABLE model_aliases ADD COLUMN classifier_model_id TEXT;

-- Shape invariant tying the four new columns together with target_model_id:
-- a static alias looks exactly as it always has (a target, nothing routing-
-- related); a routed alias has no target of its own, must name a
-- default_route, must declare a classifier_kind, and classifier_model_id is
-- present if and only if that classifier is 'model' (never set for 'rules'
-- or 'semantic_router', always set for 'model').
ALTER TABLE model_aliases ADD CONSTRAINT alias_kind_shape CHECK (
    (kind='static' AND target_model_id IS NOT NULL AND default_route IS NULL AND classifier_kind IS NULL AND classifier_model_id IS NULL)
 OR (kind='routed' AND target_model_id IS NULL AND default_route IS NOT NULL AND classifier_kind IS NOT NULL
     AND ((classifier_kind='model') = (classifier_model_id IS NOT NULL))));

-- One row per named route on a routed alias. Deleted and re-inserted as a
-- whole set on every PUT (see the api's RoutingHandler.PutAlias) rather than
-- diffed, so "remove a route" and "reorder routes" are both just "write the
-- new list" -- there is no route identity that survives a PUT. ON DELETE
-- CASCADE means deleting the alias deletes its routes in the same statement;
-- ON UPDATE CASCADE exists only for symmetry with FKs elsewhere in this
-- schema (alias values are never updated in place -- PutAlias always keys by
-- the immutable alias name).
--
-- target_model_id is NOT a foreign key into models, for the same reason
-- model_aliases.target_model_id isn't: a model row can be purged by
-- discovery (models.go's modelPurgeAfter) and re-appear later, and a route
-- pointing at a temporarily-missing model must survive that gap (the
-- gateway's per-request checks refuse it; the route definition itself is not
-- deleted). It is, however, never another alias -- the api enforces "no
-- alias chains" for routes exactly as it already does for
-- model_aliases.target_model_id, just outside the DB layer.
CREATE TABLE model_alias_routes (
    alias           TEXT NOT NULL REFERENCES model_aliases(alias) ON DELETE CASCADE ON UPDATE CASCADE,
    route_name      TEXT NOT NULL CHECK (route_name ~ '^[a-z0-9][a-z0-9_-]{0,31}$'),
    description     TEXT NOT NULL CHECK (char_length(description) BETWEEN 1 AND 500),
    target_model_id TEXT NOT NULL,
    -- Request order: the rules classifier evaluates routes in ascending
    -- position, first match wins. UNIQUE (alias, position) makes "what is
    -- route N of this alias" unambiguous without a separate ORDER BY tie-
    -- breaker.
    position        INT  NOT NULL,
    -- Admin-authored, case-folded-on-write literal substrings (never a
    -- regex) the rules classifier matches against, plus the reserved
    -- builtin:code / builtin:long / builtin:multimodal signals. Capped at 32
    -- to keep the per-request classification pass boundeded and linear, not
    -- because 32 is a meaningful business number.
    keywords        TEXT[] NOT NULL DEFAULT '{}' CHECK (cardinality(keywords) <= 32),
    PRIMARY KEY (alias, route_name),
    UNIQUE (alias, position)
);

-- purpose distinguishes the three kinds of inference_events row a single
-- routed request can produce: the dispatched request itself ('inference'),
-- the classifier's own model call when classifier_kind='model'
-- ('routing' -- always a second row sharing the dispatched request's
-- request_id, never folded into it, since it has its own model/provider/
-- tokens/latency/status), and an admin's "test a prompt" dry run that never
-- dispatches to any target ('routing_dryrun'). Every existing row and every
-- write that doesn't set this explicitly defaults to 'inference', which is
-- what pre-migration rows already are.
ALTER TABLE inference_events ADD COLUMN purpose TEXT NOT NULL DEFAULT 'inference' CHECK (purpose IN ('inference','routing','routing_dryrun'));

-- route_name / router_reason are set only on a routed alias's dispatched
-- request (purpose='inference'); router_reason's vocabulary is closed
-- (rules:default, rules:<route_name>:<keyword|builtin>,
-- fallback:classifier_invalid, fallback:unparseable, fallback:timeout,
-- fallback:circuit_open) so a request's routing decision is always
-- explainable straight from this row, without cross-referencing gateway
-- logs. NULL for a non-routed request (a static alias or a bare model id).
ALTER TABLE inference_events ADD COLUMN route_name TEXT;
ALTER TABLE inference_events ADD COLUMN router_reason TEXT;
-- Wall-clock time the classifier itself took (rules: effectively instant;
-- model: bounded by CLASSIFIER_TIMEOUT_MS), kept separate from latency_ms
-- (the whole request) so a slow classifier and a slow model backend are
-- distinguishable from this row alone.
ALTER TABLE inference_events ADD COLUMN router_latency_ms INT;

-- The provenance lookups in the api (resolveAssistantProvenance,
-- backfillProvenance) and most activity/usage aggregations only ever care
-- about dispatched requests, filtering WHERE purpose = 'inference' on top of
-- request_id/user_id -- this is the index that predicate hits, instead of a
-- sequential scan (or the pre-existing (request_id, user_id) shape, which a
-- routing/routing_dryrun row sharing the same request_id would also match).
CREATE INDEX IF NOT EXISTS idx_inference_events_request_user_inference ON inference_events (request_id, user_id) WHERE purpose = 'inference';

-- The concrete model id a routed alias resolved to on a conversation's first
-- turn, stored so the webchat can keep sending that exact model for
-- subsequent turns (client-side stickiness) instead of re-classifying every
-- message and risking a different route mid-conversation. NULL means "no
-- pin" -- either the picker isn't on a routed alias, or the pin was cleared
-- because the user changed the picker's model. Nullable and un-FK'd for the
-- same reason conversations.model is: it names a model or alias id, not a
-- row in any table this schema can join against structurally.
ALTER TABLE conversations ADD COLUMN pinned_model TEXT;
