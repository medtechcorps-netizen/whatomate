package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CoexistenceStateSyncItem is the current smb_app_state_sync item shape. Meta
// sends one or more typed changes under value.state_sync; the older flat
// contact_* fields were never part of the production contract.
type CoexistenceStateSyncItem struct {
	Type    string `json:"type"`
	Action  string `json:"action"`
	Contact struct {
		FullName     string `json:"full_name,omitempty"`
		FirstName    string `json:"first_name,omitempty"`
		PhoneNumber  string `json:"phone_number,omitempty"`
		UserID       string `json:"user_id,omitempty"`
		ParentUserID string `json:"parent_user_id,omitempty"`
		Username     string `json:"username,omitempty"`
	} `json:"contact"`
	Metadata struct {
		Timestamp string `json:"timestamp,omitempty"`
	} `json:"metadata,omitempty"`
}

// CoexistenceMessage adds the edit/revoke/history fields used by Coexistence
// to the application's regular WhatsApp message representation.
type CoexistenceMessage struct {
	IncomingTextMessage
	HistoryContext *CoexistenceHistoryContext `json:"history_context,omitempty"`
	Edit           *struct {
		OriginalMessageID string              `json:"original_message_id"`
		Message           IncomingTextMessage `json:"message"`
	} `json:"edit,omitempty"`
	Revoke *struct {
		OriginalMessageID string `json:"original_message_id"`
	} `json:"revoke,omitempty"`
}

type CoexistenceHistoryContext struct {
	Status string `json:"status"`
}

// CoexistenceWebhookContact is the identity/profile sidecar Meta includes
// when an end user is addressable by a BSUID but no longer exposes a phone.
type CoexistenceWebhookContact struct {
	Profile struct {
		Name     string `json:"name"`
		Username string `json:"username,omitempty"`
	} `json:"profile"`
	WaID         string `json:"wa_id,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	ParentUserID string `json:"parent_user_id,omitempty"`
}

type CoexistenceThreadContext struct {
	WaID         string `json:"wa_id,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	ParentUserID string `json:"parent_user_id,omitempty"`
	Username     string `json:"username,omitempty"`
}

type CoexistenceHistoryThread struct {
	ID       string                   `json:"id,omitempty"`
	Context  CoexistenceThreadContext `json:"context,omitempty"`
	Messages []CoexistenceMessage     `json:"messages,omitempty"`
}

type CoexistenceHistoryBatch struct {
	Metadata struct {
		Phase      int `json:"phase"`
		ChunkOrder int `json:"chunk_order"`
		Progress   int `json:"progress"`
	} `json:"metadata,omitempty"`
	Threads []CoexistenceHistoryThread `json:"threads,omitempty"`
	Errors  []WebhookStatusError       `json:"errors,omitempty"`
}

type CoexistenceDisconnection struct {
	Reason      string `json:"reason,omitempty"`
	InitiatedBy string `json:"initiated_by,omitempty"`
}

type coexistenceContactIdentity struct {
	Phone        string
	UserID       string
	ParentUserID string
	Username     string
	ProfileName  string
	FallbackKey  string
}

type whatsAppAdmissionWinnerKind string

const (
	whatsAppAdmissionWinnerNone    whatsAppAdmissionWinnerKind = ""
	whatsAppAdmissionWinnerMessage whatsAppAdmissionWinnerKind = "message"
	whatsAppAdmissionWinnerReview  whatsAppAdmissionWinnerKind = "identity_review"
)

type whatsAppAdmissionWinner struct {
	Kind    whatsAppAdmissionWinnerKind
	Message *models.Message
	Review  *models.InboundEvent
}

func (a *App) stagedWhatsAppIdentityReviewPayload(
	message IncomingTextMessage,
	generation uint64,
) (models.JSONB, bool, error) {
	if a == nil || strings.TrimSpace(message.ID) == "" || strings.TrimSpace(message.Type) == "" {
		return nil, false, errors.New("staged WhatsApp content is incomplete")
	}
	extracted := a.extractMessageContentForPersistence(message)
	payload := models.JSONB{
		"schema_version": 1,
		"message_type":   strings.TrimSpace(extracted.Type),
		"content":        extracted.Text,
	}
	mediaID, mediaSHA := coexistenceMediaIdentity(message)
	if mediaID == "" {
		return payload, false, nil
	}
	if generation == 0 {
		return nil, false, errors.New("staged WhatsApp media generation is required")
	}
	payload["media_id"] = mediaID
	payload["media_sha256"] = mediaSHA
	payload["media_generation"] = generation
	payload["media_status"] = "pending"
	if extracted.Media != nil {
		payload["media_mime_type"] = extracted.Media.MediaMimeType
		payload["media_filename"] = extracted.Media.MediaFilename
	}
	payload["media_revision"] = coexistenceStagedMediaRevisionDigest(payload, generation)
	return payload, true, nil
}

func (identity coexistenceContactIdentity) primaryUserID() string {
	if userID := strings.TrimSpace(identity.UserID); userID != "" {
		return userID
	}
	return strings.TrimSpace(identity.ParentUserID)
}

func (identity coexistenceContactIdentity) displayName() string {
	if profileName := strings.TrimSpace(identity.ProfileName); profileName != "" {
		return profileName
	}
	return strings.TrimSpace(identity.Username)
}

func coexistenceIdentityPlaceholder(identity coexistenceContactIdentity) string {
	kind := "event"
	value := strings.TrimSpace(identity.FallbackKey)
	if userID := identity.primaryUserID(); userID != "" {
		kind = "bsuid"
		value = userID
	} else if username := strings.TrimSpace(identity.Username); username != "" {
		kind = "user"
		value = username
	}
	if value == "" {
		value = "unknown"
	}
	digest := sha256.Sum256([]byte(value))
	// Contact.PhoneNumber is VARCHAR(50). The colon also makes this clearly
	// non-dialable, so downstream delivery must use Contact.BSUID.
	return kind + ":" + hex.EncodeToString(digest[:])[:40]
}

func isCoexistenceLifecycleEvent(event string) bool {
	switch strings.ToUpper(strings.TrimSpace(event)) {
	case "PARTNER_REMOVED", "ACCOUNT_OFFBOARDED", "ACCOUNT_RECONNECTED":
		return true
	default:
		return false
	}
}

// withCoexistencePhoneAccount resolves the tenant before opening a committed
// transaction. This keeps the same tenant/RLS boundary for both production and
// non-RLS tests and avoids relying on a credential-bearing account cache for a
// webhook that only needs account identity.
func (a *App) withCoexistencePhoneAccount(
	phoneNumberID string,
	wamids []string,
	fn func(*App, *models.WhatsAppAccount) error,
) error {
	phoneNumberID = strings.TrimSpace(phoneNumberID)
	if phoneNumberID == "" {
		return errors.New("coexistence webhook phone number ID is required")
	}
	organizationID, err := a.resolveWhatsAppOrganization(phoneNumberID)
	if err != nil {
		return fmt.Errorf("resolve coexistence tenant: %w", err)
	}
	return a.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		if err := database.LockWhatsAppWAMIDScopes(scoped.DB, organizationID, wamids...); err != nil {
			return fmt.Errorf("lock coexistence WhatsApp message admissions: %w", err)
		}
		if err := database.LockOrganizationPolicyScope(scoped.DB, organizationID); err != nil {
			return fmt.Errorf("lock coexistence organization admission: %w", err)
		}
		var account models.WhatsAppAccount
		if err := scoped.DB.Where(
			"organization_id = ? AND BTRIM(phone_id) = BTRIM(?)",
			organizationID,
			phoneNumberID,
		).First(&account).Error; err != nil {
			return fmt.Errorf("load coexistence WhatsApp account: %w", err)
		}
		if !account.IsSMB {
			// Coexistence fields can share a WABA callback with classic Cloud API
			// numbers. A phone-number match alone must never opt a classic account
			// into Coexistence persistence or create Coexistence state for it.
			scoped.Log.Info(
				"Ignoring coexistence webhook for classic WhatsApp account",
				"account_id", account.ID,
				"phone_number_id", phoneNumberID,
			)
			return nil
		}
		if _, err := channelapi.EnsureLegacyMetaWhatsAppAccount(scoped.DB, channelapi.LegacyMetaAccountRef{
			ID:             account.ID,
			OrganizationID: account.OrganizationID,
			Name:           account.Name,
			Status:         account.Status,
		}); err != nil {
			return fmt.Errorf("ensure coexistence channel account: %w", err)
		}
		// Acquire account/channel authority before history/contact callbacks take
		// their own locks. Nested persistence calls reuse this transaction token.
		if err := scoped.prepareWhatsAppMessageAuthority(&account); err != nil {
			return err
		}
		return fn(scoped, &account)
	})
}

// lookupWhatsAppAdmissionWinner is the cross-store first-admission read. Every
// caller owns the common organization policy row until its surrounding
// transaction commits, so no competing Message or reserved review receipt can
// appear between this lookup and the chosen insert.
func (a *App) lookupWhatsAppAdmissionWinner(
	organizationID uuid.UUID,
	wamid string,
) (*whatsAppAdmissionWinner, error) {
	if a == nil || a.DB == nil || organizationID == uuid.Nil || strings.TrimSpace(wamid) == "" {
		return nil, errors.New("WhatsApp admission lookup is incomplete")
	}
	wamid = strings.TrimSpace(wamid)
	var messages []models.Message
	if err := a.DB.Unscoped().Where(
		`messages.organization_id = ?
			AND BTRIM(messages.whats_app_message_id) = ?
			AND (
				messages.inbox_conversation_id IS NULL
				OR COALESCE(messages.metadata, '{}'::jsonb) @> ?::jsonb
			)`,
		organizationID,
		wamid,
		database.WhatsAppWAMIDOwnerMetadataJSON,
	).Limit(2).Find(&messages).Error; err != nil {
		return nil, fmt.Errorf("lookup admitted WhatsApp message: %w", err)
	}
	var reviews []models.InboundEvent
	if err := a.DB.Unscoped().Where(
		"organization_id = ? AND protocol = ? AND event_type = ? AND BTRIM(provider_event_id) = ?",
		organizationID,
		models.WhatsAppIdentityReviewInboundProtocol,
		models.WhatsAppIdentityReviewPendingEvent,
		wamid,
	).Limit(2).Find(&reviews).Error; err != nil {
		return nil, fmt.Errorf("lookup staged WhatsApp review: %w", err)
	}
	if len(messages) > 1 || len(reviews) > 1 || (len(messages) == 1 && len(reviews) == 1) {
		return nil, errors.New("ambiguous cross-store WhatsApp admission winner")
	}
	winner := &whatsAppAdmissionWinner{Kind: whatsAppAdmissionWinnerNone}
	if len(messages) == 1 {
		winner.Kind = whatsAppAdmissionWinnerMessage
		winner.Message = &messages[0]
	}
	if len(reviews) == 1 {
		winner.Kind = whatsAppAdmissionWinnerReview
		winner.Review = &reviews[0]
	}
	return winner, nil
}

