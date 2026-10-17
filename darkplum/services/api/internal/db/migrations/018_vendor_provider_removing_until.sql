-- vendor_provider_settings.removing_until for databases that ran an earlier
-- revision of migration 017, which created the table without it. 017 now
-- includes the column, so on a fresh database this is a no-op; on a database
-- that already had the table (where 017's CREATE TABLE IF NOT EXISTS did
-- nothing) it adds the column. See 017 for what the column means.
ALTER TABLE vendor_provider_settings
    ADD COLUMN IF NOT EXISTS removing_until TIMESTAMPTZ NULL;
