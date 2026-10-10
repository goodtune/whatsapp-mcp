// Package mcpserver defines the WhatsApp MCP tools and wires them to the
// per-user WhatsApp client manager and message store, using the official
// Model Context Protocol Go SDK (github.com/modelcontextprotocol/go-sdk).
//
// Every tool call is scoped to the authenticated user found in the request
// context: the HTTP layer (cmd/whatsapp-mcp) verifies the caller's JWT with
// an sdk auth.TokenVerifier and the sdk threads the resulting auth.TokenInfo
// through to each tool call.
package mcpserver

import (
	"context"
	"fmt"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/goodtune/whatsapp-mcp/internal/store"
	"github.com/goodtune/whatsapp-mcp/internal/wa"
)

type Server struct {
	mcp   *mcp.Server
	wa    *wa.Manager
	store *store.Store
}

func New(waManager *wa.Manager, st *store.Store) *Server {
	s := &Server{
		mcp:   mcp.NewServer(&mcp.Implementation{Name: "whatsapp", Version: "1.0.0"}, nil),
		wa:    waManager,
		store: st,
	}
	s.registerTools()
	return s
}

// MCPServer returns the underlying SDK server for mounting onto an HTTP transport.
func (s *Server) MCPServer() *mcp.Server {
	return s.mcp
}

// currentUserID resolves the authenticated user id from the bearer token
// info the HTTP auth middleware attached to the context.
func currentUserID(ctx context.Context) (int64, error) {
	info := auth.TokenInfoFromContext(ctx)
	if info == nil || info.UserID == "" {
		return 0, fmt.Errorf("no authenticated user in context")
	}
	userID, err := strconv.ParseInt(info.UserID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid user id in token: %w", err)
	}
	return userID, nil
}

func (s *Server) registerTools() {
	s.registerLinkingTools()
	s.registerContactAndChatTools()
	s.registerMessageTools()
	s.registerSendTools()
}

type linkWhatsAppInput struct {
	PhoneNumber string `json:"phone_number,omitempty" jsonschema:"Optional. The caller's WhatsApp phone number in international format with no '+' or spaces (e.g. 15551234567). When provided, a pairing code is issued instead of a QR code."`
}

func (s *Server) registerLinkingTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "link_whatsapp",
		Description: "Link the caller's WhatsApp account to this server. Call this (and keep calling it, " +
			"every few seconds, until it reports 'linked') to retrieve a QR code image to scan with " +
			"WhatsApp > Linked Devices, or pass phone_number to get an 8-character pairing code to " +
			"enter under Linked Devices > Link with phone number instead. The QR code and pairing " +
			"code expire after a short time, so keep calling this tool to get a fresh one until linking succeeds.",
	}, s.handleLinkWhatsApp)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "whatsapp_status",
		Description: "Check whether the caller has a WhatsApp number linked to this server, and whether it's currently connected.",
	}, s.handleWhatsAppStatus)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "whatsapp_logout",
		Description: "Unlink the caller's WhatsApp device from this server. A new number can be linked afterwards with link_whatsapp.",
	}, s.handleWhatsAppLogout)
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}
}

func (s *Server) handleLinkWhatsApp(ctx context.Context, _ *mcp.CallToolRequest, input linkWhatsAppInput) (*mcp.CallToolResult, any, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), nil, nil
	}

	state, err := s.wa.LinkDevice(ctx, userID, input.PhoneNumber)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to start linking: %v", err)), nil, nil
	}

	if state.Linked {
		return textResult(fmt.Sprintf("Already linked as %s.", state.JID)), nil, nil
	}
	if state.Error != nil {
		return errorResult(state.Error.Error()), nil, nil
	}
	if state.PairCode != "" {
		return textResult(fmt.Sprintf(
			"Enter this pairing code in WhatsApp under Settings > Linked Devices > Link with phone number: %s\n"+
				"If it expires before you enter it, call link_whatsapp again for a new one.", state.PairCode,
		)), nil, nil
	}
	if state.QRCode != "" {
		png, err := qrcode.Encode(state.QRCode, qrcode.Medium, 256)
		if err != nil {
			return textResult(fmt.Sprintf(
				"Scan this code with WhatsApp > Linked Devices > Link a Device: %s\n(failed to render it as an image: %v)",
				state.QRCode, err,
			)), nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Scan this QR code with WhatsApp > Linked Devices > Link a Device. " +
					"It refreshes periodically, so call link_whatsapp again if it expires before you scan it."},
				&mcp.ImageContent{Data: png, MIMEType: "image/png"},
			},
		}, nil, nil
	}

	return textResult("Starting WhatsApp linking, call link_whatsapp again in a couple of seconds to get the QR code."), nil, nil
}

func (s *Server) handleWhatsAppStatus(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, wa.Status, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), wa.Status{}, nil
	}

	status, err := s.wa.Status(ctx, userID)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to check status: %v", err)), wa.Status{}, nil
	}
	return nil, *status, nil
}

func (s *Server) handleWhatsAppLogout(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), nil, nil
	}

	if err := s.wa.Logout(ctx, userID); err != nil {
		return errorResult(fmt.Sprintf("failed to log out: %v", err)), nil, nil
	}
	return textResult("WhatsApp device unlinked."), nil, nil
}
