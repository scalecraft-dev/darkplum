-- The library: skills, context and tools a person keeps in their own
-- organisation's database instead of inside one AI vendor's account, shares
-- with colleagues, attaches to chats, and can export again.
--
-- One table for all three kinds. A skill and a context item are both text
-- (markdown); a tool item's body is a JSON configuration for an MCP server
-- (streamable HTTP endpoint) and the tool_* columns cache what that server
-- advertised when it was last verified. The credential a tool server may need
-- is NEVER stored here: it lives in the cluster Secret
-- darkplum-tool-credentials (one data key per item id), and this table only
-- remembers its last four characters so the UI can say "set, ends in ...".
--
-- Privacy: a private item is readable ONLY by its owner. Every read predicate
-- is (owner_user_id = $me OR visibility = 'org') in the SQL itself, and an id
-- that exists but is not visible 404s like one that doesn't exist. There is no
-- admin read path over private items anywhere: the admin governance routes
-- are scoped to visibility = 'org' in their SQL.
--
-- Ownership survives the owner: when a user is deleted (handler/users.go),
-- their private items go with them (like their conversations) and their
-- org-shared items are reassigned to the deleting administrator in the same
-- transaction, so a shared skill never silently vanishes or becomes an
-- uneditable orphan. ON DELETE SET NULL is only the last line of defence.
CREATE TABLE IF NOT EXISTS library_items (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind            TEXT NOT NULL CHECK (kind IN ('skill', 'context', 'tool')),
    owner_user_id   UUID REFERENCES users(id) ON DELETE SET NULL,
    -- Placeholder for the workspace concept (see migration 012's comment on
    -- conversations.workspace_id): nullable, no FK, until workspaces exist.
    workspace_id    UUID,
    visibility      TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private', 'org')),
    slug            TEXT NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,99}$'),
    name            TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    description     TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 500),
    -- Markdown for skill/context; JSON for tool. 64 KiB is a long document of
    -- instructions; the per-request injection cap (handler/chat_library.go)
    -- is the binding limit on what reaches a model.
    body            TEXT NOT NULL CHECK (octet_length(body) <= 65536),
    content_sha256  TEXT NOT NULL,
    shared_at       TIMESTAMPTZ,
    -- Tool items only.
    tool_catalog          JSONB,
    tool_catalog_at       TIMESTAMPTZ,
    tool_credential_last4 TEXT CHECK (tool_credential_last4 IS NULL OR char_length(tool_credential_last4) <= 4),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by      UUID REFERENCES users(id) ON DELETE SET NULL,
    -- The slug is a display/export filename, unique per owner; items are
    -- addressed by id everywhere else. A global slug would let shared items
    -- squat on each other's names.
    UNIQUE (owner_user_id, slug)
);
CREATE INDEX IF NOT EXISTS idx_library_items_owner ON library_items (owner_user_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_library_items_org   ON library_items (updated_at DESC) WHERE visibility = 'org';

-- The items a conversation has attached (the chip row above the composer),
-- so reopening the conversation restores them. Ids only; what each resolves
-- to is checked against visibility on every turn.
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS library_item_ids UUID[] NOT NULL DEFAULT '{}';

-- The tool calls an assistant turn made and what came back, as the chat
-- proxy reported them to the browser: [{id, server, tool, arguments, result,
-- is_error, duration_ms}]. This is the user's own conversation content, kept
-- so the exchange reads the same after a reload. NULL on turns with none.
ALTER TABLE conversation_messages ADD COLUMN IF NOT EXISTS tool_events JSONB
    CHECK (tool_events IS NULL OR octet_length(tool_events::text) <= 1048576);
