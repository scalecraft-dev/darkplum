-- Two things the chaos exercise surfaced.
--
-- 1. The per-turn content cap (256 KiB, migration 012) was far below the chat
--    request cap (16 MiB at nginx, api and gateway): a long pasted turn was
--    answered and then could not be saved. One number everywhere now.
ALTER TABLE conversation_messages DROP CONSTRAINT IF EXISTS conversation_messages_content_check;
ALTER TABLE conversation_messages ADD CONSTRAINT conversation_messages_content_check
    CHECK (octet_length(content) <= 16777216);  -- 16 MiB per turn, = the chat body cap

-- 2. Organisation-wide settings an admin edits from the UI/API. The first is
--    the platform system prompt the api prepends to every webchat turn, which
--    was a compile-time constant with no way to change it.
CREATE TABLE IF NOT EXISTS org_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID REFERENCES users(id) ON DELETE SET NULL
);
