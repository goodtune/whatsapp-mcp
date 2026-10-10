package mcpserver

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) registerContactAndChatTools() {
	s.mcp.AddTool(
		mcp.NewTool("search_contacts",
			mcp.WithDescription("Search the caller's WhatsApp contacts by name or phone number."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search term to match against contact names or phone numbers")),
		),
		s.handleSearchContacts,
	)

	s.mcp.AddTool(
		mcp.NewTool("list_chats",
			mcp.WithDescription("List the caller's WhatsApp chats, most recently active first by default."),
			mcp.WithString("query", mcp.Description("Optional search term to filter chats by name or JID")),
			mcp.WithNumber("limit", mcp.DefaultNumber(20), mcp.Description("Maximum number of chats to return")),
			mcp.WithNumber("page", mcp.DefaultNumber(0), mcp.Description("Page number for pagination")),
			mcp.WithBoolean("include_last_message", mcp.DefaultBool(true), mcp.Description("Whether to include the last message in each chat")),
			mcp.WithString("sort_by", mcp.DefaultString("last_active"), mcp.Description("Either 'last_active' or 'name'")),
		),
		s.handleListChats,
	)

	s.mcp.AddTool(
		mcp.NewTool("get_chat",
			mcp.WithDescription("Get WhatsApp chat metadata by JID."),
			mcp.WithString("chat_jid", mcp.Required(), mcp.Description("The JID of the chat to retrieve")),
			mcp.WithBoolean("include_last_message", mcp.DefaultBool(true), mcp.Description("Whether to include the last message")),
		),
		s.handleGetChat,
	)

	s.mcp.AddTool(
		mcp.NewTool("get_direct_chat_by_contact",
			mcp.WithDescription("Get WhatsApp direct-chat metadata by the contact's phone number."),
			mcp.WithString("sender_phone_number", mcp.Required(), mcp.Description("The phone number to search for")),
		),
		s.handleGetDirectChatByContact,
	)

	s.mcp.AddTool(
		mcp.NewTool("get_contact_chats",
			mcp.WithDescription("List every WhatsApp chat involving a given contact."),
			mcp.WithString("jid", mcp.Required(), mcp.Description("The contact's JID to search for")),
			mcp.WithNumber("limit", mcp.DefaultNumber(20), mcp.Description("Maximum number of chats to return")),
			mcp.WithNumber("page", mcp.DefaultNumber(0), mcp.Description("Page number for pagination")),
		),
		s.handleGetContactChats,
	)

	s.mcp.AddTool(
		mcp.NewTool("get_last_interaction",
			mcp.WithDescription("Get the most recent WhatsApp message involving a contact."),
			mcp.WithString("jid", mcp.Required(), mcp.Description("The JID of the contact to search for")),
		),
		s.handleGetLastInteraction,
	)
}

func (s *Server) handleSearchContacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	query := req.GetString("query", "")

	contacts, err := s.store.SearchContacts(ctx, userID, query)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to search contacts", err), nil
	}
	return jsonResult(newContactDTOs(contacts))
}

func (s *Server) handleListChats(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	query := req.GetString("query", "")
	limit := req.GetInt("limit", 20)
	page := req.GetInt("page", 0)
	includeLastMessage := req.GetBool("include_last_message", true)
	sortByName := req.GetString("sort_by", "last_active") == "name"

	chats, err := s.store.ListChats(ctx, userID, query, limit, page, includeLastMessage, sortByName)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to list chats", err), nil
	}
	return jsonResult(newChatDTOs(chats))
}

func (s *Server) handleGetChat(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	chatJID := req.GetString("chat_jid", "")
	includeLastMessage := req.GetBool("include_last_message", true)

	chat, err := s.store.GetChat(ctx, userID, chatJID, includeLastMessage)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to get chat", err), nil
	}
	if chat == nil {
		return mcp.NewToolResultText("null"), nil
	}
	return jsonResult(newChatDTO(*chat))
}

func (s *Server) handleGetDirectChatByContact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	phoneNumber := req.GetString("sender_phone_number", "")

	chat, err := s.store.GetDirectChatByContact(ctx, userID, phoneNumber)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to get chat", err), nil
	}
	if chat == nil {
		return mcp.NewToolResultText("null"), nil
	}
	return jsonResult(newChatDTO(*chat))
}

func (s *Server) handleGetContactChats(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	jid := req.GetString("jid", "")
	limit := req.GetInt("limit", 20)
	page := req.GetInt("page", 0)

	chats, err := s.store.GetContactChats(ctx, userID, jid, limit, page)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to get contact chats", err), nil
	}
	return jsonResult(newChatDTOs(chats))
}

func (s *Server) handleGetLastInteraction(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	jid := req.GetString("jid", "")

	msg, err := s.store.GetLastInteraction(ctx, userID, jid)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to get last interaction", err), nil
	}
	if msg == nil {
		return mcp.NewToolResultText("No interactions found."), nil
	}
	return mcp.NewToolResultText(s.store.FormatMessage(ctx, userID, *msg, true)), nil
}