// persistWhatsAppIdentityReviewEvent commits a sanitized contact-free receipt.
// The caller must already own the organization admission fence and must have
// created/reused the complete immutable review hold in this transaction.
func (a *App) persistWhatsAppIdentityReviewEvent(
	account *models.WhatsAppAccount,
	channelAccount *models.ChannelAccount,
	holdID uuid.UUID,
	message IncomingTextMessage,
) (*models.InboundEvent, bool, error) {
	if a == nil || a.DB == nil || account == nil || channelAccount == nil ||
		account.ID == uuid.Nil || account.OrganizationID == uuid.Nil || channelAccount.ID == uuid.Nil ||
		holdID == uuid.Nil || strings.TrimSpace(message.ID) == "" {
		return nil, false, errors.New("staged WhatsApp review admission is incomplete")
	}
	if channelAccount.OrganizationID != account.OrganizationID ||
		channelAccount.Channel != models.ChannelWhatsApp || channelAccount.Provider != channelapi.LegacyMetaProvider {
		return nil, false, errors.New("staged WhatsApp review channel authority is invalid")
	}
	boundAccountID, err := channelapi.LegacyMetaWhatsAppAccountID(channelAccount)
	if err != nil || boundAccountID != account.ID {
		return nil, false, errors.New("staged WhatsApp review channel binding changed")
	}
	wamid := strings.TrimSpace(message.ID)
	if err := database.LockWhatsAppWAMIDScopes(a.DB, account.OrganizationID, wamid); err != nil {
		return nil, false, fmt.Errorf("lock staged WhatsApp review admission: %w", err)
	}
	winner, err := a.lookupWhatsAppAdmissionWinner(account.OrganizationID, message.ID)
	if err != nil {
		return nil, false, err
	}
	if winner.Kind == whatsAppAdmissionWinnerMessage {
		return nil, false, errors.New("WhatsApp WAMID already belongs to a message")
	}
	if winner.Kind == whatsAppAdmissionWinnerReview {
		return winner.Review, false, nil
	}

	payload, hasMedia, err := a.stagedWhatsAppIdentityReviewPayload(message, 1)
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	event := models.InboundEvent{
		BaseModel: models.BaseModel{
			ID:        uuid.NewSHA1(account.OrganizationID, []byte("whatsapp-identity-review:"+wamid)),
			CreatedAt: now,
			UpdatedAt: now,
		},
		OrganizationID:   account.OrganizationID,
		ChannelAccountID: channelAccount.ID,
		DedupeKey:        "identity-review:" + wamid,
		ProviderEventID:  wamid,
		EventType:        models.WhatsAppIdentityReviewPendingEvent,
		Status:           models.InboundEventStatusPending,
		SignatureValid:   true,
		ReceivedAt:       now,
		Protocol:         models.WhatsAppIdentityReviewInboundProtocol,
		ReviewHoldID:     &holdID,
		Headers:          models.JSONB{},
		Payload:          payload,
	}
	create := a.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&event)
	if create.Error != nil {
		return nil, false, fmt.Errorf("create staged WhatsApp review receipt: %w", create.Error)
	}
	if create.RowsAffected != 1 {
		winner, err = a.lookupWhatsAppAdmissionWinner(account.OrganizationID, wamid)
		if err != nil {
			return nil, false, err
		}
		if winner.Kind != whatsAppAdmissionWinnerReview || winner.Review == nil {
			return nil, false, errors.New("staged WhatsApp review admission lost its winner")
		}
		return winner.Review, false, nil
	}

	// Older rolling-version writers may not yet take the organization fence.
	// Recheck both stores after insertion so their concurrent Message wins by
	// rolling this transaction back instead of allowing two durable owners.
	var messageCount int64
	if err := a.DB.Unscoped().Model(&models.Message{}).Where(
		`messages.organization_id = ?
			AND BTRIM(messages.whats_app_message_id) = ?
			AND (
				messages.inbox_conversation_id IS NULL
				OR COALESCE(messages.metadata, '{}'::jsonb) @> ?::jsonb
			)`,
		account.OrganizationID,
		wamid,
		database.WhatsAppWAMIDOwnerMetadataJSON,
	).Count(&messageCount).Error; err != nil {
		return nil, false, fmt.Errorf("recheck staged WhatsApp admission: %w", err)
	}
	if messageCount != 0 {
		return nil, false, errors.New("concurrent legacy Message won WhatsApp admission")
	}
	if hasMedia {
		if err := a.ensureCoexistenceStagedMediaJob(account, event.ID); err != nil {
			return nil, false, err
		}
	}
	return &event, true, nil
}

func ensureCoexistenceState(
	tx *gorm.DB,
	account *models.WhatsAppAccount,
) (*models.WhatsAppCoexistenceState, error) {
	if tx == nil || account == nil || account.ID == uuid.Nil || account.OrganizationID == uuid.Nil {
		return nil, errors.New("coexistence account state identity is required")
	}
	candidate := models.WhatsAppCoexistenceState{
		ID:                uuid.New(),
		OrganizationID:    account.OrganizationID,
		WhatsAppAccountID: account.ID,
		OnboardingStatus:  models.CoexistenceOnboardingStatusPending,
		SyncStatus:        models.CoexistenceSyncStatusNotRequested,
		ContactSyncStatus: models.CoexistenceSyncStatusNotRequested,
		HistoryConsent:    models.CoexistenceHistoryConsentUnknown,
		HistorySyncStatus: models.CoexistenceSyncStatusNotRequested,
		LifecycleStatus:   models.CoexistenceLifecycleStatusUnknown,
		LifecycleMetadata: models.JSONB{},
		Version:           1,
	}
	if err := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "organization_id"}, {Name: "whats_app_account_id"}},
		DoNothing: true,
	}).Create(&candidate).Error; err != nil {
		return nil, err
	}

	var state models.WhatsAppCoexistenceState
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND whats_app_account_id = ?",
		account.OrganizationID,
		account.ID,
	).First(&state).Error; err != nil {
		return nil, err
	}
	return &state, nil
}

func coexistenceSyncRollup(
	contactStatus models.CoexistenceSyncStatus,
	historyStatus models.CoexistenceSyncStatus,
) (models.CoexistenceSyncStatus, models.CoexistenceOnboardingStatus, bool) {
	if contactStatus == models.CoexistenceSyncStatusFailed || historyStatus == models.CoexistenceSyncStatusFailed {
		return models.CoexistenceSyncStatusFailed, models.CoexistenceOnboardingStatusFailed, false
	}
	if contactStatus == models.CoexistenceSyncStatusExpired || historyStatus == models.CoexistenceSyncStatusExpired {
		return models.CoexistenceSyncStatusExpired, models.CoexistenceOnboardingStatusExpired, false
	}
	historyTerminal := historyStatus == models.CoexistenceSyncStatusCompleted ||
		historyStatus == models.CoexistenceSyncStatusDeclined
	// Meta acknowledges the one-time contact request with a request_id but does
	// not send a request-correlated completion event. smb_app_state_sync remains
	// active for ordinary address-book deltas, so Requested is terminal success
	// for this stage. Completed remains accepted for pre-migration rows.
	contactTerminal := contactStatus == models.CoexistenceSyncStatusRequested ||
		contactStatus == models.CoexistenceSyncStatusCompleted
	if contactTerminal && historyTerminal {
		return models.CoexistenceSyncStatusCompleted, models.CoexistenceOnboardingStatusReady, true
	}
	if contactTerminal && historyStatus == models.CoexistenceSyncStatusRequested {
		// A history request can legitimately produce no callback. Preserve the
		// distinction between provider acceptance and an observed import, while
		// avoiding a permanently "syncing" onboarding state.
		return models.CoexistenceSyncStatusRequested, models.CoexistenceOnboardingStatusConnected, false
	}
	return models.CoexistenceSyncStatusInProgress, models.CoexistenceOnboardingStatusSyncing, false
}

func normalizeCoexistencePhone(phone string) string {
	return strings.TrimPrefix(strings.TrimSpace(phone), "+")
}

func isCoexistencePlaceholderPhone(phone string) bool {
	phone = strings.TrimSpace(phone)
	return strings.HasPrefix(phone, "bsuid:") ||
		strings.HasPrefix(phone, "user:") ||
		strings.HasPrefix(phone, "event:")
}

func (a *App) findCoexistenceContactByUserID(
	organizationID uuid.UUID,
	identity coexistenceContactIdentity,
) (*models.Contact, error) {
	identifiers := make([]string, 0, 2)
	if userID := strings.TrimSpace(identity.UserID); userID != "" {
		identifiers = append(identifiers, userID)
	}
	if parentUserID := strings.TrimSpace(identity.ParentUserID); parentUserID != "" &&
		(parentUserID != strings.TrimSpace(identity.UserID)) {
		identifiers = append(identifiers, parentUserID)
	}
	if len(identifiers) == 0 {
		return nil, gorm.ErrRecordNotFound
	}

	var contact models.Contact
	if err := a.DB.Where(
		"organization_id = ? AND bs_uid IN ?",
		organizationID,
		identifiers,
	).Order("created_at ASC").First(&contact).Error; err != nil {
		return nil, err
	}
	canonical, err := contactutil.ResolveCanonicalContactForUpdate(a.DB, organizationID, contact.ID)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func (a *App) findCoexistenceContactByPhone(
	organizationID uuid.UUID,
	phone string,
) (*models.Contact, error) {
	contact, err := contactutil.FindContact(a.DB, organizationID, phone)
	if err != nil {
		return nil, err
	}
	canonical, err := contactutil.ResolveCanonicalContactForUpdate(a.DB, organizationID, contact.ID)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func (a *App) updateCoexistenceContactIdentity(
	account *models.WhatsAppAccount,
	contact *models.Contact,
	identity coexistenceContactIdentity,
	phoneUnavailable bool,
) error {
	if account == nil || contact == nil {
		return errors.New("coexistence contact identity requires an account and contact")
	}
	metadata := cloneMessageMetadata(contact.Metadata)
	userID := strings.TrimSpace(identity.UserID)
	parentUserID := strings.TrimSpace(identity.ParentUserID)
	username := strings.TrimSpace(identity.Username)
	if userID != "" {
		metadata["coexistence_user_id"] = userID
	}
	if parentUserID != "" {
		metadata["coexistence_parent_user_id"] = parentUserID
	}
	if username != "" {
		metadata["coexistence_username"] = username
	}
	metadata["coexistence_phone_unavailable"] = phoneUnavailable
	metadata["coexistence_phone_placeholder"] = isCoexistencePlaceholderPhone(contact.PhoneNumber)

	updates := map[string]any{
		"metadata":          metadata,
		"whats_app_account": account.Name,
	}
	if profileName := identity.displayName(); profileName != "" {
		updates["profile_name"] = profileName
	}
	primaryUserID := identity.primaryUserID()
	canReplaceUserID := contact.BSUID == "" || contact.BSUID == primaryUserID ||
		(parentUserID != "" && contact.BSUID == parentUserID)
	if primaryUserID != "" && canReplaceUserID {
		updates["bs_uid"] = primaryUserID
		contact.BSUID = primaryUserID
	} else if primaryUserID != "" {
		metadata["coexistence_conflicting_user_id"] = primaryUserID
	}

	if err := a.DB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ?",
		account.OrganizationID,
		contact.ID,
	).Updates(updates).Error; err != nil {
		return err
	}
	contact.Metadata = metadata
	contact.WhatsAppAccount = account.Name
	if profileName, ok := updates["profile_name"].(string); ok {
		contact.ProfileName = profileName
	}
	return nil
}

func (a *App) noteCoexistenceContactReconciliation(
	account *models.WhatsAppAccount,
	placeholder *models.Contact,
	canonical *models.Contact,
	phone string,
) error {
	metadata := cloneMessageMetadata(placeholder.Metadata)
	metadata["coexistence_reconciled_contact_id"] = canonical.ID.String()
	metadata["coexistence_reconciled_phone"] = normalizeCoexistencePhone(phone)
	metadata["coexistence_phone_unavailable"] = false
	// The real-phone contact becomes the BSUID lookup target. Keep the old row
	// and its CRM history intact, but prevent ambiguous future BSUID resolution.
	return a.DB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ?",
		account.OrganizationID,
		placeholder.ID,
	).Updates(map[string]any{
		"bs_uid":   "",
		"metadata": metadata,
	}).Error
}

func (a *App) noteCoexistencePhoneConflict(
	account *models.WhatsAppAccount,
	contact *models.Contact,
	phoneOwner *models.Contact,
	phone string,
) error {
	metadata := cloneMessageMetadata(contact.Metadata)
	metadata["coexistence_phone_conflict"] = true
	metadata["coexistence_supplied_phone"] = normalizeCoexistencePhone(phone)
	if phoneOwner != nil {
		metadata["coexistence_phone_owner_contact_id"] = phoneOwner.ID.String()
	}
	return a.DB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ?",
		account.OrganizationID,
		contact.ID,
	).Update("metadata", metadata).Error
}

