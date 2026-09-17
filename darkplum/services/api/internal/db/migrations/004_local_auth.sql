ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS auth_provider TEXT;
UPDATE users SET auth_provider = 'oidc' WHERE oidc_subject IS NOT NULL AND auth_provider IS NULL;
