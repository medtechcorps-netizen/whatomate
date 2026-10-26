package handlers

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/internal/utils"
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
	return a.downloadAndSaveMedia(ctx, orgID, mediaID, mimeType, account, uuid.NewString())
}

// downloadAndSaveCoexistenceMedia uses a stable object name for every
// message/media pair. A retry after an uncertain database commit overwrites the
// same object instead of leaving another random orphan. Including the media ID
// digest also prevents an older edit hydration from overwriting a newer edit's
// bytes when two provider calls briefly overlap.
func (a *App) downloadAndSaveCoexistenceMedia(
	ctx context.Context,
	orgID, messageID uuid.UUID,
	mediaID string,
	mimeType string,
	account *whatsapp.Account,
) (string, error) {
	if account == nil {
		return "", errors.New("WhatsApp media account is required")
	}
	filename, err := coexistenceMediaFilename(messageID, mediaID)
	if err != nil {
		return "", err
	}
	client := a.whatsAppClient()
	mediaURL, err := client.GetMediaURL(ctx, mediaID, account)
	if err != nil {
		return "", fmt.Errorf("failed to get media URL: %w", err)
	}
	data, err := client.DownloadMedia(ctx, mediaURL, account.AccessToken)
	if err != nil {
		return "", fmt.Errorf("failed to download media: %w", err)
	}
	return a.saveTenantMedia(
		ctx,
		orgID,
		path.Join("messages", "coexistence"),
		"coexistence",
		filename,
		data,
		mimeType,
	)
}

// coexistenceMediaFilename is the single source of truth for both writes and
// cleanup retries. Keep it independent of mutable webhook MIME metadata.
func coexistenceMediaFilename(messageID uuid.UUID, mediaID string) (string, error) {
	mediaID = strings.TrimSpace(mediaID)
	if messageID == uuid.Nil || mediaID == "" {
		return "", errors.New("coexistence media storage identity is incomplete")
	}
	versionID := uuid.NewSHA1(messageID, []byte(mediaID))
	return messageID.String() + "-" + versionID.String() + ".bin", nil
}

// coexistenceMediaStorageKey re-derives the exact deterministic key so a
// durable job can retry an object deletion without persisting a provider URL.
func (a *App) coexistenceMediaStorageKey(
	orgID, messageID uuid.UUID,
	mediaID string,
) (string, error) {
	if a == nil || orgID == uuid.Nil {
		return "", errors.New("coexistence media storage identity is incomplete")
	}
	filename, err := coexistenceMediaFilename(messageID, mediaID)
	if err != nil {
		return "", err
	}
	if a.Config != nil && strings.EqualFold(a.Config.Storage.Type, "s3") {
		return tenantObjectKey(orgID, "messages", "coexistence", filename), nil
	}
	return path.Join("coexistence", filename), nil
}

// deleteTenantMedia removes one exact tenant media key. It is intentionally
// narrow: callers must have already proved that no committed message references
// the key. Missing objects are an idempotent success.
func (a *App) deleteTenantMedia(ctx context.Context, orgID uuid.UUID, key string) error {
	if a == nil || orgID == uuid.Nil {
		return errors.New("tenant media deletion identity is incomplete")
	}
	normalized := path.Clean(strings.ReplaceAll(strings.TrimSpace(key), "\\", "/"))
	if normalized == "." || normalized == "" {
		return errInvalidStoredMediaKey
	}
	if strings.HasPrefix(normalized, "organizations/") {
		if !isTenantObjectKey(orgID, normalized) {
			return errInvalidStoredMediaKey
		}
		if a.ObjectStore == nil {
			return errors.New("object storage is not initialized")
		}
		if err := a.ObjectStore.Delete(ctx, normalized); err != nil &&
			!errors.Is(err, storage.ErrObjectNotFound) {
			return err
		}
		return nil
	}

	filePath := filepath.Clean(filepath.FromSlash(normalized))
	baseDir, err := filepath.Abs(a.getMediaStoragePath())
	if err != nil {
		return fmt.Errorf("resolve media storage path: %w", err)
	}
	fullPath, err := filepath.Abs(filepath.Join(baseDir, filePath))
	if err != nil || !strings.HasPrefix(fullPath, baseDir+string(os.PathSeparator)) {
		return errInvalidStoredMediaKey
	}
	info, err := os.Lstat(fullPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errInvalidStoredMediaKey
	}
	if err := os.Remove(fullPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

const rejectedMessageMediaCleanupTimeout = 5 * time.Second

// cleanupRejectedMessageMedia removes a just-saved outgoing media object only
// after a fresh tenant read proves that no committed message references it.
// This closes the validation-failure orphan path without deleting media from a
// pending or failed message that was durably persisted.
func (a *App) cleanupRejectedMessageMedia(orgID uuid.UUID, mediaPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), rejectedMessageMediaCleanupTimeout)
	defer cancel()
	return a.deleteUnreferencedMessageMedia(ctx, orgID, mediaPath)
}

