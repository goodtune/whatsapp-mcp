package wa

import (
	"context"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// registerHandlers wires up the per-client event handler that stores
// incoming messages and history-sync blobs for the given user, and promotes
// a pairing session to a linked, active client once it connects.
func (m *Manager) registerHandlers(client *whatsmeow.Client, userID int64) {
	client.AddEventHandler(func(evt any) {
		ctx := context.Background()

		switch v := evt.(type) {
		case *events.Message:
			m.handleMessage(ctx, userID, client, v)

		case *events.HistorySync:
			m.handleHistorySync(ctx, userID, client, v)

		case *events.Connected:
			if client.Store.ID != nil {
				m.mu.Lock()
				_, pending := m.pending[userID]
				m.mu.Unlock()
				if pending {
					m.onLinked(ctx, userID, client)
				}
			}

		case *events.LoggedOut:
			m.logger.Warnf("user %d's WhatsApp device was logged out; call link_whatsapp to relink", userID)
			m.mu.Lock()
			delete(m.clients, userID)
			m.mu.Unlock()
		}
	})
}

func extractTextContent(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	if text := msg.GetConversation(); text != "" {
		return text
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	return ""
}

func extractMediaInfo(msg *waE2E.Message) (mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	ts := time.Now().Format("20060102_150405")

	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + ts + ".jpg", img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + ts + ".mp4", vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + ts + ".ogg", aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + ts
		}
		return "document", filename, doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}

// chatName determines the display name for a chat, preferring any name
// already stored, then group metadata or contact info, falling back to the
// sender or the raw JID.
func (m *Manager) chatName(ctx context.Context, userID int64, client *whatsmeow.Client, jid types.JID, chatJID string, conv *waHistorySync.Conversation, sender string) string {
	if name, ok, _ := m.store.ExistingChatName(ctx, userID, chatJID); ok {
		return name
	}

	if jid.Server == types.GroupServer {
		if conv != nil && conv.GetName() != "" {
			return conv.GetName()
		}
		if info, err := client.GetGroupInfo(ctx, jid); err == nil && info.Name != "" {
			return info.Name
		}
		return fmt.Sprintf("Group %s", jid.User)
	}

	if contact, err := client.Store.Contacts.GetContact(ctx, jid); err == nil && contact.FullName != "" {
		return contact.FullName
	}
	if sender != "" {
		return sender
	}
	return jid.User
}

func (m *Manager) handleMessage(ctx context.Context, userID int64, client *whatsmeow.Client, msg *events.Message) {
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	name := m.chatName(ctx, userID, client, msg.Info.Chat, chatJID, nil, sender)
	if err := m.store.StoreChat(ctx, userID, chatJID, name, msg.Info.Timestamp); err != nil {
		m.logger.Warnf("failed to store chat: %v", err)
	}

	content := extractTextContent(msg.Message)
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)
	if content == "" && mediaType == "" {
		return
	}

	err := m.store.StoreMessage(ctx, userID, msg.Info.ID, chatJID, sender, content, msg.Info.Timestamp, msg.Info.IsFromMe,
		mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength)
	if err != nil {
		m.logger.Warnf("failed to store message: %v", err)
	}
}

func (m *Manager) handleHistorySync(ctx context.Context, userID int64, client *whatsmeow.Client, historySync *events.HistorySync) {
	synced := 0
	for _, conversation := range historySync.Data.GetConversations() {
		chatJID := conversation.GetID()
		if chatJID == "" {
			continue
		}

		jid, err := types.ParseJID(chatJID)
		if err != nil {
			m.logger.Warnf("failed to parse JID %s: %v", chatJID, err)
			continue
		}

		name := m.chatName(ctx, userID, client, jid, chatJID, conversation, "")

		messages := conversation.GetMessages()
		if len(messages) == 0 {
			continue
		}

		latest := messages[0].GetMessage()
		if latest == nil || latest.GetMessageTimestamp() == 0 {
			continue
		}
		if err := m.store.StoreChat(ctx, userID, chatJID, name, time.Unix(int64(latest.GetMessageTimestamp()), 0)); err != nil {
			m.logger.Warnf("failed to store chat: %v", err)
			continue
		}

		for _, histMsg := range messages {
			webMsg := histMsg.GetMessage()
			if webMsg == nil {
				continue
			}

			content := extractTextContent(webMsg.GetMessage())
			mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(webMsg.GetMessage())
			if content == "" && mediaType == "" {
				continue
			}

			key := webMsg.GetKey()
			isFromMe := key.GetFromMe()
			var sender string
			if !isFromMe && key.GetParticipant() != "" {
				sender = key.GetParticipant()
			} else if isFromMe {
				if client.Store.ID != nil {
					sender = client.Store.ID.User
				}
			} else {
				sender = jid.User
			}

			timestamp := webMsg.GetMessageTimestamp()
			if timestamp == 0 {
				continue
			}

			err := m.store.StoreMessage(ctx, userID, key.GetID(), chatJID, sender, content,
				time.Unix(int64(timestamp), 0), isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength)
			if err != nil {
				m.logger.Warnf("failed to store history message: %v", err)
				continue
			}
			synced++
		}
	}

	m.logger.Infof("history sync for user %d complete: stored %d messages", userID, synced)
}
