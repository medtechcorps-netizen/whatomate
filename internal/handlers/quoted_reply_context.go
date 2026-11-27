package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

const quotedReplyContextMaxRunes = 500

type scopedReplyContext struct {
	preview     *ReplyPreview
	unavailable bool
}

type quotedReplyImage struct {
	revision            string
	key                 string
	provedLegacyCoexKey string
	contentType         string
	nativeAccountID     uuid.UUID
	nativePhoneID       string
}

type nativeReplyBridgeRow struct {
	MessageID       uuid.UUID
	ConversationID  uuid.UUID
	NativeAccountID uuid.UUID
	AccountName     string
}

type nativeReplyBridgeProof map[uuid.UUID]nativeReplyBridgeRow

func (proof nativeReplyBridgeProof) allows(message *models.Message, accountID uuid.UUID) bool {
	if message == nil || message.InboxConversationID == nil {
		return true // Pre-bridge native rows retain their exact stored relation.
	}
	row, ok := proof[message.ID]
	return ok && row.ConversationID == *message.InboxConversationID && row.AccountName == message.WhatsAppAccount &&
		row.NativeAccountID != uuid.Nil && (accountID == uuid.Nil || row.NativeAccountID == accountID)
}

// Actual rows, not account-name aliases, establish populated bridge authority.
// Queries are bounded by one transcript page and its before/after candidates.
// A proof failure only removes the new scoped projection from existing reads.
func (a *App) proveNativeReplyBridges(orgID, contactID uuid.UUID, messages []*models.Message) nativeReplyBridgeProof {
	selected := make(map[uuid.UUID]bool)
	ids := make([]uuid.UUID, 0, len(messages))
	for _, message := range messages {
		if message != nil && message.InboxConversationID != nil && message.OrganizationID == orgID &&
			message.ContactID == contactID && !selected[message.ID] {
			ids = append(ids, message.ID)
			selected[message.ID] = true
		}
	}
	if len(ids) == 0 || len(ids) > 300 || a == nil || a.DB == nil {
		return nil
	}
	var rows []nativeReplyBridgeRow
	err := readTranscriptQuoteWithinSavepoint(a.DB, func(tx *gorm.DB) error {
		return tx.Model(&models.Message{}).
			Select("messages.id AS message_id, inbox.id AS conversation_id, native.id AS native_account_id, native.name AS account_name").
			Joins(`JOIN inbox_conversations AS inbox
				ON inbox.id = messages.inbox_conversation_id AND inbox.organization_id = messages.organization_id`).
			Joins(`JOIN channel_accounts AS shadow
				ON shadow.id = inbox.channel_account_id AND shadow.organization_id = inbox.organization_id`).
			Joins(`JOIN whatsapp_accounts AS native
				ON native.organization_id = shadow.organization_id
				AND CAST(native.id AS text) = shadow.metadata ->> 'legacy_account_id'
				AND native.name = shadow.metadata ->> 'legacy_account_name'
				AND native.name = messages.whats_app_account`).
			Where(`messages.id IN ? AND messages.organization_id = ? AND messages.contact_id = ?
				AND inbox.contact_id = messages.contact_id AND inbox.deleted_at IS NULL AND inbox.channel = ?
				AND inbox.external_conversation_id = 'legacy-contact:' || CAST(messages.contact_id AS text)
				AND shadow.deleted_at IS NULL AND shadow.channel = ? AND shadow.provider = ?
				AND shadow.external_account_id = 'legacy-account:' || CAST(native.id AS text)
				AND native.deleted_at IS NULL`, ids, orgID, contactID, models.ChannelWhatsApp, models.ChannelWhatsApp, channelapi.LegacyMetaProvider).
			Find(&rows).Error
	})
	if !errors.Is(err, errTranscriptQuoteReadOnly) {
		return nil
	}
	proof := make(nativeReplyBridgeProof, len(rows))
	for _, row := range rows {
		proof[row.MessageID] = row
	}
	return proof
}