// getOrCreateCoexistenceContact accepts both legacy phone identities and the
// current BSUID-only webhook shape. When Meta later reveals a phone, the
// placeholder is upgraded only if that phone is not owned by another contact.
func (a *App) getOrCreateCoexistenceContact(
	account *models.WhatsAppAccount,
	identity coexistenceContactIdentity,
) (*models.Contact, bool, error) {
	if account == nil {
		return nil, false, errors.New("coexistence contact account is required")
	}
	identity.Phone = normalizeCoexistencePhone(identity.Phone)
	identity.UserID = strings.TrimSpace(identity.UserID)
	identity.ParentUserID = strings.TrimSpace(identity.ParentUserID)

	byUserID, userIDErr := a.findCoexistenceContactByUserID(account.OrganizationID, identity)
	if userIDErr != nil && !errors.Is(userIDErr, gorm.ErrRecordNotFound) {
		return nil, false, userIDErr
	}
	if userIDErr == nil {
		if identity.Phone != "" && normalizeCoexistencePhone(byUserID.PhoneNumber) != identity.Phone {
			phoneOwner, phoneErr := a.findCoexistenceContactByPhone(account.OrganizationID, identity.Phone)
			switch {
			case phoneErr == nil && phoneOwner.ID != byUserID.ID:
				primaryUserID := identity.primaryUserID()
				ownerAcceptsUserID := phoneOwner.BSUID == "" || phoneOwner.BSUID == primaryUserID ||
					(identity.ParentUserID != "" && phoneOwner.BSUID == identity.ParentUserID)
				if !ownerAcceptsUserID {
					if err := a.updateCoexistenceContactIdentity(account, byUserID, identity, true); err != nil {
						return nil, false, err
					}
					if err := a.noteCoexistencePhoneConflict(account, byUserID, phoneOwner, identity.Phone); err != nil {
						return nil, false, err
					}
					return byUserID, false, nil
				}
				if err := a.updateCoexistenceContactIdentity(account, phoneOwner, identity, false); err != nil {
					return nil, false, err
				}
				if err := a.noteCoexistenceContactReconciliation(account, byUserID, phoneOwner, identity.Phone); err != nil {
					return nil, false, err
				}
				return phoneOwner, false, nil
			case phoneErr != nil && !errors.Is(phoneErr, gorm.ErrRecordNotFound):
				return nil, false, phoneErr
			case errors.Is(phoneErr, gorm.ErrRecordNotFound):
				// Isolate a possible concurrent phone-owner race behind a savepoint.
				updateErr := a.DB.Transaction(func(tx *gorm.DB) error {
					return tx.Model(&models.Contact{}).Where(
						"organization_id = ? AND id = ?",
						account.OrganizationID,
						byUserID.ID,
					).Update("phone_number", identity.Phone).Error
				})
				if updateErr == nil {
					byUserID.PhoneNumber = identity.Phone
				} else if isUniqueViolation(updateErr) {
					phoneOwner, reloadErr := a.findCoexistenceContactByPhone(account.OrganizationID, identity.Phone)
					if reloadErr != nil {
						return nil, false, fmt.Errorf("reload concurrent coexistence phone owner: %w", reloadErr)
					}
					if phoneOwner.BSUID != "" && phoneOwner.BSUID != identity.primaryUserID() {
						if err := a.noteCoexistencePhoneConflict(account, byUserID, phoneOwner, identity.Phone); err != nil {
							return nil, false, err
						}
						return byUserID, false, nil
					}
					if err := a.updateCoexistenceContactIdentity(account, phoneOwner, identity, false); err != nil {
						return nil, false, err
					}
					if err := a.noteCoexistenceContactReconciliation(account, byUserID, phoneOwner, identity.Phone); err != nil {
						return nil, false, err
					}
					return phoneOwner, false, nil
				} else {
					return nil, false, updateErr
				}
			}
		}
		if err := a.updateCoexistenceContactIdentity(
			account,
			byUserID,
			identity,
			identity.Phone == "",
		); err != nil {
			return nil, false, err
		}
		return byUserID, false, nil
	}

	phone := identity.Phone
	phoneUnavailable := phone == ""
	if phoneUnavailable {
		phone = coexistenceIdentityPlaceholder(identity)
	}
	contact, created, err := a.getOrCreateInboundContact(account, phone, identity.displayName(), "")
	if err != nil {
		return nil, false, err
	}
	if err := a.updateCoexistenceContactIdentity(account, contact, identity, phoneUnavailable); err != nil {
		return nil, false, err
	}
	return contact, created, nil
}