func (a *App) deleteUnreferencedMessageMedia(
	ctx context.Context,
	orgID uuid.UUID,
	mediaPath string,
) error {
	if a == nil || orgID == uuid.Nil || strings.TrimSpace(mediaPath) == "" {
		return errors.New("message media cleanup identity is incomplete")
	}
	var unreferenced bool
	if err := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var references int64
		if err := scoped.DB.WithContext(ctx).Unscoped().Model(&models.Message{}).
			Where("organization_id = ? AND media_url = ?", orgID, mediaPath).
			Count(&references).Error; err != nil {
			return err
		}
		unreferenced = references == 0
		return nil
	}); err != nil {
		return err
	}
	if !unreferenced {
		return nil
	}
	// The randomized key is private to this rejected request. Perform the
	// object delete after the reference-read transaction commits so no tenant
	// database transaction is held across object-store I/O.
	return a.deleteTenantMedia(ctx, orgID, mediaPath)
}

func (a *App) downloadAndSaveMedia(
	ctx context.Context,
	orgID uuid.UUID,
	mediaID string,
	mimeType string,
	account *whatsapp.Account,
	filenameStem string,
) (string, error) {
	if account == nil {
		return "", errors.New("WhatsApp media account is required")
	}
	filenameStem = strings.TrimSpace(filenameStem)
	if filenameStem == "" {
		return "", errors.New("media storage filename is required")
	}
	client := a.whatsAppClient()
	// Get the media URL from Meta
	mediaURL, err := client.GetMediaURL(ctx, mediaID, account)
	if err != nil {
		return "", fmt.Errorf("failed to get media URL: %w", err)
	}

	// Download the media content
	data, err := client.DownloadMedia(ctx, mediaURL, account.AccessToken)
	if err != nil {
		return "", fmt.Errorf("failed to download media: %w", err)
	}

	// Determine file extension
	ext := getExtensionFromMimeType(mimeType)
	if ext == "" {
		ext = ".bin"
	}

	filename := filenameStem + ext

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

	servedType, inline := chatMediaResponseType(contentType)
	header := &r.RequestCtx.Response.Header
	header.Set("Content-Type", servedType)
	header.Set("Cache-Control", "private, max-age=3600") // Cache for 1 hour, private
	header.Set("X-Content-Type-Options", "nosniff")
	if !inline {
		setMediaDownloadHeaders(header, mediaDownloadFilename(message.MediaFilename, servedType))
	}
	r.RequestCtx.SetBody(data)

	return nil
}

// Media is served from this app's origin, and a customer can send any file.
// Only types a browser shows inertly are served inline: raster images and the
// video and audio types the chat plays. Everything else downloads in a
// sandbox, and types a browser could run as a page or a script are served as
// opaque bytes. Keep this list in step with the inline lists in
// frontend/src/lib/chatMedia.ts; a test compares them.
var inlineChatMediaTypes = map[string]bool{
	// Raster images only. No "+xml" type: browsers render those as documents.
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
	"image/avif": true,
	"image/bmp":  true,

	"video/mp4":       true,
	"video/3gpp":      true,
	"video/3gpp2":     true,
	"video/webm":      true,
	"video/ogg":       true,
	"video/quicktime": true,
	"video/mpeg":      true,
	"video/x-m4v":     true,

	"audio/aac":   true,
	"audio/mp4":   true,
	"audio/mpeg":  true,
	"audio/mp3":   true,
	"audio/amr":   true,
	"audio/ogg":   true,
	"audio/opus":  true,
	"audio/wav":   true,
	"audio/x-wav": true,
	"audio/wave":  true,
	"audio/webm":  true,
	"audio/x-m4a": true,
	"audio/3gpp":  true,
	"audio/flac":  true,
}

