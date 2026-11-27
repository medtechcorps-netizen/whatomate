package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	coexistenceQuoteReferenceMaxBytes = 1024
	transcriptQuoteMaxIDs             = 50
	transcriptQuoteReadTimeout        = time.Second
	transcriptQuoteStatementMillis    = 150
)

var errTranscriptQuoteReadOnly = errors.New("restore settings after the optional quote read")

type pendingCoexistenceQuote struct {
	OrganizationID uuid.UUID
	AccountID      uuid.UUID
	ContactID      uuid.UUID
	ReplyID        uuid.UUID
	Target         coexistenceQuoteTarget
}

type coexistenceQuoteTarget struct {
	WAMID        string
	Phone        string
	UserID       string
	ParentUserID string
}

func (t coexistenceQuoteTarget) validFor(child *models.Message) bool {
	return t.WAMID != "" && t.WAMID == strings.TrimSpace(t.WAMID) &&
		t.WAMID != child.WhatsAppMessageID && len(t.WAMID) <= coexistenceQuoteReferenceMaxBytes &&
		len(t.Phone) <= coexistenceQuoteReferenceMaxBytes && len(t.UserID) <= coexistenceQuoteReferenceMaxBytes &&
		len(t.ParentUserID) <= coexistenceQuoteReferenceMaxBytes &&
		(t.Phone != "" || t.UserID != "" || t.ParentUserID != "")
}

func (a *App) coexistenceQuoteEncryptionKey() string {
	configured := a.integrationEncryptionKey()
	if strings.TrimSpace(configured) == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(configured))
	mac.Write([]byte("rereply/coexistence/pending-quote/v1"))
	return string(mac.Sum(nil))
}

func ordinaryCoexistenceEcho(metadata models.JSONB) bool {
	return metadata["coexistence_source"] == "smb_message_echoes" &&
		metadata["coexistence_edit_event_id"] == nil && metadata[coexistenceMediaRevokedMetadataKey] != true
}

func (a *App) bindCoexistenceQuote(account *models.WhatsAppAccount, child *models.Message, identity coexistenceContactIdentity, wamid string, retainMissing bool) error {
	wamid = strings.TrimSpace(wamid)
	if wamid == "" || len(wamid) > coexistenceQuoteReferenceMaxBytes || wamid == child.WhatsAppMessageID {
		return nil
	}
	// The quote can target either side of the conversation. Scope and prove the
	// original independently. This reference is not a SQL trace field.
	scoped := a.scopedApp(a.DB.Session(&gorm.Session{Logger: logger.Discard}), child.OrganizationID)
	reply, err := scoped.resolveWhatsAppMessage(account, whatsAppMessageLookup{
		WAMID: wamid, ContactID: child.ContactID, Identity: identity,
	})
	if err == nil {
		child.IsReply = true
		child.ReplyToMessageID = &reply.Message.ID
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("load coexistence reply context: %w", err)
	}
	if !retainMissing || !errors.Is(err, errWhatsAppMessageOwnerNotStored) {
		return nil
	}
	key := a.coexistenceQuoteEncryptionKey()
	target := coexistenceQuoteTarget{WAMID: wamid, Phone: identity.Phone, UserID: identity.UserID, ParentUserID: identity.ParentUserID}
	quote := pendingCoexistenceQuote{
		OrganizationID: child.OrganizationID, AccountID: account.ID, ContactID: child.ContactID,
		ReplyID: child.ID, Target: target,
	}
	if key == "" || child.OrganizationID != account.OrganizationID || child.WhatsAppAccount != account.Name || !target.validFor(child) {
		return nil
	}
	body, err := json.Marshal(quote)
	if err != nil {
		return errors.New("encode pending coexistence quote")
	}
	sealed, err := appcrypto.Encrypt(string(body), key)
	if err != nil || !appcrypto.IsEncrypted(sealed) {
		return errors.New("seal pending coexistence quote")
	}
	child.Metadata[models.MessageCoexistencePendingQuoteMetadataKey] = sealed
	return nil
}

