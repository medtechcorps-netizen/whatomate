package channel

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// LegacyMetaProvider identifies the read-only ChannelAccount shadow for an
	// account that is still delivered by the established Meta WhatsApp path.
	// It deliberately has no provider adapter or ChannelCredential.
	LegacyMetaProvider = "meta_legacy"

	legacyMetaIDPrefix = "legacy-"
)

var ErrLegacyMetaBridgeConflict = errors.New("legacy Meta WhatsApp bridge conflict")

// LegacyMetaAccountRef is the complete account input accepted by the bridge.
// Keeping this separate from models.WhatsAppAccount makes it impossible for
// callers to hand credential fields to mirroring or backfill code.
type LegacyMetaAccountRef struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	Name           string
	Status         string
	PhoneID        string
	BusinessID     string
}

type LegacyMetaMirrorResult struct {
	ChannelAccountID uuid.UUID
	ConversationID   uuid.UUID
	Linked           bool
}

// LegacyMergedProjectionEvidence is captured selector evidence, never a
// capability. Every field is checked against fresh rows before it is used.
type LegacyMergedProjectionEvidence struct {
	OrganizationID     uuid.UUID
	AccountID          uuid.UUID
	MessageID          uuid.UUID
	CanonicalContactID uuid.UUID
	LegacyContactID    uuid.UUID
	ConversationID     uuid.UUID
	IdentityID         uuid.UUID
	ParticipantID      uuid.UUID
	WAMID              string
	DirectBSUID        string
	ParentBSUID        string
}

type LegacyMergedProjection struct {
	Conversation models.InboxConversation
	Identity     models.ContactIdentity
	Participant  models.ConversationParticipant
}

// Limit proof metadata before decoding it. A marker is a refusal, not an empty
// identity. The full contact/message bodies are not required by this proof.
const legacyMergedMetadataSQL = `CASE WHEN octet_length(COALESCE(metadata, '{}'::jsonb)::text) <= 65536 THEN metadata ELSE '{"legacy_proof_oversize":true}'::jsonb END AS metadata`

func legacyMergedProofContext(db *gorm.DB) error {
	if db == nil || db.Statement == nil || db.Statement.Context == nil {
		return errors.New("legacy merged proof requires caller context")
	}
	if err := db.Statement.Context.Err(); err != nil {
		return err
	}
	if deadline, ok := db.Statement.Context.Deadline(); !ok || deadline.After(time.Now().Add(5*time.Second)) {
		return errors.New("legacy merged proof requires bounded scope")
	}
	return nil
}

func legacyMergedBSUID(value string) bool {
	if value != strings.TrimSpace(value) || len(value) > 150 {
		return false
	}
	parts := strings.Split(value, ".")
	if len(parts) < 2 || len(parts) > 4 || len(parts[0]) != 2 {
		return false
	}
	for _, ch := range parts[0] {
		if ch < 'A' || ch > 'Z' {
			return false
		}
	}
	for _, part := range parts[1:] {
		if len(part) < 1 || len(part) > 128 {
			return false
		}
		for _, ch := range part {
			if !(ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9') {
				return false
			}
		}
	}
	return true
}

func legacyMergedAllowed(value, direct, parent string) bool {
	return value == direct || parent != "" && value == parent
}

func legacyMergedMetadataSafe(metadata models.JSONB, direct, parent string) bool {
	for _, key := range []string{"legacy_proof_oversize", "coexistence_conflicting_user_id", "coexistence_phone_conflict", "coexistence_reconciled_contact_id"} {
		if _, exists := metadata[key]; exists {
			return false
		}
	}
	for _, key := range []string{"coexistence_user_id", "coexistence_parent_user_id"} {
		if raw, present := metadata[key]; present {
			value, ok := raw.(string)
			if !ok {
				return false
			}
			value = strings.TrimSpace(value)
			if value != "" && (key == "coexistence_parent_user_id" && value != parent || !legacyMergedAllowed(value, direct, parent)) {
				return false
			}
		}
	}
	return true
}

func legacyMergedContactQuery(db *gorm.DB) *gorm.DB {
	return db.Unscoped().Select("id, organization_id, phone_number, bs_uid, merged_into_id, merged_at, merged_by_id, deleted_at, " + legacyMergedMetadataSQL)
}

// ProveLegacyMergedAlias corroborates one historical sender and its complete
// current family. It neither learns a selector nor authorizes a new message.
func ProveLegacyMergedAlias(db *gorm.DB, organizationID, canonicalID uuid.UUID, direct, parent string) (uuid.UUID, error) {
	if err := legacyMergedProofContext(db); err != nil {
		return uuid.Nil, err
	}
	if organizationID == uuid.Nil || canonicalID == uuid.Nil || !legacyMergedBSUID(direct) || parent != "" && !legacyMergedBSUID(parent) {
		return uuid.Nil, nil
	}
	var roots []models.Contact
	if err := legacyMergedContactQuery(db).Where("organization_id = ? AND id = ?", organizationID, canonicalID).Limit(2).Find(&roots).Error; err != nil {
		return uuid.Nil, err
	}
	if len(roots) != 1 || roots[0].DeletedAt.Valid || roots[0].MergedIntoID != nil {
		return uuid.Nil, nil
	}
	var aliases []models.Contact
	if err := legacyMergedContactQuery(db).Where("organization_id = ? AND phone_number = ? AND COALESCE(bs_uid, '') = ''", organizationID, direct).Limit(2).Find(&aliases).Error; err != nil {
		return uuid.Nil, err
	}
	if len(aliases) != 1 || !aliases[0].DeletedAt.Valid || aliases[0].MergedIntoID == nil {
		return uuid.Nil, nil
	}
	var phoneHolders []uuid.UUID
	if err := db.Unscoped().Model(&models.Contact{}).Where("organization_id = ? AND BTRIM(phone_number) IN ?", organizationID, []string{direct, "+" + direct}).Limit(2).Pluck("id", &phoneHolders).Error; err != nil {
		return uuid.Nil, err
	}
	if len(phoneHolders) != 1 || phoneHolders[0] != aliases[0].ID {
		return uuid.Nil, nil
	}
	family := map[uuid.UUID]models.Contact{canonicalID: roots[0]}
	ids, frontier := []uuid.UUID{canonicalID}, []uuid.UUID{canonicalID}
	for depth := 1; len(frontier) != 0; depth++ {
		if err := legacyMergedProofContext(db); err != nil {
			return uuid.Nil, err
		}
		var children []models.Contact
		if err := legacyMergedContactQuery(db).Where("organization_id = ? AND merged_into_id IN ?", organizationID, frontier).Order("id").Limit(65 - len(family)).Find(&children).Error; err != nil {
			return uuid.Nil, err
		}
		if len(children) == 0 {
			break
		}
		if depth >= 16 || len(children) > 64-len(family) {
			return uuid.Nil, errors.New("legacy WhatsApp message family exceeds proof bound")
		}
		frontier = nil
		for _, child := range children {
			if _, exists := family[child.ID]; exists {
				return uuid.Nil, errors.New("legacy WhatsApp message family contains a cycle")
			}
			family[child.ID] = child
			ids = append(ids, child.ID)
			frontier = append(frontier, child.ID)
		}
	}
	if _, exists := family[aliases[0].ID]; !exists {
		return uuid.Nil, nil
	}
	for _, member := range family {
		if !legacyMergedMetadataSafe(member.Metadata, direct, parent) {
			return uuid.Nil, nil
		}
		if value := strings.TrimSpace(member.BSUID); value != "" && !legacyMergedAllowed(value, direct, parent) {
			return uuid.Nil, nil
		}
		if value := strings.TrimPrefix(strings.TrimSpace(member.PhoneNumber), "+"); legacyMergedBSUID(value) && !legacyMergedAllowed(value, direct, parent) {
			return uuid.Nil, nil
		}
	}
	allowed := []string{direct}
	if parent != "" && parent != direct {
		allowed = append(allowed, parent)
	}
	var holders []uuid.UUID
	if err := db.Unscoped().Model(&models.Contact{}).Where("organization_id = ? AND BTRIM(bs_uid) IN ?", organizationID, allowed).Limit(65).Pluck("id", &holders).Error; err != nil {
		return uuid.Nil, err
	}
	if len(holders) > 64 {
		return uuid.Nil, errors.New("legacy WhatsApp holder proof bound")
	}
	for _, id := range holders {
		if _, exists := family[id]; !exists {
			return uuid.Nil, nil
		}
	}
	var decisions []uuid.UUID
	if err := db.Model(&models.WhatsAppIdentityReviewHold{}).Where("organization_id = ? AND disposition = ? AND direct_primary_bsuid NOT IN ? AND decision_target_contact_id IN ?", organizationID, models.WhatsAppIdentityReviewDispositionFutureRouting, allowed, ids).Limit(1).Pluck("id", &decisions).Error; err != nil {
		return uuid.Nil, err
	}
	if len(decisions) != 0 {
		return uuid.Nil, nil
	}
	var holds []uuid.UUID
	if err := db.Table("whatsapp_identity_review_members AS member").Joins("JOIN whatsapp_identity_review_holds AS hold ON hold.organization_id = member.organization_id AND hold.id = member.hold_id").Where("member.organization_id = ? AND member.contact_id IN ? AND hold.disposition = ?", organizationID, ids, models.WhatsAppIdentityReviewDispositionOpen).Limit(1).Pluck("hold.id", &holds).Error; err != nil {
		return uuid.Nil, err
	}
	if len(holds) != 0 {
		return uuid.Nil, nil
	}
	if err := legacyMergedHistory(db, organizationID, aliases[0].ID, canonicalID, family); err != nil {
		return uuid.Nil, err
	}
	if err := legacyMergedProofContext(db); err != nil {
		return uuid.Nil, err
	}
	return aliases[0].ID, nil
}

