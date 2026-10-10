-- Accounts are now identified by GitHub user, signed in via OAuth; passwords
-- are no longer used.
ALTER TABLE users ADD COLUMN IF NOT EXISTS github_id BIGINT;
CREATE UNIQUE INDEX IF NOT EXISTS users_github_id_idx ON users (github_id);
ALTER TABLE users DROP COLUMN IF EXISTS password_hash;

-- MCP clients registered via OAuth dynamic client registration (RFC 7591).
CREATE TABLE IF NOT EXISTS oauth_clients (
    client_id     TEXT PRIMARY KEY,
    client_name   TEXT,
    redirect_uris TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Refresh tokens, stored as SHA-256 hashes and rotated on every use.
CREATE TABLE IF NOT EXISTS oauth_refresh_tokens (
    token_hash TEXT PRIMARY KEY,
    client_id  TEXT NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL
);
