package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) registerSendTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "send_message",
		Description: "Send a WhatsApp text message to a person or group. For group chats use the JID.",
	}, s.handleSendMessage)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "send_file",
		Description: "Send a file (image, raw audio, video, or document) via WhatsApp. For group chats use the JID.",
	}, s.handleSendFile)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "send_audio_message",
		Description: "Send an audio file as a playable WhatsApp voice message. For group chats use the JID. " +
			"Non-Ogg files are converted with ffmpeg; if that fails because ffmpeg isn't installed, use send_file instead.",
	}, s.handleSendAudioMessage)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "download_media",
		Description: "Download media from a WhatsApp message and get the local file path.",
	}, s.handleDownloadMedia)
}

type sendMessageInput struct {
	Recipient string `json:"recipient" jsonschema:"The recipient - either a phone number with country code but no '+' or other symbols, or a JID (e.g. \"123456789@s.whatsapp.net\" or a group JID like \"123456789@g.us\")"`
	Message   string `json:"message" jsonschema:"The message text to send"`
}

func (s *Server) handleSendMessage(ctx context.Context, _ *mcp.CallToolRequest, input sendMessageInput) (*mcp.CallToolResult, sendResultDTO, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), sendResultDTO{}, nil
	}

	if input.Recipient == "" {
		return nil, sendResultDTO{Success: false, Message: "recipient must be provided"}, nil
	}

	success, message := s.wa.SendMessage(ctx, userID, input.Recipient, input.Message)
	return nil, sendResultDTO{Success: success, Message: message}, nil
}

type sendFileInput struct {
	Recipient string `json:"recipient" jsonschema:"The recipient - either a phone number with country code but no '+' or other symbols, or a JID (e.g. \"123456789@s.whatsapp.net\" or a group JID like \"123456789@g.us\")"`
	MediaPath string `json:"media_path" jsonschema:"Absolute path, accessible to this server, to the media file to send"`
}

func (s *Server) handleSendFile(ctx context.Context, _ *mcp.CallToolRequest, input sendFileInput) (*mcp.CallToolResult, sendResultDTO, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), sendResultDTO{}, nil
	}

	success, message := s.wa.SendFile(ctx, userID, input.Recipient, input.MediaPath)
	return nil, sendResultDTO{Success: success, Message: message}, nil
}

type sendAudioMessageInput struct {
	Recipient string `json:"recipient" jsonschema:"The recipient - either a phone number with country code but no '+' or other symbols, or a JID (e.g. \"123456789@s.whatsapp.net\" or a group JID like \"123456789@g.us\")"`
	MediaPath string `json:"media_path" jsonschema:"Absolute path, accessible to this server, to the audio file to send"`
}

func (s *Server) handleSendAudioMessage(ctx context.Context, _ *mcp.CallToolRequest, input sendAudioMessageInput) (*mcp.CallToolResult, sendResultDTO, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), sendResultDTO{}, nil
	}

	success, message := s.wa.SendAudioMessage(ctx, userID, input.Recipient, input.MediaPath)
	return nil, sendResultDTO{Success: success, Message: message}, nil
}

type downloadMediaInput struct {
	MessageID string `json:"message_id" jsonschema:"The ID of the message containing the media"`
	ChatJID   string `json:"chat_jid" jsonschema:"The JID of the chat containing the message"`
}

func (s *Server) handleDownloadMedia(ctx context.Context, _ *mcp.CallToolRequest, input downloadMediaInput) (*mcp.CallToolResult, downloadResultDTO, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return errorResult(err.Error()), downloadResultDTO{}, nil
	}

	_, _, filePath, err := s.wa.DownloadMedia(ctx, userID, input.MessageID, input.ChatJID)
	if err != nil {
		return nil, downloadResultDTO{Success: false, Message: err.Error()}, nil
	}
	return nil, downloadResultDTO{Success: true, Message: "media downloaded successfully", FilePath: filePath}, nil
}