func legacyMergedHistory(db *gorm.DB, organizationID, sourceID, targetID uuid.UUID, family map[uuid.UUID]models.Contact) error {
	var guarded bool
	if err := db.Raw(`SELECT count(*) = 1 FROM pg_trigger WHERE tgrelid = to_regclass('customer_activity_events') AND tgname = 'trg_customer_activity_events_append_only' AND NOT tgisinternal AND tgenabled IN ('O','A') AND tgtype = 27 AND tgfoid = to_regprocedure('rereply_reject_customer_activity_mutation()')`).Scan(&guarded).Error; err != nil {
		return err
	}
	if !guarded {
		return errors.New("legacy merged activity guard is unavailable")
	}
	seen := map[uuid.UUID]bool{}
	for current, depth := sourceID, 0; ; depth++ {
		if err := legacyMergedProofContext(db); err != nil {
			return err
		}
		if current == targetID {
			return nil
		}
		if depth >= 15 || seen[current] {
			return errors.New("legacy merged history exceeds proof bound or cycles")
		}
		seen[current] = true
		contact, exists := family[current]
		if !exists || !contact.DeletedAt.Valid || contact.MergedIntoID == nil || contact.MergedAt == nil || contact.MergedByID == nil || *contact.MergedByID == uuid.Nil {
			return errors.New("legacy merged history lacks retained source")
		}
		var events []models.CustomerActivityEvent
		if err := db.Select("id, organization_id, contact_id, event_type, category, actor_type, actor_user_id, source_object_type, source_object_id, occurred_at, idempotency_key, "+legacyMergedMetadataSQL).
			Where(`organization_id = ? AND (source_object_id = ? OR metadata->>'source_contact_id' = ?) AND (event_type = ? OR idempotency_key LIKE 'contact-merge:%' OR (jsonb_exists(metadata, 'source_contact_id') AND jsonb_exists(metadata, 'target_contact_id')))`, organizationID, current, current.String(), models.CustomerActivityContactMerged).
			Order("id").Limit(2).Find(&events).Error; err != nil {
			return err
		}
		if len(events) != 1 {
			return errors.New("legacy merged history is missing or ambiguous")
		}
		event := events[0]
		if event.EventType != models.CustomerActivityContactMerged || event.Category != models.CustomerActivityCategoryContact || event.SourceObjectType != "contact" || event.SourceObjectID == nil || *event.SourceObjectID != current || event.ContactID == uuid.Nil || event.ActorType != models.CustomerActivityActorUser || event.ActorUserID == nil || *event.ActorUserID != *contact.MergedByID || !event.OccurredAt.UTC().Truncate(time.Microsecond).Equal(contact.MergedAt.UTC().Truncate(time.Microsecond)) || !strings.HasPrefix(event.IdempotencyKey, "contact-merge:") || strings.TrimSpace(strings.TrimPrefix(event.IdempotencyKey, "contact-merge:")) == "" {
			return errors.New("legacy merged history contradicts retained source")
		}
		source, sourceOK := event.Metadata["source_contact_id"].(string)
		target, targetOK := event.Metadata["target_contact_id"].(string)
		if !sourceOK || !targetOK || source != current.String() || target != event.ContactID.String() {
			return errors.New("legacy merged history metadata mismatch")
		}
		if _, exists := family[event.ContactID]; !exists {
			return errors.New("legacy merged history leaves canonical family")
		}
		current = event.ContactID
	}
}

func legacyMergedReadError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("%w: historical projection dependency is missing", ErrLegacyMetaBridgeConflict)
	}
	return err
}

