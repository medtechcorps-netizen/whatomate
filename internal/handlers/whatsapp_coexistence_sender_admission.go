package handlers

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

// Coexistence sender admission without an identity question.
//
// EvaluateWhatsAppIdentityReviewAdmission fails closed for every Coexistence
// sender who is not already exactly one known contact. Two of those senders
// carry no identity question at all, yet were held contact-free in the
// protected staged queue where nobody could see them:
//
//   - new_sender: the authenticated direct BSUID, parent BSUID, phone (from and
//     contacts[].wa_id), username and their placeholder forms match no contact
//     row in the tenant (including soft-deleted and merged rows) and no earlier
//     identity-review hold with candidates. There is nobody to confuse the
//     sender with, so one contact is created and bound to that BSUID.
//   - phone_app_recipient: the only candidate is a phone-only contact that this
//     Coexistence number itself created by messaging that phone from the
//     WhatsApp Business app (a proven smb_message_echoes winner), with no
//     inbound message, no BSUID, no merge alias and no review history. The
//     reply authenticates the same phone with its BSUID, so the BSUID is bound
//     to that contact exactly as the echo path would have done had Meta
//     included the recipient BSUID in the echo.
//
// Everything else (any other candidate, ambiguous owners, parent conflicts,
// selector conflicts, a missing BSUID) keeps the existing review behaviour.
// The function runs inside the admission transaction, after the evaluator has
// taken the WAMID, organization policy/selector, account and onboarding-state
// locks, so no competing admission or contact selector write can interleave.
// It binds the contact inside a savepoint and then re-runs the evaluator: only
// a unique_direct_primary result for exactly the bound contact is accepted.
// Otherwise the savepoint is rolled back and the caller stages the message as
// before.

type coexistenceSenderAdmissionKind string

const (
	coexistenceSenderAdmissionNewSender         coexistenceSenderAdmissionKind = "new_sender"
	coexistenceSenderAdmissionPhoneAppRecipient coexistenceSenderAdmissionKind = "phone_app_recipient"
)

// Contact metadata recording why and from which WAMID a Coexistence contact
// was bound without review. It is audit evidence only; routing never reads it.
const (
	coexistenceIdentityAdmissionKey      = "coexistence_identity_admission"
	coexistenceIdentityAdmissionWAMIDKey = "coexistence_identity_admission_wamid"
	coexistenceIdentityAdmissionAtKey    = "coexistence_identity_admission_at"
)

// coexistenceContactBSUIDMaxLength mirrors models.Contact.BSUID (size:150). A
// longer value cannot be stored, so such a sender keeps the review path rather
// than failing every provider retry.
const coexistenceContactBSUIDMaxLength = 150

// Contact metadata keys that associate a contact with some WhatsApp user
// identity. A phone-only contact carrying any of them is not an unbound
// phone-app recipient.
var coexistenceContactIdentityMetadataKeys = []string{
	"coexistence_user_id",
	"coexistence_parent_user_id",
	"coexistence_conflicting_user_id",
	"coexistence_phone_conflict",
	"coexistence_reconciled_contact_id",
}

var errCoexistenceSenderAdmissionNotProven = errors.New("coexistence sender admission could not be proven")

type coexistenceSenderSelectors struct {
	userIDs      []string
	phones       []string
	placeholders []string
	username     string
}

// coexistenceSenderAdmissionSelectors widens the evaluator's candidate selectors
// (direct BSUID, parent BSUID, from) with every other authenticated value Meta
// delivered for this sender in the same signed body: contacts[].wa_id and
// contacts[].profile.username, plus the non-dialable placeholder phone numbers
// the Coexistence contact resolver derives from them.
func coexistenceSenderAdmissionSelectors(
	claim WhatsAppIdentityReviewClaim,
	message IncomingTextMessage,
) coexistenceSenderSelectors {
	selectors := coexistenceSenderSelectors{username: strings.TrimSpace(message.senderUsername)}
	seen := map[string]bool{}
	add := func(values *[]string, value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		*values = append(*values, value)
	}
	add(&selectors.userIDs, claim.DirectPrimaryBSUID)
	add(&selectors.userIDs, claim.ParentBSUID)
	add(&selectors.phones, normalizeIdentityReviewPhone(claim.Phone))
	add(&selectors.phones, normalizeIdentityReviewPhone(message.From))
	add(&selectors.phones, normalizeIdentityReviewPhone(message.senderWaID))
	for _, identity := range []coexistenceContactIdentity{
		{UserID: claim.DirectPrimaryBSUID},
		{ParentUserID: claim.ParentBSUID},
		{Username: selectors.username},
	} {
		if identity.primaryUserID() == "" && strings.TrimSpace(identity.Username) == "" {
			continue
		}
		add(&selectors.placeholders, coexistenceIdentityPlaceholder(identity))
	}
	return selectors
}