// scriptableMediaTypes are the HTML and XML document types, the JavaScript
// MIME types of the WHATWG MIME Sniffing standard, and multipart/x-mixed-replace,
// whose parts can be HTML. Every "+xml" type is scriptable as well.
var scriptableMediaTypes = map[string]bool{
	"text/html":                 true,
	"text/xml":                  true,
	"application/xml":           true,
	"text/xsl":                  true,
	"multipart/x-mixed-replace": true,

	"application/ecmascript":   true,
	"application/javascript":   true,
	"application/x-ecmascript": true,
	"application/x-javascript": true,
	"text/ecmascript":          true,
	"text/javascript":          true,
	"text/javascript1.0":       true,
	"text/javascript1.1":       true,
	"text/javascript1.2":       true,
	"text/javascript1.3":       true,
	"text/javascript1.4":       true,
	"text/javascript1.5":       true,
	"text/jscript":             true,
	"text/livescript":          true,
	"text/x-ecmascript":        true,
	"text/x-javascript":        true,
}

// mediaTypeEssence returns the lowercase type/subtype of a Content-Type value
// without its parameters, or "" when the value is not a valid media type.
func mediaTypeEssence(contentType string) string {
	base, _, _ := strings.Cut(contentType, ";")
	essence, _, err := mime.ParseMediaType(base)
	if err != nil || !strings.Contains(essence, "/") {
		return ""
	}
	return essence
}

func isScriptableMediaType(essence string) bool {
	return scriptableMediaTypes[essence] || strings.HasSuffix(essence, "+xml")
}

// chatMediaResponseType returns the Content-Type to serve stored chat media
// with, and whether it may be shown inline. Parameters are dropped so nothing
// after the essence can change how the type is parsed.
func chatMediaResponseType(contentType string) (servedType string, inline bool) {
	essence := mediaTypeEssence(contentType)
	switch {
	case inlineChatMediaTypes[essence]:
		return essence, true
	case essence == "" || isScriptableMediaType(essence):
		return "application/octet-stream", false
	default:
		return essence, false
	}
}

// setMediaDownloadHeaders makes the browser save a media response instead of
// showing it, and keeps the response inert if it is shown anyway: the sandbox
// policy with no sources replaces the app's policy, which allows same-origin
// scripts.
func setMediaDownloadHeaders(header *fasthttp.ResponseHeader, filename string) {
	disposition := "attachment"
	if filename != "" {
		if formatted := mime.FormatMediaType("attachment", map[string]string{"filename": filename}); formatted != "" {
			disposition = formatted
		}
	}
	header.Set("Content-Disposition", disposition)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "sandbox; default-src 'none'")
}

// mediaDownloadFilename returns the name a downloaded media file is saved as:
// the last path element of the sender's file name, without control or
// formatting characters such as a right-to-left override that would disguise
// its extension. Without a usable name it is "media" with the served type's
// extension; opaque types get none.
func mediaDownloadFilename(name, servedType string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = name[strings.LastIndex(name, "/")+1:]
	name = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" {
		return "media" + getExtensionFromMimeType(servedType)
	}
	return name
}

// Chat export archive serialization.
type chatExportPart struct {
	Type              string `json:"type"`
	Text              string `json:"text,omitempty"`
	Caption           string `json:"caption,omitempty"`
	Status            string `json:"status,omitempty"`
	Filename          string `json:"filename,omitempty"`
	MimeType          string `json:"mime_type,omitempty"`
	SizeBytes         int64  `json:"size_bytes,omitempty"`
	AttachmentPresent bool   `json:"attachment_present,omitempty"`
}

type chatExportMessage struct {
	ID                     uuid.UUID        `json:"id"`
	ContactID              uuid.UUID        `json:"contact_id"`
	Conversation           string           `json:"conversation"`
	Account                string           `json:"account"`
	Channel                string           `json:"channel"`
	ProviderMessageID      string           `json:"provider_message_id,omitempty"`
	ProviderConversationID string           `json:"provider_conversation_id,omitempty"`
	Direction              string           `json:"direction"`
	Type                   string           `json:"type"`
	ProviderTimestamp      time.Time        `json:"provider_timestamp"`
	IngestedAt             *time.Time       `json:"ingested_at,omitempty"`
	IngestionTimeSource    string           `json:"ingestion_time_source"`
	Text                   string           `json:"text,omitempty"`
	Template               string           `json:"template,omitempty"`
	ReplyToID              *uuid.UUID       `json:"reply_to_id,omitempty"`
	StoredStatus           string           `json:"stored_status"`
	Delivery               string           `json:"delivery"`
	Forwarded              bool             `json:"forwarded"`
	Attachments            []chatExportPart `json:"parts,omitempty"`
}

