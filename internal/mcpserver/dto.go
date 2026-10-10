package mcpserver

import (
	"time"

	"github.com/goodtune/whatsapp-mcp/internal/store"
)

type chatDTO struct {
	JID             string  `json:"jid"`
	Name            string  `json:"name,omitempty"`
	IsGroup         bool    `json:"is_group"`
	LastMessageTime *string `json:"last_message_time,omitempty"`
	LastMessage     *string `json:"last_message,omitempty"`
	LastSender      *string `json:"last_sender,omitempty"`
	LastIsFromMe    *bool   `json:"last_is_from_me,omitempty"`
}

func newChatDTO(c store.Chat) chatDTO {
	dto := chatDTO{
		JID:          c.JID,
		Name:         c.Name,
		IsGroup:      c.IsGroup(),
		LastMessage:  c.LastMessage,
		LastSender:   c.LastSender,
		LastIsFromMe: c.LastIsFromMe,
	}
	if c.LastMessageTime != nil {
		s := c.LastMessageTime.Format(time.RFC3339)
		dto.LastMessageTime = &s
	}
	return dto
}

func newChatDTOs(chats []store.Chat) []chatDTO {
	dtos := make([]chatDTO, 0, len(chats))
	for _, c := range chats {
		dtos = append(dtos, newChatDTO(c))
	}
	return dtos
}

// chatsDTO wraps a chat list in an object, since the MCP structured-content
// field must be a JSON object rather than a bare array.
type chatsDTO struct {
	Chats []chatDTO `json:"chats"`
}

type contactDTO struct {
	PhoneNumber string `json:"phone_number"`
	Name        string `json:"name,omitempty"`
	JID         string `json:"jid"`
}

func newContactDTOs(contacts []store.Contact) []contactDTO {
	dtos := make([]contactDTO, 0, len(contacts))
	for _, c := range contacts {
		dtos = append(dtos, contactDTO{PhoneNumber: c.PhoneNumber, Name: c.Name, JID: c.JID})
	}
	return dtos
}

// contactsDTO wraps a contact list in an object, since the MCP
// structured-content field must be a JSON object rather than a bare array.
type contactsDTO struct {
	Contacts []contactDTO `json:"contacts"`
}

type messageDTO struct {
	Timestamp string `json:"timestamp"`
	Sender    string `json:"sender"`
	ChatName  string `json:"chat_name,omitempty"`
	Content   string `json:"content"`
	IsFromMe  bool   `json:"is_from_me"`
	ChatJID   string `json:"chat_jid"`
	ID        string `json:"id"`
	MediaType string `json:"media_type,omitempty"`
}

func newMessageDTO(m store.Message) messageDTO {
	return messageDTO{
		Timestamp: m.Timestamp.Format(time.RFC3339),
		Sender:    m.Sender,
		ChatName:  m.ChatName,
		Content:   m.Content,
		IsFromMe:  m.IsFromMe,
		ChatJID:   m.ChatJID,
		ID:        m.ID,
		MediaType: m.MediaType,
	}
}

func newMessageDTOs(messages []store.Message) []messageDTO {
	dtos := make([]messageDTO, 0, len(messages))
	for _, m := range messages {
		dtos = append(dtos, newMessageDTO(m))
	}
	return dtos
}

type messageContextDTO struct {
	Message messageDTO   `json:"message"`
	Before  []messageDTO `json:"before"`
	After   []messageDTO `json:"after"`
}

func newMessageContextDTO(c *store.MessageContext) messageContextDTO {
	return messageContextDTO{
		Message: newMessageDTO(c.Message),
		Before:  newMessageDTOs(c.Before),
		After:   newMessageDTOs(c.After),
	}
}

type sendResultDTO struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type downloadResultDTO struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	FilePath string `json:"file_path,omitempty"`
}