// isPlausibleWhatsAppPhone accepts only what can be stored as a dialable
// contact phone: 7 to 15 ASCII digits (E.164 without "+"). Meta may put a
// non-phone value in "from" during the BSUID migration; such a value must not
// become a contact phone.
func isPlausibleWhatsAppPhone(phone string) bool {
	if len(phone) < 7 || len(phone) > 15 {
		return false
	}
	for _, char := range phone {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// admitCoexistenceSenderWithoutIdentityQuestion returns the re-evaluated
// admission when it bound the sender to a contact, or nil when the existing
// review path applies. tx-scoped: a must be the admission transaction's App.
func (a *App) admitCoexistenceSenderWithoutIdentityQuestion(
	account *models.WhatsAppAccount,
	claim *WhatsAppIdentityReviewClaim,
	admission *WhatsAppIdentityReviewAdmission,
	message IncomingTextMessage,
	profileName string,
) (*WhatsAppIdentityReviewAdmission, error) {
	if a == nil || a.DB == nil || account == nil || claim == nil || admission == nil {
		return nil, errors.New("coexistence sender admission is incomplete")
	}
	normalized, err := normalizeWhatsAppIdentityReviewClaim(*claim)
	if err != nil {
		return nil, err
	}
	// Only "a direct BSUID that no contact owns" qualifies. A missing BSUID
	// (direct_primary_missing) is never bound: the evaluator blocks every
	// BSUID-less Coexistence message, even from a unique phone owner, so a
	// phone-keyed contact would admit this one message and hold every later one.
	if !admission.Blocked || !admission.NeedsReview || admission.Reason != "direct_primary_ambiguous" ||
		admission.DirectPrimaryOwnerID != nil || admission.LatestHoldID != nil || admission.RouteContactID != nil ||
		normalized.DirectPrimaryBSUID == "" || len(normalized.DirectPrimaryBSUID) > coexistenceContactBSUIDMaxLength ||
		account.OrganizationID != normalized.OrganizationID || account.ID != normalized.WhatsAppAccountID {
		return nil, nil
	}
	phone := normalizeCoexistencePhone(message.From)
	if phone != "" && (!isPlausibleWhatsAppPhone(phone) || phone != normalized.Phone) {
		return nil, nil
	}

	kind, recipientID, err := a.classifyCoexistenceSenderAdmission(account, normalized, admission.Candidates, message, phone)
	if err != nil || kind == "" {
		return nil, err
	}

	var admitted *WhatsAppIdentityReviewAdmission
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		nested := a.scopedApp(tx, account.OrganizationID)
		contact, created, contactErr := nested.getOrCreateCoexistenceContact(account, coexistenceContactIdentity{
			Phone:        phone,
			UserID:       normalized.DirectPrimaryBSUID,
			ParentUserID: normalized.ParentBSUID,
			Username:     strings.TrimSpace(message.senderUsername),
			ProfileName:  strings.TrimSpace(profileName),
			FallbackKey:  strings.TrimSpace(message.ID),
		})
		if contactErr != nil {
			return contactErr
		}
		switch kind {
		case coexistenceSenderAdmissionNewSender:
			if !created {
				return errCoexistenceSenderAdmissionNotProven
			}
		case coexistenceSenderAdmissionPhoneAppRecipient:
			if created || contact.ID != recipientID {
				return errCoexistenceSenderAdmissionNotProven
			}
		default:
			return errCoexistenceSenderAdmissionNotProven
		}
		if contact.BSUID != normalized.DirectPrimaryBSUID || contact.MergedIntoID != nil {
			return errCoexistenceSenderAdmissionNotProven
		}
		metadata := cloneMessageMetadata(contact.Metadata)
		metadata[coexistenceIdentityAdmissionKey] = string(kind)
		metadata[coexistenceIdentityAdmissionWAMIDKey] = strings.TrimSpace(message.ID)
		metadata[coexistenceIdentityAdmissionAtKey] = time.Now().UTC().Format(time.RFC3339)
		if updateErr := tx.Model(&models.Contact{}).Where(
			"organization_id = ? AND id = ?", account.OrganizationID, contact.ID,
		).Update("metadata", metadata).Error; updateErr != nil {
			return fmt.Errorf("record coexistence sender admission: %w", updateErr)
		}

		// The evaluator is the proof: the bound contact must now be the unique
		// direct-primary owner and the complete candidate set must be exactly it.
		reevaluated, evaluateErr := nested.EvaluateWhatsAppIdentityReviewAdmission(tx, claim)
		if evaluateErr != nil {
			return evaluateErr
		}
		if reevaluated.Blocked || reevaluated.NeedsReview || reevaluated.Reason != "unique_direct_primary" ||
			reevaluated.RouteContactID == nil || *reevaluated.RouteContactID != contact.ID ||
			len(reevaluated.Candidates) != 1 || reevaluated.Candidates[0].ContactID != contact.ID {
			return errCoexistenceSenderAdmissionNotProven
		}
		admitted = reevaluated
		a.adoptAfterCommit(nested)
		return nil
	})
	if errors.Is(err, errCoexistenceSenderAdmissionNotProven) {
		a.Log.Warn(
			"Coexistence sender admission was not proven; holding the message for identity review",
			"organization_id", account.OrganizationID,
			"account_id", account.ID,
			"admission", string(kind),
		)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("admit coexistence sender: %w", err)
	}
	return admitted, nil
}