func scopedReplyContextVisible(child, parent *models.Message, cutoff *time.Time) bool {
	return readableReplyContextChild(child, cutoff) && parent != nil && parent.ID != uuid.Nil &&
		transcriptQuoteVisible(child, parent, cutoff) &&
		(child.InboxConversationID == nil || parent.InboxConversationID == nil ||
			*child.InboxConversationID == *parent.InboxConversationID)
}

func readableReplyContextChild(child *models.Message, cutoff *time.Time) bool {
	return child.ID != uuid.Nil && child.OrganizationID != uuid.Nil && child.ContactID != uuid.Nil &&
		child.WhatsAppAccount != "" && !child.DeletedAt.Valid &&
		child.Metadata[coexistenceMediaRevokedMetadataKey] != true &&
		(cutoff == nil || !child.CreatedAt.Before(*cutoff))
}

func hasStoredReplyContext(child *models.Message) bool {
	return child.IsReply && child.ReplyToMessageID != nil && *child.ReplyToMessageID != uuid.Nil
}

func boundedReplyContextText(text string) string {
	runes := 0
	for index := range text {
		if runes == quotedReplyContextMaxRunes {
			return text[:index]
		}
		runes++
	}
	return text
}

func supportedReplyContextType(messageType models.MessageType) bool {
	return messageType == models.MessageTypeText || messageType == models.MessageTypeImage
}

func projectStoredReplyContext(child *models.Message, cutoff *time.Time) *ReplyPreview {
	if !hasStoredReplyContext(child) || !scopedReplyContextVisible(child, child.ReplyToMessage, cutoff) ||
		child.ReplyToMessage.ID != *child.ReplyToMessageID {
		return nil
	}
	parent := child.ReplyToMessage
	if !supportedReplyContextType(parent.MessageType) {
		return nil
	}
	preview := transcriptReplyPreview(parent)
	preview.ScopedContext = true
	preview.Content = map[string]string{"body": boundedReplyContextText(parent.Content)}
	if image, ok := prepareQuotedReplyImage(child, parent, nil); ok {
		preview.ImageRevision = image.revision
	}
	return preview
}