// ValidateLegacyMergedProjection reads a pre-existing linked projection. It
// never creates links or treats the supplied evidence as authority.
func ValidateLegacyMergedProjection(db *gorm.DB, accountRef LegacyMetaAccountRef, message *models.Message, evidence *LegacyMergedProjectionEvidence) (*LegacyMergedProjection, error) {
	if err := legacyMergedProofContext(db); err != nil {
		return nil, legacyMergedReadError(err)
	}
	if message == nil || evidence == nil || message.OrganizationID != accountRef.OrganizationID || evidence.CanonicalContactID == uuid.Nil || accountRef.ID == uuid.Nil || accountRef.OrganizationID == uuid.Nil || evidence.OrganizationID != accountRef.OrganizationID || evidence.AccountID != accountRef.ID || evidence.MessageID != message.ID || evidence.CanonicalContactID != message.ContactID || evidence.WAMID == "" || evidence.WAMID != message.WhatsAppMessageID || message.Direction != models.DirectionOutgoing || message.InboxConversationID == nil || evidence.LegacyContactID == uuid.Nil {
		return nil, errors.New("legacy merged projection input mismatch")
	}
	var native models.WhatsAppAccount
	if err := db.Select("id, organization_id").Where("organization_id = ? AND id = ?", accountRef.OrganizationID, accountRef.ID).First(&native).Error; err != nil {
		return nil, legacyMergedReadError(err)
	}
	var messages []models.Message
	if err := db.Unscoped().Select("id, organization_id, contact_id, whats_app_message_id, direction, inbox_conversation_id, deleted_at").Where("organization_id = ? AND BTRIM(whats_app_message_id) = ?", accountRef.OrganizationID, evidence.WAMID).Limit(2).Find(&messages).Error; err != nil {
		return nil, legacyMergedReadError(err)
	}
	if len(messages) != 1 || messages[0].DeletedAt.Valid || messages[0].ID != message.ID || messages[0].ContactID != evidence.CanonicalContactID || messages[0].Direction != models.DirectionOutgoing || messages[0].WhatsAppMessageID != evidence.WAMID || messages[0].InboxConversationID == nil || *messages[0].InboxConversationID != *message.InboxConversationID {
		return nil, errors.New("legacy merged message changed or is ambiguous")
	}
	legacyID, err := ProveLegacyMergedAlias(db, accountRef.OrganizationID, evidence.CanonicalContactID, evidence.DirectBSUID, evidence.ParentBSUID)
	if err != nil {
		return nil, legacyMergedReadError(err)
	}
	if legacyID == uuid.Nil || legacyID != evidence.LegacyContactID {
		return nil, errors.New("legacy merged sender proof changed")
	}
	var conversation models.InboxConversation
	if err := db.Select("id, organization_id, channel_account_id, contact_id, contact_identity_id, channel, external_conversation_id, last_message_at, last_inbound_at, last_outbound_at, service_window_ends_at, deleted_at, "+legacyMergedMetadataSQL).Where("organization_id = ? AND id = ?", accountRef.OrganizationID, *message.InboxConversationID).First(&conversation).Error; err != nil {
		return nil, legacyMergedReadError(err)
	}
	key := "legacy-contact:" + legacyID.String()
	if conversation.ContactID != evidence.CanonicalContactID || conversation.Channel != models.ChannelWhatsApp || conversation.ContactIdentityID == nil || conversation.ExternalConversationID != key || conversation.Metadata["legacy_contact_id"] != legacyID.String() {
		return nil, errors.New("legacy merged conversation proof mismatch")
	}
	var shadow models.ChannelAccount
	if err := db.Select("id, organization_id, channel, provider, external_account_id, "+legacyMergedMetadataSQL).Where("organization_id = ? AND id = ?", accountRef.OrganizationID, conversation.ChannelAccountID).First(&shadow).Error; err != nil {
		return nil, legacyMergedReadError(err)
	}
	boundID, err := LegacyMetaWhatsAppAccountID(&shadow)
	if err != nil || boundID != accountRef.ID {
		return nil, errors.New("legacy merged channel account proof mismatch")
	}
	var shadowIDs []uuid.UUID
	if err := db.Unscoped().Model(&models.ChannelAccount{}).Where("organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?", accountRef.OrganizationID, models.ChannelWhatsApp, LegacyMetaProvider, legacyMetaIDPrefix+"account:"+accountRef.ID.String()).Limit(2).Pluck("id", &shadowIDs).Error; err != nil {
		return nil, err
	}
	if len(shadowIDs) != 1 || shadowIDs[0] != shadow.ID {
		return nil, errors.New("legacy merged channel account is ambiguous")
	}
	var identities []models.ContactIdentity
	if err := db.Unscoped().Select("id, organization_id, channel_account_id, contact_id, channel, external_id, deleted_at, "+legacyMergedMetadataSQL).Where("organization_id = ? AND channel_account_id = ? AND external_id = ?", accountRef.OrganizationID, shadow.ID, key).Limit(2).Find(&identities).Error; err != nil {
		return nil, legacyMergedReadError(err)
	}
	if len(identities) != 1 {
		return nil, errors.New("legacy merged identity is missing or ambiguous")
	}
	identity := identities[0]
	if identity.DeletedAt.Valid || identity.ID != *conversation.ContactIdentityID || identity.ContactID != evidence.CanonicalContactID || identity.Channel != models.ChannelWhatsApp || identity.Metadata["legacy_contact_id"] != legacyID.String() {
		return nil, errors.New("legacy merged identity proof mismatch")
	}
	var conversations []uuid.UUID
	if err := db.Unscoped().Model(&models.InboxConversation{}).Where("organization_id = ? AND channel_account_id = ? AND external_conversation_id = ?", accountRef.OrganizationID, shadow.ID, key).Limit(2).Pluck("id", &conversations).Error; err != nil {
		return nil, legacyMergedReadError(err)
	}
	if len(conversations) != 1 || conversations[0] != conversation.ID {
		return nil, errors.New("legacy merged conversation is ambiguous")
	}
	var participants []models.ConversationParticipant
	if err := db.Unscoped().Select("id, organization_id, conversation_id, role, participant_key, external_id, contact_identity_id, deleted_at, left_at, "+legacyMergedMetadataSQL).Where("organization_id = ? AND conversation_id = ? AND (role = ? OR participant_key = ? OR external_id = ? OR contact_identity_id = ?)", accountRef.OrganizationID, conversation.ID, models.ConversationParticipantRoleCustomer, key, key, identity.ID).Limit(2).Find(&participants).Error; err != nil {
		return nil, legacyMergedReadError(err)
	}
	if len(participants) != 1 {
		return nil, errors.New("legacy merged participant is missing or ambiguous")
	}
	participant := participants[0]
	if participant.DeletedAt.Valid || participant.LeftAt != nil || participant.Role != models.ConversationParticipantRoleCustomer || participant.ContactIdentityID == nil || *participant.ContactIdentityID != identity.ID || participant.ParticipantKey != key || participant.ExternalID != key || participant.Metadata["legacy_contact_id"] != legacyID.String() {
		return nil, errors.New("legacy merged participant proof mismatch")
	}
	if evidence.ConversationID != uuid.Nil && evidence.ConversationID != conversation.ID || evidence.IdentityID != uuid.Nil && evidence.IdentityID != identity.ID || evidence.ParticipantID != uuid.Nil && evidence.ParticipantID != participant.ID {
		return nil, errors.New("legacy merged projection changed during recheck")
	}
	if err := legacyMergedProofContext(db); err != nil {
		return nil, legacyMergedReadError(err)
	}
	return &LegacyMergedProjection{Conversation: conversation, Identity: identity, Participant: participant}, nil
}

type LegacyMetaBackfillStats struct {
	Accounts int
	Messages int
	Linked   int
}

// EnsureLegacyMetaWhatsAppAccount resolves the real read-only ChannelAccount
// shadow for an established WhatsApp account without manufacturing a contact,
// conversation, message, or credential. Callers must already be inside the
// tenant transaction whose organization/account authority they are extending,
// and must already own database.LockOrganizationPolicyScope: they then wait for
// a busy shadow behind their own fence, which is the admission lock order.
//
// Identity-review staging uses this narrow entry point because InboundEvent
// requires a genuine ChannelAccountID even when no physical Contact can yet be
// selected safely.
func EnsureLegacyMetaWhatsAppAccount(
	db *gorm.DB,
	ref LegacyMetaAccountRef,
) (*models.ChannelAccount, error) {
	if db == nil || ref.ID == uuid.Nil || ref.OrganizationID == uuid.Nil {
		return nil, errors.New("legacy Meta account-only bridge scope is required")
	}
	if err := requireLegacyMetaTransaction(db, "legacy Meta account-only bridge"); err != nil {
		return nil, err
	}
	if err := requireLegacyMetaPolicyFenceOwner(db, ref.OrganizationID); err != nil {
		return nil, err
	}
	verified, err := verifiedLegacyMetaAccountRef(db, ref)
	if err != nil {
		return nil, err
	}
	return ensureLegacyMetaAccountWithin(db, verified, true)
}

// LegacyMetaWhatsAppAccountID resolves the immutable established WhatsApp
// account behind a read-only omnichannel shadow. Both the private metadata and
// deterministic external ID must agree, so callers fail closed on stale or
// manually altered bridge rows.
func LegacyMetaWhatsAppAccountID(account *models.ChannelAccount) (uuid.UUID, error) {
	if account == nil || account.ID == uuid.Nil || account.OrganizationID == uuid.Nil ||
		account.Channel != models.ChannelWhatsApp || account.Provider != LegacyMetaProvider {
		return uuid.Nil, errors.New("legacy Meta WhatsApp shadow account is invalid")
	}
	rawID, ok := account.Metadata["legacy_account_id"].(string)
	if !ok {
		return uuid.Nil, errors.New("legacy Meta WhatsApp account binding is missing")
	}
	accountID, err := uuid.Parse(strings.TrimSpace(rawID))
	if err != nil || accountID == uuid.Nil {
		return uuid.Nil, errors.New("legacy Meta WhatsApp account binding is invalid")
	}
	if account.ExternalAccountID != legacyMetaIDPrefix+"account:"+accountID.String() {
		return uuid.Nil, errors.New("legacy Meta WhatsApp account binding is inconsistent")
	}
	return accountID, nil
}

// LegacyMetaAIBookingRouteBinding binds physical routing, never mutable display
// names or credentials. Callers must load the current native row from the tenant.
func LegacyMetaAIBookingRouteBinding(account *models.WhatsAppAccount) string {
	if account == nil || account.ID == uuid.Nil || account.OrganizationID == uuid.Nil ||
		account.DeletedAt.Valid || strings.TrimSpace(account.PhoneID) == "" ||
		strings.TrimSpace(account.BusinessID) == "" {
		return ""
	}
	body, _ := json.Marshal([]string{"rereply:ai-booking:native-route:v1",
		account.OrganizationID.String(), account.ID.String(), account.PhoneID, account.BusinessID})
	return fmt.Sprintf("%x", sha256.Sum256(body))
}

// LegacyMetaAIBookingAuthority verifies an existing shadow against the current
// native account. The caller separately proves exactly one live matching shadow;
// this function never creates a shadow or grants generic channel outbound access.
func LegacyMetaAIBookingAuthority(shadow *models.ChannelAccount, native *models.WhatsAppAccount) (string, bool) {
	revision, ok := models.AIBookingAuthority(shadow)
	if !ok || native == nil || native.Status != "active" ||
		native.OrganizationID != shadow.OrganizationID ||
		shadow.Config["legacy_read_only"] != true || shadow.Config["outbound_enabled"] != false ||
		shadow.Config["reply_route"] != "chat" {
		return "", false
	}
	boundID, err := LegacyMetaWhatsAppAccountID(shadow)
	binding := LegacyMetaAIBookingRouteBinding(native)
	if err != nil || boundID != native.ID || binding == "" ||
		shadow.Config[models.ChannelConfigAIBookingRouteBinding] != binding {
		return "", false
	}
	return revision, true
}

