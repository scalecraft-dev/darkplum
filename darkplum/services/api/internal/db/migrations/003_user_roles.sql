ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'reader';

UPDATE users SET role = 'admin' WHERE email = 'system@darkplum.internal';