// classifyCoexistenceSenderAdmission returns the admission kind, and the
// contact for phone_app_recipient, or "" when the review path applies.
func (a *App) classifyCoexistenceSenderAdmission(
	account *models.WhatsAppAccount,
	claim WhatsAppIdentityReviewClaim,
	candidates []WhatsAppIdentityReviewCandidate,
	message IncomingTextMessage,
	phone string,
) (coexistenceSenderAdmissionKind, uuid.UUID, error) {
	selectors := coexistenceSenderAdmissionSelectors(claim, message)
	matches, err := a.coexistenceSenderSelectorMatches(account.OrganizationID, selectors)
	if err != nil {
		return "", uuid.Nil, err
	}
	reviewed, err := a.coexistenceSenderHasReviewEvidence(account.OrganizationID, selectors)
	if err != nil || reviewed {
		return "", uuid.Nil, err
	}
	if len(candidates) == 0 {
		if len(matches) == 0 {
			return coexistenceSenderAdmissionNewSender, uuid.Nil, nil
		}
		return "", uuid.Nil, nil
	}
	if len(candidates) != 1 || phone == "" ||
		candidates[0].SelectorReasons != models.WhatsAppIdentityReviewSelectorPhone ||
		len(matches) != 1 || matches[0].ID != candidates[0].ContactID {
		return "", uuid.Nil, nil
	}
	proven, err := a.coexistencePhoneAppRecipientAwaitingFirstReply(account, &matches[0], phone, selectors.username)
	if err != nil || !proven {
		return "", uuid.Nil, err
	}
	return coexistenceSenderAdmissionPhoneAppRecipient, matches[0].ID, nil
}

// coexistenceSenderSelectorMatches returns up to two raw contact rows (deleted
// and merged rows included) that any widened selector names.
func (a *App) coexistenceSenderSelectorMatches(
	organizationID uuid.UUID,
	selectors coexistenceSenderSelectors,
) ([]models.Contact, error) {
	conditions := make([]string, 0, 7)
	args := make([]any, 0, 7)
	if len(selectors.userIDs) > 0 {
		for _, condition := range []string{
			"bs_uid IN ?",
			"metadata->>'coexistence_user_id' IN ?",
			"metadata->>'coexistence_parent_user_id' IN ?",
			"metadata->>'coexistence_conflicting_user_id' IN ?",
		} {
			conditions = append(conditions, condition)
			args = append(args, selectors.userIDs)
		}
	}
	if len(selectors.phones) > 0 {
		conditions = append(conditions, "regexp_replace(phone_number, '[^0-9]', '', 'g') IN ?")
		args = append(args, selectors.phones)
	}
	if len(selectors.placeholders) > 0 {
		conditions = append(conditions, "phone_number IN ?")
		args = append(args, selectors.placeholders)
	}
	if selectors.username != "" {
		conditions = append(conditions, "LOWER(metadata->>'coexistence_username') = LOWER(?)")
		args = append(args, selectors.username)
	}
	if len(conditions) == 0 {
		return nil, errors.New("coexistence sender admission has no selector")
	}
	var rows []models.Contact
	if err := a.DB.Unscoped().
		Where("organization_id = ?", organizationID).
		Where("("+strings.Join(conditions, " OR ")+")", args...).
		Order("id").Limit(2).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("read coexistence sender selector owners: %w", err)
	}
	return rows, nil
}