func (a *App) findExistingCoexistenceContact(
	account *models.WhatsAppAccount,
	identity coexistenceContactIdentity,
) (*models.Contact, error) {
	if contact, err := a.findCoexistenceContactByUserID(account.OrganizationID, identity); err == nil {
		return contact, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if phone := normalizeCoexistencePhone(identity.Phone); phone != "" {
		return a.findCoexistenceContactByPhone(account.OrganizationID, phone)
	}
	if identity.primaryUserID() != "" || strings.TrimSpace(identity.Username) != "" {
		return a.findCoexistenceContactByPhone(account.OrganizationID, coexistenceIdentityPlaceholder(identity))
	}
	return nil, gorm.ErrRecordNotFound
}

func (a *App) persistContactStateSyncBeforeAck(
	phoneNumberID string,
	items []CoexistenceStateSyncItem,
) error {
	return a.withCoexistencePhoneAccount(phoneNumberID, nil, func(scoped *App, account *models.WhatsAppAccount) error {
		if _, err := ensureCoexistenceState(scoped.DB, account); err != nil {
			return fmt.Errorf("load coexistence state: %w", err)
		}
		for _, item := range items {
			if item.Type != "" && !strings.EqualFold(strings.TrimSpace(item.Type), "contact") {
				scoped.Log.Warn("Ignoring unsupported smb_app_state_sync item", "type", item.Type)
				continue
			}
			identity := coexistenceContactIdentity{
				Phone:        item.Contact.PhoneNumber,
				UserID:       item.Contact.UserID,
				ParentUserID: item.Contact.ParentUserID,
				Username:     item.Contact.Username,
				ProfileName:  item.Contact.FullName,
				FallbackKey:  item.Metadata.Timestamp,
			}
			if identity.ProfileName == "" {
				identity.ProfileName = item.Contact.FirstName
			}
			if identity.Phone == "" && identity.primaryUserID() == "" && strings.TrimSpace(identity.Username) == "" {
				scoped.Log.Warn("Ignoring contact state sync item without an addressable identity")
				continue
			}
			switch strings.ToLower(strings.TrimSpace(item.Action)) {
			case "add":
				contact, _, err := scoped.getOrCreateCoexistenceContact(account, identity)
				if err != nil {
					return fmt.Errorf("upsert synced coexistence contact: %w", err)
				}
				contactMetadata := cloneMessageMetadata(contact.Metadata)
				contactMetadata["coexistence_app_contact"] = true
				contactMetadata["coexistence_app_contact_synced_at"] = coexistenceMessageTime(
					item.Metadata.Timestamp,
					time.Now().UTC(),
				).Format(time.RFC3339)
				if err := scoped.DB.Model(&models.Contact{}).Where(
					"organization_id = ? AND id = ?",
					account.OrganizationID,
					contact.ID,
				).Updates(map[string]any{
					"whats_app_account": account.Name,
					"metadata":          contactMetadata,
				}).Error; err != nil {
					return fmt.Errorf("bind synced contact to account: %w", err)
				}
			case "remove":
				contact, err := scoped.findExistingCoexistenceContact(account, identity)
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				if err != nil {
					return fmt.Errorf("find synced contact for removal: %w", err)
				}
				// An address-book removal is not a CRM deletion. Preserve the
				// canonical Contact and all messages, assignments, CRM data, and
				// audit history; only record that it is no longer an app contact.
				contactMetadata := cloneMessageMetadata(contact.Metadata)
				contactMetadata["coexistence_app_contact"] = false
				contactMetadata["coexistence_app_contact_removed_at"] = coexistenceMessageTime(
					item.Metadata.Timestamp,
					time.Now().UTC(),
				).Format(time.RFC3339)
				if err := scoped.DB.Model(&models.Contact{}).Where(
					"organization_id = ? AND id = ?",
					account.OrganizationID,
					contact.ID,
				).Update("metadata", contactMetadata).Error; err != nil {
					return fmt.Errorf("mark synced contact removed from app: %w", err)
				}
			default:
				return fmt.Errorf("unsupported contact state sync action %q", item.Action)
			}
		}

		// smb_app_state_sync is a continuous address-book delta stream. It has no
		// request ID or terminal marker, so receiving any item cannot complete (or
		// repair) the one-time contact request state.
		return nil
	})
}

func coexistenceEchoDirection(_ CoexistenceMessage) models.Direction {
	// smb_message_echoes exclusively represents messages sent by the companion
	// Business App. Recipient identity may be phone-based, BSUID-only, or in the
	// contacts sidecar, but the direction is always outgoing.
	return models.DirectionOutgoing
}

func coexistenceOwnerWAMID(message CoexistenceMessage) (string, error) {
	switch strings.ToLower(strings.TrimSpace(message.Type)) {
	case "edit":
		if message.Edit == nil || strings.TrimSpace(message.Edit.OriginalMessageID) == "" {
			return "", errors.New("coexistence edit is missing original_message_id")
		}
		return strings.TrimSpace(message.Edit.OriginalMessageID), nil
	case "revoke":
		if message.Revoke == nil || strings.TrimSpace(message.Revoke.OriginalMessageID) == "" {
			return "", errors.New("coexistence revoke is missing original_message_id")
		}
		return strings.TrimSpace(message.Revoke.OriginalMessageID), nil
	default:
		if strings.TrimSpace(message.ID) == "" {
			return "", errors.New("coexistence message has no WhatsApp message ID")
		}
		return strings.TrimSpace(message.ID), nil
	}
}

func coexistenceOwnerWAMIDs(messages []CoexistenceMessage) ([]string, error) {
	wamids := make([]string, 0, len(messages))
	for _, message := range messages {
		wamid, err := coexistenceOwnerWAMID(message)
		if err != nil {
			return nil, err
		}
		wamids = append(wamids, wamid)
	}
	return wamids, nil
}

func coexistenceHistoryWAMIDs(
	batches []CoexistenceHistoryBatch,
	mediaDetails []CoexistenceMessage,
) ([]string, error) {
	wamids := make([]string, 0, len(mediaDetails))
	for _, batch := range batches {
		if len(batch.Errors) > 0 {
			continue
		}
		for _, thread := range batch.Threads {
			for _, message := range thread.Messages {
				if strings.TrimSpace(message.ID) == "" {
					return nil, errors.New("coexistence history message has no WhatsApp message ID")
				}
				wamids = append(wamids, strings.TrimSpace(message.ID))
			}
		}
	}
	for _, detail := range mediaDetails {
		if strings.TrimSpace(detail.ID) == "" {
			return nil, errors.New("history media detail has no WhatsApp message ID")
		}
		wamids = append(wamids, strings.TrimSpace(detail.ID))
	}
	return wamids, nil
}

func coexistenceMessageContactIdentity(
	message CoexistenceMessage,
	direction models.Direction,
	fallback coexistenceContactIdentity,
	contacts []CoexistenceWebhookContact,
) coexistenceContactIdentity {
	identity := fallback
	identity.FallbackKey = strings.TrimSpace(message.ID)
	if direction == models.DirectionOutgoing {
		if value := strings.TrimSpace(message.To); value != "" {
			identity.Phone = value
		}
		if value := strings.TrimSpace(message.ToUserID); value != "" {
			identity.UserID = value
		}
		if value := strings.TrimSpace(message.ToParentUserID); value != "" {
			identity.ParentUserID = value
		}
	} else {
		if value := strings.TrimSpace(message.From); value != "" {
			identity.Phone = value
		}
		if value := strings.TrimSpace(message.FromUserID); value != "" {
			identity.UserID = value
		}
		if value := strings.TrimSpace(message.FromParentUserID); value != "" {
			identity.ParentUserID = value
		}
	}

	matched := -1
	for index, contact := range contacts {
		phoneMatches := identity.Phone != "" && sameWhatsAppPhone(identity.Phone, contact.WaID)
		userMatches := identity.primaryUserID() != "" &&
			(identity.primaryUserID() == strings.TrimSpace(contact.UserID) ||
				identity.primaryUserID() == strings.TrimSpace(contact.ParentUserID))
		if phoneMatches || userMatches {
			matched = index
			break
		}
	}
	if matched < 0 && len(contacts) == 1 {
		matched = 0
	}
	if matched >= 0 {
		contact := contacts[matched]
		if identity.Phone == "" {
			identity.Phone = strings.TrimSpace(contact.WaID)
		}
		if identity.UserID == "" {
			identity.UserID = strings.TrimSpace(contact.UserID)
		}
		if identity.ParentUserID == "" {
			identity.ParentUserID = strings.TrimSpace(contact.ParentUserID)
		}
		if identity.Username == "" {
			identity.Username = strings.TrimSpace(contact.Profile.Username)
		}
		if identity.ProfileName == "" {
			identity.ProfileName = strings.TrimSpace(contact.Profile.Name)
		}
	}
	return identity
}

type whatsAppIdentityReviewVerifiedBinding struct {
	Protocol          string    `json:"protocol"`
	OrganizationID    uuid.UUID `json:"organization_id"`
	WhatsAppAccountID uuid.UUID `json:"whatsapp_account_id"`
	WAMID             string    `json:"wamid"`
	WebhookBodySHA256 string    `json:"webhook_body_sha256"`
}

type whatsAppIdentityReviewSelectorBinding struct {
	Protocol           string    `json:"protocol"`
	OrganizationID     uuid.UUID `json:"organization_id"`
	WhatsAppAccountID  uuid.UUID `json:"whatsapp_account_id"`
	OnboardingCycle    uint64    `json:"onboarding_cycle"`
	WAMID              string    `json:"wamid"`
	DirectPrimaryBSUID string    `json:"direct_primary_bsuid"`
	ParentBSUID        string    `json:"parent_bsuid"`
	Phone              string    `json:"phone"`
}

func whatsappIdentityReviewBindingDigest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode WhatsApp identity-review binding: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func whatsappIdentityReviewVisibleIntakeTarget(
	routeContactID *uuid.UUID,
	candidates []WhatsAppIdentityReviewCandidate,
) (uuid.UUID, bool) {
	if routeContactID == nil || *routeContactID == uuid.Nil {
		return uuid.Nil, false
	}
	targetMatches := 0
	for _, candidate := range candidates {
		if candidate.ContactID == uuid.Nil || !candidate.SelectorReasons.Valid() {
			return uuid.Nil, false
		}
		if candidate.ContactID == *routeContactID {
			targetMatches++
		}
	}
	if targetMatches != 1 {
		return uuid.Nil, false
	}
	// Storage chooses this route under the same tenant/account/cycle/complete-set
	// fence. It may be the proven direct owner for a phone-only conflict, or a
	// reviewed future-routing target while another independent hold keeps AI
	// blocked. The webhook must not replace that decision with the direct owner.
	return *routeContactID, true
}

// persistAuthenticatedIncomingMessageBeforeAck is the sole regular-message
// admission boundary after Meta's payload-wide signature has been verified. A
// Coexistence delivery chooses Message or the reserved contact-free review inbox
// atomically under the organization policy fence. A WAMID that already has a
// durable owner is an accepted no-op and never revives work or changes routing.
func (a *App) persistAuthenticatedIncomingMessageBeforeAck(
	phoneNumberID string,
	message IncomingTextMessage,
	profileName string,
	webhookBodySHA256 string,
) (work *persistedIncomingMessage, duplicate bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			work = nil
			duplicate = false
			err = fmt.Errorf("panic while admitting authenticated incoming message: %v", recovered)
		}
	}()

	phoneNumberID = strings.TrimSpace(phoneNumberID)
	message.ID = strings.TrimSpace(message.ID)
	webhookBodySHA256 = strings.ToLower(strings.TrimSpace(webhookBodySHA256))
	if a == nil || a.DB == nil || phoneNumberID == "" || message.ID == "" ||
		!isSHA256Hex(webhookBodySHA256) {
		return nil, false, errors.New("authenticated incoming admission is incomplete")
	}
	if strings.EqualFold(strings.TrimSpace(message.Type), "reaction") {
		return nil, false, errors.New("reaction messages use the specialized inbound path")
	}

	organizationID, err := a.resolveWhatsAppOrganization(phoneNumberID)
	if err != nil {
		return nil, false, fmt.Errorf("resolve authenticated incoming tenant: %w", err)
	}
	err = a.WithCommittedTenantApp(organizationID, func(scoped *App) error {
		if lockErr := database.LockWhatsAppWAMIDScopes(scoped.DB, organizationID, message.ID); lockErr != nil {
			return fmt.Errorf("lock authenticated incoming WhatsApp message admission: %w", lockErr)
		}
		var accounts []models.WhatsAppAccount
		if loadErr := scoped.DB.Where(
			"organization_id = ? AND BTRIM(phone_id) = ?",
			organizationID,
			phoneNumberID,
		).Limit(2).Find(&accounts).Error; loadErr != nil {
			return fmt.Errorf("load authenticated incoming account: %w", loadErr)
		}
		if len(accounts) != 1 {
			return errors.New("authenticated incoming account is missing or ambiguous")
		}
		account := &accounts[0]
		if !account.IsSMB {
			var persistErr error
			work, duplicate, persistErr = scoped.persistIncomingMessageForAccountWithAdmission(
				phoneNumberID,
				message,
				profileName,
				account,
				nil,
			)
			return persistErr
		}

		if lockErr := database.LockOrganizationPolicyScope(scoped.DB, organizationID); lockErr != nil {
			return fmt.Errorf("lock authenticated incoming admission: %w", lockErr)
		}
		channelAccount, shadowErr := channelapi.EnsureLegacyMetaWhatsAppAccount(
			scoped.DB,
			channelapi.LegacyMetaAccountRef{
				ID:             account.ID,
				OrganizationID: account.OrganizationID,
				Name:           account.Name,
				Status:         account.Status,
			},
		)
		if shadowErr != nil {
			return fmt.Errorf("ensure authenticated incoming channel account: %w", shadowErr)
		}
		if authorityErr := scoped.prepareWhatsAppMessageAuthority(account); authorityErr != nil {
			return authorityErr
		}
		winner, winnerErr := scoped.lookupWhatsAppAdmissionWinner(organizationID, message.ID)
		if winnerErr != nil {
			return winnerErr
		}
		if winner.Kind != whatsAppAdmissionWinnerNone {
			// First durable admission wins. Do not re-run classification, contact
			// mutation, continuation creation, media jobs, Resume, or sends.
			work = nil
			duplicate = true
			return nil
		}

		var state models.WhatsAppCoexistenceState
		if stateErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"organization_id = ? AND whats_app_account_id = ?",
			organizationID,
			account.ID,
		).First(&state).Error; stateErr != nil {
			return fmt.Errorf("load authenticated incoming onboarding state: %w", stateErr)
		}
		if state.OnboardingCycle == 0 || state.LifecycleStatus != models.CoexistenceLifecycleStatusConnected ||
			state.OnboardingStatus == models.CoexistenceOnboardingStatusOffboarded {
			return ErrWhatsAppIdentityReviewConflict
		}

		directPrimary := strings.TrimSpace(message.FromUserID)
		parent := strings.TrimSpace(message.FromParentUserID)
		phone := normalizeIdentityReviewPhone(message.From)
		verifiedDigest, digestErr := whatsappIdentityReviewBindingDigest(
			whatsAppIdentityReviewVerifiedBinding{
				Protocol:          models.WhatsAppIdentityReviewInboundProtocol,
				OrganizationID:    organizationID,
				WhatsAppAccountID: account.ID,
				WAMID:             message.ID,
				WebhookBodySHA256: webhookBodySHA256,
			},
		)
		if digestErr != nil {
			return digestErr
		}
		selectorDigest, digestErr := whatsappIdentityReviewBindingDigest(
			whatsAppIdentityReviewSelectorBinding{
				Protocol:           models.WhatsAppIdentityReviewInboundProtocol,
				OrganizationID:     organizationID,
				WhatsAppAccountID:  account.ID,
				OnboardingCycle:    state.OnboardingCycle,
				WAMID:              message.ID,
				DirectPrimaryBSUID: directPrimary,
				ParentBSUID:        parent,
				Phone:              phone,
			},
		)
		if digestErr != nil {
			return digestErr
		}
		claim := WhatsAppIdentityReviewClaim{
			OrganizationID:          organizationID,
			WhatsAppAccountID:       account.ID,
			OnboardingCycle:         state.OnboardingCycle,
			DirectPrimaryBSUID:      directPrimary,
			ParentBSUID:             parent,
			Phone:                   phone,
			VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
			VerifiedEventDigest:     verifiedDigest,
			SelectorBodyDigest:      selectorDigest,
		}
		admission, admissionErr := scoped.EvaluateWhatsAppIdentityReviewAdmission(scoped.DB, &claim)
		if admissionErr != nil {
			return admissionErr
		}
		if admission.Blocked != admission.NeedsReview {
			return errors.New("WhatsApp identity-review admission state is inconsistent")
		}
		if !admission.Blocked {
			if admission.RouteContactID == nil || *admission.RouteContactID == uuid.Nil {
				return errors.New("WhatsApp identity-review route is missing")
			}
			policy := &incomingMessageAdmissionPolicy{CanonicalContactID: *admission.RouteContactID}
			if admission.Reason == "reviewed_future_route" {
				if admission.LatestHoldID == nil || *admission.LatestHoldID == uuid.Nil {
					return errors.New("reviewed WhatsApp identity route lacks its durable hold")
				}
				policy.IdentityReviewHoldID = *admission.LatestHoldID
				policy.IdentityReviewReason = admission.Reason
				policy.IdentityReviewRouteMode = incomingIdentityReviewRouteReviewedFuture
				policy.IdentityReviewSelectorHash = selectorDigest
			}
			var persistErr error
			work, duplicate, persistErr = scoped.persistIncomingMessageForAccountWithAdmission(
				phoneNumberID,
				message,
				profileName,
				account,
				policy,
			)
			return persistErr
		}

		hold, _, holdErr := scoped.CreateOrReuseWhatsAppIdentityReviewHold(scoped.DB, &claim)
		if holdErr != nil {
			return holdErr
		}
		if hold == nil || hold.HoldID == uuid.Nil {
			return errors.New("WhatsApp identity-review hold is missing")
		}
		if visibleTarget, visible := whatsappIdentityReviewVisibleIntakeTarget(
			admission.RouteContactID,
			hold.Candidates,
		); visible {
			routeMode := incomingIdentityReviewRouteHeldDirect
			if hold.StoredHold.Disposition == models.WhatsAppIdentityReviewDispositionFutureRouting &&
				hold.StoredHold.DecisionTargetContactID != nil &&
				*hold.StoredHold.DecisionTargetContactID == visibleTarget {
				routeMode = incomingIdentityReviewRouteReviewedFuture
			}
			var persistErr error
			work, duplicate, persistErr = scoped.persistIncomingMessageForAccountWithAdmission(
				phoneNumberID,
				message,
				profileName,
				account,
				&incomingMessageAdmissionPolicy{
					CanonicalContactID:         visibleTarget,
					SuppressAutomaticAI:        true,
					SuppressionReason:          "whatsapp_identity_review:" + strings.TrimSpace(admission.Reason),
					IdentityReviewHoldID:       hold.HoldID,
					IdentityReviewReason:       strings.TrimSpace(admission.Reason),
					IdentityReviewRouteMode:    routeMode,
					IdentityReviewSelectorHash: selectorDigest,
				},
			)
			return persistErr
		}
		_, _, stageErr := scoped.persistWhatsAppIdentityReviewEvent(
			account,
			channelAccount,
			hold.HoldID,
			message,
		)
		if stageErr != nil {
			return stageErr
		}
		work = nil
		duplicate = false
		return nil
	})
	if err == nil && work != nil {
		var refreshed models.Message
		reloadErr := a.WithCommittedTenantApp(organizationID, func(scoped *App) error {
			return scoped.DB.Where(
				"id = ? AND organization_id = ?",
				work.Persisted.ID,
				organizationID,
			).First(&refreshed).Error
		})
		if reloadErr != nil {
			return nil, false, fmt.Errorf("reload admitted incoming message projection: %w", reloadErr)
		}
		work.Persisted = refreshed
	}
	return work, duplicate, err
}

func (a *App) persistMessageEchoesBeforeAck(
	phoneNumberID string,
	messages []CoexistenceMessage,
	contacts []CoexistenceWebhookContact,
) error {
	wamids, err := coexistenceOwnerWAMIDs(messages)
	if err != nil {
		return err
	}
	return a.withCoexistencePhoneAccount(phoneNumberID, wamids, func(scoped *App, account *models.WhatsAppAccount) error {
		for _, message := range messages {
			if err := scoped.persistOneMessageEcho(account, message, contacts); err != nil {
				return err
			}
		}
		return nil
	})
}

func (a *App) persistOneMessageEcho(
	account *models.WhatsAppAccount,
	message CoexistenceMessage,
	contacts []CoexistenceWebhookContact,
) error {
	switch strings.ToLower(strings.TrimSpace(message.Type)) {
	case "edit":
		return a.persistCoexistenceEdit(
			account,
			message,
			contacts,
			models.DirectionOutgoing,
			models.MessageStatusSent,
			"smb_message_echoes",
			true,
		)
	case "revoke":
		return a.persistCoexistenceRevoke(
			account,
			message,
			contacts,
			models.DirectionOutgoing,
			models.MessageStatusSent,
			"smb_message_echoes",
			true,
		)
	default:
		direction := coexistenceEchoDirection(message)
		_, _, err := a.persistCoexistenceMessage(
			account,
			coexistenceMessageContactIdentity(message, direction, coexistenceContactIdentity{}, contacts),
			message,
			direction,
			models.MessageStatusSent,
			models.JSONB{"coexistence_source": "smb_message_echoes"},
			true,
		)
		return err
	}
}

func (a *App) persistCoexistenceInboundMutationBeforeAck(
	phoneNumberID string,
	message CoexistenceMessage,
	contacts []CoexistenceWebhookContact,
) error {
	wamid, err := coexistenceOwnerWAMID(message)
	if err != nil {
		return err
	}
	return a.withCoexistencePhoneAccount(phoneNumberID, []string{wamid}, func(scoped *App, account *models.WhatsAppAccount) error {
		switch strings.ToLower(strings.TrimSpace(message.Type)) {
		case "edit":
			return scoped.persistCoexistenceEdit(
				account,
				message,
				contacts,
				models.DirectionIncoming,
				models.MessageStatusReceived,
				"messages",
				false,
			)
		case "revoke":
			return scoped.persistCoexistenceRevoke(
				account,
				message,
				contacts,
				models.DirectionIncoming,
				models.MessageStatusReceived,
				"messages",
				false,
			)
		default:
			return fmt.Errorf("unsupported coexistence message mutation %q", message.Type)
		}
	})
}