type chatExportRow struct {
	ID                  uuid.UUID
	ContactID           uuid.UUID
	GroupKey            string
	AccountKey          string
	AccountName         string
	ContactName         string
	Channel             string
	WhatsAppMessageID   string
	ConversationID      string
	Direction           string
	MessageType         string
	CreatedAt           time.Time
	EffectiveIngestedAt time.Time
	IngestedAt          *time.Time
	Content             string
	TemplateName        string
	ReplyToMessageID    *uuid.UUID
	Status              string
	Forwarded           bool
	ForwardPhase        string
	AIPhase             string
	AttachmentPresent   bool
	MediaFilename       string
	MediaMimeType       string
	InboxConversationID *uuid.UUID
}

const chatExportRowSelect = `m.id, m.contact_id, ` + chatExportGroupSQL + ` AS group_key, ` + chatExportAccountSQL + ` AS account_key,
 CASE WHEN ` + chatExportLegacySQL + ` THEN m.whats_app_account ELSE ca.name END AS account_name,
 c.profile_name AS contact_name, CASE WHEN ` + chatExportLegacySQL + ` THEN 'whatsapp' ELSE ic.channel END AS channel,
 m.whats_app_message_id AS whats_app_message_id, m.conversation_id, m.inbox_conversation_id,
 m.direction, m.message_type, m.created_at, m.ingested_at, COALESCE(m.ingested_at,m.created_at) AS effective_ingested_at,
 m.content, m.template_name, m.reply_to_message_id, m.status,
 (m.metadata ? 'forwarded_from') AS forwarded,
 m.metadata->>'forward_dispatch' AS forward_phase,
 m.metadata->>'automatic_ai_dispatch_state' AS ai_phase,
 (COALESCE(m.media_url,'') <> '') AS attachment_present, m.media_filename, m.media_mime_type`

func chatExportDisplayName(name string, mask bool) string {
	if mask {
		name = utils.MaskIfPhoneNumber(name)
	}
	if strings.TrimSpace(name) == "" {
		return "Unnamed contact"
	}
	return name
}

func chatExportDelivery(row chatExportRow) string {
	if row.Direction == "incoming" {
		return "received"
	}
	switch row.Status {
	case "read", "delivered", "sent":
		return row.Status
	case "failed":
		return "failed"
	}
	if row.ForwardPhase == "provider" || row.AIPhase == "uncertain" {
		return "unconfirmed: may have been sent"
	}
	if row.ForwardPhase == "queued" {
		return "queued: delivery not confirmed"
	}
	return "pending: delivery not confirmed"
}

func chatExportRecord(row chatExportRow, parts []models.MessagePart) chatExportMessage {
	record := chatExportMessage{
		ID: row.ID, ContactID: row.ContactID, Conversation: row.GroupKey, Account: row.AccountKey, Channel: row.Channel,
		ProviderMessageID: row.WhatsAppMessageID, ProviderConversationID: row.ConversationID,
		Direction: row.Direction, Type: row.MessageType, ProviderTimestamp: row.CreatedAt.UTC(), IngestionTimeSource: "unavailable_provider_time_used_for_ordering",
		Text: row.Content, Template: row.TemplateName, ReplyToID: row.ReplyToMessageID, StoredStatus: row.Status,
		Delivery: chatExportDelivery(row), Forwarded: row.Forwarded,
	}
	if row.IngestedAt != nil {
		at := row.IngestedAt.UTC()
		record.IngestedAt = &at
		record.IngestionTimeSource = "stored"
	}
	if len(parts) == 0 && (row.AttachmentPresent || row.MediaFilename != "" || row.MediaMimeType != "") {
		record.Attachments = append(record.Attachments, chatExportPart{Type: row.MessageType, Filename: row.MediaFilename, MimeType: row.MediaMimeType, AttachmentPresent: row.AttachmentPresent})
	}
	for _, part := range parts {
		record.Attachments = append(record.Attachments, chatExportPart{Type: string(part.Type), Text: part.Text, Caption: part.Caption, Status: string(part.Status), Filename: part.Filename, MimeType: part.MimeType, SizeBytes: part.SizeBytes, AttachmentPresent: part.MediaURL != "" || part.StorageKey != "" || part.ProviderMediaRef != ""})
	}
	return record
}

