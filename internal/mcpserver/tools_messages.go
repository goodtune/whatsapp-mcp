package mcpserver

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/goodtune/whatsapp-mcp/internal/store"
)

func (s *Server) registerMessageTools() {
	s.mcp.AddTool(
		mcp.NewTool("list_messages",
			mcp.WithDescription("Get WhatsApp messages matching the specified criteria, with optional surrounding context."),
			mcp.WithString("after", mcp.Description("Optional ISO-8601 timestamp; only return messages after this time")),
			mcp.WithString("before", mcp.Description("Optional ISO-8601 timestamp; only return messages before this time")),
			mcp.WithString("sender_phone_number", mcp.Description("Optional phone number to filter messages by sender")),
			mcp.WithString("chat_jid", mcp.Description("Optional chat JID to filter messages by chat")),
			mcp.WithString("query", mcp.Description("Optional search term to filter messages by content")),
			mcp.WithNumber("limit", mcp.DefaultNumber(20), mcp.Description("Maximum number of messages to return")),
			mcp.WithNumber("page", mcp.DefaultNumber(0), mcp.Description("Page number for pagination")),
			mcp.WithBoolean("include_context", mcp.DefaultBool(true), mcp.Description("Whether to include messages before and after each match")),
			mcp.WithNumber("context_before", mcp.DefaultNumber(1), mcp.Description("Number of messages of context to include before each match")),
			mcp.WithNumber("context_after", mcp.DefaultNumber(1), mcp.Description("Number of messages of context to include after each match")),
		),
		s.handleListMessages,
	)

	s.mcp.AddTool(
		mcp.NewTool("get_message_context",
			mcp.WithDescription("Get the messages immediately before and after a specific WhatsApp message."),
			mcp.WithString("message_id", mcp.Required(), mcp.Description("The ID of the message to get context for")),
			mcp.WithNumber("before", mcp.DefaultNumber(5), mcp.Description("Number of messages to include before the target message")),
			mcp.WithNumber("after", mcp.DefaultNumber(5), mcp.Description("Number of messages to include after the target message")),
		),
		s.handleGetMessageContext,
	)
}

func optionalString(req mcp.CallToolRequest, key string) *string {
	v := req.GetString(key, "")
	if v == "" {
		return nil
	}
	return &v
}

func (s *Server) handleListMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	filter := store.ListMessagesFilter{
		After:             optionalString(req, "after"),
		Before:            optionalString(req, "before"),
		SenderPhoneNumber: optionalString(req, "sender_phone_number"),
		ChatJID:           optionalString(req, "chat_jid"),
		Query:             optionalString(req, "query"),
		Limit:             req.GetInt("limit", 20),
		Page:              req.GetInt("page", 0),
	}

	messages, err := s.store.ListMessages(ctx, userID, filter)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to list messages", err), nil
	}

	if !req.GetBool("include_context", true) || len(messages) == 0 {
		return mcp.NewToolResultText(s.store.FormatMessagesList(ctx, userID, messages, true)), nil
	}

	contextBefore := req.GetInt("context_before", 1)
	contextAfter := req.GetInt("context_after", 1)

	var withContext []store.Message
	for _, m := range messages {
		msgContext, err := s.store.GetMessageContext(ctx, userID, m.ID, contextBefore, contextAfter)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to load message context", err), nil
		}
		withContext = append(withContext, msgContext.Before...)
		withContext = append(withContext, msgContext.Message)
		withContext = append(withContext, msgContext.After...)
	}

	return mcp.NewToolResultText(s.store.FormatMessagesList(ctx, userID, withContext, true)), nil
}

func (s *Server) handleGetMessageContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	messageID := req.GetString("message_id", "")
	before := req.GetInt("before", 5)
	after := req.GetInt("after", 5)

	msgContext, err := s.store.GetMessageContext(ctx, userID, messageID, before, after)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to get message context", err), nil
	}
	return jsonResult(newMessageContextDTO(msgContext))
}
