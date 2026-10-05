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
//     identity-review hold other than a zero-member copy of this same BSUID.
//     There is nobody to confuse the sender with, so one contact is created and
//     bound to that BSUID (and to wa_id as its phone when "from" is absent).
//   - phone_app_recipient: the only match is a phone-only contact that the
//     echo of this number's own WhatsApp Business app message created
//     (coexistencePhoneAppRecipientAwaitingFirstReply): the contact row was
//     written together with that echo, its first message is that echo, it has
//     no inbound message, BSUID, merge alias, address-book or identity
//     metadata, assignment, CRM lead, booking, package or review history, and
//     this number first messaged it from the app within the last seven days. The
//     reply authenticates the same phone ("from", or contacts[].wa_id when
//     "from" is absent) with its BSUID, so the BSUID is bound to that contact
//     exactly as the echo path would have done had Meta included the recipient
//     BSUID in the echo. An older CRM, address-book or history contact that
//     merely received a later echo is never adopted: its number may have been
//     recycled to someone else.
//   - classic_history: the only match is a live contact, without a BSUID or
//     holding this sender's parent BSUID, that already received inbound
//     messages from this same phone on this same WhatsApp account
//     (coexistenceClassicHistoryContact): Meta attested that phone as the
//     sender before the number joined Coexistence. The owner chose to trust
//     this number's history, so the BSUID is bound to that contact exactly as
//     classic phone-keyed routing would have addressed it. That accepts the
//     same recycled-number risk classic routing takes. Contacts known only
//     from the address book, chat history or another account are not
//     adopted, and no open or decided review of these selectors may exist.
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
	coexistenceSenderAdmissionClassicHistory    coexistenceSenderAdmissionKind = "classic_history"
)

// coexistenceClassicHistoryEvidenceLimit caps the inbound continuation jobs
// read for the classic-history proof, newest first. Every job read must agree
// with the sender; at least one must attest the sender's phone.
const coexistenceClassicHistoryEvidenceLimit = 200

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
// identity, or with the Business app's address book (smb_app_state_sync). A
// phone-only contact carrying any of them is not one that a phone-app echo
// created.
var coexistenceContactIdentityMetadataKeys = []string{
	"coexistence_user_id",
	"coexistence_parent_user_id",
	"coexistence_conflicting_user_id",
	"coexistence_phone_conflict",
	"coexistence_reconciled_contact_id",
	"coexistence_app_contact",
}

// coexistencePhoneAppRecipientCreationWindow bounds how long before the
// creating echo's ingested_at the contact row may have been written.
// persistCoexistenceMessage creates the contact and only then stamps the
// echo's ingested_at, both from this process's clock inside one transaction,
// so an echo-created contact is never newer than its echo and normally only
// milliseconds older. A contact written earlier than this (manually, by
// import or by a sync just before staff messaged the number) is not one the
// echo created.
const coexistencePhoneAppRecipientCreationWindow = 5 * time.Second

// coexistencePhoneAppRecipientEchoMaxAge bounds how long after this number's
// first phone-app message to a recipient (the echo that created the contact)
// that recipient's first reply may still be adopted. Every earlier outgoing
// message on the contact then falls inside the same window; a number can
// change hands over a longer lifetime.
const coexistencePhoneAppRecipientEchoMaxAge = 7 * 24 * time.Hour

// coexistencePhoneAppRecipientMessageLimit caps the history read for the
// creation proof. A contact with more messages than this, all outgoing, is
// not a fresh phone-app recipient; it keeps the review path.
const coexistencePhoneAppRecipientMessageLimit = 200

var errCoexistenceSenderAdmissionNotProven = errors.New("coexistence sender admission could not be proven")