func (a *App) persistCoexistenceEdit(
	account *models.WhatsAppAccount,
	event CoexistenceMessage,
	contacts []CoexistenceWebhookContact,
	direction models.Direction,
	status models.MessageStatus,
	source string,
	emit bool,
) error {
	if event.Edit == nil || strings.TrimSpace(event.Edit.OriginalMessageID) == "" {
		return errors.New("coexistence edit is missing original_message_id")
	}
	originalID := strings.TrimSpace(event.Edit.OriginalMessageID)
	if a == nil || a.DB == nil || account == nil || account.OrganizationID == uuid.Nil {
		return errors.New("coexistence edit account is incomplete")
	}
	if err := database.LockWhatsAppWAMIDScopes(a.DB, account.OrganizationID, originalID); err != nil {
		return fmt.Errorf("lock coexistence edit admission: %w", err)
	}
	if err := a.prepareWhatsAppMessageAuthority(account); err != nil {
		return err
	}
	targetEvent := event
	targetEvent.ID = originalID
	identity := coexistenceMessageContactIdentity(targetEvent, direction, coexistenceContactIdentity{}, contacts)
	resolved, err := a.resolveWhatsAppMessage(account, whatsAppMessageLookup{
		WAMID: originalID, Identity: identity, Direction: direction, Lock: true, RepairProjection: true,
	})
	if errors.Is(err, errWhatsAppMessageOwnerDeleted) {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		replacement := CoexistenceMessage{IncomingTextMessage: event.Edit.Message}
		// Key an out-of-order edit by the original WAMID. The edit event ID is
		// correlation metadata only; using it as the row key would let the later
		// original delivery create a second, unreconciled message.
		replacement.ID = originalID
		replacement.To = event.To
		replacement.ToUserID = event.ToUserID
		replacement.ToParentUserID = event.ToParentUserID
		replacement.From = event.From
		replacement.FromUserID = event.FromUserID
		replacement.FromParentUserID = event.FromParentUserID
		if strings.TrimSpace(replacement.Timestamp) == "" {
			replacement.Timestamp = event.Timestamp
		}
		persisted, created, persistErr := a.persistCoexistenceMessage(
			account,
			coexistenceMessageContactIdentity(
				replacement,
				direction,
				coexistenceContactIdentity{},
				contacts,
			),
			replacement,
			direction,
			status,
			models.JSONB{
				"coexistence_source":         source,
				"coexistence_edit_event_id":  strings.TrimSpace(event.ID),
				"coexistence_original_wamid": originalID,
			},
			emit,
		)
		if persistErr != nil || created {
			return persistErr
		}
		if persisted == nil {
			// A contact-free staged winner remains staged. An edit cannot create a
			// Message or Contact behind the review boundary.
			return nil
		}
		// A concurrent original may win the insert. Revalidate that exact winner
		// before applying the edit, rather than treating detail merge as an edit.
		resolved, err = a.resolveWhatsAppMessage(account, whatsAppMessageLookup{
			WAMID: originalID, MessageID: persisted.ID, ContactID: persisted.ContactID,
			Identity: identity, Direction: direction, Lock: false,
		})
	}
	if err != nil {
		return fmt.Errorf("load coexistence edit target: %w", err)
	}
	original := resolved.Message
	if revoked, _ := original.Metadata[coexistenceMediaRevokedMetadataKey].(bool); revoked {
		return nil
	}

	extracted := a.extractMessageContentForPersistence(event.Edit.Message)
	metadata := cloneMessageMetadata(original.Metadata)
	if strings.TrimSpace(event.ID) != "" && metadata["coexistence_edit_event_id"] == strings.TrimSpace(event.ID) {
		return a.ensureProvenCoexistenceMediaJob(account, &original)
	}
	metadata["coexistence_source"] = source
	metadata["coexistence_edit_event_id"] = strings.TrimSpace(event.ID)
	updates := coexistenceMediaReplacementUpdates(&original, event.Edit.Message, extracted, metadata, true)
	updates["message_type"] = models.MessageType(extracted.Type)
	updates["content"] = extracted.Text
	updates["metadata"] = metadata
	if err := a.DB.Model(&models.Message{}).Where(
		"organization_id = ? AND id = ?",
		account.OrganizationID,
		original.ID,
	).Updates(updates).Error; err != nil {
		return err
	}
	if err := a.ensureProvenCoexistenceMediaJob(account, &original); err != nil {
		return err
	}
	messageID := original.ID
	contactID := original.ContactID
	a.publishRealtimeEvent(queue.RealtimeEvent{
		OrganizationID: account.OrganizationID,
		Kind:           queue.RealtimeEventConversationChanged,
		ContactID:      &contactID,
		MessageID:      &messageID,
		Status:         "edited",
		EventCount:     1,
		OccurredAt:     time.Now().UTC(),
	}, nil)
	return nil
}

func (a *App) persistCoexistenceRevoke(
	account *models.WhatsAppAccount,
	event CoexistenceMessage,
	contacts []CoexistenceWebhookContact,
	direction models.Direction,
	status models.MessageStatus,
	source string,
	emit bool,
) error {
	if event.Revoke == nil || strings.TrimSpace(event.Revoke.OriginalMessageID) == "" {
		return errors.New("coexistence revoke is missing original_message_id")
	}
	originalID := strings.TrimSpace(event.Revoke.OriginalMessageID)
	if a == nil || a.DB == nil || account == nil || account.OrganizationID == uuid.Nil {
		return errors.New("coexistence revoke account is incomplete")
	}
	if err := database.LockWhatsAppWAMIDScopes(a.DB, account.OrganizationID, originalID); err != nil {
		return fmt.Errorf("lock coexistence revoke admission: %w", err)
	}
	if err := a.prepareWhatsAppMessageAuthority(account); err != nil {
		return err
	}
	targetEvent := event
	targetEvent.ID = originalID
	identity := coexistenceMessageContactIdentity(targetEvent, direction, coexistenceContactIdentity{}, contacts)
	resolved, err := a.resolveWhatsAppMessage(account, whatsAppMessageLookup{
		WAMID: originalID, Identity: identity, Direction: direction, Lock: true, RepairProjection: true,
	})
	if errors.Is(err, errWhatsAppMessageOwnerDeleted) {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		tombstone := event
		// As with edits, reserve the original WAMID so a later delivery is a
		// replay of this durable tombstone rather than a second message.
		tombstone.ID = originalID
		tombstone.Type = "text"
		tombstone.Text = &struct {
			Body string `json:"body"`
		}{Body: "[Message deleted from WhatsApp Business App]"}
		persisted, created, persistErr := a.persistCoexistenceMessage(
			account,
			coexistenceMessageContactIdentity(
				tombstone,
				direction,
				coexistenceContactIdentity{},
				contacts,
			),
			tombstone,
			direction,
			status,
			models.JSONB{
				"coexistence_source":          source,
				"coexistence_revoked":         true,
				"coexistence_revoke_event_id": strings.TrimSpace(event.ID),
				"coexistence_original_wamid":  originalID,
			},
			emit,
		)
		if persistErr != nil || created {
			return persistErr
		}
		if persisted == nil {
			return nil
		}
		// A delivery can insert the original after our first lookup. The replay
		// path returns its locked row; it still needs full tombstone sanitization.
		resolved, err = a.resolveWhatsAppMessage(account, whatsAppMessageLookup{
			WAMID: originalID, MessageID: persisted.ID, ContactID: persisted.ContactID,
			Identity: identity, Direction: direction, Lock: false,
		})
	}
	if err != nil {
		return fmt.Errorf("load coexistence revoke target: %w", err)
	}
	original := resolved.Message
	metadata := cloneMessageMetadata(original.Metadata)
	eventID := strings.TrimSpace(event.ID)
	alreadySanitized := metadata[coexistenceMediaRevokedMetadataKey] == true &&
		eventID != "" && metadata["coexistence_revoke_event_id"] == eventID &&
		original.Content == "[Message deleted from WhatsApp Business App]" && original.MessageType == models.MessageTypeText &&
		original.MediaURL == "" && original.MediaMimeType == "" && original.MediaFilename == "" &&
		coexistenceMediaMetadataString(metadata, coexistenceMediaProviderIDMetadataKey) == "" &&
		coexistenceMediaMetadataString(metadata, coexistenceMediaHydratedIDMetadataKey) == ""
	if alreadySanitized {
		return nil
	}
	metadata["coexistence_source"] = source
	metadata["coexistence_revoked"] = true
	metadata["coexistence_revoke_event_id"] = eventID
	delete(metadata, coexistenceMediaProviderIDMetadataKey)
	delete(metadata, coexistenceMediaHydratedIDMetadataKey)
	delete(metadata, "coexistence_media_sha256")
	delete(metadata, "history_media_placeholder")
	if err := a.DB.Model(&models.Message{}).Where(
		"organization_id = ? AND id = ?",
		account.OrganizationID,
		original.ID,
	).Updates(map[string]any{
		"content":         "[Message deleted from WhatsApp Business App]",
		"message_type":    models.MessageTypeText,
		"media_url":       "",
		"media_mime_type": "",
		"media_filename":  "",
		"metadata":        metadata,
	}).Error; err != nil {
		return err
	}

	messageID := original.ID
	contactID := original.ContactID
	a.publishRealtimeEvent(queue.RealtimeEvent{
		OrganizationID: account.OrganizationID,
		Kind:           queue.RealtimeEventConversationChanged,
		ContactID:      &contactID,
		MessageID:      &messageID,
		Status:         "revoked",
		EventCount:     1,
		OccurredAt:     time.Now().UTC(),
	}, nil)
	return nil
}

func cloneMessageMetadata(source models.JSONB) models.JSONB {
	result := make(models.JSONB, len(source)+4)
	for key, value := range source {
		result[key] = value
	}
	return result
}

// Only the locked row's stored edit marker establishes precedence over an
// unversioned original/history payload. An empty event ID still represents a
// stored edit, but does not impose an order on later distinct edit events.
func coexistenceMessageHasStoredEdit(message *models.Message) bool {
	if message == nil {
		return false
	}
	_, edited := message.Metadata["coexistence_edit_event_id"].(string)
	return edited
}

// Apply replacement media metadata and columns in the same UPDATE as the new
// content/type. In particular, text cannot retain a prior provider identifier
// for a durable worker to hydrate, and SHA-less replacement B cannot inherit A's
// hash or hydrated marker. This does not delete an already-stored object.
func coexistenceMediaReplacementUpdates(
	existing *models.Message,
	message IncomingTextMessage,
	extracted ExtractedMessage,
	metadata models.JSONB,
	replaceContent bool,
) map[string]any {
	updates := map[string]any{
		"media_mime_type": "",
		"media_filename":  "",
	}
	mediaID, mediaSHA := coexistenceMediaIdentity(message)
	delete(metadata, "coexistence_media_sha256")
	if mediaID == "" {
		delete(metadata, coexistenceMediaProviderIDMetadataKey)
		delete(metadata, coexistenceMediaHydratedIDMetadataKey)
		if _, placeholder := metadata["history_media_placeholder"]; placeholder {
			metadata["history_media_placeholder"] = false
		}
		updates["media_url"] = ""
		return updates
	}
	metadata[coexistenceMediaProviderIDMetadataKey] = mediaID
	if mediaSHA != "" {
		metadata["coexistence_media_sha256"] = mediaSHA
	}
	if extracted.Media != nil {
		updates["media_mime_type"] = extracted.Media.MediaMimeType
		updates["media_filename"] = extracted.Media.MediaFilename
	}
	// Match the exact columns the caller will commit, not merely the provider
	// ID. A distinct stored edit/caption/MIME/file revision needs its own
	// hydration even when Meta reuses the same media ID and SHA. Normal replay
	// does not replace existing content unless it fills a history placeholder.
	projected := *existing
	projected.MessageType = models.MessageType(extracted.Type)
	projected.Metadata = metadata
	projected.MediaMimeType = updates["media_mime_type"].(string)
	projected.MediaFilename = updates["media_filename"].(string)
	if replaceContent {
		projected.Content = extracted.Text
	}
	_, previousRevision, _ := coexistenceMediaRevision(existing)
	_, nextRevision, supported := coexistenceMediaRevision(&projected)
	if !supported || previousRevision != nextRevision {
		delete(metadata, coexistenceMediaHydratedIDMetadataKey)
		updates["media_url"] = ""
	}
	return updates
}

