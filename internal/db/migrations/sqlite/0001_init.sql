CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS whatsapp_links (
    user_id   INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    jid       TEXT NOT NULL UNIQUE,
    linked_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS chats (
    user_id           INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    jid               TEXT NOT NULL,
    name              TEXT,
    last_message_time TIMESTAMP,
    PRIMARY KEY (user_id, jid)
);

CREATE TABLE IF NOT EXISTS messages (
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    id              TEXT NOT NULL,
    chat_jid        TEXT NOT NULL,
    sender          TEXT,
    content         TEXT,
    timestamp       TIMESTAMP NOT NULL,
    is_from_me      BOOLEAN NOT NULL DEFAULT FALSE,
    media_type      TEXT,
    filename        TEXT,
    url             TEXT,
    media_key       BLOB,
    file_sha256     BLOB,
    file_enc_sha256 BLOB,
    file_length     INTEGER,
    PRIMARY KEY (user_id, id, chat_jid),
    FOREIGN KEY (user_id, chat_jid) REFERENCES chats(user_id, jid) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS messages_user_chat_ts_idx ON messages (user_id, chat_jid, timestamp);
CREATE INDEX IF NOT EXISTS messages_user_ts_idx ON messages (user_id, timestamp);