type coexistenceSenderSelectors struct {
	userIDs      []string
	phones       []string
	placeholders []string
	// legacyPhones are this sender's BSUIDs as the previous release stored
	// them in phone_number for an echo addressed by BSUID
	// (coexistenceLegacyBSUIDPhoneContact): such a row is the sender's own
	// earlier thread, so the sender is not new.
	legacyPhones []string
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
	for _, userID := range selectors.userIDs {
		if isCoexistenceBSUIDAddress(userID) {
			selectors.legacyPhones = append(selectors.legacyPhones, userID)
		}
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

// coexistenceSenderAdmissionPhone returns the phone a bound contact gets.
// "from" must be a plausible phone equal to the claim's phone selector. When
// Meta omitted "from", a plausible contacts[].wa_id stands in, as it does for
// the classic resolver (inboundSenderIdentity), so a later phone-app echo to
// that number finds the same contact. ok=false keeps the review path.
func coexistenceSenderAdmissionPhone(message IncomingTextMessage, claimPhone string) (phone string, fromWaID, ok bool) {
	if phone = normalizeCoexistencePhone(message.From); phone != "" {
		return phone, false, isPlausibleWhatsAppPhone(phone) && phone == claimPhone
	}
	if waID := normalizeCoexistencePhone(message.senderWaID); isPlausibleWhatsAppPhone(waID) {
		return waID, true, true
	}
	// No usable phone: the contact gets a bsuid:/user: placeholder phone.
	return "", false, true
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
	phone, phoneFromWaID, ok := coexistenceSenderAdmissionPhone(message, normalized.Phone)
	if !ok {
		return nil, nil
	}

	kind, recipientID, err := a.classifyCoexistenceSenderAdmission(
		account, normalized, admission.Candidates, message, phone, phoneFromWaID,
	)
	if err != nil || kind == "" {
		return nil, err
	}
	if phoneFromWaID {
		// Same rule the classic resolver applies to a wa_id-only sender
		// (getOrCreateNewInboundSenderContact): wa_id may become the contact
		// phone only when it cannot move this BSUID onto, or a new phone onto,
		// a contact that already belongs to someone.
		keepWaID, _, waIDErr := a.inboundWaIDAcceptsUserID(account.OrganizationID, coexistenceContactIdentity{
			Phone:        phone,
			UserID:       normalized.DirectPrimaryBSUID,
			ParentUserID: normalized.ParentBSUID,
		})
		if waIDErr != nil || !keepWaID {
			return nil, waIDErr
		}
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
		case coexistenceSenderAdmissionPhoneAppRecipient, coexistenceSenderAdmissionClassicHistory:
			if created || contact.ID != recipientID {
				return errCoexistenceSenderAdmissionNotProven
			}
		default:
			return errCoexistenceSenderAdmissionNotProven
		}
		if contact.BSUID != normalized.DirectPrimaryBSUID || contact.MergedIntoID != nil ||
			(phone != "" && normalizeCoexistencePhone(contact.PhoneNumber) != phone) {
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
	phoneFromWaID bool,
) (coexistenceSenderAdmissionKind, uuid.UUID, error) {
	selectors := coexistenceSenderAdmissionSelectors(claim, message)
	matches, err := a.coexistenceSenderSelectorMatches(account.OrganizationID, selectors)
	if err != nil {
		return "", uuid.Nil, err
	}
	reviewed, err := a.coexistenceSenderHasReviewEvidence(account.OrganizationID, claim, selectors)
	if err != nil {
		return "", uuid.Nil, err
	}
	if !reviewed && len(candidates) == 0 && len(matches) == 0 {
		return coexistenceSenderAdmissionNewSender, uuid.Nil, nil
	}
	if phone == "" || len(matches) != 1 {
		return "", uuid.Nil, nil
	}
	var reasons models.WhatsAppIdentityReviewSelectorReason
	switch len(candidates) {
	case 1:
		// The evaluator's only candidate must be the one widened match.
		if matches[0].ID != candidates[0].ContactID {
			return "", uuid.Nil, nil
		}
		reasons = candidates[0].SelectorReasons
	case 0:
		// The evaluator never sees contacts[].wa_id. With "from" absent, the
		// only widened match may be the contact that wa_id names.
		if !phoneFromWaID {
			return "", uuid.Nil, nil
		}
	default:
		return "", uuid.Nil, nil
	}
	phoneOnly := (len(candidates) == 1 && reasons == models.WhatsAppIdentityReviewSelectorPhone) ||
		(len(candidates) == 0 && phoneFromWaID)
	if !reviewed && phoneOnly {
		proven, proofErr := a.coexistencePhoneAppRecipientAwaitingFirstReply(account, &matches[0], phone, selectors.username)
		if proofErr != nil {
			return "", uuid.Nil, proofErr
		}
		if proven {
			return coexistenceSenderAdmissionPhoneAppRecipient, matches[0].ID, nil
		}
	}

	// Classic history accepts the phone owner named by "from" (or, with "from"
	// absent, by wa_id), optionally also holding this sender's parent BSUID.
	expectedPhoneReason := models.WhatsAppIdentityReviewSelectorPhone
	if phoneFromWaID {
		expectedPhoneReason = 0
	}
	if reasons&^models.WhatsAppIdentityReviewSelectorParentBSUID != expectedPhoneReason {
		return "", uuid.Nil, nil
	}
	if reviewed {
		// This sender's own review closed only by an onboarding-cycle change
		// neither stays open nor records a decision, so it does not bar classic
		// history; any other review of these selectors does.
		live, liveErr := a.coexistenceSenderHasLiveReviewEvidence(account.OrganizationID, claim, selectors)
		if liveErr != nil || live {
			return "", uuid.Nil, liveErr
		}
	}
	proven, err := a.coexistenceClassicHistoryContact(account, &matches[0], claim, phone, selectors.username)
	if err != nil || !proven {
		return "", uuid.Nil, err
	}
	return coexistenceSenderAdmissionClassicHistory, matches[0].ID, nil
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
	if exact := append(append([]string{}, selectors.placeholders...), selectors.legacyPhones...); len(exact) > 0 {
		conditions = append(conditions, "phone_number IN ?")
		args = append(args, exact)
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
// the tenant names one of these selectors, other than an earlier held copy of
// this same sender. A hold that captured candidates means the sender once
// matched a contact; a hold left by a different direct BSUID on the same phone
// or parent is a second WhatsApp user behind these selectors. Either way the
// review path, not a fresh bootstrap, owns the sender. Only a zero-member hold
// whose direct BSUID is this sender's own (and whose parent is empty or the
// same) is skipped: it is an earlier message from this same unknown sender.
func (a *App) coexistenceSenderHasReviewEvidence(
	organizationID uuid.UUID,
	claim WhatsAppIdentityReviewClaim,
	selectors coexistenceSenderSelectors,
) (bool, error) {
	return a.coexistenceSenderReviewEvidence(organizationID, claim, selectors, false)
}

// coexistenceSenderHasLiveReviewEvidence is coexistenceSenderHasReviewEvidence
// without this sender's own holds that an onboarding-cycle change closed, from
// the phone it writes from now and with an unchanged parent BSUID: such a hold
// neither stays open nor records a decision, and it names no other user or
// number. A cycle-closed hold left by any other direct BSUID on these
// selectors still shows that a second WhatsApp user wrote from them.
func (a *App) coexistenceSenderHasLiveReviewEvidence(
	organizationID uuid.UUID,
	claim WhatsAppIdentityReviewClaim,
	selectors coexistenceSenderSelectors,
) (bool, error) {
	return a.coexistenceSenderReviewEvidence(organizationID, claim, selectors, true)
}

func (a *App) coexistenceSenderReviewEvidence(
	organizationID uuid.UUID,
	claim WhatsAppIdentityReviewClaim,
	selectors coexistenceSenderSelectors,
	ignoreOwnCycleClosed bool,
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
	directBSUID := strings.TrimSpace(claim.DirectPrimaryBSUID)
	if directBSUID == "" {
		return true, nil
	}
	query := a.DB.Model(&models.WhatsAppIdentityReviewHold{}).
		Where("organization_id = ?", organizationID).
		Where("("+strings.Join(conditions, " OR ")+")", args...).
		Where(
			"(member_count > 0 OR direct_primary_bsuid <> ? OR (parent_bsuid <> '' AND parent_bsuid <> ?))",
			directBSUID, strings.TrimSpace(claim.ParentBSUID),
		)
	if ignoreOwnCycleClosed {
		// Only the sender's own closed hold from the phone it writes from now
		// (or from no phone) and with the same or no parent BSUID is ignored. A
		// closed hold the sender left from another phone shows it changed
		// numbers, so it still counts.
		ownPhones := append([]string{""}, selectors.phones...)
		query = query.Where(
			"NOT (disposition = ? AND direct_primary_bsuid = ? AND phone IN ? AND parent_bsuid IN ?)",
			models.WhatsAppIdentityReviewDispositionSupersededByCycle, directBSUID,
			ownPhones, []string{"", strings.TrimSpace(claim.ParentBSUID)},
		)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return true, fmt.Errorf("read coexistence sender review evidence: %w", err)
	}
	return count > 0, nil
}

// coexistenceClassicHistoryContact proves that the only contact matching this
// sender has already received inbound messages from the sender's phone on
// this same WhatsApp account, with Meta attesting that phone as the sender,
// and that nothing ties it to a different WhatsApp user. Every condition
// fails closed:
//
//   - live, unmerged, a dialable phone equal to the sender's, and no BSUID or
//     this sender's parent BSUID (the direct BSUID has no owner);
//   - no stored username other than the sender's, nothing tying its canonical
//     family to another WhatsApp user (identityReviewMembersOfOtherUsers: a
//     merged contact with another BSUID, stored identity naming another user
//     or a conflict, or a reviewer's decision routing another principal to
//     it in this cycle), and no open identity-review hold;
//   - at least one inbound message stored on this exact contact (not on a
//     merge alias), not soft-deleted and not placed by identity review, whose
//     inbound continuation job passes validateInboundContinuationJobProof for
//     this account. That proof binds the job's idempotency key to account.ID
//     (immutable, unlike the message's account-name projection), its payload
//     to account.PhoneID, the message ID and the WAMID, and returns Meta's raw
//     signed "from", from_user_id and from_parent_user_id, stored verbatim at
//     admission. The raw "from" must be the sender's phone;
//   - no validated job among this account's newest
//     coexistenceClassicHistoryEvidenceLimit (filtered by Meta phone ID before
//     the limit) names a different sender BSUID or parent BSUID.
//
// Only the live inbound admission path creates continuation jobs: history
// imports and echoes never do, so address-book and history-only contacts and
// contacts known only from another account keep the review path.
func (a *App) coexistenceClassicHistoryContact(
	account *models.WhatsAppAccount,
	contact *models.Contact,
	claim WhatsAppIdentityReviewClaim,
	phone, username string,
) (bool, error) {
	if contact == nil || contact.OrganizationID != account.OrganizationID || contact.DeletedAt.Valid ||
		contact.MergedIntoID != nil || !contactHasDialablePhone(contact) ||
		normalizeIdentityReviewPhone(contact.PhoneNumber) != phone {
		return false, nil
	}
	direct := strings.TrimSpace(claim.DirectPrimaryBSUID)
	parent := strings.TrimSpace(claim.ParentBSUID)
	if bsuid := strings.TrimSpace(contact.BSUID); bsuid != "" && (parent == "" || bsuid != parent) {
		return false, nil
	}
	if stored, present := contact.Metadata["coexistence_username"].(string); present &&
		!strings.EqualFold(strings.TrimSpace(stored), strings.TrimSpace(username)) {
		return false, nil
	}
	// The same family rule that makes a review member unroutable: another
	// user's BSUID on a merged contact, stored identity naming another user,
	// or a reviewer's decision for another principal in this cycle.
	otherUsers, err := identityReviewMembersOfOtherUsers(a.DB, claim, []uuid.UUID{contact.ID},
		map[uuid.UUID]*models.Contact{contact.ID: contact})
	if err != nil || otherUsers[contact.ID] {
		return false, err
	}
	held, err := database.ContactHasBlockingIdentityReviewHold(a.DB, account.OrganizationID, contact.ID)
	if err != nil || held {
		return false, err
	}

	type evidenceRow struct {
		JobID     uuid.UUID `gorm:"column:job_id"`
		MessageID uuid.UUID `gorm:"column:message_id"`
		WAMID     string    `gorm:"column:wamid"`
	}
	var rows []evidenceRow
	if err := a.DB.Table("scheduled_jobs AS job").
		Select("job.id AS job_id, message.id AS message_id, message.whats_app_message_id AS wamid").
		Joins("JOIN messages AS message ON message.organization_id = job.organization_id AND message.id = job.aggregate_id").
		// Filter to this account's Meta phone ID before the window so another
		// account's traffic can neither crowd out nor hide this account's jobs;
		// validateInboundContinuationJobProof still checks every row.
		Where(`job.organization_id = ? AND job.kind = ? AND job.aggregate_type = ?
			AND BTRIM(job.payload ->> 'phone_number_id') = ?
			AND message.contact_id = ? AND message.direction = ? AND message.deleted_at IS NULL
			AND (COALESCE(message.metadata, '{}'::jsonb) ->> ?) IS NULL`,
			account.OrganizationID, inboundContinuationJobKind, "message", strings.TrimSpace(account.PhoneID),
			contact.ID, models.DirectionIncoming, incomingIdentityReviewHoldIDKey).
		Order("job.created_at DESC, job.id DESC").
		Limit(coexistenceClassicHistoryEvidenceLimit).
		Scan(&rows).Error; err != nil {
		return false, fmt.Errorf("read classic-history inbound evidence: %w", err)
	}
	if len(rows) == 0 {
		return false, nil
	}
	jobIDs := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		jobIDs = append(jobIDs, row.JobID)
	}
	var jobs []models.ScheduledJob
	if err := a.DB.Where("organization_id = ? AND id IN ?", account.OrganizationID, jobIDs).
		Find(&jobs).Error; err != nil {
		return false, fmt.Errorf("read classic-history inbound jobs: %w", err)
	}
	jobsByID := make(map[uuid.UUID]*models.ScheduledJob, len(jobs))
	for index := range jobs {
		jobsByID[jobs[index].ID] = &jobs[index]
	}
	attested := false
	for _, row := range rows {
		job := jobsByID[row.JobID]
		inbound, proofErr := validateInboundContinuationJobProof(job, account, row.MessageID, strings.TrimSpace(row.WAMID))
		if proofErr != nil {
			// Another account's traffic, or not a proof at all: no evidence.
			continue
		}
		if userID := strings.TrimSpace(inbound.FromUserID); userID != "" && userID != direct {
			return false, nil
		}
		if parentID := strings.TrimSpace(inbound.FromParentUserID); parentID != "" && parentID != parent {
			return false, nil
		}
		if normalizeIdentityReviewPhone(inbound.From) == phone {
			attested = true
		}
	}
	return attested, nil
}

// coexistencePhoneAppRecipientAwaitingFirstReply proves that a phone-only
// contact was created by the echo of a message this Coexistence number sent
// from the WhatsApp Business app, has received nothing from anyone, and holds
// nothing a person entered about someone else. Every condition fails closed:
//
//   - live, unmerged, no BSUID, a dialable phone equal to the sender's, no
//     identity or address-book metadata, no assignment and no merge alias;
//   - no open identity-review hold and no inbound message;
//   - its first message (by ingestion) is a proven echo winner of this
//     account, ingested no earlier than the contact row and at most
//     coexistencePhoneAppRecipientCreationWindow after it, and no message on
//     it is dated before that echo unless it is another proven echo of this
//     account (so no history import);
//   - no CRM lead, booking or contact package;
//   - that creating echo is at most coexistencePhoneAppRecipientEchoMaxAge
//     old, so a later echo does not refresh an older contact.
//
// A manually created, imported or address-book contact that later received
// one echo therefore stays in review: its phone may since have been recycled
// to a different person, whose BSUID must not be bound to that record.
func (a *App) coexistencePhoneAppRecipientAwaitingFirstReply(
	account *models.WhatsAppAccount,
	contact *models.Contact,
	phone, username string,
) (bool, error) {
	if contact == nil || contact.OrganizationID != account.OrganizationID || contact.DeletedAt.Valid ||
		contact.MergedIntoID != nil || contact.AssignedUserID != nil || strings.TrimSpace(contact.BSUID) != "" ||
		!contactHasDialablePhone(contact) || normalizeIdentityReviewPhone(contact.PhoneNumber) != phone {
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
	proven, err := a.coexistencePhoneAppEchoCreatedContact(account, contact)
	if err != nil || !proven {
		return false, err
	}
	// Records a person entered for this contact. Follow-up tasks are not
	// listed: automations create them on contact.created for every contact,
	// including one an echo created.
	for _, record := range []struct {
		model any
		name  string
	}{
		{model: &models.CRMLead{}, name: "CRM leads"},
		{model: &models.Booking{}, name: "bookings"},
		{model: &models.ContactPackage{}, name: "packages"},
	} {
		var count int64
		if err := a.DB.Unscoped().Model(record.model).Where(
			"organization_id = ? AND contact_id = ?", account.OrganizationID, contact.ID,
		).Count(&count).Error; err != nil {
			return false, fmt.Errorf("read phone-app recipient %s: %w", record.name, err)
		}
		if count > 0 {
			return false, nil
		}
	}
	return true, nil
}

// coexistencePhoneAppEchoCreatedContact checks the message-history half of
// coexistencePhoneAppRecipientAwaitingFirstReply.
func (a *App) coexistencePhoneAppEchoCreatedContact(
	account *models.WhatsAppAccount,
	contact *models.Contact,
) (bool, error) {
	var messages []models.Message
	if err := a.DB.Unscoped().
		Select("id", "whats_app_message_id", "direction", "metadata", "created_at", "ingested_at").
		Where("organization_id = ? AND contact_id = ?", account.OrganizationID, contact.ID).
		Order("COALESCE(ingested_at, created_at) ASC, id ASC").
		Limit(coexistencePhoneAppRecipientMessageLimit + 1).
		Find(&messages).Error; err != nil {
		return false, fmt.Errorf("read phone-app recipient messages: %w", err)
	}
	if len(messages) == 0 || len(messages) > coexistencePhoneAppRecipientMessageLimit {
		return false, nil
	}
	isOwnEcho := func(message *models.Message) bool {
		wamid := strings.TrimSpace(message.WhatsAppMessageID)
		source, _ := message.Metadata["coexistence_source"].(string)
		// persistCoexistenceMessage keys every echo winner by the receiving
		// account's ID, so a matching row proves THIS number's phone app
		// addressed the contact.
		return message.Direction == models.DirectionOutgoing && source == "smb_message_echoes" && wamid != "" &&
			message.ID == uuid.NewSHA1(account.ID, []byte("coexistence-message:"+wamid))
	}
	creating := &messages[0]
	if !isOwnEcho(creating) || creating.IngestedAt == nil {
		return false, nil
	}
	// The contact must not predate its creating echo by more than the
	// in-transaction gap, and can never be newer than it.
	gap := creating.IngestedAt.Sub(contact.CreatedAt)
	if gap < 0 || gap > coexistencePhoneAppRecipientCreationWindow {
		return false, nil
	}
	// The creating echo (and so the contact and every message on it) must be
	// recent; a later echo does not refresh an older contact.
	if time.Since(*creating.IngestedAt) > coexistencePhoneAppRecipientEchoMaxAge {
		return false, nil
	}
	for index := range messages {
		message := &messages[index]
		if message.Direction != models.DirectionOutgoing {
			return false, nil
		}
		if !isOwnEcho(message) && message.CreatedAt.Before(creating.CreatedAt) {
			// Dated before the echo that created the contact: imported history
			// or another earlier conversation with this number.
			return false, nil
		}
	}
	return true, nil
}
