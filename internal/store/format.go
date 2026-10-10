package store

import (
	"context"
	"fmt"
)

// FormatMessage renders a single message as a human-readable line, mirroring
// the WhatsApp MCP server's original CLI-style output.
func (s *Store) FormatMessage(ctx context.Context, userID int64, m Message, showChatInfo bool) string {
	var out string
	if showChatInfo && m.ChatName != "" {
		out += fmt.Sprintf("[%s] Chat: %s ", m.Timestamp.Format("2006-01-02 15:04:05"), m.ChatName)
	} else {
		out += fmt.Sprintf("[%s] ", m.Timestamp.Format("2006-01-02 15:04:05"))
	}

	contentPrefix := ""
	if m.MediaType != "" {
		contentPrefix = fmt.Sprintf("[%s - Message ID: %s - Chat JID: %s] ", m.MediaType, m.ID, m.ChatJID)
	}

	senderName := "Me"
	if !m.IsFromMe {
		senderName = s.GetSenderName(ctx, userID, m.Sender)
	}

	out += fmt.Sprintf("From: %s: %s%s\n", senderName, contentPrefix, m.Content)
	return out
}

// FormatMessagesList renders a sequence of messages as the original server did.
func (s *Store) FormatMessagesList(ctx context.Context, userID int64, messages []Message, showChatInfo bool) string {
	if len(messages) == 0 {
		return "No messages to display."
	}
	out := ""
	for _, m := range messages {
		out += s.FormatMessage(ctx, userID, m, showChatInfo)
	}
	return out
}