// coexistenceSenderHasReviewEvidence reports whether any earlier review hold in
// the tenant captured a candidate for one of these selectors. Such a sender
// once matched a contact; the review path, not a fresh bootstrap, owns it.
// Zero-member holds (an earlier held message from this same unknown sender)
// are not evidence of another identity.
func (a *App) coexistenceSenderHasReviewEvidence(
	organizationID uuid.UUID,
	selectors coexistenceSenderSelectors,
) (bool, error) {
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if len(selectors.userIDs) > 0 {
		conditions = append(conditions, "direct_primary_bsuid IN ?", "parent_bsuid IN ?")
		args = append(args, selectors.userIDs, selectors.userIDs)
	}
	if len(selectors.phones) > 0 {
		conditions = append(conditions, "phone IN ?")
		args = append(args, selectors.phones)
	}
	if len(conditions) == 0 {
		return true, nil
	}
	var count int64
	if err := a.DB.Model(&models.WhatsAppIdentityReviewHold{}).
		Where("organization_id = ? AND member_count > 0", organizationID).
		Where("("+strings.Join(conditions, " OR ")+")", args...).
		Count(&count).Error; err != nil {
		return true, fmt.Errorf("read coexistence sender review evidence: %w", err)
	}
	return count > 0, nil
}

// coexistencePhoneAppRecipientAwaitingFirstReply proves that a phone-only
// contact exists only because this Coexistence number messaged that phone from
// the WhatsApp Business app, and has never received anything from anyone.
func (a *App) coexistencePhoneAppRecipientAwaitingFirstReply(
	account *models.WhatsAppAccount,
	contact *models.Contact,
	phone, username string,
) (bool, error) {
	if contact == nil || contact.OrganizationID != account.OrganizationID || contact.DeletedAt.Valid ||
		contact.MergedIntoID != nil || strings.TrimSpace(contact.BSUID) != "" || !contactHasDialablePhone(contact) ||
		normalizeIdentityReviewPhone(contact.PhoneNumber) != phone {
		return false, nil
	}
	for _, key := range coexistenceContactIdentityMetadataKeys {
		if _, present := contact.Metadata[key]; present {
			return false, nil
		}
	}
	if stored, present := contact.Metadata["coexistence_username"].(string); present &&
		!strings.EqualFold(strings.TrimSpace(stored), strings.TrimSpace(username)) {
		return false, nil
	}
	var aliases int64
	if err := a.DB.Unscoped().Model(&models.Contact{}).Where(
		"organization_id = ? AND merged_into_id = ?", account.OrganizationID, contact.ID,
	).Count(&aliases).Error; err != nil {
		return false, fmt.Errorf("read phone-app recipient merge aliases: %w", err)
	}
	if aliases > 0 {
		return false, nil
	}
	held, err := database.ContactHasBlockingIdentityReviewHold(a.DB, account.OrganizationID, contact.ID)
	if err != nil || held {
		return false, err
	}
	var incoming int64
	if err := a.DB.Unscoped().Model(&models.Message{}).Where(
		"organization_id = ? AND contact_id = ? AND direction = ?",
		account.OrganizationID, contact.ID, models.DirectionIncoming,
	).Count(&incoming).Error; err != nil {
		return false, fmt.Errorf("read phone-app recipient inbound history: %w", err)
	}
	if incoming > 0 {
		return false, nil
	}
	var echoes []models.Message
	if err := a.DB.Unscoped().Select("id", "whats_app_message_id").Where(
		"organization_id = ? AND contact_id = ? AND direction = ? AND COALESCE(metadata, '{}'::jsonb) @> ?::jsonb",
		account.OrganizationID, contact.ID, models.DirectionOutgoing, `{"coexistence_source":"smb_message_echoes"}`,
	).Order("id").Limit(50).Find(&echoes).Error; err != nil {
		return false, fmt.Errorf("read phone-app recipient echoes: %w", err)
	}
	for _, echo := range echoes {
		wamid := strings.TrimSpace(echo.WhatsAppMessageID)
		// persistCoexistenceMessage keys every echo winner by this account's ID,
		// so a matching row proves THIS number's phone app addressed the contact.
		if wamid != "" && echo.ID == uuid.NewSHA1(account.ID, []byte("coexistence-message:"+wamid)) {
			return true, nil
		}
	}
	return false, nil
}