func (a *App) persistCoexistenceHistoryBeforeAck(
	phoneNumberID string,
	businessDisplayPhone string,
	entryTimestamp int64,
	batches []CoexistenceHistoryBatch,
	mediaDetails []CoexistenceMessage,
	contacts []CoexistenceWebhookContact,
) error {
	wamids, err := coexistenceHistoryWAMIDs(batches, mediaDetails)
	if err != nil {
		return err
	}
	return a.withCoexistencePhoneAccount(phoneNumberID, wamids, func(scoped *App, account *models.WhatsAppAccount) error {
		state, err := ensureCoexistenceState(scoped.DB, account)
		if err != nil {
			return fmt.Errorf("load coexistence state: %w", err)
		}

		latestPhase := state.HistoryLastPhase
		latestChunk := state.HistoryLastChunkOrder
		progress := state.HistoryProgressPercent
		historyStatus := state.HistorySyncStatus
		consent := state.HistoryConsent
		wasCompleted := historyStatus == models.CoexistenceSyncStatusCompleted
		wasDeclined := historyStatus == models.CoexistenceSyncStatusDeclined ||
			consent == models.CoexistenceHistoryConsentDeclined
		stageCanAdvance := historyStatus == models.CoexistenceSyncStatusRequesting ||
			historyStatus == models.CoexistenceSyncStatusRequested ||
			historyStatus == models.CoexistenceSyncStatusInProgress
		var webhookErrors []WebhookStatusError
		hasHistoryData := len(mediaDetails) > 0

		for _, batch := range batches {
			if len(batch.Errors) > 0 {
				webhookErrors = append(webhookErrors, batch.Errors...)
				continue
			}
			hasHistoryData = true
			for _, thread := range batch.Threads {
				threadIdentity := coexistenceHistoryThreadIdentity(thread)
				for _, message := range thread.Messages {
					status := coexistenceHistoryStatus(message.HistoryContext)
					direction := coexistenceHistoryDirection(
						businessDisplayPhone,
						thread,
						message,
					)
					_, _, err := scoped.persistCoexistenceMessage(
						account,
						coexistenceMessageContactIdentity(message, direction, threadIdentity, contacts),
						message,
						direction,
						status,
						models.JSONB{
							"coexistence_source":       "history",
							"history_phase":            batch.Metadata.Phase,
							"history_chunk_order":      batch.Metadata.ChunkOrder,
							"history_progress_percent": batch.Metadata.Progress,
							"history_provider_status":  coexistenceHistoryStatusText(message.HistoryContext),
						},
						false,
					)
					if err != nil {
						return fmt.Errorf("persist history message: %w", err)
					}
				}
			}

			phase := batch.Metadata.Phase
			chunk := batch.Metadata.ChunkOrder
			if latestPhase == nil || phase > *latestPhase ||
				(phase == *latestPhase && (latestChunk == nil || chunk > *latestChunk)) {
				latestPhase = intPointer(phase)
				latestChunk = intPointer(chunk)
			}
			if batch.Metadata.Progress > progress {
				progress = batch.Metadata.Progress
			}
		}

		for _, detail := range mediaDetails {
			if err := scoped.persistHistoryMediaDetail(account, businessDisplayPhone, detail, contacts); err != nil {
				return err
			}
		}
		// History payloads do not carry the POST request_id. Always retain their
		// messages/media, but never let an unsolicited or delayed callback move a
		// newly reset stage that has not actually issued its one-time request.
		// From cycle two onward, an entry timestamp at or after OnboardedAt is also
		// required to fence callbacks still in flight from the previous cycle.
		currentCycleEvidence := state.OnboardingCycle <= 1
		if entryTimestamp > 0 && state.OnboardedAt != nil {
			currentCycleEvidence = !time.Unix(entryTimestamp, 0).UTC().Before(
				state.OnboardedAt.UTC().Truncate(time.Second),
			)
		} else if state.OnboardingCycle > 1 {
			currentCycleEvidence = false
		}
		if !stageCanAdvance || !currentCycleEvidence {
			return nil
		}

		now := time.Now().UTC()
		updates := map[string]any{
			"version": gorm.Expr("version + 1"),
		}
		if len(webhookErrors) > 0 && !wasCompleted {
			first := webhookErrors[0]
			if first.Code == 2593109 || wasDeclined {
				historyStatus = models.CoexistenceSyncStatusDeclined
				consent = models.CoexistenceHistoryConsentDeclined
				if state.HistoryConsentAt == nil {
					updates["history_consent_at"] = now
				}
			} else {
				historyStatus = models.CoexistenceSyncStatusFailed
			}
			updates["history_sync_error_code"] = strconv.Itoa(first.Code)
			updates["history_sync_error_message"] = coexistenceWebhookErrorText(first)
		} else if hasHistoryData && !wasCompleted && !wasDeclined {
			consent = models.CoexistenceHistoryConsentGranted
			if state.HistoryConsentAt == nil {
				updates["history_consent_at"] = now
			}
			updates["history_sync_error_code"] = ""
			updates["history_sync_error_message"] = ""
			if progress >= 100 || historyStatus == models.CoexistenceSyncStatusCompleted {
				historyStatus = models.CoexistenceSyncStatusCompleted
				if state.HistoryCompletedAt == nil {
					updates["history_completed_at"] = now
				}
			} else {
				historyStatus = models.CoexistenceSyncStatusInProgress
			}
		}

		updates["history_consent"] = consent
		updates["history_sync_status"] = historyStatus
		updates["history_last_phase"] = latestPhase
		updates["history_last_chunk_order"] = latestChunk
		updates["history_progress_percent"] = progress
		updates["history_progress_at"] = now

		syncStatus, onboardingStatus, complete := coexistenceSyncRollup(
			state.ContactSyncStatus,
			historyStatus,
		)
		updates["sync_status"] = syncStatus
		if state.OnboardingStatus != models.CoexistenceOnboardingStatusOffboarded {
			updates["onboarding_status"] = onboardingStatus
		}
		if complete && state.SyncCompletedAt == nil {
			updates["sync_completed_at"] = now
		}
		return scoped.DB.Model(&models.WhatsAppCoexistenceState{}).Where(
			"organization_id = ? AND whats_app_account_id = ?",
			account.OrganizationID,
			account.ID,
		).Updates(updates).Error
	})
}

func (a *App) persistHistoryMediaDetail(
	account *models.WhatsAppAccount,
	businessDisplayPhone string,
	detail CoexistenceMessage,
	contacts []CoexistenceWebhookContact,
) error {
	id := strings.TrimSpace(detail.ID)
	if id == "" {
		return errors.New("history media detail has no WhatsApp message ID")
	}
	if a == nil || a.DB == nil || account == nil || account.OrganizationID == uuid.Nil {
		return errors.New("history media detail account is incomplete")
	}
	if err := database.LockWhatsAppWAMIDScopes(a.DB, account.OrganizationID, id); err != nil {
		return fmt.Errorf("lock history media detail admission: %w", err)
	}
	if err := a.prepareWhatsAppMessageAuthority(account); err != nil {
		return err
	}
	direction := coexistenceStandaloneHistoryDirection(businessDisplayPhone, detail)
	identity := coexistenceMessageContactIdentity(detail, direction, coexistenceContactIdentity{}, contacts)
	resolved, err := a.resolveWhatsAppMessage(account, whatsAppMessageLookup{
		WAMID: id, Identity: identity, Direction: direction, Lock: true, RepairProjection: true,
	})
	if errors.Is(err, errWhatsAppMessageOwnerDeleted) {
		return nil
	}
	if err == nil {
		existing := resolved.Message
		if revoked, _ := existing.Metadata[coexistenceMediaRevokedMetadataKey].(bool); revoked {
			return nil
		}
		if coexistenceMessageHasStoredEdit(&existing) {
			// History has no edit revision with which to supersede this stored
			// edit. Keep B intact and resume only B's media hydration if needed.
			return a.ensureProvenCoexistenceMediaJob(account, &existing)
		}
		extracted := a.extractMessageContentForPersistence(detail.IncomingTextMessage)
		metadata := cloneMessageMetadata(existing.Metadata)
		metadata["coexistence_source"] = "history"
		metadata["history_media_detail_received"] = true
		metadata["history_media_placeholder"] = false
		replaceContent := extracted.Text != "" || existing.Content == ""
		updates := coexistenceMediaReplacementUpdates(&existing, detail.IncomingTextMessage, extracted, metadata, replaceContent)
		updates["message_type"] = models.MessageType(extracted.Type)
		updates["metadata"] = metadata
		if replaceContent {
			updates["content"] = extracted.Text
		}
		if err := a.DB.Model(&models.Message{}).Where(
			"organization_id = ? AND id = ?",
			account.OrganizationID,
			existing.ID,
		).Updates(updates).Error; err != nil {
			return err
		}
		return a.ensureProvenCoexistenceMediaJob(account, &existing)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("load history media placeholder: %w", err)
	}

	status := models.MessageStatusSent
	if direction == models.DirectionIncoming {
		status = models.MessageStatusReceived
	}
	_, _, err = a.persistCoexistenceMessage(
		account,
		identity,
		detail,
		direction,
		status,
		models.JSONB{
			"coexistence_source":            "history",
			"history_media_detail_received": true,
		},
		false,
	)
	return err
}

// Every media enqueue revalidates the same winner after its mutation. The
// surrounding resolver already holds canonical-contact/message locks; this
// read must not acquire account/channel locks behind them.
func (a *App) ensureProvenCoexistenceMediaJob(account *models.WhatsAppAccount, message *models.Message) error {
	resolved, err := a.resolveWhatsAppMessage(account, whatsAppMessageLookup{
		WAMID: message.WhatsAppMessageID, MessageID: message.ID, ContactID: message.ContactID,
		Direction: message.Direction,
	})
	if err != nil {
		return fmt.Errorf("revalidate coexistence media message: %w", err)
	}
	if _, _, supported := coexistenceMediaRevision(&resolved.Message); !supported {
		return nil
	}
	return a.ensureCoexistenceMediaJob(account, resolved.Message.ID)
}