func quotedReplyRasterType(contentType string) bool {
	switch contentType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

// The transport revision is opaque. For the read itself the caller also
// supplies the currently selected native account and repeats the authorization.
func prepareQuotedReplyImage(child, parent *models.Message, native *models.WhatsAppAccount) (quotedReplyImage, bool) {
	var image quotedReplyImage
	if parent.MessageType != models.MessageTypeImage || !quotedReplyRasterType(mediaTypeEssence(parent.MediaMimeType)) ||
		(native != nil && (native.ID == uuid.Nil || native.OrganizationID != child.OrganizationID || native.Name != child.WhatsAppAccount)) {
		return image, false
	}
	key := parent.MediaURL
	orgID := parent.OrganizationID
	provedCoexKey := ""
	switch {
	case nativeMediaDeclared(parent.Metadata):
		binding, err := readNativeMediaBinding(parent)
		if err != nil || binding.Phase != "ready" || binding.ObjectKey != key || !nativeMediaCurrent(parent, binding) ||
			binding.Identity.ContactID != parent.ContactID || binding.Identity.MessageType != parent.MessageType ||
			(native != nil && (binding.Identity.AccountID != native.ID ||
				binding.Identity.PhoneID != strings.TrimSpace(native.PhoneID))) {
			return image, false
		}
		leaf := binding.Epoch.String() + ".bin"
		if key != tenantObjectKey(orgID, "messages", "native", parent.ID.String(), leaf) &&
			key != path.Join("native", orgID.String(), parent.ID.String(), leaf) {
			return image, false
		}
	case strings.HasPrefix(key, "coexistence/"), strings.HasPrefix(key, tenantObjectKey(orgID, "messages", "coexistence")+"/"):
		mediaID, revision, supported := coexistenceMediaRevision(parent)
		if !supported || coexistenceMediaMetadataString(parent.Metadata, coexistenceMediaHydratedIDMetadataKey) != mediaID {
			return image, false
		}
		name, err := coexistenceMediaFilename(coexistenceMediaStorageRevisionID(parent.ID, revision), mediaID)
		if err != nil {
			return image, false
		}
		provedCoexKey = path.Join("coexistence", name)
		if key != provedCoexKey && key != tenantObjectKey(orgID, "messages", "coexistence", name) {
			return image, false
		}
	case strings.HasPrefix(key, "native/"), strings.HasPrefix(key, tenantObjectKey(orgID, "messages", "native")+"/"):
		return image, false
	case strings.HasPrefix(key, inboxForwardedLocalDirectory+"/"):
		if path.Dir(key) != inboxForwardedLocalMediaDir(orgID, parent.ID) ||
			path.Ext(key) != downloadedMediaExtension(parent.MediaMimeType) {
			return image, false
		}
	}
	if !isInboxAttachmentStorageKey(orgID, key, provedCoexKey) {
		return image, false
	}
	contentType := mediaTypeEssence(parent.MediaMimeType)
	revision := sha256.Sum256([]byte(strings.Join([]string{
		orgID.String(), child.ContactID.String(), child.WhatsAppAccount, child.ID.String(), parent.ID.String(),
		key, contentType, parent.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}, "\x00")))
	image = quotedReplyImage{revision: hex.EncodeToString(revision[:]), key: key,
		provedLegacyCoexKey: provedCoexKey, contentType: contentType}
	if native != nil {
		image.nativeAccountID = native.ID
		image.nativePhoneID = strings.TrimSpace(native.PhoneID)
	}
	return image, true
}

func validQuotedReplyImageRevision(revision string) bool {
	if len(revision) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(revision)
	return err == nil && hex.EncodeToString(decoded) == revision
}

type quotedReplyImageTarget struct {
	orgID, actorID, threadID, childID uuid.UUID
	account                           string
	revision                          string
}

func parseQuotedReplyImageTarget(a *App, r *fastglue.Request, inbox bool) (quotedReplyImageTarget, bool) {
	r.RequestCtx.Response.Header.Set("Cache-Control", "private, no-store")
	bad := func() {
		_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid quote image request", nil, "")
	}
	var target quotedReplyImageTarget
	args := r.RequestCtx.QueryArgs()
	target.revision = string(args.Peek("revision"))
	headers := r.RequestCtx.Request.Header.PeekAll("X-Organization-ID")
	if !validQuotedReplyImageRevision(target.revision) || len(headers) != 1 {
		bad()
		return target, false
	}
	if inbox {
		if args.Len() != 1 {
			bad()
			return target, false
		}
	} else {
		target.account = string(args.Peek("account"))
		if args.Len() != 2 || !validConversationAccount(target.account) {
			bad()
			return target, false
		}
	}
	threadName := "contact"
	if inbox {
		threadName = "conversation"
	}
	var err error
	if target.threadID, err = parsePathUUID(r, "id", threadName); err != nil {
		return target, false
	}
	if target.childID, err = parsePathUUID(r, "message_id", "message"); err != nil {
		return target, false
	}
	if target.orgID, target.actorID, err = a.chatExportActor(r); err != nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusForbidden, "Selected organization is not available", nil, "")
		return target, false
	}
	return target, true
}

// Register the handlers directly on App so each bounded snapshot ends before the
// storage read. A second snapshot then establishes current ownership before any
// bytes are sent.
func (a *App) ServeNativeReplyContextMedia(r *fastglue.Request) error {
	target, ok := parseQuotedReplyImageTarget(a, r, false)
	if !ok {
		return nil
	}
	return a.serveQuotedReplyImage(r, target, false)
}

func (a *App) ServeInboxReplyContextMedia(r *fastglue.Request) error {
	target, ok := parseQuotedReplyImageTarget(a, r, true)
	if !ok {
		return nil
	}
	return a.serveQuotedReplyImage(r, target, true)
}

