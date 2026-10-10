package wa

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow"

	"github.com/goodtune/whatsapp-mcp/internal/audioconv"
)

// mediaDownloader implements whatsmeow.DownloadableMessage and
// whatsmeow.MediaTypeable so previously-received media can be re-downloaded
// from the metadata stored at receipt time, without needing the original
// protobuf message.
type mediaDownloader struct {
	directPath    string
	mediaKey      []byte
	fileSHA256    []byte
	fileEncSHA256 []byte
	mediaType     whatsmeow.MediaType
}

func (d *mediaDownloader) GetDirectPath() string             { return d.directPath }
func (d *mediaDownloader) GetMediaKey() []byte               { return d.mediaKey }
func (d *mediaDownloader) GetFileSHA256() []byte             { return d.fileSHA256 }
func (d *mediaDownloader) GetFileEncSHA256() []byte          { return d.fileEncSHA256 }
func (d *mediaDownloader) GetMediaType() whatsmeow.MediaType { return d.mediaType }

// DownloadMedia downloads (or returns the already-downloaded copy of) the
// media attached to a message, returning its media type, filename, and the
// absolute local path it was saved to.
func (m *Manager) DownloadMedia(ctx context.Context, userID int64, messageID, chatJID string) (mediaType, filename, absPath string, err error) {
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err := m.store.GetMediaInfo(ctx, userID, messageID, chatJID)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to find message: %w", err)
	}
	if mediaType == "" {
		return "", "", "", fmt.Errorf("not a media message")
	}

	chatDir := filepath.Join(m.mediaDir, strconv.FormatInt(userID, 10), strings.ReplaceAll(chatJID, ":", "_"))
	localPath := filepath.Join(chatDir, audioconv.SanitizeFilename(filename))

	if _, statErr := os.Stat(localPath); statErr == nil {
		abs, _ := filepath.Abs(localPath)
		return mediaType, filename, abs, nil
	}

	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return "", "", "", fmt.Errorf("incomplete media information for download")
	}

	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	client, err := m.getClient(ctx, userID)
	if err != nil {
		return "", "", "", err
	}

	downloader := &mediaDownloader{
		directPath:    extractDirectPathFromURL(url),
		mediaKey:      mediaKey,
		fileSHA256:    fileSHA256,
		fileEncSHA256: fileEncSHA256,
		mediaType:     waMediaType,
	}

	data, err := client.Download(ctx, downloader)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to download media: %w", err)
	}

	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		return "", "", "", fmt.Errorf("failed to create chat directory: %w", err)
	}
	if err := os.WriteFile(localPath, data, 0o644); err != nil {
		return "", "", "", fmt.Errorf("failed to save media file: %w", err)
	}

	abs, err := filepath.Abs(localPath)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get absolute path: %w", err)
	}

	return mediaType, filename, abs, nil
}

// extractDirectPathFromURL recovers the WhatsApp media server's direct path
// from a previously-stored media URL (e.g.
// "https://mmg.whatsapp.net/v/t62.../file.enc?ccb=...&oh=...&oe=...").
//
// The query string must be kept, not stripped: whatsmeow's Download builds
// the retry URL as host+directPath+"&hash=...", appending straight onto
// whatever GetDirectPath() returns with no "?" of its own. That only forms
// a valid query string because directPath is expected to still carry its
// own "?ccb=...&oh=...&oe=..." — oh/oe are the object hash and expiry that
// authorize the signed request, so a directPath with the query cut off is
// missing credentials the CDN needs and gets rejected with 403, regardless
// of which host ends up serving it.
func extractDirectPathFromURL(url string) string {
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url
	}
	return "/" + parts[1]
}
