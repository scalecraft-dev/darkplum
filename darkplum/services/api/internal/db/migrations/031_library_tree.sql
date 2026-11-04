-- The library as a navigable tree, with a complete write-through copy in the
-- organisation's own object store (S3 first; the bucket layout is
-- library/v1/{org,people/<person>}/<folders>/<slug>.<kind>.{md,json}).
--
-- Postgres stays canonical: identity, ACL, folders, metadata and the body.
-- The bucket is a full, versioned copy written by a queue-driven worker in
-- the api (handler/library_sync.go); nothing reads it on the chat path.
--
-- Privacy is unchanged: a private item is readable only by its owner, and
-- folders follow the same rule (a person's folders are theirs; org folders
-- hold only org-visible items). Folder names are user content: they never
-- reach a log line or an activity row, only ids and counts.
--
-- Keys are built only from Postgres values (internal/libkey). Display names
-- are separate columns and never key material.

-- --- Folders ------------------------------------------------------------
-- scope 'user': a person's private tree (owner_user_id set). scope 'org': the
-- shared tree (owner_user_id NULL). `path` is the slug path from the root,
-- maintained by the handlers on create/move; its CHECK is the charset and
-- the depth limit (at most five segments) in the database itself.
CREATE TABLE IF NOT EXISTS library_folders (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope         TEXT NOT NULL CHECK (scope IN ('user', 'org')),
    owner_user_id UUID REFERENCES users(id) ON DELETE CASCADE,
    parent_id     UUID REFERENCES library_folders(id) ON DELETE CASCADE,
    slug          TEXT NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,63}$'),
    display_name  TEXT NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 100),
    path          TEXT NOT NULL CHECK (path ~ '^[a-z0-9][a-z0-9-]{0,63}(/[a-z0-9][a-z0-9-]{0,63}){0,4}$'),
    -- The zero-byte "<folder>/" marker the worker last wrote.
    s3_key        TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT library_folders_scope_owner CHECK ((scope = 'user') = (owner_user_id IS NOT NULL)),
    CONSTRAINT library_folders_parent_slug_key UNIQUE NULLS NOT DISTINCT (scope, owner_user_id, parent_id, slug),
    CONSTRAINT library_folders_path_key UNIQUE NULLS NOT DISTINCT (scope, owner_user_id, path)
);
CREATE INDEX IF NOT EXISTS idx_library_folders_parent ON library_folders (parent_id);

-- --- People ---------------------------------------------------------------
-- library_slug names a person's directory in the bucket. Derived from the
-- email at creation (lowercase, anything outside [a-z0-9] becomes '-', runs
-- collapsed, at most 64), a short suffix on collision. IMMUTABLE afterwards:
-- an email change does not move anyone's files.
ALTER TABLE users ADD COLUMN IF NOT EXISTS library_slug TEXT;

CREATE OR REPLACE FUNCTION library_person_slug(uid UUID, mail TEXT) RETURNS TEXT AS $$
DECLARE
    base TEXT;
    cand TEXT;
    n    INT;
BEGIN
    base := regexp_replace(lower(coalesce(mail, '')), '[^a-z0-9]+', '-', 'g');
    base := btrim(left(btrim(base, '-'), 64), '-');
    IF base = '' OR base ~ '^(con|prn|aux|nul|com[1-9]|lpt[1-9])$' THEN
        base := 'user';
        IF EXISTS (SELECT 1 FROM users WHERE library_slug = base AND id <> uid) THEN
            base := 'user-' || substr(md5(uid::text), 1, 4);
        END IF;
    END IF;
    cand := base;
    n := 4;
    WHILE EXISTS (SELECT 1 FROM users WHERE library_slug = cand AND id <> uid) AND n <= 32 LOOP
        cand := btrim(left(base, 63 - n), '-') || '-' || substr(md5(uid::text), 1, n);
        n := n + 4;
    END LOOP;
    RETURN cand;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION users_set_library_slug() RETURNS trigger AS $$
BEGIN
    IF NEW.library_slug IS NULL THEN
        NEW.library_slug := library_person_slug(NEW.id, NEW.email);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS users_library_slug ON users;
CREATE TRIGGER users_library_slug BEFORE INSERT ON users
    FOR EACH ROW EXECUTE FUNCTION users_set_library_slug();

-- Oldest account keeps the bare slug.
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN SELECT id, email FROM users WHERE library_slug IS NULL ORDER BY created_at, id LOOP
        UPDATE users SET library_slug = library_person_slug(r.id, r.email) WHERE id = r.id;
    END LOOP;
END $$;
ALTER TABLE users ALTER COLUMN library_slug SET NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS users_library_slug_key ON users (library_slug);

