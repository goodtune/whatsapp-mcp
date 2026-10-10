package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func intOrDefault(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

func boolOrDefault(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func (s *Server) registerContactAndChatTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "search_contacts",
		Description: "Search the caller's WhatsApp contacts by name or phone number.",
	}, s.handleSearchContacts)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_chats",
		Description: "List the caller's WhatsApp chats, most recently active first by default.",
	}, s.handleListChats)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "get_chat",
		Description: "Get WhatsApp chat metadata by JID.",
	}, s.handleGetChat)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "get_direct_chat_by_contact",
		Description: "Get WhatsApp direct-chat metadata by the contact's phone number.",
	}, s.handleGetDirectChatByContact)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "get_contact_chats",
		Description: "List every WhatsApp chat involving a given contact.",
	}, s.handleGetContactChats)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "get_last_interaction",
		Description: "Get the most recent WhatsApp message involving a contact.",
	}, s.handleGetLastInteraction)
}

type searchContactsInput struct {
	Query string `json:"query" jsonschema:"Search term to match against contact names or phone numbers"`
}

func (s *Server) handleSearchContacts(ctx context.Context, _ *mcp.CallToolRequest, input searchContactsInput) (*mcp.CallToolResult, contactsDTO, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), contactsDTO{}, nil
	}

	contacts, err := s.store.SearchContacts(ctx, userID, input.Query)
	if err != nil {
		return nil, contactsDTO{}, fmt.Errorf("failed to search contacts: %w", err)
	}
	return nil, contactsDTO{Contacts: newContactDTOs(contacts)}, nil
}

type listChatsInput struct {
	Query              string `json:"query,omitempty" jsonschema:"Optional search term to filter chats by name or JID"`
	Limit              *int   `json:"limit,omitempty" jsonschema:"Maximum number of chats to return (default 20)"`
	Page               *int   `json:"page,omitempty" jsonschema:"Page number for pagination (default 0)"`
	IncludeLastMessage *bool  `json:"include_last_message,omitempty" jsonschema:"Whether to include the last message in each chat (default true)"`
	SortBy             string `json:"sort_by,omitempty" jsonschema:"Either 'last_active' or 'name' (default last_active)"`
}

func (s *Server) handleListChats(ctx context.Context, _ *mcp.CallToolRequest, input listChatsInput) (*mcp.CallToolResult, chatsDTO, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), chatsDTO{}, nil
	}

	chats, err := s.store.ListChats(ctx, userID, input.Query,
		intOrDefault(input.Limit, 20), intOrDefault(input.Page, 0),
		boolOrDefault(input.IncludeLastMessage, true), input.SortBy == "name")
	if err != nil {
		return nil, chatsDTO{}, fmt.Errorf("failed to list chats: %w", err)
	}
	return nil, chatsDTO{Chats: newChatDTOs(chats)}, nil
}

type getChatInput struct {
	ChatJID            string `json:"chat_jid" jsonschema:"The JID of the chat to retrieve"`
	IncludeLastMessage *bool  `json:"include_last_message,omitempty" jsonschema:"Whether to include the last message (default true)"`
}

func (s *Server) handleGetChat(ctx context.Context, _ *mcp.CallToolRequest, input getChatInput) (*mcp.CallToolResult, *chatDTO, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), nil, nil
	}

	chat, err := s.store.GetChat(ctx, userID, input.ChatJID, boolOrDefault(input.IncludeLastMessage, true))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get chat: %w", err)
	}
	if chat == nil {
		return textResult("No chat found with that JID."), nil, nil
	}
	dto := newChatDTO(*chat)
	return nil, &dto, nil
}

type getDirectChatByContactInput struct {
	SenderPhoneNumber string `json:"sender_phone_number" jsonschema:"The phone number to search for"`
}

func (s *Server) handleGetDirectChatByContact(ctx context.Context, _ *mcp.CallToolRequest, input getDirectChatByContactInput) (*mcp.CallToolResult, *chatDTO, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), nil, nil
	}

	chat, err := s.store.GetDirectChatByContact(ctx, userID, input.SenderPhoneNumber)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get chat: %w", err)
	}
	if chat == nil {
		return textResult("No direct chat found with that contact."), nil, nil
	}
	dto := newChatDTO(*chat)
	return nil, &dto, nil
}

type getContactChatsInput struct {
	JID   string `json:"jid" jsonschema:"The contact's JID to search for"`
	Limit *int   `json:"limit,omitempty" jsonschema:"Maximum number of chats to return (default 20)"`
	Page  *int   `json:"page,omitempty" jsonschema:"Page number for pagination (default 0)"`
}

func (s *Server) handleGetContactChats(ctx context.Context, _ *mcp.CallToolRequest, input getContactChatsInput) (*mcp.CallToolResult, chatsDTO, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), chatsDTO{}, nil
	}

	chats, err := s.store.GetContactChats(ctx, userID, input.JID, intOrDefault(input.Limit, 20), intOrDefault(input.Page, 0))
	if err != nil {
		return nil, chatsDTO{}, fmt.Errorf("failed to get contact chats: %w", err)
	}
	return nil, chatsDTO{Chats: newChatDTOs(chats)}, nil
}

type getLastInteractionInput struct {
	JID string `json:"jid" jsonschema:"The JID of the contact to search for"`
}

func (s *Server) handleGetLastInteraction(ctx context.Context, _ *mcp.CallToolRequest, input getLastInteractionInput) (*mcp.CallToolResult, any, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), nil, nil
	}

	msg, err := s.store.GetLastInteraction(ctx, userID, input.JID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get last interaction: %w", err)
	}
	if msg == nil {
		return textResult("No interactions found."), nil, nil
	}
	return textResult(s.store.FormatMessage(ctx, userID, *msg, true)), nil, nil
}