func (a *App) persistCoexistenceMessage(
	account *models.WhatsAppAccount,
	identity coexistenceContactIdentity,
	message CoexistenceMessage,
	direction models.Direction,
	status models.MessageStatus,
	metadata models.JSONB,
	emit bool,
) (*models.Message, bool, error) {
	if account == nil {
		return nil, false, errors.New("coexistence message account is required")
	}
	wamid := strings.TrimSpace(message.ID)
	if wamid == "" {
		return nil, false, errors.New("coexistence message has no WhatsApp message ID")
	}
	if a == nil || a.DB == nil || account.OrganizationID == uuid.Nil {
		return nil, false, errors.New("coexistence message account is incomplete")
	}
	if err := database.LockWhatsAppWAMIDScopes(a.DB, account.OrganizationID, wamid); err != nil {
		return nil, false, fmt.Errorf("lock coexistence message admission: %w", err)
	}
	if err := a.prepareWhatsAppMessageAuthority(account); err != nil {
		return nil, false, err
	}
	identity = coexistenceMessageContactIdentity(message, direction, identity, nil)
	winner, err := a.lookupWhatsAppAdmissionWinner(account.OrganizationID, wamid)
	if err != nil {
		return nil, false, err
	}
	if winner.Kind == whatsAppAdmissionWinnerReview {
		// The first durable destination is immutable. History/echo replay may not
		// promote staged content into a Message or manufacture a Contact.
		return nil, false, nil
	}
	if winner.Kind == whatsAppAdmissionWinnerMessage && winner.Message != nil && winner.Message.DeletedAt.Valid {
		// A soft-deleted first owner remains a successful immutable replay. Do
		// not resolve, revive, enrich, mirror, or merge any details into it.
		return nil, false, nil
	}

	resolved, err := a.resolveWhatsAppMessage(account, whatsAppMessageLookup{
		WAMID: wamid, Identity: identity, Direction: direction,
	})
	if err == nil {
		existing := resolved.Message
		if existing.InboxConversationID == nil && identity.Phone != "" && identity.primaryUserID() != "" &&
			isCoexistencePlaceholderPhone(resolved.Contact.PhoneNumber) && resolved.Contact.BSUID == identity.primaryUserID() {
			// Reveal a phone only on the already-proven canonical placeholder.
			// The general contact upsert may transfer a BSUID to a separate phone
			// owner; that is not proof that this existing message belongs there.
			contact, contactErr := contactutil.ResolveCanonicalContactForUpdate(a.DB, account.OrganizationID, resolved.Contact.ID)
			if contactErr != nil || contact.ID != resolved.Contact.ID {
				return nil, false, contactutil.ErrCanonicalContactChanged
			}
			proof, proofErr := a.resolveWhatsAppMessage(account, whatsAppMessageLookup{
				WAMID: wamid, MessageID: existing.ID, ContactID: contact.ID, Identity: identity, Direction: direction,
			})
			if proofErr != nil {
				return nil, false, proofErr
			}
			if proof.Message.InboxConversationID != nil {
				return nil, false, contactutil.ErrCanonicalContactChanged
			}
			if isCoexistencePlaceholderPhone(contact.PhoneNumber) && contact.BSUID == identity.primaryUserID() {
				phone := normalizeCoexistencePhone(identity.Phone)
				// A concurrent phone owner must fail closed. The savepoint keeps a
				// uniqueness error from poisoning the enclosing tenant transaction.
				if err := a.DB.Transaction(func(tx *gorm.DB) error {
					return tx.Model(&models.Contact{}).Where("organization_id = ? AND id = ?", account.OrganizationID, contact.ID).
						Update("phone_number", phone).Error
				}); err != nil {
					return nil, false, fmt.Errorf("reveal coexistence replay contact phone: %w", err)
				}
				contact.PhoneNumber = phone
				if err := a.updateCoexistenceContactIdentity(account, contact, identity, false); err != nil {
					return nil, false, err
				}
			}
		}
		locked, lockErr := a.resolveWhatsAppMessage(account, whatsAppMessageLookup{
			WAMID: wamid, MessageID: existing.ID, ContactID: resolved.Contact.ID,
			Identity: identity, Direction: direction, Lock: true, RepairProjection: true,
		})
		if lockErr != nil {
			return nil, false, lockErr
		}
		existing = locked.Message
		if mergeErr := a.mergeCoexistenceMessageDetails(account, &existing, message, status, metadata, identity); mergeErr != nil {
			return nil, false, mergeErr
		}
		if emit {
			a.mirrorLegacyWhatsAppMessageAfterCommit(account, existing.ID)
		}
		return &existing, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, fmt.Errorf("check coexistence message replay: %w", err)
	}

	contact, _, err := a.getOrCreateCoexistenceContact(account, identity)
	if err != nil {
		return nil, false, fmt.Errorf("get or create coexistence message contact: %w", err)
	}

	extracted := a.extractMessageContentForPersistence(message.IncomingTextMessage)
	if strings.TrimSpace(extracted.Type) == "" {
		return nil, false, errors.New("coexistence message has no type")
	}
	metadata = cloneMessageMetadata(metadata)
	if identity.UserID != "" {
		metadata["coexistence_user_id"] = identity.UserID
	}
	if identity.ParentUserID != "" {
		metadata["coexistence_parent_user_id"] = identity.ParentUserID
	}
	if identity.Username != "" {
		metadata["coexistence_username"] = identity.Username
	}
	mediaID, mediaSHA := coexistenceMediaIdentity(message.IncomingTextMessage)
	if mediaID != "" {
		metadata["coexistence_media_id"] = mediaID
	}
	if mediaSHA != "" {
		metadata["coexistence_media_sha256"] = mediaSHA
	}
	if message.Type == "media_placeholder" {
		metadata["history_media_placeholder"] = true
	}

	now := time.Now().UTC()
	occurredAt := coexistenceMessageTime(message.Timestamp, now)
	messageRow := models.Message{
		BaseModel: models.BaseModel{
			ID:        uuid.NewSHA1(account.ID, []byte("coexistence-message:"+wamid)),
			CreatedAt: occurredAt,
			UpdatedAt: occurredAt,
		},
		OrganizationID:    account.OrganizationID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: wamid,
		Direction:         direction,
		MessageType:       models.MessageType(extracted.Type),
		Content:           extracted.Text,
		Status:            status,
		IngestedAt:        &now,
		Metadata:          metadata,
	}
	if extracted.Media != nil {
		messageRow.MediaMimeType = extracted.Media.MediaMimeType
		messageRow.MediaFilename = extracted.Media.MediaFilename
	}
	if message.Context != nil && strings.TrimSpace(message.Context.ID) != "" {
		// A reply may reference either side of this conversation. Prove the
		// target's own direction rather than imposing the envelope's direction.
		reply, err := a.resolveWhatsAppMessage(account, whatsAppMessageLookup{
			WAMID: strings.TrimSpace(message.Context.ID), ContactID: contact.ID, Identity: identity,
		})
		if err == nil {
			messageRow.IsReply = true
			messageRow.ReplyToMessageID = &reply.Message.ID
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, fmt.Errorf("load coexistence reply context: %w", err)
		}
	}

	var inserted bool
	createErr := a.DB.Transaction(func(tx *gorm.DB) error {
		create := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&messageRow)
		inserted = create.RowsAffected == 1
		return create.Error
	})
	if createErr != nil && !isUniqueViolation(createErr) {
		return nil, false, fmt.Errorf("create coexistence message: %w", createErr)
	}
	if createErr != nil || !inserted {
		// Nested Transaction has rolled any 23505 back to its savepoint. Only
		// now may the usable outer transaction reconcile a proven winner.
		winner, err := a.resolveWhatsAppMessage(account, whatsAppMessageLookup{
			WAMID: wamid, ContactID: contact.ID, Identity: identity, Direction: direction,
			Lock: true, RepairProjection: true,
		})
		if err != nil {
			return nil, false, fmt.Errorf("load concurrent coexistence replay: %w", err)
		}
		existing := winner.Message
		if err := a.mergeCoexistenceMessageDetails(account, &existing, message, status, metadata, identity); err != nil {
			return nil, false, err
		}
		if emit {
			a.mirrorLegacyWhatsAppMessageAfterCommit(account, existing.ID)
		}
		return &existing, false, nil
	}
	if winner, err := a.lookupWhatsAppAdmissionWinner(account.OrganizationID, wamid); err != nil {
		return nil, false, err
	} else if winner.Kind != whatsAppAdmissionWinnerMessage || winner.Message == nil || winner.Message.ID != messageRow.ID {
		return nil, false, errors.New("coexistence Message lost cross-store admission")
	}
	if err := a.ensureProvenCoexistenceMediaJob(account, &messageRow); err != nil {
		return nil, false, err
	}

	preview := coexistenceMessagePreview(extracted.Type, extracted.Text)
	contactUpdates := map[string]any{
		"last_message_at":      occurredAt,
		"last_message_preview": preview,
		"is_read":              true,
		"whats_app_account":    account.Name,
	}
	if err := a.DB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ? AND (last_message_at IS NULL OR last_message_at <= ?)",
		account.OrganizationID,
		contact.ID,
		occurredAt,
	).Updates(contactUpdates).Error; err != nil {
		return nil, false, fmt.Errorf("update coexistence contact summary: %w", err)
	}
	if direction == models.DirectionIncoming {
		if err := a.DB.Model(&models.Contact{}).Where(
			"organization_id = ? AND id = ? AND (last_inbound_at IS NULL OR last_inbound_at <= ?)",
			account.OrganizationID,
			contact.ID,
			occurredAt,
		).Update("last_inbound_at", occurredAt).Error; err != nil {
			return nil, false, fmt.Errorf("update coexistence last inbound time: %w", err)
		}
	}

	if emit {
		a.mirrorLegacyWhatsAppMessageAfterCommit(account, messageRow.ID)
		accountCopy := *account
		messageCopy := messageRow
		contactCopy := *contact
		a.afterTenantCommit(func() {
			root := a.rootApp()
			root.broadcastNewMessage(accountCopy.OrganizationID, &messageCopy, &contactCopy)
			root.DispatchWebhook(accountCopy.OrganizationID, models.WebhookEventMessageOutgoing, MessageEventData{
				MessageID:       messageCopy.ID.String(),
				ContactID:       contactCopy.ID.String(),
				ContactPhone:    contactCopy.PhoneNumber,
				ContactName:     contactCopy.ProfileName,
				MessageType:     messageCopy.MessageType,
				Content:         messageCopy.Content,
				WhatsAppAccount: accountCopy.Name,
				Direction:       models.DirectionOutgoing,
			})
		})
	}
	return &messageRow, true, nil
}

func (a *App) mergeCoexistenceMessageDetails(
	account *models.WhatsAppAccount,
	existing *models.Message,
	message CoexistenceMessage,
	status models.MessageStatus,
	metadata models.JSONB,
	identities ...coexistenceContactIdentity,
) error {
	if existing == nil {
		return nil
	}
	if err := a.prepareWhatsAppMessageAuthority(account); err != nil {
		return err
	}
	identity := coexistenceMessageContactIdentity(message, existing.Direction, coexistenceContactIdentity{}, nil)
	if len(identities) != 0 {
		identity = identities[0]
	}
	// This also covers direct callers with a stale snapshot: the shared
	// resolver locks canonical Contact then Message and rechecks every proof.
	resolved, err := a.resolveWhatsAppMessage(account, whatsAppMessageLookup{
		WAMID: existing.WhatsAppMessageID, MessageID: existing.ID, ContactID: existing.ContactID,
		Identity: identity, Direction: existing.Direction, Lock: true, RepairProjection: true,
	})
	if err != nil {
		return fmt.Errorf("lock coexistence message details: %w", err)
	}
	*existing = resolved.Message
	if revoked, _ := existing.Metadata[coexistenceMediaRevokedMetadataKey].(bool); revoked {
		return nil
	}
	// A revoke that raced first delivery proceeds to the full sanitization
	// path after this replay. Do not attach revoked metadata to live media or
	// enqueue hydration before that transition.
	if revoked, _ := metadata[coexistenceMediaRevokedMetadataKey].(bool); revoked {
		return nil
	}
	// The missing-target edit path also continues with the full locked edit
	// when a concurrent original wins. Its event marker must not make that
	// subsequent mutation look already applied before the content is changed.
	if _, editing := metadata["coexistence_edit_event_id"]; editing {
		return nil
	}
	if coexistenceMessageHasStoredEdit(existing) {
		// The original/history replay has no revision authority over a stored
		// edit. Preserve its exact content/type/media and enqueue only that
		// current edited media, never the replay's obsolete provider object.
		return a.ensureProvenCoexistenceMediaJob(account, existing)
	}
	extracted := a.extractMessageContentForPersistence(message.IncomingTextMessage)
	mergedMetadata := cloneMessageMetadata(existing.Metadata)
	for key, value := range metadata {
		switch key {
		case coexistenceMediaProviderIDMetadataKey, "coexistence_media_sha256", coexistenceMediaHydratedIDMetadataKey:
			// Only a compatible declared media payload below may replace these;
			// incoming correlation metadata is not a media revision authority.
			continue
		}
		mergedMetadata[key] = value
	}
	updates := map[string]any{"metadata": mergedMetadata}
	if existing.MessageType == models.MessageType("media_placeholder") || existing.MessageType == models.MessageType(extracted.Type) {
		replaceContent := existing.MessageType == models.MessageType("media_placeholder")
		for key, value := range coexistenceMediaReplacementUpdates(existing, message.IncomingTextMessage, extracted, mergedMetadata, replaceContent) {
			updates[key] = value
		}
	}
	if existing.MessageType == models.MessageType("media_placeholder") {
		updates["message_type"] = models.MessageType(extracted.Type)
		updates["content"] = extracted.Text
		mergedMetadata["history_media_placeholder"] = false
		mergedMetadata["history_media_detail_received"] = true
	}
	if statusPriority(status) > statusPriority(existing.Status) {
		updates["status"] = status
	}
	if err := a.DB.Model(&models.Message{}).Where(
		"organization_id = ? AND id = ?",
		existing.OrganizationID,
		existing.ID,
	).Updates(updates).Error; err != nil {
		return err
	}
	return a.ensureProvenCoexistenceMediaJob(account, existing)
}

func coexistenceHistoryThreadIdentity(thread CoexistenceHistoryThread) coexistenceContactIdentity {
	identity := coexistenceContactIdentity{
		Phone:        strings.TrimSpace(thread.Context.WaID),
		UserID:       strings.TrimSpace(thread.Context.UserID),
		ParentUserID: strings.TrimSpace(thread.Context.ParentUserID),
		Username:     strings.TrimSpace(thread.Context.Username),
		FallbackKey:  strings.TrimSpace(thread.ID),
	}
	// Before BSUID rollout the thread id was the customer's phone. Current
	// payloads put all identifiers in context and can omit id entirely.
	if identity.Phone == "" && identity.primaryUserID() == "" {
		identity.Phone = strings.TrimSpace(thread.ID)
	}
	return identity
}

