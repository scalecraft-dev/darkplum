-- Removes the in-house "model" classifier kind (an Arch-Router prompt-
-- protocol re-implementation, gateway internal/router.ModelClassifier) from
-- routed aliases. Owner decision, ratified by product: a routed alias's
-- classifier_kind now supports exactly two values, 'rules' and
-- 'semantic_router'.

-- Existing rows using the removed kind degrade to 'rules' rather than being
-- left in a state the new CHECK constraint below would reject, or deleted
-- outright: ordered keyword matching is always a safe, I/O-free classifier,
-- so the alias keeps working unattended once this migration runs. (The
-- gateway tolerates classifier_kind='model' on its own read path too, for
-- the window before this migration lands against a given customer
-- database -- see internal/policy/pgstore.go's buildAliases.)
UPDATE model_aliases SET classifier_kind = 'rules', classifier_model_id = NULL WHERE classifier_kind = 'model';

-- Drop the shape constraint before touching the columns/CHECK it
-- references; it is re-added below without the classifier_model_id clause.
ALTER TABLE model_aliases DROP CONSTRAINT alias_kind_shape;

ALTER TABLE model_aliases DROP CONSTRAINT model_aliases_classifier_kind_check;
ALTER TABLE model_aliases ADD CONSTRAINT model_aliases_classifier_kind_check
    CHECK (classifier_kind IN ('rules', 'semantic_router'));

-- classifier_model_id only ever held the self-hosted model id a 'model'
-- classifier called (validated per request by the gateway). With that kind
-- gone, neither the api (internal/handler/routing.go) nor the gateway
-- (internal/policy/pgstore.go) reads or writes this column anymore.
ALTER TABLE model_aliases DROP COLUMN classifier_model_id;

-- Reinstated without the classifier_model_id clause: a routed alias still
-- has no target of its own, must name a default_route, and must declare a
-- classifier_kind (now always 'rules' or 'semantic_router').
ALTER TABLE model_aliases ADD CONSTRAINT alias_kind_shape CHECK (
    (kind = 'static' AND target_model_id IS NOT NULL AND default_route IS NULL AND classifier_kind IS NULL)
 OR (kind = 'routed' AND target_model_id IS NULL AND default_route IS NOT NULL AND classifier_kind IS NOT NULL)
);

-- inference_events.purpose's CHECK constraint (migration 013) is left
-- exactly as-is: 'routing' and 'routing_dryrun' were only ever produced by
-- the removed classifier's own model call, and historic rows may still
-- carry them. Nothing writes those values anymore (both the api and the
-- gateway now always write/leave 'inference'), but there is no need to
-- reject them retroactively either.