// StageLegacyMetaWhatsAppAccountRename refreshes an established account's
// mutable display-name projection. Invoke it before updating the native
// account or auditing, including when the name is unchanged. It takes the
// organization and native row, plus the shadow on a rename, following the
// protocol documented in legacy_meta_fence.go. Sends hold the native row
// across Meta; while waiting for them this stage leaves the organization and
// shadow available (except the bounded plain-order fallback).
//
// A missing shadow is valid: the first mirror/backfill will create one from the
// renamed account. An existing shadow, however, must still carry the exact
// immutable account binding and the expected previous name. Any disagreement
// fails closed so a rename cannot silently bless a corrupt or cross-account
// projection.
func StageLegacyMetaWhatsAppAccountRename(
	db *gorm.DB,
	organizationID, accountID uuid.UUID,
	previousName, nextName string,
) (bool, error) {
	if db == nil || organizationID == uuid.Nil || accountID == uuid.Nil {
		return false, errors.New("legacy Meta account rename scope is required")
	}
	if err := requireLegacyMetaTransaction(db, "legacy Meta account rename"); err != nil {
		return false, err
	}
	previousName = strings.TrimSpace(previousName)
	nextName = strings.TrimSpace(nextName)
	if previousName == "" || nextName == "" {
		return false, errors.New("legacy Meta account rename names are required")
	}
	if previousName == nextName {
		if err := LockLegacyMetaOrganizationAndWhatsAppAccount(db, organizationID, accountID); err != nil {
			return false, err
		}
		return true, nil
	}

	externalID := legacyMetaIDPrefix + "account:" + accountID.String()
	shadowRows := func(tx *gorm.DB, lockOptions string) *gorm.DB {
		return tx.Unscoped().
			Clauses(clause.Locking{Strength: "UPDATE", Options: lockOptions}).
			Where(
				"organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
				organizationID,
				models.ChannelWhatsApp,
				LegacyMetaProvider,
				externalID,
			)
	}
	if err := lockLegacyMetaOrganizationThenShadow(db, organizationID, func(tx *gorm.DB, wait legacyMetaRowWait) error {
		accountOptions := wait.lockOptions()
		if wait == legacyMetaRowsAlone {
			// Sends hold the account row FOR SHARE across their Meta call.
			// Wait for them holding nothing, not while owning the shadow,
			// which an admission on this account locks after its fence.
			if err := waitForLegacyMetaRow(tx, func(tx *gorm.DB) error {
				return lockLegacyMetaWhatsAppAccount(tx, organizationID, accountID, "")
			}); err != nil {
				return err
			}
			accountOptions = legacyMetaNowaitOption(true)
		}
		var shadows []models.ChannelAccount
		if err := shadowRows(tx, wait.lockOptions()).Find(&shadows).Error; err != nil {
			return fmt.Errorf("lock legacy Meta shadow for account rename: %w", err)
		}
		err := lockLegacyMetaWhatsAppAccount(tx, organizationID, accountID, accountOptions)
		if wait == legacyMetaRowsAlone && legacyMetaLockNotAvailable(err) {
			return fmt.Errorf("%w: %w", errLegacyMetaLockRoundLost, err)
		}
		return err
	}); err != nil {
		return false, err
	}
	var shadow models.ChannelAccount
	err := shadowRows(db, "").First(&shadow).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock legacy Meta shadow for account rename: %w", err)
	}
	boundAccountID, bindingErr := LegacyMetaWhatsAppAccountID(&shadow)
	if bindingErr != nil || boundAccountID != accountID {
		return false, fmt.Errorf(
			"%w: shadow account binding changed before rename",
			ErrLegacyMetaBridgeConflict,
		)
	}
	shadowName, ok := shadow.Metadata["legacy_account_name"].(string)
	if !ok || strings.TrimSpace(shadowName) != previousName {
		return false, fmt.Errorf(
			"%w: shadow account name changed before rename",
			ErrLegacyMetaBridgeConflict,
		)
	}

	metadata := make(models.JSONB, len(shadow.Metadata))
	for key, value := range shadow.Metadata {
		metadata[key] = value
	}
	metadata["legacy_account_name"] = nextName
	result := db.Unscoped().Model(&models.ChannelAccount{}).
		Where(
			"id = ? AND organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
			shadow.ID,
			organizationID,
			models.ChannelWhatsApp,
			LegacyMetaProvider,
			externalID,
		).
		Updates(map[string]any{
			"name":     legacyMetaAccountName(nextName, accountID),
			"metadata": metadata,
		})
	if result.Error != nil {
		return false, fmt.Errorf("refresh legacy Meta shadow account name: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return false, fmt.Errorf(
			"%w: shadow account changed before rename",
			ErrLegacyMetaBridgeConflict,
		)
	}
	return true, nil
}

// FinalizeLegacyMetaWhatsAppAccountRename closes the only creation race left
// when StageLegacyMetaWhatsAppAccountRename found no shadow. It must run after
// the caller has updated (and therefore locked) the established account row in
// the same transaction. NOWAIT is intentional: taking a new ChannelAccount
// lock after WhatsAppAccount would otherwise invert the normal bridge order.
//
// If a concurrent mirror's new shadow is still invisible, that mirror must be
// waiting for this transaction's WhatsAppAccount lock in
// ensureLegacyMetaAccount; after commit it revalidates the name and rolls back.
func FinalizeLegacyMetaWhatsAppAccountRename(
	db *gorm.DB,
	organizationID, accountID uuid.UUID,
	previousName, nextName string,
) error {
	if db == nil || organizationID == uuid.Nil || accountID == uuid.Nil {
		return errors.New("legacy Meta account rename scope is required")
	}
	previousName = strings.TrimSpace(previousName)
	nextName = strings.TrimSpace(nextName)
	if previousName == "" || nextName == "" {
		return errors.New("legacy Meta account rename names are required")
	}
	if previousName == nextName {
		return nil
	}

	externalID := legacyMetaIDPrefix + "account:" + accountID.String()
	var shadow models.ChannelAccount
	err := db.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"}).
		Where(
			"organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
			organizationID,
			models.ChannelWhatsApp,
			LegacyMetaProvider,
			externalID,
		).
		First(&shadow).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock newly created legacy Meta shadow after account rename: %w", err)
	}
	boundAccountID, bindingErr := LegacyMetaWhatsAppAccountID(&shadow)
	if bindingErr != nil || boundAccountID != accountID {
		return fmt.Errorf(
			"%w: newly created shadow account binding changed before rename",
			ErrLegacyMetaBridgeConflict,
		)
	}
	shadowName, ok := shadow.Metadata["legacy_account_name"].(string)
	if !ok {
		return fmt.Errorf(
			"%w: newly created shadow account name is missing",
			ErrLegacyMetaBridgeConflict,
		)
	}
	shadowName = strings.TrimSpace(shadowName)
	if shadowName != previousName && shadowName != nextName {
		return fmt.Errorf(
			"%w: newly created shadow account name changed before rename",
			ErrLegacyMetaBridgeConflict,
		)
	}

	metadata := make(models.JSONB, len(shadow.Metadata))
	for key, value := range shadow.Metadata {
		metadata[key] = value
	}
	metadata["legacy_account_name"] = nextName
	result := db.Unscoped().Model(&models.ChannelAccount{}).
		Where(
			"id = ? AND organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
			shadow.ID,
			organizationID,
			models.ChannelWhatsApp,
			LegacyMetaProvider,
			externalID,
		).
		Updates(map[string]any{
			"name":     legacyMetaAccountName(nextName, accountID),
			"metadata": metadata,
		})
	if result.Error != nil {
		return fmt.Errorf("finalize legacy Meta shadow account name: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf(
			"%w: newly created shadow account changed before rename",
			ErrLegacyMetaBridgeConflict,
		)
	}
	return nil
}

