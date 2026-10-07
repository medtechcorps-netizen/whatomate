package channel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
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

// StageLegacyMetaWhatsAppAccountRename refreshes the mutable display-name
// projection of an established WhatsApp account. Callers must invoke it first
// in the transaction that updates the whatsapp_accounts row and then writes
// the update's audit row, before taking that account row lock. It takes the
// bridge-wide lock order (Organization -> ChannelAccount -> WhatsAppAccount)
// used by strict replies, and the organization and the account row even when
// the name is unchanged and there is no shadow to refresh. The organization
// comes first because the audit row's INSERT locks it (the platform-compliance
// write guard takes it FOR SHARE) while the transaction owns the account row
// and the shadow, which a Coexistence admission locks after its policy fence.
// The account and shadow UPDATEs themselves do not lock it: the guard fires
// only for an UPDATE that sets organization_id, which they never do (keep it
// out of their SET lists), and their organization foreign keys are unchanged
// and checked again only when a row is updated twice in one transaction. The
// shadow and the account row are taken with
// lockLegacyMetaOrganizationThenShadow, so an update queued on either (sends
// hold the account row FOR SHARE across their Meta call) never holds the
// organization, except in that helper's bounded fallback after
// lockLegacyMetaShadowRounds lost rounds.
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
	if err := lockLegacyMetaOrganizationThenShadow(db, organizationID, func(tx *gorm.DB, nowait bool) error {
		lockOptions := legacyMetaNowaitOption(nowait)
		var shadows []models.ChannelAccount
		if err := shadowRows(tx, lockOptions).Find(&shadows).Error; err != nil {
			return fmt.Errorf("lock legacy Meta shadow for account rename: %w", err)
		}
		return lockLegacyMetaWhatsAppAccount(tx, organizationID, accountID, lockOptions)
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

// LockLegacyMetaOrganizationAndWhatsAppAccount takes the organization row FOR
// SHARE behind the policy fence queue and then the whatsapp_accounts row FOR
// UPDATE, never waiting for the row while it holds the organization (sends
// hold it FOR SHARE across their Meta call), except in the bounded fallback of
// lockLegacyMetaOrganizationThenShadow. A transaction that updates the account
// row and then writes an audit row must take both first: with the
// platform-compliance write guard the audit INSERT locks the organization FOR
// SHARE, after the row, and a Coexistence admission locks the row only after
// its policy fence owns the organization FOR UPDATE.
func LockLegacyMetaOrganizationAndWhatsAppAccount(db *gorm.DB, organizationID, accountID uuid.UUID) error {
	if db == nil || organizationID == uuid.Nil || accountID == uuid.Nil {
		return errors.New("legacy Meta account lock scope is required")
	}
	return lockLegacyMetaOrganizationThenShadow(db, organizationID, func(tx *gorm.DB, nowait bool) error {
		return lockLegacyMetaWhatsAppAccount(tx, organizationID, accountID, legacyMetaNowaitOption(nowait))
	})
}

func lockLegacyMetaWhatsAppAccount(db *gorm.DB, organizationID, accountID uuid.UUID, lockOptions string) error {
	var accounts []models.WhatsAppAccount
	if err := db.Clauses(clause.Locking{Strength: "UPDATE", Options: lockOptions}).
		Select("id").
		Where("id = ? AND organization_id = ?", accountID, organizationID).
		Find(&accounts).Error; err != nil {
		return fmt.Errorf("lock WhatsApp account: %w", err)
	}
	return nil
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

// lockLegacyMetaShadowRounds bounds the savepointed rounds of
// lockLegacyMetaOrganizationThenShadow. A round is lost only to a policy fence,
// an organization update or another shadow owner that made progress, so the
// bound is reached only under sustained traffic on one tenant; the caller then
// waits in plain order instead of failing.
const lockLegacyMetaShadowRounds = 64

var errLegacyMetaPolicyFenceQueued = errors.New("legacy Meta policy fence is queued")

// legacyMetaPolicyFenceQueueWait bounds how long one lock acquisition queues
// for the policy fence key behind a fence that waits for the key, or holds it
// and is not blocked, summed over all its rounds. Fences take the key in FIFO
// order and normally own it and the organization row within milliseconds, so
// the queue serves them before sharers that arrive later on any account; the
// bound caps what each acquisition pays while admissions keep arriving. Past it
// the acquisition takes the organization row without the key ("bypasses"),
// which a waiting FOR UPDATE does not hold back.
//
// Two states of the fence that holds the key change that
// (legacyMetaPolicyFenceQueueStateSQL):
//   - Stuck: it waits for a running backend outside the queue, such as an AI
//     attempt fence that holds the organization FOR KEY SHARE while its
//     goroutine waits for this mirror or recovery on another connection, a
//     cycle PostgreSQL cannot detect. Sharers bypass at once.
//   - Blocked only by members, running transactions that hold the fence key
//     or went past the queue: those finish within milliseconds and the fence
//     then gets the row, unless newcomers keep bypassing, because each
//     bypassing FOR SHARE overtakes the waiting FOR UPDATE and enough
//     overlapping bypassers keep it out for as long as they overlap. Sharers
//     queue behind such a fence up to legacyMetaPolicyFenceMemberWait instead,
//     a bound only for a cycle through a member's own goroutine.
var legacyMetaPolicyFenceQueueWait = 250 * time.Millisecond

// legacyMetaPolicyFenceMemberWait bounds the queue behind a fence that holds
// the key and is blocked only by members (see legacyMetaPolicyFenceQueueWait).
var legacyMetaPolicyFenceMemberWait = time.Second

const (
	legacyMetaPolicyFencePollInterval = 5 * time.Millisecond
	// legacyMetaPolicyFenceStateCheckInterval spaces a poller's checks of the
	// fence state; the first one runs at its first failed try.
	legacyMetaPolicyFenceStateCheckInterval = 25 * time.Millisecond

	legacyMetaPolicyFenceBypassNamespace = "rereply:legacy_meta_policy_fence_bypass:v1:"
	// legacyMetaPolicyFenceBypassSetting holds, transaction-locally, the
	// organization this transaction went past the queue for.
	legacyMetaPolicyFenceBypassSetting = "rereply.legacy_meta_fence_bypass"
)

// legacyMetaPolicyFenceStateTTL is how long a process reuses one organization's
// fence state. The check reads the whole lock table and takes every
// lock-manager partition lock, so waiting sharers of one tenant share it.
var legacyMetaPolicyFenceStateTTL = 25 * time.Millisecond

// legacyMetaPolicyFenceBypassKey is held in shared mode by a transaction that
// took the organization without the policy fence key. No one takes it
// exclusively, so taking it never waits. It tells fences waiting behind such a
// transaction apart from locks outside the queue.
func legacyMetaPolicyFenceBypassKey(organizationID uuid.UUID) string {
	return legacyMetaPolicyFenceBypassNamespace + organizationID.String()
}

// legacyMetaPolicyFenceJoin is how an acquisition passed the policy fence queue.
type legacyMetaPolicyFenceJoin int

const (
	// legacyMetaPolicyFenceQueued: a fence holds or waits for the key.
	legacyMetaPolicyFenceQueued legacyMetaPolicyFenceJoin = iota
	// legacyMetaPolicyFenceJoined: the key is held in shared mode.
	legacyMetaPolicyFenceJoined
	// legacyMetaPolicyFenceBypassed: past the queue without the key; the
	// bypass key is held.
	legacyMetaPolicyFenceBypassed
)

// legacyMetaPolicyFenceState is what holds up the fence that holds the key
// (legacyMetaPolicyFenceQueueStateSQL).
type legacyMetaPolicyFenceState int

const (
	// legacyMetaPolicyFenceWaiting: no fence holds the key yet, the holder is
	// not blocked, or its chain of blockers is too long to tell.
	legacyMetaPolicyFenceWaiting legacyMetaPolicyFenceState = iota
	// legacyMetaPolicyFenceMemberBlocked: the holder waits, directly or
	// through other waiting backends, only for running members.
	legacyMetaPolicyFenceMemberBlocked
	// legacyMetaPolicyFenceStuck: the holder waits, directly or through other
	// waiting backends, for a running backend that is not a member.
	legacyMetaPolicyFenceStuck
)

// legacyMetaPolicyFenceDeadlines are one acquisition's queue limits, both
// counted from its start.
type legacyMetaPolicyFenceDeadlines struct {
	queue, member time.Time
}

func newLegacyMetaPolicyFenceDeadlines() legacyMetaPolicyFenceDeadlines {
	start := time.Now()
	return legacyMetaPolicyFenceDeadlines{
		queue:  start.Add(legacyMetaPolicyFenceQueueWait),
		member: start.Add(max(legacyMetaPolicyFenceQueueWait, legacyMetaPolicyFenceMemberWait)),
	}
}

func (deadlines legacyMetaPolicyFenceDeadlines) limit(state legacyMetaPolicyFenceState) time.Time {
	if state == legacyMetaPolicyFenceMemberBlocked {
		return deadlines.member
	}
	return deadlines.queue
}

// legacyMetaPolicyFenceQueueStateSQL classifies what holds up the fences of
// the organization: the one that holds the key exclusively (at most one does)
// and those that wait for it. It follows each chain of blockers, up to four
// deep, through every backend that is itself waiting for a lock, and
// classifies the backends at the ends of the chains:
//   - any running backend that is not a member makes the fences stuck: an AI
//     attempt fence idle in its transaction while its goroutine waits for
//     this mirror (the only shape a cycle through Go can take), this
//     transaction itself, or a send that holds a contact across its Meta call
//     while a member (which joined the key or bypassed) waits for that
//     contact. Queueing behind those would buy the fences nothing.
//   - a running member (one that holds the fence key or the bypass key and
//     waits for no lock) finishes within milliseconds. When the key holder's
//     chains end only at those, it is blocked only by members. A fence that
//     waits for the key behind them counts as waiting.
//
// So an organization writer queued between the key holder and a stream of
// sharers (organization settings take the row FOR UPDATE without the key)
// only passes the wait on to those sharers. pg_locks and pg_blocking_pids read
// the whole lock table and take every lock-manager partition lock, so
// legacyMetaPolicyFenceStateOf shares one result per organization and process.
const legacyMetaPolicyFenceQueueStateSQL = `WITH RECURSIVE keys AS (
	SELECT pg_catalog.hashtextextended(?, 0) AS fence,
	       pg_catalog.hashtextextended(?, 0) AS bypass,
	       (SELECT d.oid FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database()) AS database
), locks AS MATERIALIZED (
	SELECT l.pid, l.locktype, l.mode, l.granted, l.database, l.objsubid,
	       (l.classid::int8 << 32) | l.objid::int8 AS key
	FROM pg_catalog.pg_locks l
), members AS MATERIALIZED (
	SELECT DISTINCT locks.pid
	FROM locks CROSS JOIN keys
	WHERE locks.locktype = 'advisory' AND locks.objsubid = 1 AND locks.database = keys.database
	  AND locks.key IN (keys.fence, keys.bypass)
), lock_waiters AS MATERIALIZED (
	SELECT DISTINCT locks.pid FROM locks WHERE NOT locks.granted
), blockers(pid, depth, holder) AS (
	SELECT blocker.pid, 1, fence.granted
	FROM locks fence
	CROSS JOIN keys
	CROSS JOIN LATERAL pg_catalog.unnest(pg_catalog.pg_blocking_pids(fence.pid)) AS blocker(pid)
	WHERE fence.locktype = 'advisory' AND fence.objsubid = 1 AND fence.database = keys.database
	  AND fence.key = keys.fence AND fence.mode = 'ExclusiveLock'
	UNION
	SELECT next.pid, blockers.depth + 1, blockers.holder
	FROM blockers
	CROSS JOIN LATERAL pg_catalog.unnest(pg_catalog.pg_blocking_pids(blockers.pid)) AS next(pid)
	WHERE blockers.depth < 4
	  AND blockers.pid IN (SELECT lock_waiters.pid FROM lock_waiters)
), running AS (
	SELECT blockers.pid, blockers.holder, blockers.pid IN (SELECT members.pid FROM members) AS member
	FROM blockers
	WHERE blockers.pid NOT IN (SELECT lock_waiters.pid FROM lock_waiters)
)
SELECT CASE
	WHEN EXISTS (SELECT 1 FROM running WHERE NOT running.member) THEN 2
	WHEN EXISTS (SELECT 1 FROM running WHERE running.member AND running.holder) THEN 1
	ELSE 0
END`

type legacyMetaPolicyFenceStateSample struct {
	state legacyMetaPolicyFenceState
	at    time.Time
}

// legacyMetaPolicyFenceStateCache is one organization's last fence state in
// this process. refresh admits one query at a time; other callers meanwhile
// use the last sample.
type legacyMetaPolicyFenceStateCache struct {
	refresh sync.Mutex
	sample  atomic.Pointer[legacyMetaPolicyFenceStateSample]
}

func (cache *legacyMetaPolicyFenceStateCache) current() (legacyMetaPolicyFenceState, bool) {
	sample := cache.sample.Load()
	if sample == nil {
		return legacyMetaPolicyFenceWaiting, false
	}
	return sample.state, time.Since(sample.at) < legacyMetaPolicyFenceStateTTL
}

var (
	// legacyMetaPolicyFenceStates maps an organization to its
	// *legacyMetaPolicyFenceStateCache.
	legacyMetaPolicyFenceStates sync.Map
	// legacyMetaPolicyFenceStateQueries counts state queries, for tests.
	legacyMetaPolicyFenceStateQueries atomic.Int64
)

// legacyMetaPolicyFenceStateOf returns the organization's fence state. A sample
// younger than legacyMetaPolicyFenceStateTTL is reused; otherwise one caller
// per organization and process queries the lock table while the others use
// the last sample. The query fails safe: an error counts as waiting, the short
// bound, and is cached like any other result.
func legacyMetaPolicyFenceStateOf(db *gorm.DB, organizationID uuid.UUID) legacyMetaPolicyFenceState {
	value, _ := legacyMetaPolicyFenceStates.LoadOrStore(organizationID, &legacyMetaPolicyFenceStateCache{})
	cache := value.(*legacyMetaPolicyFenceStateCache)
	if state, fresh := cache.current(); fresh {
		return state
	}
	if !cache.refresh.TryLock() {
		state, _ := cache.current()
		return state
	}
	defer cache.refresh.Unlock()
	if state, fresh := cache.current(); fresh {
		return state
	}
	legacyMetaPolicyFenceStateQueries.Add(1)
	state := legacyMetaPolicyFenceWaiting
	if err := legacyMetaSavepoint(db, "legacy_meta_fence_state", func(tx *gorm.DB) error {
		return tx.Raw(
			legacyMetaPolicyFenceQueueStateSQL,
			database.WhatsAppIdentityReviewContactSelectorFenceKey(organizationID),
			legacyMetaPolicyFenceBypassKey(organizationID),
		).Scan(&state).Error
	}); err != nil {
		state = legacyMetaPolicyFenceWaiting
	}
	cache.sample.Store(&legacyMetaPolicyFenceStateSample{state: state, at: time.Now()})
	return state
}

// legacyMetaSavepoint runs fn in a savepoint that it always releases, so a lost
// round or a fence check leaves no nesting level behind. GORM's nested
// Transaction never releases its savepoints, and once the transaction takes a
// row lock every open level becomes a subtransaction with its own XID; past 64
// of them a backend's subtransaction cache overflows and every snapshot in the
// cluster pays for it. A rollback to the savepoint releases what fn locked. A
// caller that is not in a transaction gets one.
func legacyMetaSavepoint(db *gorm.DB, name string, fn func(tx *gorm.DB) error) error {
	if _, inTransaction := db.Statement.ConnPool.(gorm.TxCommitter); !inTransaction {
		return db.Transaction(fn)
	}
	if err := db.Exec("SAVEPOINT " + name).Error; err != nil {
		return err
	}
	if err := fn(db); err != nil {
		if rollbackErr := db.Exec("ROLLBACK TO SAVEPOINT " + name).Error; rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		if releaseErr := db.Exec("RELEASE SAVEPOINT " + name).Error; releaseErr != nil {
			return errors.Join(err, releaseErr)
		}
		return err
	}
	return db.Exec("RELEASE SAVEPOINT " + name).Error
}

// lockLegacyMetaOrganization takes the tenant organization row FOR SHARE. A
// bridge transaction that locks a legacy ChannelAccount shadow later writes
// organization-scoped rows that need this row anyway (the platform-compliance
// write guard takes FOR SHARE and foreign-key checks take FOR KEY SHARE), but
// acquiring it there, while already owning the shadow, inverts Coexistence
// admission's database.LockOrganizationPolicyScope -> ChannelAccount order and
// deadlocks the two. The row lock is compatible with other mirrors, attempt
// fences (FOR KEY SHARE) and implicit checks, and it does not queue behind a
// waiting FOR UPDATE; a caller that already owns the policy fence FOR UPDATE
// re-enters it without waiting.
func lockLegacyMetaOrganization(db *gorm.DB, organizationID uuid.UUID) error {
	return lockLegacyMetaOrganizationWith(db, organizationID, "")
}

func lockLegacyMetaOrganizationWith(db *gorm.DB, organizationID uuid.UUID, lockOptions string) error {
	var organization struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	result := db.Table("organizations").
		Clauses(clause.Locking{Strength: "SHARE", Options: lockOptions}).
		Select("id").
		Where("id = ?", organizationID).
		Limit(1).
		Scan(&organization)
	if result.Error != nil {
		return fmt.Errorf("lock legacy Meta organization: %w", result.Error)
	}
	if result.RowsAffected != 1 || organization.ID != organizationID {
		return fmt.Errorf("lock legacy Meta organization: %w", gorm.ErrRecordNotFound)
	}
	return nil
}

// joinLegacyMetaPolicyFence passes the policy fence queue. It takes the fence
// key in shared mode with try-locks. A try fails while a fence holds the key or
// waits for it, so a poller stays behind fences without joining PostgreSQL's
// lock queue, where it could not give up. With wait it polls until the limit
// that the fence's state sets (legacyMetaPolicyFenceDeadlines.limit); without
// it, it tries once and reports legacyMetaPolicyFenceQueued while that limit
// has not passed. Past the limit, or at once while the fence is stuck, it goes
// past the queue: it takes the bypass key and records the organization in the
// transaction-local legacyMetaPolicyFenceBypassSetting, so a later acquisition
// in the same transaction (re-entry) does not queue again. Both revert with a
// savepoint rollback, together with the organization lock they precede.
func joinLegacyMetaPolicyFence(
	db *gorm.DB,
	organizationID uuid.UUID,
	deadlines legacyMetaPolicyFenceDeadlines,
	wait bool,
) (legacyMetaPolicyFenceJoin, error) {
	key := database.WhatsAppIdentityReviewContactSelectorFenceKey(organizationID)
	organization := organizationID.String()
	ctx := db.Statement.Context
	if ctx == nil {
		ctx = context.Background()
	}
	state := legacyMetaPolicyFenceWaiting
	var nextStateCheck time.Time
	for {
		var outcome int
		if err := db.Raw(
			`SELECT CASE
				WHEN pg_catalog.current_setting(?, true) = ? THEN 2
				WHEN pg_catalog.pg_try_advisory_xact_lock_shared(pg_catalog.hashtextextended(?, 0)) THEN 1
				ELSE 0
			END`,
			legacyMetaPolicyFenceBypassSetting, organization, key,
		).Scan(&outcome).Error; err != nil {
			return legacyMetaPolicyFenceQueued, fmt.Errorf("join legacy Meta policy fence: %w", err)
		}
		switch outcome {
		case 2:
			return legacyMetaPolicyFenceBypassed, nil
		case 1:
			return legacyMetaPolicyFenceJoined, nil
		}
		now := time.Now()
		if !now.Before(nextStateCheck) {
			state = legacyMetaPolicyFenceStateOf(db, organizationID)
			nextStateCheck = now.Add(legacyMetaPolicyFenceStateCheckInterval)
		}
		limit := deadlines.limit(state)
		if state == legacyMetaPolicyFenceStuck || !now.Before(limit) {
			if err := db.Exec(
				"SELECT pg_catalog.pg_advisory_xact_lock_shared(pg_catalog.hashtextextended(?, 0)), pg_catalog.set_config(?, ?, true)",
				legacyMetaPolicyFenceBypassKey(organizationID), legacyMetaPolicyFenceBypassSetting, organization,
			).Error; err != nil {
				return legacyMetaPolicyFenceQueued, fmt.Errorf("bypass legacy Meta policy fence: %w", err)
			}
			return legacyMetaPolicyFenceBypassed, nil
		}
		if !wait {
			return legacyMetaPolicyFenceQueued, nil
		}
		timer := time.NewTimer(min(legacyMetaPolicyFencePollInterval, time.Until(limit)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return legacyMetaPolicyFenceQueued, fmt.Errorf("join legacy Meta policy fence: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// lockLegacyMetaOrganizationShare passes the tenant's policy fence queue
// (joinLegacyMetaPolicyFence) and then takes the organization row FOR SHARE.
// database.LockOrganizationPolicyScope takes the same advisory key exclusively
// before its FOR UPDATE, so a held or waiting fence holds back later sharers on
// every account. The row lock alone would not: a new FOR SHARE overtakes a
// waiting FOR UPDATE, and SHARE windows of mirrors on different accounts would
// overlap without end. The key wait ends at the deadlines' limit; after it the
// organization row is taken without the key. With nowait, a held or queued
// fence returns errLegacyMetaPolicyFenceQueued until that limit, and a
// conflicting row lock returns 55P03, instead of waiting.
func lockLegacyMetaOrganizationShare(
	db *gorm.DB,
	organizationID uuid.UUID,
	deadlines legacyMetaPolicyFenceDeadlines,
	nowait bool,
) error {
	join, err := joinLegacyMetaPolicyFence(db, organizationID, deadlines, !nowait)
	if err != nil {
		return err
	}
	if join == legacyMetaPolicyFenceQueued {
		return errLegacyMetaPolicyFenceQueued
	}
	return lockLegacyMetaOrganizationWith(db, organizationID, legacyMetaNowaitOption(nowait))
}

// lockLegacyMetaOrganizationThenShadow ends owning the policy fence key in
// shared mode (or, past the queue, the bypass key), the organization row (FOR
// SHARE) and the shadow (FOR UPDATE) without ever waiting for one of them while
// it holds another, outside the final fallback. Waiting for the shadow while
// holding the organization would let queued holders on a busy shadow (other
// mirrors, or a strict reply holding it across a Meta call) keep
// LockOrganizationPolicyScope out; waiting for the organization while owning
// the shadow is the admission deadlock. Each round runs in savepoints, so a
// failed attempt releases what it took:
//
//  1. pass the fence queue and wait for the organization, then take the
//     shadow NOWAIT (the usual case);
//  2. if the shadow is busy, wait for it holding nothing, then pass the fence
//     queue and take the organization NOWAIT, so the waiter that inherits the
//     shadow keeps it;
//  3. if a conflicting holder (a held or queued policy fence, or an
//     organization update) blocks step 2, give the shadow back and queue
//     behind it in the next round.
//
// Only a NOWAIT conflict loses a round; a lock_timeout on a blocking step is
// returned to the caller. After lockLegacyMetaShadowRounds lost rounds the
// caller passes the fence queue, takes the organization and then waits for the
// shadow in plain order. That cannot deadlock inside PostgreSQL: a fence that
// owns the organization FOR UPDATE never coexists with this SHARE, and one
// still waiting for it does not own the shadow yet. It may hold back the fence
// for one shadow owner.
//
// All waits for the fence key in one call share one set of deadlines, counted
// from the call's start, because the fence can in turn wait for a lock held by
// this caller's goroutine on another connection: legacyMetaPolicyFenceQueueWait
// in general, legacyMetaPolicyFenceMemberWait while the fence holds the key and
// is blocked only by members. Once the applicable one has passed, step 1 and
// the fallback try the key once and a queued fence no longer loses step 2 its
// round, so a call queues at most that long however many rounds a busy shadow
// costs it. Before it, step 2 gives the shadow back rather than bypass, so a
// waiter that inherits a shadow never overtakes a fence. A stuck fence is not
// queued for at all, and a transaction that already went past the queue does
// not queue again when it re-enters (a strict reply's pre-provider transaction
// mirrors after taking these locks). Every round's savepoint is released
// (legacyMetaSavepoint), so lost rounds do not deepen the transaction.
// lockShadow must lock the same shadow row(s) on every call. A caller that
// already owns the policy fence does not use this; it waits behind its own
// fence by design.
func lockLegacyMetaOrganizationThenShadow(
	db *gorm.DB,
	organizationID uuid.UUID,
	lockShadow func(tx *gorm.DB, nowait bool) error,
) error {
	deadlines := newLegacyMetaPolicyFenceDeadlines()
	for round := 0; round < lockLegacyMetaShadowRounds; round++ {
		shadowBusy := false
		err := legacyMetaSavepoint(db, "legacy_meta_round", func(tx *gorm.DB) error {
			if err := lockLegacyMetaOrganizationShare(tx, organizationID, deadlines, false); err != nil {
				return err
			}
			err := lockShadow(tx, true)
			shadowBusy = legacyMetaLockNotAvailable(err)
			return err
		})
		if err == nil || !shadowBusy {
			return err
		}
		organizationBusy := false
		err = legacyMetaSavepoint(db, "legacy_meta_round", func(tx *gorm.DB) error {
			if err := lockShadow(tx, false); err != nil {
				return err
			}
			err := lockLegacyMetaOrganizationShare(tx, organizationID, deadlines, true)
			organizationBusy = errors.Is(err, errLegacyMetaPolicyFenceQueued) ||
				legacyMetaLockNotAvailable(err)
			return err
		})
		if err == nil || !organizationBusy {
			return err
		}
	}
	if err := lockLegacyMetaOrganizationShare(db, organizationID, deadlines, false); err != nil {
		return err
	}
	return lockShadow(db, false)
}

// LockLegacyMetaOrganization passes the policy fence queue, with the same
// limits as lockLegacyMetaOrganizationThenShadow and not at all while the fence
// is stuck, and takes the tenant organization row FOR SHARE, before any other
// lock, for a transaction that owns no legacy shadow. Outgoing delivery
// recovery uses it for every non-AI WhatsApp send, classic accounts included;
// such a send may run under an AI attempt fence that a waiting policy fence is
// itself queued behind.
func LockLegacyMetaOrganization(db *gorm.DB, organizationID uuid.UUID) error {
	if db == nil || organizationID == uuid.Nil {
		return errors.New("legacy Meta organization lock scope is required")
	}
	return lockLegacyMetaOrganizationShare(db, organizationID, newLegacyMetaPolicyFenceDeadlines(), false)
}

// LockLegacyMetaOrganizationAndShadow takes the organization row FOR SHARE and
// the legacy shadow FOR UPDATE for a transaction that does not own the policy
// fence and will write organization-scoped rows while it owns the shadow, using
// lockLegacyMetaOrganizationThenShadow. It locks exactly the live row that a
// later id-scoped shadow lock in the same transaction re-enters.
func LockLegacyMetaOrganizationAndShadow(
	db *gorm.DB,
	organizationID, channelAccountID uuid.UUID,
) error {
	if db == nil || organizationID == uuid.Nil || channelAccountID == uuid.Nil {
		return errors.New("legacy Meta shadow lock scope is required")
	}
	return lockLegacyMetaOrganizationThenShadow(db, organizationID, func(tx *gorm.DB, nowait bool) error {
		var shadows []models.ChannelAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: legacyMetaNowaitOption(nowait)}).
			Select("id").
			Where("id = ? AND organization_id = ?", channelAccountID, organizationID).
			Find(&shadows).Error; err != nil {
			return fmt.Errorf("lock legacy Meta shadow: %w", err)
		}
		return nil
	})
}

func legacyMetaNowaitOption(nowait bool) string {
	if nowait {
		return "NOWAIT"
	}
	return ""
}

func legacyMetaLockNotAvailable(err error) bool {
	var sqlState interface {
		SQLState() string
	}
	return errors.As(err, &sqlState) && sqlState.SQLState() == "55P03"
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
		// Re-entering the caller's FOR UPDATE fence never waits.
		if err := lockLegacyMetaOrganization(db, ref.OrganizationID); err != nil {
			return nil, err
		}
	} else if err := lockLegacyMetaOrganizationThenShadow(db, ref.OrganizationID, func(tx *gorm.DB, nowait bool) error {
		var candidates []models.ChannelAccount
		if err := shadowRows(tx, legacyMetaNowaitOption(nowait)).Find(&candidates).Error; err != nil {
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
