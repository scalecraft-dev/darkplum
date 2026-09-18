-- Server-side conversation history for the webchat. Previously localStorage
-- only (services/web/src/lib/conversations.ts); moving it to Postgres so a
-- user doesn't lose history by clearing site data or switching browsers.
--
-- Privacy: a conversation is readable ONLY by its owner (user_id). There is
-- no admin read path over this data anywhere in the codebase -- see the
-- comment at the top of internal/handler/conversations.go.

CREATE TABLE IF NOT EXISTS conversations (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- Placeholder for the workspace concept described in
    -- DARKPLUM_IMPLEMENTATION.md (routing_policy, migration 010, has the same
    -- caveat). No workspaces table exists yet, so this has no FK and is
    -- nullable. When workspaces land, this should become NOT NULL + FK'd and
    -- every query in conversations.go should scope by (workspace_id, user_id)
    -- instead of user_id alone.
    workspace_id UUID,
    title        TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    model        TEXT NOT NULL DEFAULT '',            -- model or alias last used; '' = none chosen
    import_key   TEXT CHECK (char_length(import_key) <= 200), -- client's pre-migration local id; idempotent import
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, import_key)
);
CREATE INDEX IF NOT EXISTS idx_conversations_user_updated ON conversations (user_id, updated_at DESC);

-- Deletion here is a hard delete, never a soft delete. DELETE
-- /conversations/:id issues a real SQL DELETE against this table (cascading
-- to conversation_messages below) with no tombstone and no recovery path --
-- conversation content is end-user chat content, not an audited business
-- record, so the right-to-erasure case wins over retention. What IS retained
-- is the fact that a deletion happened (activity_events "conversation.deleted",
-- metadata only -- see conversations.go), never the content.
CREATE TABLE IF NOT EXISTS conversation_messages (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    seq             INT  NOT NULL,
    role            TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
    content         TEXT NOT NULL CHECK (octet_length(content) <= 262144),   -- 256 KiB per turn
    -- model/request_id are provenance, not client input. The api fills both
    -- by looking up inference_events for the caller-supplied request_id,
    -- scoped to the message's owning user_id -- never from a client-supplied
    -- model value -- so a caller cannot forge which model produced an
    -- assistant turn. Both NULL for user turns, and both NULL for an
    -- assistant turn whose request_id doesn't resolve to that user's own
    -- inference_events row (missing, or belongs to someone else).
    model           TEXT,
    request_id      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, seq)
);
-- Supports future retention sweeps (e.g. "delete messages older than N days")
-- without a sequential scan.
CREATE INDEX IF NOT EXISTS idx_conversation_messages_created_at ON conversation_messages (created_at);