type chatExportManifest struct {
	Version       int               `json:"version"`
	Complete      bool              `json:"complete"`
	SnapshotAt    time.Time         `json:"snapshot_at"`
	GeneratedAt   time.Time         `json:"generated_at"`
	Scope         chatExportRequest `json:"scope"`
	Messages      int               `json:"messages"`
	Conversations int               `json:"conversations"`
	Files         []string          `json:"files"`
	Notes         []string          `json:"notes"`
}

type chatExportArchive struct {
	File         *os.File
	JSONL        *os.File
	ZIP          *zip.Writer
	Transcript   io.Writer
	CurrentGroup string
	IDs          []uuid.UUID
	Manifest     chatExportManifest
	Bytes        int64
	closed       bool
}

func newChatExportArchive(snapshot time.Time, req chatExportRequest) (*chatExportArchive, error) {
	f, err := os.CreateTemp("", "rereply-chat-export-*.zip")
	if err != nil {
		return nil, err
	}
	spool, err := os.CreateTemp("", "rereply-chat-export-*.jsonl")
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	req.After = ""
	req.RequestID = uuid.Nil
	return &chatExportArchive{File: f, JSONL: spool, ZIP: zip.NewWriter(f), Manifest: chatExportManifest{
		Version: 1, SnapshotAt: snapshot, Scope: req, Files: []string{"manifest.json", "messages.jsonl"},
		Notes: []string{"Read-only snapshot taken when the download started; later arrivals are excluded.", "Attachments are metadata only; object paths, signed URLs and raw internal metadata are omitted.", "Outgoing pending and unconfirmed records are not proof of delivery.", "Review exported content before using it as AI context; this export changes no AI settings."},
	}}, nil
}

func (a *chatExportArchive) cleanup() {
	if a == nil {
		return
	}
	if a.ZIP != nil && !a.closed {
		_ = a.ZIP.Close()
		a.closed = true
	}
	for _, f := range []*os.File{a.JSONL, a.File} {
		if f != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}
}

func (a *chatExportArchive) add(row chatExportRow, parts []models.MessagePart, mask bool) error {
	if len(a.IDs) >= chatExportMaxMessages {
		return errChatExportTooLarge
	}
	record := chatExportRecord(row, parts)
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	var text strings.Builder
	fmt.Fprintf(&text, "[%s] %s · %s\nMessage: %s\n", record.ProviderTimestamp.Format(time.RFC3339Nano), strings.ToUpper(record.Direction), record.Delivery, record.ID)
	if record.ReplyToID != nil {
		fmt.Fprintf(&text, "Reply to: %s\n", *record.ReplyToID)
	}
	if record.Forwarded {
		text.WriteString("Forwarded message\n")
	}
	if record.Template != "" {
		fmt.Fprintf(&text, "Template: %s\n", record.Template)
	}
	if record.Text != "" {
		text.WriteString(record.Text)
		text.WriteByte('\n')
	}
	for _, part := range record.Attachments {
		if part.Text != "" && part.Text != record.Text {
			text.WriteString(part.Text)
			text.WriteByte('\n')
		}
		if part.Caption != "" && part.Caption != record.Text && part.Caption != part.Text {
			text.WriteString(part.Caption)
			text.WriteByte('\n')
		}
		if part.AttachmentPresent || part.Filename != "" {
			fmt.Fprintf(&text, "[Attachment: %s; %s; %s; %d bytes; binary not included]\n", part.Type, part.Filename, part.MimeType, part.SizeBytes)
		}
	}
	text.WriteByte('\n')
	a.Bytes += int64(len(encoded) + 1 + text.Len())
	if a.Bytes > chatExportMaxBytes {
		return errChatExportTooLarge
	}
	if row.GroupKey != a.CurrentGroup {
		a.CurrentGroup = row.GroupKey
		a.Manifest.Conversations++
		// Names never become paths. A server-owned ordinal and immutable contact
		// ID are portable and cannot perform ZIP directory traversal.
		path := fmt.Sprintf("transcripts/%05d-%s.txt", a.Manifest.Conversations, row.ContactID)
		a.Transcript, err = a.ZIP.Create(path)
		if err != nil {
			return err
		}
		a.Manifest.Files = append(a.Manifest.Files, path)
		_, err = fmt.Fprintf(a.Transcript, "%s\nAccount: %s (%s)\nConversation: %s\nContact: %s\n\n", chatExportDisplayName(row.ContactName, mask), row.AccountName, row.Channel, row.GroupKey, row.ContactID)
		if err != nil {
			return err
		}
	}
	if _, err = a.JSONL.Write(append(encoded, '\n')); err != nil {
		return err
	}
	if _, err = io.WriteString(a.Transcript, text.String()); err != nil {
		return err
	}
	a.IDs = append(a.IDs, row.ID)
	return nil
}

