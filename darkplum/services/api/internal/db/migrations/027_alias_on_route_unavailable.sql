-- What a routed alias does when the classifier's picked non-default route
-- targets a model that is disabled or whose backend is unreachable:
--   refuse                     never use another route (503 route_target_unavailable)
--   fallback_same_or_stricter  use the default route only if its target is
--                              usable and its backend class is the same as or
--                              stricter than the picked route's; else refuse
--   fallback_any_permitted     use the default route whenever it is usable
--                              (residency and allowlist still apply to it)
-- Existing and new aliases default to fallback_same_or_stricter: a local
-- outage is never silently retried on a vendor. Classifier failure (timeout,
-- circuit open, low confidence, ...) always goes to the default route and is
-- not governed by this column. Static aliases carry the default and ignore it.
ALTER TABLE model_aliases
    ADD COLUMN IF NOT EXISTS on_route_unavailable TEXT NOT NULL DEFAULT 'fallback_same_or_stricter';

ALTER TABLE model_aliases DROP CONSTRAINT IF EXISTS model_aliases_on_route_unavailable_check;
ALTER TABLE model_aliases ADD CONSTRAINT model_aliases_on_route_unavailable_check
    CHECK (on_route_unavailable IN ('refuse', 'fallback_same_or_stricter', 'fallback_any_permitted'));
