package handlers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

var errInvalidStoredMediaKey = errors.New("invalid stored media key")

// getMediaStoragePath returns the base path for media storage
func (a *App) getMediaStoragePath() string {
	basePath := a.Config.Storage.LocalPath
	if basePath == "" {
		basePath = "./media"
	}
	return basePath
}

// ensureMediaDir ensures the media directory exists
func (a *App) ensureMediaDir(subdir string) error {
	path := filepath.Join(a.getMediaStoragePath(), subdir)
	return os.MkdirAll(path, 0755)
}

func tenantObjectKey(orgID uuid.UUID, parts ...string) string {
	keyParts := []string{"organizations", orgID.String()}
	keyParts = append(keyParts, parts...)
	return path.Join(keyParts...)
}

func isTenantObjectKey(orgID uuid.UUID, key string) bool {
	normalized := path.Clean(strings.ReplaceAll(key, "\\", "/"))
	prefix := tenantObjectKey(orgID) + "/"
	return strings.HasPrefix(normalized, prefix)
}

// saveTenantMedia stores new uploads in object storage when configured. Local
// storage remains the development fallback and keeps its historic relative
// paths so existing installations continue to work.
func (a *App) saveTenantMedia(
	ctx context.Context,
	orgID uuid.UUID,
	objectSubdir string,
	localSubdir string,
	filename string,
	data []byte,
	contentType string,
) (string, error) {
	if a.Config != nil && strings.EqualFold(a.Config.Storage.Type, "s3") {
		if a.ObjectStore == nil {
			return "", errors.New("object storage is not initialized")
		}
		key := tenantObjectKey(orgID, objectSubdir, filename)
		if err := a.ObjectStore.Put(ctx, key, data, contentType); err != nil {
			return "", fmt.Errorf("failed to store media object: %w", err)
		}
		a.Log.Info("Media stored in object storage", "key", key, "size", len(data), "org_id", orgID)
		return key, nil
	}

	if err := a.ensureMediaDir(localSubdir); err != nil {
		return "", fmt.Errorf("failed to create media directory: %w", err)
	}
	filePath := filepath.Join(a.getMediaStoragePath(), localSubdir, filename)
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return "", fmt.Errorf("failed to save media file: %w", err)
	}

	relativePath := path.Join(localSubdir, filename)
	a.Log.Info("Media stored locally", "path", relativePath, "size", len(data), "org_id", orgID)
	return relativePath, nil
}

// loadTenantMedia reads either a tenant-prefixed object key or a legacy local
// path. This dual-read behavior keeps existing disk media available while new
// uploads move to object storage.
func (a *App) loadTenantMedia(ctx context.Context, orgID uuid.UUID, key string) ([]byte, string, error) {
	normalized := path.Clean(strings.ReplaceAll(key, "\\", "/"))
	if strings.HasPrefix(normalized, "organizations/") {
		if !isTenantObjectKey(orgID, normalized) {
			return nil, "", errInvalidStoredMediaKey
		}
		if a.ObjectStore == nil {
			return nil, "", errors.New("object storage is not initialized")
		}
		return a.ObjectStore.Get(ctx, normalized)
	}

	filePath := filepath.Clean(filepath.FromSlash(key))
	baseDir, err := filepath.Abs(a.getMediaStoragePath())
	if err != nil {
		return nil, "", fmt.Errorf("resolve media storage path: %w", err)
	}
	fullPath, err := filepath.Abs(filepath.Join(baseDir, filePath))
	if err != nil || !strings.HasPrefix(fullPath, baseDir+string(os.PathSeparator)) {
		return nil, "", errInvalidStoredMediaKey
	}

	info, err := os.Lstat(fullPath)
	if err != nil {
		return nil, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, "", errInvalidStoredMediaKey
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, "", err
	}
	return data, "", nil
}

// getExtensionFromMimeType returns file extension based on mime type
func getExtensionFromMimeType(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(mimeType, "image/png"):
		return ".png"
	case strings.HasPrefix(mimeType, "image/gif"):
		return ".gif"
	case strings.HasPrefix(mimeType, "image/webp"):
		return ".webp"
	case strings.HasPrefix(mimeType, "video/mp4"):
		return ".mp4"
	case strings.HasPrefix(mimeType, "video/3gpp"):
		return ".3gp"
	case strings.HasPrefix(mimeType, "audio/aac"):
		return ".aac"
	case strings.HasPrefix(mimeType, "audio/mp4"):
		return ".m4a"
	case strings.HasPrefix(mimeType, "audio/mpeg"):
		return ".mp3"
	case strings.HasPrefix(mimeType, "audio/amr"):
		return ".amr"
	case strings.HasPrefix(mimeType, "audio/ogg"):
		return ".ogg"
	case strings.HasPrefix(mimeType, "application/pdf"):
		return ".pdf"
	case strings.HasPrefix(mimeType, "application/vnd.ms-powerpoint"):
		return ".ppt"
	case strings.HasPrefix(mimeType, "application/msword"):
		return ".doc"
	case strings.HasPrefix(mimeType, "application/vnd.ms-excel"):
		return ".xls"
	case strings.HasPrefix(mimeType, "application/vnd.openxmlformats-officedocument.wordprocessingml"):
		return ".docx"
	case strings.HasPrefix(mimeType, "application/vnd.openxmlformats-officedocument.spreadsheetml"):
		return ".xlsx"
	case strings.HasPrefix(mimeType, "application/vnd.openxmlformats-officedocument.presentationml"):
		return ".pptx"
	case strings.HasPrefix(mimeType, "text/plain"):
		return ".txt"
	default:
		return ""
	}
}

