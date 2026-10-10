CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS whatsapp_links (
    user_id   BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    jid       TEXT NOT NULL UNIQUE,
    linked_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS chats (
    user_id           BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    jid               TEXT NOT NULL,
    name              TEXT,
    last_message_time TIMESTAMPTZ,
    PRIMARY KEY (user_id, jid)
);

CREATE TABLE IF NOT EXISTS messages (
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    id              TEXT NOT NULL,
    chat_jid        TEXT NOT NULL,
    sender          TEXT,
    content         TEXT,
    timestamp       TIMESTAMPTZ NOT NULL,
    is_from_me      BOOLEAN NOT NULL DEFAULT false,
    media_type      TEXT,
    filename        TEXT,
    url             TEXT,
    media_key       BYTEA,
    file_sha256     BYTEA,
    file_enc_sha256 BYTEA,
    file_length     BIGINT,
    PRIMARY KEY (user_id, id, chat_jid),
    FOREIGN KEY (user_id, chat_jid) REFERENCES chats(user_id, jid) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS messages_user_chat_ts_idx ON messages (user_id, chat_jid, timestamp);
CREATE INDEX IF NOT EXISTS messages_user_ts_idx ON messages (user_id, timestamp);
