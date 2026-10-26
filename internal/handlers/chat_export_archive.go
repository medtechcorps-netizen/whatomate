package handlers

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/utils"
)

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