// MirrorLegacyWhatsAppMessage links an existing legacy Message envelope into
// the provider-neutral inbox. It never creates an outbound job, sends to Meta,
// copies credentials, or duplicates message content.
func MirrorLegacyWhatsAppMessage(
	db *gorm.DB,
	accountRef LegacyMetaAccountRef,
	messageID uuid.UUID,
) (LegacyMetaMirrorResult, error) {
	var result LegacyMetaMirrorResult
	if db == nil {
		return result, errors.New("legacy Meta bridge database is required")
	}
	if accountRef.ID == uuid.Nil || accountRef.OrganizationID == uuid.Nil ||
		strings.TrimSpace(accountRef.Name) == "" || messageID == uuid.Nil {
		return result, errors.New("legacy Meta bridge account and message identifiers are required")
	}

	err := db.Transaction(func(tx *gorm.DB) error {
		verifiedRef, err := verifiedLegacyMetaAccountRef(tx, accountRef)
		if err != nil {
			return err
		}
		accountRef = verifiedRef

		var message models.Message
		if err := tx.
			Where("id = ? AND organization_id = ?", messageID, accountRef.OrganizationID).
			First(&message).Error; err != nil {
			return fmt.Errorf("load legacy WhatsApp message: %w", err)
		}
		if message.WhatsAppAccount != accountRef.Name {
			return fmt.Errorf(
				"%w: message account does not match the selected legacy account",
				ErrLegacyMetaBridgeConflict,
			)
		}

		var contact models.Contact
		if err := tx.
			Where("id = ? AND organization_id = ?", message.ContactID, accountRef.OrganizationID).
			First(&contact).Error; err != nil {
			return fmt.Errorf("load legacy WhatsApp contact: %w", err)
		}

		account, err := ensureLegacyMetaAccount(tx, accountRef)
		if err != nil {
			return err
		}
		result.ChannelAccountID = account.ID

		identity, err := ensureLegacyMetaIdentity(tx, account, &contact, message.CreatedAt)
		if err != nil {
			return err
		}
		conversation, err := ensureLegacyMetaConversation(
			tx,
			account,
			identity,
			&contact,
			message.CreatedAt,
		)
		if err != nil {
			return err
		}
		result.ConversationID = conversation.ID
		// Read cursors lock this same row before advancing. Acquire it before
		// the message row so a legacy link cannot commit behind a newer cursor
		// with an older ingestion position, and so mirror/read lock order cannot
		// deadlock.
		var lockedConversation models.InboxConversation
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"id = ? AND organization_id = ?",
				conversation.ID,
				accountRef.OrganizationID,
			).
			First(&lockedConversation).Error; err != nil {
			return fmt.Errorf("lock legacy WhatsApp conversation: %w", err)
		}
		*conversation = lockedConversation
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
			Where("id = ? AND organization_id = ?", contact.ID, accountRef.OrganizationID).
			First(&contact).Error; err != nil {
			return fmt.Errorf("lock legacy WhatsApp contact: %w", err)
		}
		var lockedMessage models.Message
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", messageID, accountRef.OrganizationID).
			First(&lockedMessage).Error; err != nil {
			return fmt.Errorf("lock legacy WhatsApp message: %w", err)
		}
		if lockedMessage.WhatsAppAccount != accountRef.Name ||
			lockedMessage.ContactID != message.ContactID {
			return fmt.Errorf(
				"%w: message binding changed before legacy mirror",
				ErrLegacyMetaBridgeConflict,
			)
		}
		message = lockedMessage

		if err := ensureLegacyMetaParticipant(tx, conversation, identity, &contact); err != nil {
			return err
		}

		if message.InboxConversationID != nil {
			if *message.InboxConversationID != conversation.ID {
				return fmt.Errorf(
					"%w: message is already linked to another inbox conversation",
					ErrLegacyMetaBridgeConflict,
				)
			}
		} else {
			update := tx.Model(&models.Message{}).
				Where(
					"id = ? AND organization_id = ? AND inbox_conversation_id IS NULL",
					message.ID,
					accountRef.OrganizationID,
				).
				Update("inbox_conversation_id", conversation.ID)
			if update.Error != nil {
				return fmt.Errorf("link legacy WhatsApp message: %w", update.Error)
			}
			result.Linked = update.RowsAffected == 1
			if !result.Linked {
				var current models.Message
				if err := tx.Select("inbox_conversation_id").
					Where("id = ? AND organization_id = ?", message.ID, accountRef.OrganizationID).
					First(&current).Error; err != nil {
					return fmt.Errorf("verify legacy WhatsApp message link: %w", err)
				}
				if current.InboxConversationID == nil || *current.InboxConversationID != conversation.ID {
					return fmt.Errorf(
						"%w: concurrent message link targeted another conversation",
						ErrLegacyMetaBridgeConflict,
					)
				}
			}
			// The ingestion trigger refreshes the tuple when a previously
			// unlinked legacy row enters this normalized conversation.
			if err := tx.Where(
				"id = ? AND organization_id = ?",
				message.ID,
				accountRef.OrganizationID,
			).First(&message).Error; err != nil {
				return fmt.Errorf("reload linked legacy WhatsApp message: %w", err)
			}
		}

		if err := refreshLegacyMetaConversation(tx, conversation, account, &contact, &message); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

// MirrorLegacyMergedWhatsAppMessage refreshes only an existing historical
// projection after rereading every supplied selector under the bridge fences.
// It is separate from the ordinary backfill root, which remains unchanged.
func MirrorLegacyMergedWhatsAppMessage(
	db *gorm.DB,
	accountRef LegacyMetaAccountRef,
	messageID uuid.UUID,
	evidence *LegacyMergedProjectionEvidence,
) (LegacyMetaMirrorResult, error) {
	var result LegacyMetaMirrorResult
	if db == nil {
		return result, errors.New("legacy Meta bridge database is required")
	}
	if accountRef.ID == uuid.Nil || accountRef.OrganizationID == uuid.Nil ||
		strings.TrimSpace(accountRef.Name) == "" || messageID == uuid.Nil {
		return result, errors.New("legacy Meta bridge account and message identifiers are required")
	}
	if evidence == nil {
		return result, errors.New("legacy merged projection evidence is invalid")
	}
	var proofBudget LegacyMergedProjectionBudget
	bounded, cancel, err := proofBudget.Scope(db)
	if err != nil {
		return result, err
	}
	defer cancel()
	db = bounded
	err = db.Transaction(func(tx *gorm.DB) error {
		verifiedRef, err := verifiedLegacyMetaAccountRef(tx, accountRef)
		if err != nil {
			return err
		}
		accountRef = verifiedRef
		var message models.Message
		if err := tx.Where("id = ? AND organization_id = ?", messageID, accountRef.OrganizationID).First(&message).Error; err != nil {
			return fmt.Errorf("load legacy WhatsApp message: %w", err)
		}
		if message.WhatsAppAccount != accountRef.Name {
			return fmt.Errorf("%w: message account does not match the selected legacy account", ErrLegacyMetaBridgeConflict)
		}
		var contact models.Contact
		if err := tx.Where("id = ? AND organization_id = ?", message.ContactID, accountRef.OrganizationID).First(&contact).Error; err != nil {
			return fmt.Errorf("load legacy WhatsApp contact: %w", err)
		}
		account, err := lockLegacyMergedMetaAccount(tx, accountRef, &message)
		if err != nil {
			return err
		}
		projection, err := mirrorLegacyMergedProjection(tx, accountRef, account, &message, evidence)
		if err != nil {
			return err
		}
		result.ChannelAccountID = account.ID
		result.ConversationID = projection.Conversation.ID
		return nil
	})
	return result, err
}

// The historical path never enters account ensure: ordinary ensure deliberately
// revives a retained shadow, which cannot establish historical proof. Lock only
// the existing same-tenant shadow, then the native account in bridge order.
func lockLegacyMergedMetaAccount(db *gorm.DB, accountRef LegacyMetaAccountRef, message *models.Message) (*models.ChannelAccount, error) {
	if message == nil || message.InboxConversationID == nil {
		return nil, errors.New("legacy merged mirror requires an existing link")
	}
	var conversation models.InboxConversation
	if err := db.Select("id, organization_id, channel_account_id").Where("organization_id = ? AND id = ?", accountRef.OrganizationID, *message.InboxConversationID).First(&conversation).Error; err != nil {
		return nil, legacyMergedReadError(err)
	}
	if err := LockLegacyMetaOrganizationAndShadow(db, accountRef.OrganizationID, conversation.ChannelAccountID); err != nil {
		return nil, err
	}
	if _, err := verifiedLegacyMetaAccountRefWithLock(db, accountRef, true); err != nil {
		return nil, legacyMergedReadError(err)
	}
	var shadows []models.ChannelAccount
	if err := db.Unscoped().Select("id, organization_id, channel, provider, external_account_id, deleted_at, last_inbound_at, last_outbound_at, "+legacyMergedMetadataSQL).Where("organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?", accountRef.OrganizationID, models.ChannelWhatsApp, LegacyMetaProvider, legacyMetaIDPrefix+"account:"+accountRef.ID.String()).Limit(2).Find(&shadows).Error; err != nil {
		return nil, err
	}
	if len(shadows) != 1 || shadows[0].DeletedAt.Valid || shadows[0].ID != conversation.ChannelAccountID {
		return nil, errors.New("legacy merged shadow is missing, deleted or ambiguous")
	}
	boundID, err := LegacyMetaWhatsAppAccountID(&shadows[0])
	if err != nil || boundID != accountRef.ID {
		return nil, errors.New("legacy merged shadow binding mismatch")
	}
	return &shadows[0], nil
}

// The historical branch retains all projection keys. In particular it never
// calls ensureLegacyMetaIdentity/Conversation/Participant with the new target.
func mirrorLegacyMergedProjection(db *gorm.DB, accountRef LegacyMetaAccountRef, account *models.ChannelAccount, message *models.Message, evidence *LegacyMergedProjectionEvidence) (*LegacyMergedProjection, error) {
	if evidence == nil || evidence.ConversationID == uuid.Nil || evidence.IdentityID == uuid.Nil || evidence.ParticipantID == uuid.Nil {
		return nil, errors.New("legacy merged mirror cannot establish a new link")
	}
	projection, err := ValidateLegacyMergedProjection(db, accountRef, message, evidence)
	if err != nil {
		return nil, err
	}
	// Keep the established bridge order: identity, conversation, contact,
	// message. No caller holding Contact/Message enters this after-commit path.
	var identity models.ContactIdentity
	if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("organization_id = ? AND id = ?", accountRef.OrganizationID, projection.Identity.ID).First(&identity).Error; err != nil {
		return nil, err
	}
	var conversation models.InboxConversation
	if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("organization_id = ? AND id = ?", accountRef.OrganizationID, projection.Conversation.ID).First(&conversation).Error; err != nil {
		return nil, err
	}
	var contact models.Contact
	if err := db.Clauses(clause.Locking{Strength: "SHARE"}).Where("organization_id = ? AND id = ?", accountRef.OrganizationID, evidence.CanonicalContactID).First(&contact).Error; err != nil {
		return nil, err
	}
	var locked models.Message
	if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("organization_id = ? AND id = ?", accountRef.OrganizationID, message.ID).First(&locked).Error; err != nil {
		return nil, err
	}
	if locked.WhatsAppAccount != accountRef.Name {
		return nil, errors.New("legacy merged mirror account changed")
	}
	verified, err := ValidateLegacyMergedProjection(db, accountRef, &locked, evidence)
	if err != nil {
		return nil, err
	}
	if verified.Conversation.ChannelAccountID != account.ID || verified.Identity.ID != identity.ID || verified.Conversation.ID != conversation.ID || verified.Participant.ID != projection.Participant.ID {
		return nil, errors.New("legacy merged mirror projection changed")
	}
	if err := refreshLegacyMetaConversation(db, &verified.Conversation, account, &contact, &locked); err != nil {
		return nil, err
	}
	if err := legacyMergedProofContext(db); err != nil {
		return nil, err
	}
	return verified, nil
}

// MarkLegacyWhatsAppConversationRead mirrors the established contact-level read
// state without touching conversations belonging to another tenant or provider.
func MarkLegacyWhatsAppConversationRead(
	db *gorm.DB,
	organizationID uuid.UUID,
	contactID uuid.UUID,
) error {
	if db == nil || organizationID == uuid.Nil || contactID == uuid.Nil {
		return errors.New("legacy Meta read mirror identifiers are required")
	}
	accountIDs := db.Model(&models.ChannelAccount{}).
		Select("id").
		Where(
			"organization_id = ? AND channel = ? AND provider = ?",
			organizationID,
			models.ChannelWhatsApp,
			LegacyMetaProvider,
		)
	return db.Model(&models.InboxConversation{}).
		Where(
			"organization_id = ? AND contact_id = ? AND channel_account_id IN (?)",
			organizationID,
			contactID,
			accountIDs,
		).
		Update("unread_count", 0).Error
}

// BackfillLegacyWhatsAppInbox safely links existing WhatsApp messages in
// bounded batches. The account query explicitly selects non-secret columns and
// rerunning the function is idempotent.
func BackfillLegacyWhatsAppInbox(
	db *gorm.DB,
	batchSize int,
) (LegacyMetaBackfillStats, error) {
	var stats LegacyMetaBackfillStats
	if db == nil {
		return stats, errors.New("legacy Meta backfill database is required")
	}
	if batchSize <= 0 {
		batchSize = 500
	}
	if batchSize > 1000 {
		batchSize = 1000
	}

	var accounts []LegacyMetaAccountRef
	if err := db.Table("whatsapp_accounts").
		Select("id, organization_id, name, status, phone_id, business_id").
		Where("deleted_at IS NULL").
		Order("organization_id, id").
		Scan(&accounts).Error; err != nil {
		return stats, fmt.Errorf("list legacy WhatsApp accounts: %w", err)
	}

	for _, accountRef := range accounts {
		if err := db.Transaction(func(tx *gorm.DB) error {
			_, err := ensureLegacyMetaAccount(tx, accountRef)
			return err
		}); err != nil {
			return stats, err
		}
		stats.Accounts++

		for {
			var messageIDs []uuid.UUID
			if err := db.Model(&models.Message{}).
				Select("id").
				Where(
					"organization_id = ? AND whats_app_account = ? AND inbox_conversation_id IS NULL",
					accountRef.OrganizationID,
					accountRef.Name,
				).
				Order("COALESCE(ingested_at, created_at), id").
				Limit(batchSize).
				Find(&messageIDs).Error; err != nil {
				return stats, fmt.Errorf("list legacy WhatsApp messages: %w", err)
			}
			if len(messageIDs) == 0 {
				break
			}
			for _, messageID := range messageIDs {
				result, err := MirrorLegacyWhatsAppMessage(db, accountRef, messageID)
				if err != nil {
					return stats, err
				}
				stats.Messages++
				if result.Linked {
					stats.Linked++
				}
			}
			if len(messageIDs) < batchSize {
				break
			}
		}
	}
	return stats, nil
}

func verifiedLegacyMetaAccountRef(
	db *gorm.DB,
	supplied LegacyMetaAccountRef,
) (LegacyMetaAccountRef, error) {
	return verifiedLegacyMetaAccountRefWithLock(db, supplied, false)
}

func verifiedLegacyMetaAccountRefWithLock(
	db *gorm.DB,
	supplied LegacyMetaAccountRef,
	lock bool,
) (LegacyMetaAccountRef, error) {
	var persisted LegacyMetaAccountRef
	query := db.Table("whatsapp_accounts")
	if lock {
		query = query.Clauses(clause.Locking{Strength: "SHARE"})
	}
	result := query.
		Select("id, organization_id, name, status, phone_id, business_id").
		Where(
			"id = ? AND organization_id = ? AND deleted_at IS NULL",
			supplied.ID,
			supplied.OrganizationID,
		).
		Limit(1).
		Scan(&persisted)
	if result.Error != nil {
		return persisted, fmt.Errorf("verify legacy Meta account: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return persisted, fmt.Errorf("verify legacy Meta account: %w", gorm.ErrRecordNotFound)
	}
	if persisted.Name != supplied.Name {
		return persisted, fmt.Errorf(
			"%w: supplied legacy account name does not match its database record",
			ErrLegacyMetaBridgeConflict,
		)
	}
	return persisted, nil
}

// ensureLegacyMetaAccount is the mirror and backfill entry point. Its caller
// does not own the policy fence, so it acquires the organization and shadow
// with lockLegacyMetaOrganizationThenShadow.
func ensureLegacyMetaAccount(
	db *gorm.DB,
	ref LegacyMetaAccountRef,
) (*models.ChannelAccount, error) {
	return ensureLegacyMetaAccountWithin(db, ref, false)
}

func ensureLegacyMetaAccountWithin(
	db *gorm.DB,
	ref LegacyMetaAccountRef,
	policyFenced bool,
) (*models.ChannelAccount, error) {
	externalID := legacyMetaIDPrefix + "account:" + ref.ID.String()
	now := time.Now().UTC()
	status := models.ChannelAccountStatusSuspended
	if strings.EqualFold(strings.TrimSpace(ref.Status), "active") {
		status = models.ChannelAccountStatusActive
	}
	account := models.ChannelAccount{
		OrganizationID:    ref.OrganizationID,
		Channel:           models.ChannelWhatsApp,
		Provider:          LegacyMetaProvider,
		Name:              legacyMetaAccountName(ref.Name, ref.ID),
		ExternalAccountID: externalID,
		Status:            status,
		Capabilities: models.JSONB{
			"text":           true,
			"media":          true,
			"replies":        true,
			"templates":      true,
			"service_window": true,
			"read_receipts":  true,
			// Preserve the original bridge aliases for older clients while
			// exposing the provider-neutral capability names additively.
			"template":    true,
			"mark_read":   true,
			"attachments": true,
		},
		Config: models.JSONB{
			"legacy_read_only": true,
			"outbound_enabled": false,
			"reply_route":      "chat",
		},
		Metadata: models.JSONB{
			"legacy_account_id":   ref.ID.String(),
			"legacy_account_name": ref.Name,
		},
		ConnectedAt: &now,
	}
	// Active-only uniqueness permits another insert beside a soft-deleted
	// shadow. Resolve retained identity first so revival never manufactures a
	// competing row, and never choose arbitrarily among historical bindings.
	shadowRows := func(tx *gorm.DB, lockOptions string) *gorm.DB {
		return tx.Unscoped().
			Clauses(clause.Locking{Strength: "UPDATE", Options: lockOptions}).
			Where(
				"organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
				ref.OrganizationID,
				models.ChannelWhatsApp,
				LegacyMetaProvider,
				externalID,
			).
			Order("id").Limit(2)
	}
	if policyFenced {
		if err := reenterLegacyMetaPolicyFenceOrganization(db, ref.OrganizationID); err != nil {
			return nil, err
		}
	} else if err := lockLegacyMetaOrganizationThenShadow(db, ref.OrganizationID, func(tx *gorm.DB, wait legacyMetaRowWait) error {
		var candidates []models.ChannelAccount
		if err := shadowRows(tx, wait.lockOptions()).Find(&candidates).Error; err != nil {
			return fmt.Errorf("load legacy Meta channel account: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	lockShadow := func() (models.ChannelAccount, bool, error) {
		var candidates []models.ChannelAccount
		if err := shadowRows(db, "").Find(&candidates).Error; err != nil {
			return models.ChannelAccount{}, false, fmt.Errorf("load legacy Meta channel account: %w", err)
		}
		if len(candidates) > 1 {
			return models.ChannelAccount{}, false, fmt.Errorf(
				"%w: shadow account binding is ambiguous", ErrLegacyMetaBridgeConflict,
			)
		}
		if len(candidates) == 0 {
			return models.ChannelAccount{}, false, nil
		}
		return candidates[0], true, nil
	}
	persisted, found, err := lockShadow()
	if err != nil {
		return nil, err
	}
	if !found {
		// Concurrent first mirrors may both observe absence. Keep the existing
		// unique-index arbitration, then lock the one committed winner.
		if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&account).Error; err != nil {
			return nil, fmt.Errorf("create legacy Meta channel account: %w", err)
		}
		persisted, found, err = lockShadow()
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf(
				"%w: shadow account name is already in use",
				ErrLegacyMetaBridgeConflict,
			)
		}
	}
	// Revalidate the established account only after owning the shadow lock.
	// Account renames take this same ChannelAccount -> WhatsAppAccount order;
	// without the second check, a mirror that read the old name just before a
	// rename could wait here and then overwrite the newly committed metadata.
	verifiedRef, err := verifiedLegacyMetaAccountRefWithLock(db, ref, true)
	if err != nil {
		return nil, err
	}
	ref = verifiedRef
	// Refresh mutable shadow details only after its immutable binding agrees
	// with the established account. A refresh must not bless a corrupt bridge.
	boundAccountID, bindingErr := LegacyMetaWhatsAppAccountID(&persisted)
	if bindingErr != nil || boundAccountID != ref.ID {
		return nil, fmt.Errorf(
			"%w: shadow account binding changed before refresh",
			ErrLegacyMetaBridgeConflict,
		)
	}
	status = models.ChannelAccountStatusSuspended
	if strings.EqualFold(strings.TrimSpace(ref.Status), "active") {
		status = models.ChannelAccountStatusActive
	}
	account.Name = legacyMetaAccountName(ref.Name, ref.ID)
	account.Status = status
	account.Metadata["legacy_account_name"] = ref.Name
	native := &models.WhatsAppAccount{
		BaseModel: models.BaseModel{ID: ref.ID}, OrganizationID: ref.OrganizationID,
		PhoneID: ref.PhoneID, BusinessID: ref.BusinessID, Status: ref.Status,
	}
	if _, allowed := LegacyMetaAIBookingAuthority(&persisted, native); allowed {
		// Preserve only the four reserved, verified booking fields. Arbitrary
		// config, credentials and outbound approval are never mirrored back.
		for _, key := range []string{models.ChannelConfigAIBookingEnabled,
			models.ChannelConfigAIBookingRevision, models.ChannelConfigAIBookingEnabledAt,
			models.ChannelConfigAIBookingRouteBinding} {
			account.Config[key] = persisted.Config[key]
		}
	} else {
		account.Config[models.ChannelConfigAIBookingEnabled] = false
		// Keep an existing disabled generation stable across ordinary mirrors.
		// A revoked live grant/revival/route change must never resurrect it.
		if enabled, _ := persisted.Config[models.ChannelConfigAIBookingEnabled].(bool); enabled {
			account.Config[models.ChannelConfigAIBookingRevision] = uuid.NewString()
		} else if revision, ok := persisted.Config[models.ChannelConfigAIBookingRevision].(string); ok {
			if id, err := uuid.Parse(revision); err == nil && id != uuid.Nil && id.String() == revision {
				account.Config[models.ChannelConfigAIBookingRevision] = revision
			}
		}
	}
	if err := db.Unscoped().Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", persisted.ID, ref.OrganizationID).
		Updates(map[string]any{
			"deleted_at":   nil,
			"status":       status,
			"capabilities": account.Capabilities,
			"config":       account.Config,
			"metadata":     account.Metadata,
		}).Error; err != nil {
		return nil, fmt.Errorf("refresh legacy Meta channel account: %w", err)
	}
	persisted.Status = status
	persisted.Capabilities = account.Capabilities
	persisted.Config = account.Config
	persisted.Metadata = account.Metadata
	persisted.DeletedAt = gorm.DeletedAt{}
	return &persisted, nil
}

func ensureLegacyMetaIdentity(
	db *gorm.DB,
	account *models.ChannelAccount,
	contact *models.Contact,
	seenAt time.Time,
) (*models.ContactIdentity, error) {
	if seenAt.IsZero() {
		seenAt = time.Now().UTC()
	}
	externalID := legacyMetaIDPrefix + "contact:" + contact.ID.String()
	identity := models.ContactIdentity{
		OrganizationID:    account.OrganizationID,
		ContactID:         contact.ID,
		ChannelAccountID:  account.ID,
		Channel:           models.ChannelWhatsApp,
		ExternalID:        externalID,
		Address:           contact.PhoneNumber,
		NormalizedAddress: normalizeLegacyPhone(contact.PhoneNumber),
		DisplayName:       contact.ProfileName,
		IsPrimary:         true,
		IsVerified:        true,
		FirstSeenAt:       &seenAt,
		LastSeenAt:        &seenAt,
		Metadata:          models.JSONB{"legacy_contact_id": contact.ID.String()},
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&identity).Error; err != nil {
		return nil, fmt.Errorf("create legacy Meta contact identity: %w", err)
	}

	var persisted models.ContactIdentity
	if err := db.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND external_id = ?",
			account.OrganizationID,
			account.ID,
			externalID,
		).
		First(&persisted).Error; err != nil {
		return nil, fmt.Errorf("load legacy Meta contact identity: %w", err)
	}
	if persisted.ContactID != contact.ID {
		return nil, fmt.Errorf(
			"%w: shadow identity belongs to another contact",
			ErrLegacyMetaBridgeConflict,
		)
	}
	lastSeen := seenAt
	if persisted.LastSeenAt != nil && persisted.LastSeenAt.After(lastSeen) {
		lastSeen = *persisted.LastSeenAt
	}
	if err := db.Unscoped().Model(&models.ContactIdentity{}).
		Where("id = ? AND organization_id = ?", persisted.ID, account.OrganizationID).
		Updates(map[string]any{
			"deleted_at":         nil,
			"address":            contact.PhoneNumber,
			"normalized_address": normalizeLegacyPhone(contact.PhoneNumber),
			"display_name":       contact.ProfileName,
			"is_primary":         true,
			"is_verified":        true,
			"last_seen_at":       lastSeen,
		}).Error; err != nil {
		return nil, fmt.Errorf("refresh legacy Meta contact identity: %w", err)
	}
	persisted.Address = contact.PhoneNumber
	persisted.NormalizedAddress = normalizeLegacyPhone(contact.PhoneNumber)
	persisted.DisplayName = contact.ProfileName
	persisted.LastSeenAt = &lastSeen
	persisted.DeletedAt = gorm.DeletedAt{}
	return &persisted, nil
}

func ensureLegacyMetaConversation(
	db *gorm.DB,
	account *models.ChannelAccount,
	identity *models.ContactIdentity,
	contact *models.Contact,
	openedAt time.Time,
) (*models.InboxConversation, error) {
	if openedAt.IsZero() {
		openedAt = time.Now().UTC()
	}
	externalID := legacyMetaIDPrefix + "contact:" + contact.ID.String()
	conversation := models.InboxConversation{
		OrganizationID:         account.OrganizationID,
		ChannelAccountID:       account.ID,
		ContactID:              contact.ID,
		ContactIdentityID:      &identity.ID,
		Channel:                models.ChannelWhatsApp,
		ExternalConversationID: externalID,
		Status:                 models.InboxConversationStatusOpen,
		AssignedUserID:         contact.AssignedUserID,
		OpenedAt:               openedAt,
		Config: models.JSONB{
			"legacy_read_only": true,
			"reply_route":      "chat",
		},
		Metadata: models.JSONB{"legacy_contact_id": contact.ID.String()},
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&conversation).Error; err != nil {
		return nil, fmt.Errorf("create legacy Meta inbox conversation: %w", err)
	}

	var persisted models.InboxConversation
	if err := db.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND external_conversation_id = ?",
			account.OrganizationID,
			account.ID,
			externalID,
		).
		First(&persisted).Error; err != nil {
		return nil, fmt.Errorf("load legacy Meta inbox conversation: %w", err)
	}
	if persisted.ContactID != contact.ID {
		return nil, fmt.Errorf(
			"%w: shadow conversation belongs to another contact",
			ErrLegacyMetaBridgeConflict,
		)
	}
	if err := db.Unscoped().Model(&models.InboxConversation{}).
		Where("id = ? AND organization_id = ?", persisted.ID, account.OrganizationID).
		Updates(map[string]any{
			"deleted_at":          nil,
			"contact_identity_id": identity.ID,
			"assigned_user_id":    contact.AssignedUserID,
			"config": models.JSONB{
				"legacy_read_only": true,
				"reply_route":      "chat",
			},
		}).Error; err != nil {
		return nil, fmt.Errorf("refresh legacy Meta inbox conversation: %w", err)
	}
	persisted.ContactIdentityID = &identity.ID
	persisted.AssignedUserID = contact.AssignedUserID
	persisted.Config = models.JSONB{
		"legacy_read_only": true,
		"reply_route":      "chat",
	}
	persisted.DeletedAt = gorm.DeletedAt{}
	return &persisted, nil
}

func ensureLegacyMetaParticipant(
	db *gorm.DB,
	conversation *models.InboxConversation,
	identity *models.ContactIdentity,
	contact *models.Contact,
) error {
	participantKey := legacyMetaIDPrefix + "contact:" + contact.ID.String()
	participant := models.ConversationParticipant{
		OrganizationID:    conversation.OrganizationID,
		ConversationID:    conversation.ID,
		ParticipantKey:    participantKey,
		Role:              models.ConversationParticipantRoleCustomer,
		ContactIdentityID: &identity.ID,
		ExternalID:        identity.ExternalID,
		DisplayName:       contact.ProfileName,
		Address:           contact.PhoneNumber,
		JoinedAt:          conversation.OpenedAt,
		Metadata:          models.JSONB{"legacy_contact_id": contact.ID.String()},
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&participant).Error; err != nil {
		return fmt.Errorf("create legacy Meta conversation participant: %w", err)
	}
	return db.Unscoped().Model(&models.ConversationParticipant{}).
		Where(
			"organization_id = ? AND conversation_id = ? AND participant_key = ?",
			conversation.OrganizationID,
			conversation.ID,
			participantKey,
		).
		Updates(map[string]any{
			"deleted_at":          nil,
			"contact_identity_id": identity.ID,
			"external_id":         identity.ExternalID,
			"display_name":        contact.ProfileName,
			"address":             contact.PhoneNumber,
			"left_at":             nil,
		}).Error
}

func refreshLegacyMetaConversation(
	db *gorm.DB,
	conversation *models.InboxConversation,
	account *models.ChannelAccount,
	contact *models.Contact,
	message *models.Message,
) error {
	activityAt := message.EffectiveIngestedAt()
	if activityAt.IsZero() {
		activityAt = time.Now().UTC()
	}
	providerAt := message.CreatedAt.UTC()
	if providerAt.IsZero() {
		providerAt = activityAt
	}
	updates := map[string]any{
		"assigned_user_id": contact.AssignedUserID,
		"unread_count":     legacyUnreadCount(contact),
	}
	if conversation.LastMessageAt == nil || !conversation.LastMessageAt.After(activityAt) {
		updates["last_message_at"] = activityAt
		updates["last_message_preview"] = legacyMessagePreview(message)
	}
	if message.Direction == models.DirectionIncoming &&
		(conversation.LastInboundAt == nil || !conversation.LastInboundAt.After(activityAt)) {
		updates["last_inbound_at"] = activityAt
	}
	if message.Direction == models.DirectionIncoming {
		serviceWindowEnd := providerAt.Add(24 * time.Hour)
		if conversation.ServiceWindowEndsAt == nil ||
			conversation.ServiceWindowEndsAt.Before(serviceWindowEnd) {
			updates["service_window_ends_at"] = serviceWindowEnd
		}
	}
	if message.Direction == models.DirectionOutgoing &&
		(conversation.LastOutboundAt == nil || !conversation.LastOutboundAt.After(activityAt)) {
		updates["last_outbound_at"] = activityAt
	}
	if err := db.Model(&models.InboxConversation{}).
		Where("id = ? AND organization_id = ?", conversation.ID, conversation.OrganizationID).
		Updates(updates).Error; err != nil {
		return fmt.Errorf("refresh legacy Meta inbox conversation activity: %w", err)
	}

	accountUpdates := map[string]any{}
	if message.Direction == models.DirectionIncoming &&
		(account.LastInboundAt == nil || !account.LastInboundAt.After(activityAt)) {
		accountUpdates["last_inbound_at"] = activityAt
	}
	if message.Direction == models.DirectionOutgoing &&
		(account.LastOutboundAt == nil || !account.LastOutboundAt.After(activityAt)) {
		accountUpdates["last_outbound_at"] = activityAt
	}
	if len(accountUpdates) == 0 {
		return nil
	}
	if err := db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, account.OrganizationID).
		Updates(accountUpdates).Error; err != nil {
		return fmt.Errorf("refresh legacy Meta channel account activity: %w", err)
	}
	return nil
}

func legacyMetaAccountName(name string, accountID uuid.UUID) string {
	const maxRunes = 100
	prefix := "WhatsApp "
	suffix := " [" + accountID.String() + "]"
	name = strings.TrimSpace(name)
	available := maxRunes - utf8.RuneCountInString(prefix) - utf8.RuneCountInString(suffix)
	if available < 0 {
		available = 0
	}
	runes := []rune(name)
	if len(runes) > available {
		runes = runes[:available]
	}
	return prefix + string(runes) + suffix
}

func normalizeLegacyPhone(value string) string {
	value = strings.TrimSpace(value)
	var normalized strings.Builder
	normalized.Grow(len(value))
	for _, char := range value {
		if char >= '0' && char <= '9' {
			normalized.WriteRune(char)
		}
	}
	return normalized.String()
}

func legacyUnreadCount(contact *models.Contact) int {
	if contact.IsRead {
		return 0
	}
	return 1
}

func legacyMessagePreview(message *models.Message) string {
	if content := strings.TrimSpace(message.Content); content != "" {
		runes := []rune(content)
		if len(runes) > 100 {
			return string(runes[:97]) + "..."
		}
		return content
	}
	if message.MessageType != "" {
		return "[" + string(message.MessageType) + "]"
	}
	return "[Message]"
}