func openPendingCoexistenceQuote(child *models.Message, key string) (pendingCoexistenceQuote, bool) {
	var quote pendingCoexistenceQuote
	sealed, ok := child.Metadata[models.MessageCoexistencePendingQuoteMetadataKey].(string)
	if !ok || key == "" || len(sealed) > 64*1024 || !appcrypto.IsEncrypted(sealed) {
		return quote, false
	}
	plain, err := appcrypto.Decrypt(sealed, key)
	if err != nil || json.Unmarshal([]byte(plain), &quote) != nil {
		return quote, false
	}
	return quote, quote.OrganizationID == child.OrganizationID && quote.AccountID != uuid.Nil &&
		quote.ContactID == child.ContactID && quote.ReplyID == child.ID && quote.Target.validFor(child)
}

func transcriptQuoteVisible(child, parent *models.Message, cutoff *time.Time) bool {
	return parent != nil && parent.ID != child.ID && !parent.DeletedAt.Valid &&
		parent.OrganizationID == child.OrganizationID && parent.ContactID == child.ContactID &&
		parent.WhatsAppAccount == child.WhatsAppAccount &&
		child.Metadata[coexistenceMediaRevokedMetadataKey] != true &&
		parent.Metadata[coexistenceMediaRevokedMetadataKey] != true &&
		(cutoff == nil || !parent.CreatedAt.Before(*cutoff))
}