func (a *chatExportArchive) finish() error {
	if _, err := a.JSONL.Seek(0, io.SeekStart); err != nil {
		return err
	}
	entry, err := a.ZIP.Create("messages.jsonl")
	if err != nil {
		return err
	}
	if _, err = io.Copy(entry, a.JSONL); err != nil {
		return err
	}
	a.Manifest.Messages = len(a.IDs)
	a.Manifest.Complete = true
	a.Manifest.GeneratedAt = time.Now().UTC()
	entry, err = a.ZIP.Create("manifest.json")
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(entry)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(a.Manifest); err != nil {
		return err
	}
	if err = a.ZIP.Close(); err != nil {
		return err
	}
	a.closed = true
	info, err := a.File.Stat()
	if err != nil {
		return err
	}
	if info.Size() > chatExportMaxBytes {
		return errChatExportTooLarge
	}
	if _, err = a.File.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_ = a.JSONL.Close()
	_ = os.Remove(a.JSONL.Name())
	a.JSONL = nil
	return nil
}

func (a *App) buildChatExport(ctx context.Context, orgID, userID uuid.UUID, req chatExportRequest, access chatExportAccess, archive *chatExportArchive) error {
	var cursor *chatExportRow
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		q := a.chatExportQuery(orgID, userID, req, access).Select(chatExportRowSelect)
		if cursor != nil {
			q = q.Where("("+chatExportGroupSQL+", COALESCE(m.ingested_at,m.created_at), m.id) > (?, ?, ?)", cursor.GroupKey, cursor.EffectiveIngestedAt, cursor.ID)
		}
		var rows []chatExportRow
		if err := q.Order(chatExportGroupSQL + " ASC, COALESCE(m.ingested_at,m.created_at) ASC, m.id ASC").Limit(chatExportBatchSize).Scan(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		ids := make([]uuid.UUID, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		// Bound all part fields before loading them; an oversized text part
		// cannot allocate an arbitrary buffer while preparing a refused export.
		var partSize struct {
			Bytes   int64
			Largest int64
		}
		if err := a.DB.Model(&models.MessagePart{}).Where("organization_id = ? AND message_id IN ?", orgID, ids).Select("COALESCE(SUM(COALESCE(octet_length(text),0) + COALESCE(octet_length(caption),0) + 2048),0) AS bytes, COALESCE(MAX(COALESCE(octet_length(text),0) + COALESCE(octet_length(caption),0)),0) AS largest").Scan(&partSize).Error; err != nil {
			return err
		}
		if partSize.Bytes > chatExportMaxBytes-archive.Bytes || partSize.Largest > 4<<20 {
			return errChatExportTooLarge
		}
		var parts []models.MessagePart
		if err := a.DB.Select("id", "message_id", "conversation_id", "type", "status", "text", "caption", "filename", "mime_type", "size_bytes", "CASE WHEN COALESCE(media_url,'') <> '' OR COALESCE(storage_key,'') <> '' OR COALESCE(provider_media_ref,'') <> '' THEN 'present' ELSE '' END AS media_url").Where("organization_id = ? AND message_id IN ?", orgID, ids).Order("message_id, position, id").Find(&parts).Error; err != nil {
			return err
		}
		byID := make(map[uuid.UUID][]models.MessagePart)
		for _, part := range parts {
			byID[part.MessageID] = append(byID[part.MessageID], part)
		}
		for _, row := range rows {
			validated := byID[row.ID]
			for _, part := range validated {
				if row.InboxConversationID == nil || *row.InboxConversationID != part.ConversationID {
					return errors.New("message part conversation mismatch")
				}
			}
			if err := archive.add(row, validated, access.MaskPhones); err != nil {
				return err
			}
		}
		last := rows[len(rows)-1]
		cursor = &last
	}
}
