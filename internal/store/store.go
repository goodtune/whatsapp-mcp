// Package store is the Postgres-backed repository for chat and message
// history. Every row is scoped by user_id so that each logged-in user only
// ever sees their own WhatsApp data.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Message struct {
	Timestamp time.Time
	Sender    string
	ChatName  string
	Content   string
	IsFromMe  bool
	ChatJID   string
	ID        string
	MediaType string
	Filename  string
}

type Chat struct {
	JID             string
	Name            string
	LastMessageTime *time.Time
	LastMessage     *string
	LastSender      *string
	LastIsFromMe    *bool
}

func (c Chat) IsGroup() bool {
	return len(c.JID) > 5 && c.JID[len(c.JID)-5:] == "@g.us"
}

type Contact struct {
	PhoneNumber string
	Name        string
	JID         string
}

type MessageContext struct {
	Message Message
	Before  []Message
	After   []Message
}

type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// StoreChat inserts or updates a chat's name and last-message timestamp.
func (s *Store) StoreChat(ctx context.Context, userID int64, jid, name string, lastMessageTime time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chats (user_id, jid, name, last_message_time)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, jid) DO UPDATE SET
			name = EXCLUDED.name,
			last_message_time = EXCLUDED.last_message_time
	`, userID, jid, name, lastMessageTime)
	if err != nil {
		return fmt.Errorf("failed to store chat: %w", err)
	}
	return nil
}

// StoreMessage inserts or updates a message. Messages with neither text
// content nor a media attachment are silently skipped, same as the original
// bridge.
func (s *Store) StoreMessage(
	ctx context.Context,
	userID int64,
	id, chatJID, sender, content string,
	timestamp time.Time,
	isFromMe bool,
	mediaType, filename, url string,
	mediaKey, fileSHA256, fileEncSHA256 []byte,
	fileLength uint64,
) error {
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO messages
			(user_id, id, chat_jid, sender, content, timestamp, is_from_me,
			 media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (user_id, id, chat_jid) DO UPDATE SET
			sender = EXCLUDED.sender,
			content = EXCLUDED.content,
			timestamp = EXCLUDED.timestamp,
			is_from_me = EXCLUDED.is_from_me,
			media_type = EXCLUDED.media_type,
			filename = EXCLUDED.filename,
			url = EXCLUDED.url,
			media_key = EXCLUDED.media_key,
			file_sha256 = EXCLUDED.file_sha256,
			file_enc_sha256 = EXCLUDED.file_enc_sha256,
			file_length = EXCLUDED.file_length
	`, userID, id, chatJID, sender, content, timestamp, isFromMe,
		nullableString(mediaType), nullableString(filename), nullableString(url),
		nullableBytes(mediaKey), nullableBytes(fileSHA256), nullableBytes(fileEncSHA256), int64(fileLength))
	if err != nil {
		return fmt.Errorf("failed to store message: %w", err)
	}
	return nil
}

// StoreMediaInfo updates the attachment metadata of an already-stored message.
func (s *Store) StoreMediaInfo(
	ctx context.Context,
	userID int64,
	id, chatJID, url string,
	mediaKey, fileSHA256, fileEncSHA256 []byte,
	fileLength uint64,
) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE messages
		SET url = $1, media_key = $2, file_sha256 = $3, file_enc_sha256 = $4, file_length = $5
		WHERE user_id = $6 AND id = $7 AND chat_jid = $8
	`, nullableString(url), nullableBytes(mediaKey), nullableBytes(fileSHA256), nullableBytes(fileEncSHA256),
		int64(fileLength), userID, id, chatJID)
	if err != nil {
		return fmt.Errorf("failed to update media info: %w", err)
	}
	return nil
}

// GetMediaInfo returns the attachment metadata stored for a message.
func (s *Store) GetMediaInfo(ctx context.Context, userID int64, id, chatJID string) (
	mediaType, filename, url string,
	mediaKey, fileSHA256, fileEncSHA256 []byte,
	fileLength uint64,
	err error,
) {
	var mt, fn, u sql.NullString
	var fl sql.NullInt64
	err = s.db.QueryRowContext(ctx, `
		SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length
		FROM messages WHERE user_id = $1 AND id = $2 AND chat_jid = $3
	`, userID, id, chatJID).Scan(&mt, &fn, &u, &mediaKey, &fileSHA256, &fileEncSHA256, &fl)
	if err != nil {
		return "", "", "", nil, nil, nil, 0, err
	}
	return mt.String, fn.String, u.String, mediaKey, fileSHA256, fileEncSHA256, uint64(fl.Int64), nil
}

// ExistingChatName returns the name already stored for a chat, if any.
func (s *Store) ExistingChatName(ctx context.Context, userID int64, chatJID string) (string, bool, error) {
	var name sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT name FROM chats WHERE user_id = $1 AND jid = $2`, userID, chatJID).Scan(&name)
	if err == sql.ErrNoRows {
		return "", false, nil
	} else if err != nil {
		return "", false, fmt.Errorf("failed to look up chat name: %w", err)
	}
	return name.String, name.String != "", nil
}

func nullableString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func nullableBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}