func (a *App) serveQuotedReplyImage(r *fastglue.Request, target quotedReplyImageTarget, inbox bool) error {
	ctx, cancel := context.WithTimeout(requestContext(r), 30*time.Second)
	defer cancel()
	unavailable := func() error {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Original image unavailable", nil, "")
	}
	load := func() (quotedReplyImage, error) {
		return a.loadScopedQuotedReplyImage(ctx, target, inbox)
	}
	image, err := load()
	if err != nil || image.revision != target.revision {
		return unavailable()
	}
	select {
	case inboxAttachmentSlots <- struct{}{}:
	case <-ctx.Done():
		return unavailable()
	}
	bodyOwnsSlot := false
	defer func() {
		if !bodyOwnsSlot {
			<-inboxAttachmentSlots
		}
	}()
	data, _, err := a.readInboxAttachment(ctx, target.orgID, image.key, forwardImageMaxBytes, image.provedLegacyCoexKey)
	if err != nil || len(data) == 0 || len(data) > forwardImageMaxBytes ||
		http.DetectContentType(data) != image.contentType {
		return unavailable()
	}
	current, err := load()
	if err != nil || current != image || ctx.Err() != nil {
		return unavailable()
	}
	header := &r.RequestCtx.Response.Header
	header.SetContentType(image.contentType)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	r.RequestCtx.Response.SetBodyStream(&inboxAttachmentBody{Reader: bytes.NewReader(data)}, len(data))
	bodyOwnsSlot = true
	return nil
}

