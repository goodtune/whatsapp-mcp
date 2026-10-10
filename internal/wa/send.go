package wa

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/goodtune/whatsapp-mcp/internal/audioconv"
)

// SendMessage sends a plain text message to a contact or group.
func (m *Manager) SendMessage(ctx context.Context, userID int64, recipient, message string) (bool, string) {
	client, err := m.getClient(ctx, userID)
	if err != nil {
		return false, err.Error()
	}
	return m.sendWhatsAppMessage(ctx, client, recipient, message, "", false)
}

// SendFile sends a file (image, video, document, or raw audio) to a contact or group.
func (m *Manager) SendFile(ctx context.Context, userID int64, recipient, mediaPath string) (bool, string) {
	client, err := m.getClient(ctx, userID)
	if err != nil {
		return false, err.Error()
	}
	if _, err := os.Stat(mediaPath); err != nil {
		return false, fmt.Sprintf("media file not found: %s", mediaPath)
	}
	return m.sendWhatsAppMessage(ctx, client, recipient, "", mediaPath, false)
}

// SendAudioMessage sends an audio file as a playable WhatsApp voice message,
// converting it to Opus/Ogg with ffmpeg first if necessary.
func (m *Manager) SendAudioMessage(ctx context.Context, userID int64, recipient, mediaPath string) (bool, string) {
	client, err := m.getClient(ctx, userID)
	if err != nil {
		return false, err.Error()
	}
	if _, err := os.Stat(mediaPath); err != nil {
		return false, fmt.Sprintf("media file not found: %s", mediaPath)
	}

	if !strings.HasSuffix(strings.ToLower(mediaPath), ".ogg") {
		converted, err := audioconv.ConvertToOpusOggTemp(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("error converting file to opus ogg, you likely need to install ffmpeg: %v", err)
		}
		defer os.Remove(converted)
		mediaPath = converted
	}

	return m.sendWhatsAppMessage(ctx, client, recipient, "", mediaPath, true)
}

func (m *Manager) sendWhatsAppMessage(ctx context.Context, client *whatsmeow.Client, recipient, message, mediaPath string, forceVoice bool) (bool, string) {
	if !client.IsConnected() {
		return false, "not connected to WhatsApp"
	}

	var recipientJID types.JID
	var err error
	if strings.Contains(recipient, "@") {
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("error parsing JID: %v", err)
		}
	} else {
		recipientJID = types.JID{User: recipient, Server: types.DefaultUserServer}
	}

	msg := &waE2E.Message{}

	if mediaPath != "" {
		mediaData, err := os.ReadFile(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("error reading media file: %v", err)
		}

		mediaType, mimeType := classifyMedia(mediaPath, forceVoice)

		resp, err := client.Upload(ctx, mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("error uploading media: %v", err)
		}

		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waE2E.ImageMessage{
				Caption: proto.String(message), Mimetype: proto.String(mimeType),
				URL: &resp.URL, DirectPath: &resp.DirectPath, MediaKey: resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256, FileSHA256: resp.FileSHA256, FileLength: &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			var seconds uint32 = 30
			var waveform []byte
			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := audioconv.AnalyzeOggOpus(mediaData)
				if err != nil {
					return false, fmt.Sprintf("failed to analyze Ogg Opus file: %v", err)
				}
				seconds, waveform = analyzedSeconds, analyzedWaveform
			}
			msg.AudioMessage = &waE2E.AudioMessage{
				Mimetype: proto.String(mimeType),
				URL:      &resp.URL, DirectPath: &resp.DirectPath, MediaKey: resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256, FileSHA256: resp.FileSHA256, FileLength: &resp.FileLength,
				Seconds: proto.Uint32(seconds), PTT: proto.Bool(true), Waveform: waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waE2E.VideoMessage{
				Caption: proto.String(message), Mimetype: proto.String(mimeType),
				URL: &resp.URL, DirectPath: &resp.DirectPath, MediaKey: resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256, FileSHA256: resp.FileSHA256, FileLength: &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = &waE2E.DocumentMessage{
				Title: proto.String(filepath.Base(mediaPath)), Caption: proto.String(message), Mimetype: proto.String(mimeType),
				URL: &resp.URL, DirectPath: &resp.DirectPath, MediaKey: resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256, FileSHA256: resp.FileSHA256, FileLength: &resp.FileLength,
			}
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	if _, err := client.SendMessage(ctx, recipientJID, msg); err != nil {
		return false, fmt.Sprintf("error sending message: %v", err)
	}

	return true, fmt.Sprintf("message sent to %s", recipient)
}

func classifyMedia(mediaPath string, forceVoice bool) (whatsmeow.MediaType, string) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(mediaPath), "."))

	switch ext {
	case "jpg", "jpeg":
		return whatsmeow.MediaImage, "image/jpeg"
	case "png":
		return whatsmeow.MediaImage, "image/png"
	case "gif":
		return whatsmeow.MediaImage, "image/gif"
	case "webp":
		return whatsmeow.MediaImage, "image/webp"
	case "ogg":
		return whatsmeow.MediaAudio, "audio/ogg; codecs=opus"
	case "mp4":
		if forceVoice {
			return whatsmeow.MediaAudio, "audio/mp4"
		}
		return whatsmeow.MediaVideo, "video/mp4"
	case "avi":
		return whatsmeow.MediaVideo, "video/avi"
	case "mov":
		return whatsmeow.MediaVideo, "video/quicktime"
	default:
		if forceVoice {
			return whatsmeow.MediaAudio, "audio/ogg; codecs=opus"
		}
		return whatsmeow.MediaDocument, "application/octet-stream"
	}
}