// getMimeTypeFromExtension returns a MIME type for stored media when one was
// not persisted by the uploader.
func getMimeTypeFromExtension(ext string) string {
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".mp4":
		return "video/mp4"
	case ".3gp":
		return "video/3gpp"
	case ".mp3":
		return "audio/mpeg"
	case ".aac":
		return "audio/aac"
	case ".m4a":
		return "audio/mp4"
	case ".ogg":
		return "audio/ogg"
	case ".amr":
		return "audio/amr"
	case ".pdf":
		return "application/pdf"
	case ".doc":
		return "application/msword"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xls":
		return "application/vnd.ms-excel"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".txt":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

// DownloadAndSaveMedia downloads media from Meta and saves it to the configured
// tenant media store. It returns the object key or legacy local relative path.
func (a *App) DownloadAndSaveMedia(ctx context.Context, orgID uuid.UUID, mediaID string, mimeType string, account *whatsapp.Account) (string, error) {
	// Get the media URL from Meta
	mediaURL, err := a.WhatsApp.GetMediaURL(ctx, mediaID, account)
	if err != nil {
		return "", fmt.Errorf("failed to get media URL: %w", err)
	}

	// Download the media content
	data, err := a.WhatsApp.DownloadMedia(ctx, mediaURL, account.AccessToken)
	if err != nil {
		return "", fmt.Errorf("failed to download media: %w", err)
	}

	// Determine file extension
	ext := getExtensionFromMimeType(mimeType)
	if ext == "" {
		ext = ".bin"
	}

	// Generate unique filename
	filename := uuid.New().String() + ext

	// Determine subdirectory based on media type
	var subdir string
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		subdir = "images"
	case strings.HasPrefix(mimeType, "video/"):
		subdir = "videos"
	case strings.HasPrefix(mimeType, "audio/"):
		subdir = "audio"
	default:
		subdir = "documents"
	}

	return a.saveTenantMedia(ctx, orgID, path.Join("messages", subdir), subdir, filename, data, mimeType)
}

// ServeMedia serves media files from local storage
// Only authorized users who have access to the message can view the media
func (a *App) ServeMedia(r *fastglue.Request) error {
	// Get auth context
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	// Get the message ID from URL parameter
	messageIDStr := r.RequestCtx.UserValue("message_id").(string)
	messageID, err := uuid.Parse(messageIDStr)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid message ID", nil, "")
	}

	// Find the message and verify access
	message, err := findByIDAndOrg[models.Message](a.DB, r, messageID, orgID, "Message")
	if err != nil {
		return nil
	}

	// Users without contacts:read permission can only access media from contacts
	// assigned to them — the persistent owner or an active transfer assigned
	// directly to them (via scopeAssignedContact) — or from contacts with an
	// active team transfer where the user is a team member.
	if !a.HasPermission(userID, models.ResourceContacts, models.ActionRead, orgID) {
		var contact models.Contact
		q := a.scopeAssignedContact(a.DB.Where("id = ? AND organization_id = ?", message.ContactID, orgID), userID, orgID)
		if err := q.First(&contact).Error; err != nil {
			// Not owner / not directly assigned — check team membership via active transfer
			var transfer models.AgentTransfer
			if err := a.DB.Where("contact_id = ? AND organization_id = ? AND status = ? AND team_id IS NOT NULL",
				message.ContactID, orgID, models.TransferStatusActive).First(&transfer).Error; err != nil {
				return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Access denied", nil, "")
			}
			var count int64
			a.DB.Model(&models.TeamMember{}).Where("team_id = ? AND user_id = ?", transfer.TeamID, userID).Count(&count)
			if count == 0 {
				return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Access denied", nil, "")
			}
		}
	}

	// Check if message has media
	if message.MediaURL == "" {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "No media found", nil, "")
	}

	data, storedContentType, err := a.loadTenantMedia(r.RequestCtx, orgID, message.MediaURL)
	if err != nil {
		if errors.Is(err, errInvalidStoredMediaKey) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid media path", nil, "")
		}
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, storage.ErrObjectNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "File not found", nil, "")
		}
		a.Log.Error("Failed to read media", "key", message.MediaURL, "error", err, "org_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to read file", nil, "")
	}

	contentType := storedContentType
	if contentType == "" {
		contentType = message.MediaMimeType
	}
	if contentType == "" {
		contentType = getMimeTypeFromExtension(strings.ToLower(path.Ext(message.MediaURL)))
	}

	r.RequestCtx.Response.Header.Set("Content-Type", contentType)
	r.RequestCtx.Response.Header.Set("Cache-Control", "private, max-age=3600") // Cache for 1 hour, private
	r.RequestCtx.SetBody(data)

	return nil
}