func (a *App) loadScopedQuotedReplyImage(ctx context.Context, target quotedReplyImageTarget, inbox bool) (quotedReplyImage, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var image quotedReplyImage
	err := a.chatExportReadTransaction(ctx, target.orgID, true, func(reader *App) error {
		var native models.WhatsAppAccount
		var cutoff *time.Time
		var thread *uuid.UUID
		var query *gorm.DB
		contactID := target.threadID
		if inbox {
			conversation, err := loadInboxConversation(reader.DB, target.orgID, target.threadID, false)
			if err != nil {
				return err
			}
			scope, err := reader.inboxAttachmentScope(target.orgID, target.actorID, conversation)
			if err != nil {
				return err
			}
			native, cutoff, contactID = scope.nativeAccount, scope.cutoff, scope.contactID
			thread = &scope.conversationID
			query = reader.DB.Model(&models.Message{}).Where(
				"organization_id = ? AND contact_id = ? AND whats_app_account = ? AND inbox_conversation_id = ?",
				target.orgID, contactID, native.Name, scope.conversationID)
			if cutoff != nil {
				query = query.Where("created_at >= ?", cutoff.UTC())
			}
		} else {
			scope, err := reader.readConversationScope(target.orgID, target.actorID, contactID)
			if err != nil {
				return err
			}
			if scope.hideHistory {
				return gorm.ErrRecordNotFound
			}
			if !scope.cutoff.IsZero() {
				cutoff = &scope.cutoff
			}
			if err := reader.DB.Select("id", "organization_id", "name", "phone_id").
				Where("organization_id = ? AND name = ?", target.orgID, target.account).Take(&native).Error; err != nil {
				return err
			}
			query = reader.conversationScopedMessages(target.orgID, target.actorID, contactID, native.Name, scope)
		}
		var child models.Message
		if err := query.Preload("ReplyToMessage", func(parent *gorm.DB) *gorm.DB {
			parent = parent.Where("organization_id = ? AND contact_id = ? AND whats_app_account = ?",
				target.orgID, contactID, native.Name)
			if thread != nil {
				parent = parent.Where("inbox_conversation_id = ?", *thread)
			}
			if cutoff != nil {
				parent = parent.Where("created_at >= ?", cutoff.UTC())
			}
			return parent
		}).Where("id = ?", target.childID).Take(&child).Error; err != nil {
			return err
		}
		visible := []models.Message{child}
		reader.resolveTranscriptQuotes(visible, target.orgID, contactID, cutoff)
		child = visible[0]
		parent := child.ReplyToMessage
		if !hasStoredReplyContext(&child) || !scopedReplyContextVisible(&child, parent, cutoff) ||
			parent.ID != *child.ReplyToMessageID ||
			(thread != nil && (parent.InboxConversationID == nil || *parent.InboxConversationID != *thread)) {
			return gorm.ErrRecordNotFound
		}
		if !inbox {
			proof := reader.proveNativeReplyBridges(target.orgID, contactID, []*models.Message{&child, parent})
			if !proof.allows(&child, native.ID) || !proof.allows(parent, native.ID) {
				return gorm.ErrRecordNotFound
			}
		}
		var ok bool
		image, ok = prepareQuotedReplyImage(&child, parent, &native)
		if !ok {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	return image, err
}

// Inbox conversations are projected separately so no native reply relation or
// private metadata is added to the existing provider-neutral message row.
func (a *App) projectInboxReplyContexts(messages []models.Message, scope inboxAttachmentScope) (map[uuid.UUID]scopedReplyContext, error) {
	candidates := make([]models.Message, 0, len(messages))
	known := make(map[uuid.UUID]bool)
	var parentIDs []uuid.UUID
	for _, child := range messages {
		if child.OrganizationID != scope.nativeAccount.OrganizationID || child.ContactID != scope.contactID ||
			child.InboxConversationID == nil || *child.InboxConversationID != scope.conversationID ||
			child.WhatsAppAccount != scope.nativeAccount.Name || !readableReplyContextChild(&child, scope.cutoff) {
			continue
		}
		stored := hasStoredReplyContext(&child)
		if !stored && child.Metadata[models.MessageCoexistencePendingQuoteMetadataKey] == nil {
			continue
		}
		if stored {
			known[child.ID] = true
			parentIDs = append(parentIDs, *child.ReplyToMessageID)
		}
		child.ReplyToMessage = nil
		candidates = append(candidates, child)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if len(parentIDs) != 0 {
		var parents []models.Message
		query := a.DB.Where("id IN ? AND organization_id = ? AND contact_id = ? AND whats_app_account = ? AND inbox_conversation_id = ?",
			parentIDs, scope.nativeAccount.OrganizationID, scope.contactID, scope.nativeAccount.Name, scope.conversationID)
		if scope.cutoff != nil {
			query = query.Where("created_at >= ?", scope.cutoff.UTC())
		}
		if err := query.Find(&parents).Error; err != nil {
			return nil, err
		}
		byID := make(map[uuid.UUID]*models.Message, len(parents))
		for i := range parents {
			byID[parents[i].ID] = &parents[i]
		}
		for i := range candidates {
			child := &candidates[i]
			if known[child.ID] {
				child.ReplyToMessage = byID[*child.ReplyToMessageID]
			}
		}
	}
	pending := a.resolveTranscriptQuotes(candidates, scope.nativeAccount.OrganizationID, scope.contactID, scope.cutoff)
	for _, index := range pending {
		known[candidates[index].ID] = true
	}
	result := make(map[uuid.UUID]scopedReplyContext)
	for i := range candidates {
		child := &candidates[i]
		var preview *ReplyPreview
		if parent := child.ReplyToMessage; parent != nil && parent.InboxConversationID != nil &&
			*parent.InboxConversationID == scope.conversationID {
			if !supportedReplyContextType(parent.MessageType) {
				continue
			}
			preview = projectStoredReplyContext(child, scope.cutoff)
			if preview != nil && preview.ImageRevision != "" {
				if _, ok := prepareQuotedReplyImage(child, parent, &scope.nativeAccount); !ok {
					preview.ImageRevision = ""
				}
			}
		}
		if preview != nil || known[child.ID] {
			result[child.ID] = scopedReplyContext{preview: preview, unavailable: preview == nil}
		}
	}
	return result, nil
}
