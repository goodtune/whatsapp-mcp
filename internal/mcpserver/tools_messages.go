package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/goodtune/whatsapp-mcp/internal/store"
)

func (s *Server) registerMessageTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_messages",
		Description: "Get WhatsApp messages matching the specified criteria, with optional surrounding context.",
		Title:       "List messages",
		Annotations: readOnlyAnnotations("List messages"),
	}, s.handleListMessages)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "get_message_context",
		Description: "Get the messages immediately before and after a specific WhatsApp message.",
		Title:       "Get message context",
		Annotations: readOnlyAnnotations("Get message context"),
	}, s.handleGetMessageContext)
}

type listMessagesInput struct {
	After             string `json:"after,omitempty" jsonschema:"Optional ISO-8601 timestamp; only return messages after this time"`
	Before            string `json:"before,omitempty" jsonschema:"Optional ISO-8601 timestamp; only return messages before this time"`
	SenderPhoneNumber string `json:"sender_phone_number,omitempty" jsonschema:"Optional phone number to filter messages by sender"`
	ChatJID           string `json:"chat_jid,omitempty" jsonschema:"Optional chat JID to filter messages by chat"`
	Query             string `json:"query,omitempty" jsonschema:"Optional search term to filter messages by content"`
	Limit             *int   `json:"limit,omitempty" jsonschema:"Maximum number of messages to return (default 20)"`
	Page              *int   `json:"page,omitempty" jsonschema:"Page number for pagination (default 0)"`
	IncludeContext    *bool  `json:"include_context,omitempty" jsonschema:"Whether to include messages before and after each match (default true)"`
	ContextBefore     *int   `json:"context_before,omitempty" jsonschema:"Number of messages of context to include before each match (default 1)"`
	ContextAfter      *int   `json:"context_after,omitempty" jsonschema:"Number of messages of context to include after each match (default 1)"`
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *Server) handleListMessages(ctx context.Context, _ *mcp.CallToolRequest, input listMessagesInput) (*mcp.CallToolResult, any, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), nil, nil
	}

	filter := store.ListMessagesFilter{
		After:             optionalString(input.After),
		Before:            optionalString(input.Before),
		SenderPhoneNumber: optionalString(input.SenderPhoneNumber),
		ChatJID:           optionalString(input.ChatJID),
		Query:             optionalString(input.Query),
		Limit:             intOrDefault(input.Limit, 20),
		Page:              intOrDefault(input.Page, 0),
	}

	messages, err := s.store.ListMessages(ctx, userID, filter)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list messages: %w", err)
	}

	if !boolOrDefault(input.IncludeContext, true) || len(messages) == 0 {
		return textResult(s.store.FormatMessagesList(ctx, userID, messages, true)), nil, nil
	}

	contextBefore := intOrDefault(input.ContextBefore, 1)
	contextAfter := intOrDefault(input.ContextAfter, 1)

	var withContext []store.Message
	for _, m := range messages {
		msgContext, err := s.store.GetMessageContext(ctx, userID, m.ID, contextBefore, contextAfter)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load message context: %w", err)
		}
		withContext = append(withContext, msgContext.Before...)
		withContext = append(withContext, msgContext.Message)
		withContext = append(withContext, msgContext.After...)
	}

	return textResult(s.store.FormatMessagesList(ctx, userID, withContext, true)), nil, nil
}

type getMessageContextInput struct {
	MessageID string `json:"message_id" jsonschema:"The ID of the message to get context for"`
	Before    *int   `json:"before,omitempty" jsonschema:"Number of messages to include before the target message (default 5)"`
	After     *int   `json:"after,omitempty" jsonschema:"Number of messages to include after the target message (default 5)"`
}

func (s *Server) handleGetMessageContext(ctx context.Context, _ *mcp.CallToolRequest, input getMessageContextInput) (*mcp.CallToolResult, *messageContextDTO, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), nil, nil
	}

	msgContext, err := s.store.GetMessageContext(ctx, userID, input.MessageID, intOrDefault(input.Before, 5), intOrDefault(input.After, 5))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get message context: %w", err)
	}
	dto := newMessageContextDTO(msgContext)
	return nil, &dto, nil
}
