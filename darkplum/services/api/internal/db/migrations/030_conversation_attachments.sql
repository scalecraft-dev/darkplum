-- Images and files a person attaches to a chat turn: the bytes live here, in
-- the organisation's own database, next to the conversation they belong to.
--
-- Privacy: an attachment is readable ONLY by the owner of its conversation.
-- There is no user_id column on purpose (like conversation_messages); every
-- read joins up to conversations and filters conversations.user_id in the SQL
-- itself (handler/conversations_attachments.go, handler/chat_attachments.go),
-- so an id that exists but is not yours 404s like one that does not exist.
-- No admin route reads these rows.
--
-- Never logged, never audited: a filename or any byte of `data` must not
-- reach a log line or activity_events. Audit rows carry counts, media types
-- and byte sizes only.
--
-- Deletion is a hard delete and nothing more: ON DELETE CASCADE removes the
-- rows with their message (truncate / regenerate), their conversation, or
-- their user. Postgres keeps dead tuples (and TOAST chunks) until VACUUM, and
-- database snapshots and backups taken earlier still contain the bytes, so
-- "deleted" here means "no longer readable through the application", not
-- "scrubbed from storage".
--
-- The limits mirror what the gateway and Bedrock accept (one image <= 3.75 MB,
-- one document <= 4.5 MB, at most 20 images + 5 documents, 10 MiB per turn);
-- the per-turn count and total are enforced by the handler, the per-row
-- bounds below are the last line of defence.
CREATE TABLE IF NOT EXISTS conversation_attachments (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id UUID NOT NULL REFERENCES conversation_messages(id) ON DELETE CASCADE,
    position   SMALLINT NOT NULL CHECK (position BETWEEN 0 AND 24),
    kind       TEXT NOT NULL CHECK (kind IN ('image', 'file')),
    media_type TEXT NOT NULL CHECK (media_type IN (
        'image/png', 'image/jpeg', 'image/gif', 'image/webp',
        'application/pdf', 'text/csv', 'text/plain', 'text/markdown', 'text/html',
        'application/msword',
        'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
        'application/vnd.ms-excel',
        'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet'
    )),
    -- An image is always a raster image type and a file never is.
    CONSTRAINT conversation_attachments_kind_media_type CHECK (
        (kind = 'image') = (media_type LIKE 'image/%')
    ),
    name       TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 255),
    size_bytes INTEGER NOT NULL CHECK (size_bytes BETWEEN 1 AND 4500000),
    data       BYTEA NOT NULL,
    CONSTRAINT conversation_attachments_size_matches CHECK (octet_length(data) = size_bytes),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Also serves every lookup by message_id (leading column), so no separate
    -- index on message_id is created.
    UNIQUE (message_id, position)
);