func transcriptQuoteIDs(args *fasthttp.Args) ([]uuid.UUID, bool) {
	raw := args.PeekMulti("quote_ids")
	ack := args.PeekMulti("acknowledge")
	accounts := args.PeekMulti("account")
	if len(raw) != 1 || len(raw[0]) < 36 || len(raw[0]) > transcriptQuoteMaxIDs*37-1 ||
		len(ack) != 1 || string(ack[0]) != "false" ||
		len(accounts) > 1 || (len(accounts) == 1 && len(accounts[0]) == 0) ||
		args.Has("page") || args.Has("limit") || args.Has("before_id") {
		return nil, false
	}
	parts := strings.Split(string(raw[0]), ",")
	if len(parts) > transcriptQuoteMaxIDs {
		return nil, false
	}
	ids := make([]uuid.UUID, 0, len(parts))
	seen := make(map[uuid.UUID]bool, len(parts))
	for _, part := range parts {
		id, err := uuid.Parse(part)
		if err != nil || id == uuid.Nil || id.String() != part || seen[id] {
			return nil, false
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, true
}

type transcriptQuoteUpdate struct {
	ID               uuid.UUID     `json:"id"`
	ReplyToMessageID uuid.UUID     `json:"reply_to_message_id"`
	ReplyToMessage   *ReplyPreview `json:"reply_to_message"`
}

func transcriptReplyPreview(parent *models.Message) *ReplyPreview {
	return &ReplyPreview{
		ID:          parent.ID.String(),
		Content:     map[string]string{"body": parent.Content},
		MessageType: parent.MessageType,
		Direction:   parent.Direction,
	}
}

func (a *App) getTranscriptQuoteUpdates(r *fastglue.Request, scope *gorm.DB, ids []uuid.UUID,
	organizationID, contactID uuid.UUID, cutoff *time.Time) error {
	var children []models.Message
	if err := scope.Select("id", "organization_id", "contact_id", "whats_app_account", "whats_app_message_id",
		"created_at", "inbox_conversation_id", "is_reply", "metadata").
		Where("id IN ? AND reply_to_message_id IS NULL", ids).
		Order("COALESCE(ingested_at, created_at) ASC, id ASC").
		Limit(transcriptQuoteMaxIDs).
		Find(&children).Error; err != nil {
		a.Log.Error("Failed to load transcript quote references", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load transcript quotes", nil, "")
	}
	a.resolveTranscriptQuotes(children, organizationID, contactID, cutoff)
	proofRows := make([]*models.Message, 0, len(children)*2)
	for i := range children {
		child := &children[i]
		if child.ReplyToMessageID != nil && child.ReplyToMessage != nil {
			proofRows = append(proofRows, child, child.ReplyToMessage)
		}
	}
	proof := a.proveNativeReplyBridges(organizationID, contactID, proofRows)
	updates := make([]transcriptQuoteUpdate, 0, len(children))
	for i := range children {
		child := &children[i]
		if child.ReplyToMessageID == nil || child.ReplyToMessage == nil {
			continue
		}
		preview := transcriptReplyPreview(child.ReplyToMessage)
		if proof.allows(child, uuid.Nil) && proof.allows(child.ReplyToMessage, uuid.Nil) {
			if scoped := projectStoredReplyContext(child, cutoff); scoped != nil {
				preview = scoped
			}
		}
		updates = append(updates, transcriptQuoteUpdate{ID: child.ID, ReplyToMessageID: *child.ReplyToMessageID,
			ReplyToMessage: preview})
	}
	return r.SendEnvelope(map[string]any{"quote_updates": updates})
}

func (a *App) buildTranscriptMessagesResponse(messages []models.Message, organizationID, contactID uuid.UUID, cutoff *time.Time) []MessageResponse {
	known := make([]bool, len(messages))
	originalParents := make([]*models.Message, len(messages))
	for i := range messages {
		child := &messages[i]
		known[i] = child.OrganizationID == organizationID && child.ContactID == contactID &&
			readableReplyContextChild(child, cutoff) && hasStoredReplyContext(child)
		if known[i] {
			originalParents[i] = child.ReplyToMessage
		}
	}
	pending := a.resolveTranscriptQuotes(messages, organizationID, contactID, cutoff)
	response := a.buildMessagesResponse(messages)
	for _, i := range pending {
		response[i].QuotePending = response[i].ReplyToMessage == nil
		known[i] = response[i].QuotePending || known[i]
	}
	proofRows := make([]*models.Message, 0, len(messages)*3)
	for i := range messages {
		child := &messages[i]
		if known[i] || child.ReplyToMessage != nil {
			proofRows = append(proofRows, child, originalParents[i], child.ReplyToMessage)
		}
	}
	proof := a.proveNativeReplyBridges(organizationID, contactID, proofRows)
	for i := range messages {
		child := &messages[i]
		if child.OrganizationID != organizationID || child.ContactID != contactID ||
			!readableReplyContextChild(child, cutoff) || !proof.allows(child, uuid.Nil) ||
			!proof.allows(originalParents[i], uuid.Nil) || !proof.allows(child.ReplyToMessage, uuid.Nil) {
			continue
		}
		if preview := projectStoredReplyContext(child, cutoff); preview != nil {
			response[i].ReplyToMessage = preview
		} else if child.ReplyToMessage == nil {
			response[i].ReplyContextUnavailable = known[i]
		}
	}
	return response
}

// Peek at the original only from an already-authorized native page. The link
// stays an in-memory projection: another viewer may have a different cutoff.
// Return authenticated pending child positions so the caller can expose a
// boolean without sharing the reference or the reason it could not resolve.
func (a *App) resolveTranscriptQuotes(messages []models.Message, organizationID, contactID uuid.UUID, cutoff *time.Time) []int {
	for i := range messages {
		child := &messages[i]
		if child.ReplyToMessageID != nil && (!transcriptQuoteVisible(child, child.ReplyToMessage, cutoff) ||
			child.ReplyToMessage.ID != *child.ReplyToMessageID) {
			child.ReplyToMessageID, child.ReplyToMessage = nil, nil
		}
	}
	if len(messages) == 0 || len(messages) > 100 {
		return nil
	}
	key := a.coexistenceQuoteEncryptionKey()
	if key == "" {
		return nil
	}
	pending := make(map[int]pendingCoexistenceQuote)
	var pendingChildren []int
	wamids := make([]string, 0)
	accountIDs := make([]uuid.UUID, 0)
	for i := range messages {
		child := &messages[i]
		if child.IsReply || child.OrganizationID != organizationID || child.ContactID != contactID ||
			child.Metadata[coexistenceMediaRevokedMetadataKey] == true {
			continue
		}
		if quote, ok := openPendingCoexistenceQuote(child, key); ok {
			pending[i] = quote
			pendingChildren = append(pendingChildren, i)
			wamids = append(wamids, quote.Target.WAMID)
			accountIDs = append(accountIDs, quote.AccountID)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	deadline := time.Now().Add(transcriptQuoteReadTimeout)
	_ = readTranscriptQuoteWithinSavepoint(a.DB, func(tx *gorm.DB) error {
		return a.readPendingTranscriptQuotes(tx, messages, organizationID, contactID, cutoff, deadline, pending, wamids, accountIDs)
	})
	return pendingChildren
}

func readTranscriptQuoteWithinSavepoint(db *gorm.DB, read func(*gorm.DB) error) error {
	if db.DisableNestedTransaction {
		return gorm.ErrInvalidTransaction
	}
	// Keep the caller's context for rollback. PostgreSQL cancels the timed out
	// statement without closing the connection or retiring the caller's context.
	return db.Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) error {
		var previous int
		if err := tx.Raw("SELECT setting::int FROM pg_catalog.pg_settings WHERE name = 'statement_timeout'").Scan(&previous).Error; err != nil {
			return err
		}
		millis := transcriptQuoteStatementMillis
		if previous > 0 && previous < millis {
			millis = previous
		}
		if err := tx.Exec("SELECT set_config('statement_timeout', ?, true)", fmt.Sprintf("%dms", millis)).Error; err != nil {
			return err
		}
		if err := read(tx); err != nil {
			return err
		}
		// Roll back this optional read scope on success as well to restore SET LOCAL.
		return errTranscriptQuoteReadOnly
	})
}

func (a *App) readPendingTranscriptQuotes(db *gorm.DB, messages []models.Message, organizationID, contactID uuid.UUID,
	cutoff *time.Time, deadline time.Time, pending map[int]pendingCoexistenceQuote, wamids []string, accountIDs []uuid.UUID) error {
	// Skip unresolved references whose targets are still missing or outside the
	// viewer's page scope before doing their more expensive ownership proof.
	candidates := db.Model(&models.Message{}).Where("organization_id = ? AND contact_id = ? AND whats_app_message_id IN ?",
		organizationID, contactID, wamids)
	if cutoff != nil {
		candidates = candidates.Where("created_at >= ?", cutoff.UTC())
	}
	var present []string
	if err := candidates.Distinct().Pluck("whats_app_message_id", &present).Error; err != nil {
		return err
	}
	if len(present) == 0 {
		return nil
	}
	visibleTargets := make(map[string]bool, len(present))
	for _, wamid := range present {
		visibleTargets[wamid] = true
	}
	var accounts []models.WhatsAppAccount
	if err := db.Select("id", "organization_id", "name", "phone_id").Where("organization_id = ? AND id IN ?",
		organizationID, accountIDs).Find(&accounts).Error; err != nil {
		return err
	}
	accountByID := make(map[uuid.UUID]*models.WhatsAppAccount, len(accounts))
	for i := range accounts {
		accountByID[accounts[i].ID] = &accounts[i]
	}
	type proofKey struct {
		account uuid.UUID
		target  coexistenceQuoteTarget
	}
	proven := make(map[proofKey]*models.Message)
	scoped := a.scopedApp(db, organizationID)
	// Start with the most recent messages if the optional budget is exhausted.
	for i := len(messages) - 1; i >= 0; i-- {
		quote, ok := pending[i]
		if !ok {
			continue
		}
		if !time.Now().Before(deadline) {
			break
		}
		child := &messages[i]
		account := accountByID[quote.AccountID]
		if account == nil || account.Name != child.WhatsAppAccount || !visibleTargets[quote.Target.WAMID] {
			continue
		}
		k := proofKey{account: quote.AccountID, target: quote.Target}
		parent, checked := proven[k]
		if !checked {
			identity := coexistenceContactIdentity{
				Phone: quote.Target.Phone, UserID: quote.Target.UserID, ParentUserID: quote.Target.ParentUserID,
			}
			resolved, err := scoped.resolveWhatsAppMessage(account, whatsAppMessageLookup{
				WAMID: quote.Target.WAMID, ContactID: contactID, Identity: identity, exactContact: true,
			})
			var queryError *pgconn.PgError
			if errors.As(err, &queryError) {
				return err
			}
			if err == nil && resolved.Contact.ID == contactID {
				parent = &resolved.Message
			}
			proven[k] = parent
		}
		if transcriptQuoteVisible(child, parent, cutoff) {
			child.IsReply = true
			child.ReplyToMessageID, child.ReplyToMessage = &parent.ID, parent
		}
	}
	return nil
}
