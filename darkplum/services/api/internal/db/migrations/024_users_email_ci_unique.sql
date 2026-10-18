-- Emails are case-insensitive identifiers: Foo@x and foo@x are one account.
-- The api folds case on create/login/setup (chaos fixes, 2026-10); this
-- index makes the database agree, closing the race two concurrent creates
-- could still win. It fails loudly, naming the offenders, if an existing
-- install already holds case-variant duplicates: resolve those by hand
-- (delete or re-address one of each pair) and rerun.
DO $$
DECLARE
    dups text;
BEGIN
    SELECT string_agg(lower(email) || ' x' || n, ', ')
      INTO dups
      FROM (SELECT lower(email) AS email, COUNT(*) AS n FROM users GROUP BY lower(email) HAVING COUNT(*) > 1) d;
    IF dups IS NOT NULL THEN
        RAISE EXCEPTION 'users.email has case-variant duplicates (%); resolve them before this migration can add the case-insensitive unique index', dups;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower_key ON users (lower(email));