-- --- Items ----------------------------------------------------------------
ALTER TABLE library_items ADD COLUMN IF NOT EXISTS folder_id UUID REFERENCES library_folders(id) ON DELETE RESTRICT;
-- Where the item sat in its owner's private tree when it was shared, so an
-- unshare can put it back.
ALTER TABLE library_items ADD COLUMN IF NOT EXISTS previous_folder_path TEXT;
-- Object-store bookkeeping (see library_sync below). s3_key is the key last
-- written; s3_pending_key the key a Put was started for; s3_synced_sha the
-- content_sha256 that was written; s3_obj_sha the sha256 of the object's
-- bytes (trailing newline normalised), which Sync compares a bucket edit to.
ALTER TABLE library_items ADD COLUMN IF NOT EXISTS s3_key TEXT;
ALTER TABLE library_items ADD COLUMN IF NOT EXISTS s3_pending_key TEXT;
ALTER TABLE library_items ADD COLUMN IF NOT EXISTS s3_synced_sha TEXT;
ALTER TABLE library_items ADD COLUMN IF NOT EXISTS s3_obj_sha TEXT;
ALTER TABLE library_items ADD COLUMN IF NOT EXISTS s3_synced_at TIMESTAMPTZ;
-- Set when the object in the bucket was found to differ from what the worker
-- last wrote (a console upload, aws s3 cp). The worker then refuses to
-- overwrite it until someone runs Sync from bucket, which reports it; the
-- worker clears it whenever it writes the object.
ALTER TABLE library_items ADD COLUMN IF NOT EXISTS bucket_changed_at TIMESTAMPTZ;

-- Slugs that are Windows reserved device names cannot be file names in a
-- synced bucket; give an existing one its id suffix. Deterministic.
UPDATE library_items SET slug = left(slug, 91) || '-' || left(id::text, 8)
 WHERE slug ~ '^(con|prn|aux|nul|com[1-9]|lpt[1-9])$';

-- Uniqueness moves from (owner, slug) to the item's location. Items shared
-- with the organisation could collide (two people each shared a "code-review"
-- skill): every duplicate after the oldest keeps its slug plus -<id8>.
UPDATE library_items li SET slug = left(li.slug, 91) || '-' || left(li.id::text, 8)
  FROM (
    SELECT id, row_number() OVER (
             PARTITION BY visibility, CASE WHEN visibility = 'private' THEN owner_user_id END, folder_id, kind, slug
             ORDER BY created_at, id) AS rn
      FROM library_items
  ) d
 WHERE d.id = li.id AND d.rn > 1;

ALTER TABLE library_items DROP CONSTRAINT IF EXISTS library_items_owner_user_id_slug_key;
CREATE UNIQUE INDEX IF NOT EXISTS library_items_location_key ON library_items
    (visibility, (CASE WHEN visibility = 'private' THEN owner_user_id END), folder_id, kind, slug) NULLS NOT DISTINCT;
CREATE INDEX IF NOT EXISTS idx_library_items_folder ON library_items (folder_id);

-- --- The sync queue -------------------------------------------------------
-- One row per item or folder whose copy in the bucket may be stale. Rows are
-- inserted/bumped in the same transaction as the change (create, update,
-- move, rename, share, unshare, delete, admin delete, user delete, folder
-- create/move/delete). The worker re-applies the CURRENT state; it never
-- replays operations. A deleted item or folder has no row to read, so its
-- row carries the keys to remove (every version is purged: erasure runs
-- through the queue, never fire-and-forget).
--
-- With LIBRARY_STORE_URL unset the worker is off and rows simply accumulate
-- (one per item at most); they drain when a store is configured.
--
-- rev is bumped by every enqueue; the worker deletes a row only if rev is
-- unchanged since it claimed it, so a change that lands mid-sync is re-run.
CREATE TABLE IF NOT EXISTS library_sync (
    id           BIGSERIAL PRIMARY KEY,
    item_id      UUID,
    folder_id    UUID,
    stale_keys   TEXT[] NOT NULL DEFAULT '{}',
    rev          BIGINT NOT NULL DEFAULT 1,
    enqueued_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    not_before   TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until TIMESTAMPTZ,
    attempts     INT NOT NULL DEFAULT 0,
    -- A short code (never a key, name, path or SDK message).
    last_error   TEXT,
    CONSTRAINT library_sync_one_target CHECK ((item_id IS NULL) <> (folder_id IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS library_sync_item_key ON library_sync (item_id) WHERE item_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS library_sync_folder_key ON library_sync (folder_id) WHERE folder_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_library_sync_due ON library_sync (not_before);

-- Existing items land at the root of their scope on first sync.
INSERT INTO library_sync (item_id) SELECT id FROM library_items ON CONFLICT DO NOTHING;
