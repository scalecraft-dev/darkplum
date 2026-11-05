-- Library item bodies grow from 64 KiB to 4 MiB. The library lives in Postgres
-- (the bucket is an optional mirror), and a reference document is often far
-- longer than a page of instructions. The per-turn injection cap in
-- handler/chat_library.go is still the binding limit on what reaches a model.
--
-- Additive for overlapping pods: an old api still refuses anything over
-- 64 KiB in code, a new one accepts up to the new constraint. The original
-- constraint was declared inline in migration 026, so it carries Postgres's
-- generated name.
ALTER TABLE library_items DROP CONSTRAINT IF EXISTS library_items_body_check;
ALTER TABLE library_items DROP CONSTRAINT IF EXISTS library_items_body_max;
ALTER TABLE library_items ADD CONSTRAINT library_items_body_max CHECK (octet_length(body) <= 4194304);
