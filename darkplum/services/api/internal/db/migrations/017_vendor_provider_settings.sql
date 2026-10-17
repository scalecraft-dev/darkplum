-- Backs vendor API key install/rotate/remove from the admin UI (PLAN-UI's
-- "Filter ownership" ruling): POST/PUT/DELETE
-- /api/v1/providers/:vendor/{verify,key,filter} in internal/handler/
-- providers.go. One row per vendor, keyed by name rather than a UUID id --
-- there are, and only ever will be, two vendors (anthropic, openai),
-- mirroring the gateway's own provider.ProviderStatus vendor set.
--
-- model_filter is the same catalog bound the Makefile's `make set-*-key
-- MODELS=` targets have always written into the ConfigMap
-- (<VENDOR>_MODEL_FILTER): a list of glob patterns or exact ids, kept as a
-- real array (not a comma-joined string) so it can be validated and diffed
-- server-side without re-parsing; the api joins it with commas only when
-- mirroring into the ConfigMap, matching the CLI path's own wire format.
--
-- filter_set_at is the "last writer wins" timestamp compared against the
-- gateway-reported env ConfigMap SET_AT: the DB row governs iff
-- filter_set_at >= the env value (missing env SET_AT = epoch). The api
-- always stamps filter_set_at = GREATEST(now(), env_filter_set_at + 1s,
-- previous filter_set_at + 1s) on every UI write (skew-proof: a UI write
-- always outranks whatever env stamp exists, even one from a laptop clock in
-- the future) -- see providers.go's nextFilterSetAt.
--
-- installing_until is non-NULL only while an install/rotate hint window is
-- open (5 minutes): it drives both the UI's "Installing" progress state and
-- the api's own faster model-sync ticking (cmd/main.go) while any vendor is
-- mid-install. NULL means "not currently installing" (either never
-- installed, or the window has closed/been cleared by a key removal).
--
-- removing_until mirrors installing_until for the opposite transition: DELETE
-- .../key sets it to now()+5m (and clears installing_until, since the two
-- windows are mutually exclusive -- a vendor is either mid-install or
-- mid-removal, never both) so the UI's card can show a "Removing" progress
-- state the same way it shows "Installing", and so the api's sync loop keeps
-- ticking every 5s while it's open (cmd/main.go). A PUT of a new/rotated key
-- clears it back to NULL -- reinstalling cancels an in-flight removal's UI
-- hint. The gateway has no concept of this column and never reads it (it is
-- purely an api-side / UI-facing progress hint, unlike model_filter and
-- filter_set_at, which the gateway DOES read for last-writer-wins).
--
-- CREATE TABLE IF NOT EXISTS (rather than a bare CREATE TABLE) keeps this
-- migration re-runnable by hand against a schema that already has the
-- table, matching migration 016's DROP CONSTRAINT IF EXISTS re-runnability
-- pattern for the same reason: a DBA restoring from a partial/manual run
-- should not get a bare "relation already exists" error. removing_until was
-- added to this file after an earlier revision of it had already run on
-- development clusters, where CREATE TABLE IF NOT EXISTS is a no-op; migration
-- 018 adds the column there. Fresh databases get it from this statement.
CREATE TABLE IF NOT EXISTS vendor_provider_settings (
    vendor           TEXT PRIMARY KEY CHECK (vendor IN ('anthropic', 'openai')),
    model_filter     TEXT[] NOT NULL,
    filter_set_at    TIMESTAMPTZ NOT NULL,
    installing_until TIMESTAMPTZ NULL,
    removing_until   TIMESTAMPTZ NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by       UUID
);