func coexistenceHistoryDirection(
	businessDisplayPhone string,
	thread CoexistenceHistoryThread,
	message CoexistenceMessage,
) models.Direction {
	if sameWhatsAppPhone(message.From, businessDisplayPhone) {
		return models.DirectionOutgoing
	}
	if sameWhatsAppPhone(message.To, businessDisplayPhone) {
		return models.DirectionIncoming
	}
	if strings.TrimSpace(message.FromUserID) != "" &&
		(message.FromUserID == strings.TrimSpace(thread.Context.UserID) ||
			message.FromUserID == strings.TrimSpace(thread.Context.ParentUserID)) {
		return models.DirectionIncoming
	}
	if strings.TrimSpace(message.FromParentUserID) != "" &&
		message.FromParentUserID == strings.TrimSpace(thread.Context.ParentUserID) {
		return models.DirectionIncoming
	}
	if strings.TrimSpace(message.ToUserID) != "" &&
		(message.ToUserID == strings.TrimSpace(thread.Context.UserID) ||
			message.ToUserID == strings.TrimSpace(thread.Context.ParentUserID)) {
		return models.DirectionOutgoing
	}
	if strings.TrimSpace(message.ToParentUserID) != "" &&
		message.ToParentUserID == strings.TrimSpace(thread.Context.ParentUserID) {
		return models.DirectionOutgoing
	}
	threadIdentity := coexistenceHistoryThreadIdentity(thread)
	if sameWhatsAppPhone(message.From, threadIdentity.Phone) {
		return models.DirectionIncoming
	}
	if sameWhatsAppPhone(message.To, threadIdentity.Phone) {
		return models.DirectionOutgoing
	}
	if strings.TrimSpace(message.ToUserID) != "" || strings.TrimSpace(message.To) != "" {
		return models.DirectionOutgoing
	}
	return models.DirectionIncoming
}

func coexistenceStandaloneHistoryDirection(
	businessDisplayPhone string,
	message CoexistenceMessage,
) models.Direction {
	if sameWhatsAppPhone(message.From, businessDisplayPhone) {
		return models.DirectionOutgoing
	}
	if sameWhatsAppPhone(message.To, businessDisplayPhone) {
		return models.DirectionIncoming
	}
	if strings.TrimSpace(message.ToUserID) != "" {
		return models.DirectionOutgoing
	}
	if strings.TrimSpace(message.FromUserID) != "" {
		return models.DirectionIncoming
	}
	if strings.TrimSpace(message.To) != "" {
		return models.DirectionOutgoing
	}
	return models.DirectionIncoming
}

func coexistenceHistoryStatus(context *CoexistenceHistoryContext) models.MessageStatus {
	if context == nil {
		return models.MessageStatusSent
	}
	switch strings.ToUpper(strings.TrimSpace(context.Status)) {
	case "PENDING":
		return models.MessageStatusPending
	case "SENT":
		return models.MessageStatusSent
	case "DELIVERED":
		return models.MessageStatusDelivered
	case "READ", "PLAYED":
		return models.MessageStatusRead
	case "ERROR", "FAILED":
		return models.MessageStatusFailed
	default:
		return models.MessageStatusSent
	}
}

func coexistenceHistoryStatusText(context *CoexistenceHistoryContext) string {
	if context == nil {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(context.Status))
}

func coexistenceMessageTime(timestamp string, fallback time.Time) time.Time {
	seconds, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil || seconds <= 0 {
		return fallback.UTC()
	}
	return time.Unix(seconds, 0).UTC()
}

func coexistenceMessagePreview(messageType, content string) string {
	preview := content
	if len(preview) > 100 {
		preview = preview[:97] + "..."
	}
	if messageType != "text" && messageType != "button_reply" && messageType != "nfm_reply" {
		preview = "[" + messageType + "]"
	}
	return preview
}

func coexistenceMediaIdentity(message IncomingTextMessage) (string, string) {
	switch {
	case message.Type == "image" && message.Image != nil:
		return strings.TrimSpace(message.Image.ID), strings.TrimSpace(message.Image.SHA256)
	case message.Type == "document" && message.Document != nil:
		return strings.TrimSpace(message.Document.ID), strings.TrimSpace(message.Document.SHA256)
	case message.Type == "video" && message.Video != nil:
		return strings.TrimSpace(message.Video.ID), strings.TrimSpace(message.Video.SHA256)
	case message.Type == "audio" && message.Audio != nil:
		return strings.TrimSpace(message.Audio.ID), ""
	case message.Type == "sticker" && message.Sticker != nil:
		return strings.TrimSpace(message.Sticker.ID), strings.TrimSpace(message.Sticker.SHA256)
	default:
		return "", ""
	}
}

func sameWhatsAppPhone(left, right string) bool {
	normalize := func(value string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsDigit(r) {
				return r
			}
			return -1
		}, value)
	}
	left = normalize(left)
	right = normalize(right)
	return left != "" && left == right
}

func coexistenceWebhookErrorText(webhookError WebhookStatusError) string {
	if text := strings.TrimSpace(webhookError.ErrorData.Details); text != "" {
		return text
	}
	if text := strings.TrimSpace(webhookError.Message); text != "" {
		return text
	}
	return strings.TrimSpace(webhookError.Title)
}

func intPointer(value int) *int {
	result := value
	return &result
}

func (a *App) persistCoexistenceLifecycleBeforeAck(
	wabaID string,
	entryTimestamp int64,
	event string,
	phoneNumber string,
	disconnection *CoexistenceDisconnection,
) error {
	wabaID = strings.TrimSpace(wabaID)
	event = strings.ToUpper(strings.TrimSpace(event))
	if wabaID == "" || !isCoexistenceLifecycleEvent(event) {
		return errors.New("valid coexistence lifecycle WABA and event are required")
	}
	organizationIDs, err := a.resolveWABAOrganizations(wabaID)
	if err != nil {
		return fmt.Errorf("resolve coexistence lifecycle tenants: %w", err)
	}
	eventAt := time.Now().UTC().Truncate(time.Second)
	if entryTimestamp > 0 {
		eventAt = time.Unix(entryTimestamp, 0).UTC()
	}
	for _, organizationID := range organizationIDs {
		if err := a.WithCommittedTenantApp(organizationID, func(scoped *App) error {
			var accounts []models.WhatsAppAccount
			// Keep the global Coexistence lock order account -> state. Onboarding
			// and recovery use the same order, avoiding a state/account deadlock
			// when a lifecycle webhook arrives concurrently.
			if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
				"organization_id = ? AND business_id = ? AND is_smb = ?",
				organizationID,
				wabaID,
				true,
			).Find(&accounts).Error; err != nil {
				return err
			}
			if len(accounts) == 0 {
				// This account_update is not for a coexistence-enabled number in
				// the tenant. It is safe to acknowledge without mutating classic
				// Cloud API accounts that happen to share the WABA.
				return nil
			}
			states := make([]*models.WhatsAppCoexistenceState, len(accounts))
			for index := range accounts {
				state, err := ensureCoexistenceState(scoped.DB, &accounts[index])
				if err != nil {
					return err
				}
				states[index] = state
			}

			targets := make([]bool, len(accounts))
			for index := range targets {
				targets[index] = true
			}
			if event == "PARTNER_REMOVED" && strings.TrimSpace(phoneNumber) != "" {
				clear(targets)
				mappedMatches := make([]int, 0, 1)
				unmapped := make([]int, 0, 1)
				for index, state := range states {
					if strings.TrimSpace(state.BusinessPhoneNumber) == "" {
						unmapped = append(unmapped, index)
						continue
					}
					if sameWhatsAppPhone(state.BusinessPhoneNumber, phoneNumber) {
						mappedMatches = append(mappedMatches, index)
					}
				}
				switch {
				case len(mappedMatches) == 1:
					targets[mappedMatches[0]] = true
				case len(mappedMatches) > 1:
					return fmt.Errorf(
						"ambiguous PARTNER_REMOVED phone %q matches %d coexistence accounts",
						phoneNumber,
						len(mappedMatches),
					)
				case len(accounts) == 1 && len(unmapped) == 1:
					// Conservative compatibility for one legacy state created before
					// BusinessPhoneNumber was persisted.
					targets[0] = true
				case len(unmapped) > 0:
					return fmt.Errorf(
						"cannot safely route PARTNER_REMOVED phone %q across %d SMB accounts with %d unmapped coexistence states",
						phoneNumber,
						len(accounts),
						len(unmapped),
					)
				default:
					// Every coexistence account is mapped and this display number is
					// not one of them, so this tenant has nothing to mutate.
					return nil
				}
			}

			for index := range accounts {
				if !targets[index] {
					continue
				}
				account := &accounts[index]
				state := states[index]
				lastLifecycleSecond := time.Time{}
				if state.LastLifecycleEventAt != nil {
					lastLifecycleSecond = state.LastLifecycleEventAt.UTC().Truncate(time.Second)
				}
				if !lastLifecycleSecond.IsZero() && eventAt.Before(lastLifecycleSecond) {
					scoped.Log.Info(
						"Ignoring stale coexistence lifecycle event",
						"event", event,
						"event_at", eventAt,
						"last_event", state.LastLifecycleEvent,
						"last_event_at", state.LastLifecycleEventAt,
						"account_id", account.ID,
					)
					continue
				}
				if !lastLifecycleSecond.IsZero() &&
					eventAt.Equal(lastLifecycleSecond) &&
					state.LastLifecycleEvent == event {
					continue
				}
				metadata := models.JSONB{
					"event":        event,
					"waba_id":      wabaID,
					"phone_number": strings.TrimSpace(phoneNumber),
				}
				if disconnection != nil {
					metadata["reason"] = strings.TrimSpace(disconnection.Reason)
					metadata["initiated_by"] = strings.TrimSpace(disconnection.InitiatedBy)
				}
				stateUpdates := map[string]any{
					"last_lifecycle_event":    event,
					"last_lifecycle_event_at": eventAt,
					"lifecycle_metadata":      metadata,
					"version":                 gorm.Expr("version + 1"),
				}
				shouldDisconnectAccount := false
				switch event {
				case "PARTNER_REMOVED":
					shouldDisconnectAccount = true
					stateUpdates["lifecycle_status"] = models.CoexistenceLifecycleStatusDisconnected
					stateUpdates["disconnected_at"] = eventAt
					if disconnection != nil {
						stateUpdates["disconnect_reason_code"] = strings.TrimSpace(disconnection.Reason)
						stateUpdates["disconnect_reason_message"] = coexistenceDisconnectReasonText(disconnection)
					}
				case "ACCOUNT_OFFBOARDED":
					shouldDisconnectAccount = true
					stateUpdates["lifecycle_status"] = models.CoexistenceLifecycleStatusOffboarded
					stateUpdates["offboarded_at"] = eventAt
					stateUpdates["onboarding_status"] = models.CoexistenceOnboardingStatusOffboarded
				case "ACCOUNT_RECONNECTED":
					// This event only proves that the Business App companion link
					// returned. It does not prove that ReReply's stored access token
					// is current, so API sending remains fail-closed until a fresh
					// Embedded Signup/token exchange activates the account.
					shouldDisconnectAccount = true
					stateUpdates["lifecycle_status"] = models.CoexistenceLifecycleStatusConnected
					stateUpdates["reconnected_at"] = eventAt
					stateUpdates["disconnect_reason_code"] = ""
					stateUpdates["disconnect_reason_message"] = ""
					stateUpdates["onboarding_status"] = models.CoexistenceOnboardingStatusPending
				}
				if err := scoped.DB.Model(&models.WhatsAppCoexistenceState{}).Where(
					"organization_id = ? AND whats_app_account_id = ?",
					account.OrganizationID,
					account.ID,
				).Updates(stateUpdates).Error; err != nil {
					return err
				}
				if shouldDisconnectAccount {
					if err := scoped.DB.Model(&models.WhatsAppAccount{}).Where(
						"organization_id = ? AND id = ?",
						account.OrganizationID,
						account.ID,
					).Update("status", "disconnected").Error; err != nil {
						return err
					}
				}
				phoneID := account.PhoneID
				scoped.afterTenantCommit(func() {
					scoped.rootApp().InvalidateWhatsAppAccountCache(phoneID)
				})
			}
			return nil
		}); err != nil {
			return fmt.Errorf("persist coexistence lifecycle for organization %s: %w", organizationID, err)
		}
	}
	return nil
}

func coexistenceDisconnectReasonText(disconnection *CoexistenceDisconnection) string {
	if disconnection == nil {
		return ""
	}
	reason := strings.TrimSpace(disconnection.Reason)
	initiatedBy := strings.TrimSpace(disconnection.InitiatedBy)
	if reason == "" {
		return initiatedBy
	}
	if initiatedBy == "" {
		return reason
	}
	return reason + " (initiated by " + initiatedBy + ")"
}
