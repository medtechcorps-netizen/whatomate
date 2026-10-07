package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

// This separate path accepts authenticated phone identity. Phone continuity
// cannot establish permanent person identity or detect silent number recycling.
// It never adopts an old contact, resolves a hold, or invents a BSUID.
const coexistencePhoneAdmissionKey = "coexistence_phone_admission_v1"

// Both the inbound Message and its account-bound continuation job receive this
// server-owned provenance. Contact metadata is editable and is not authority.
// These JSON values are durable application evidence, not immutable DB columns.
// Continuation depends on retaining the initial inbound job; no production job
// cleanup currently removes it. Missing evidence always returns to review.
type coexistencePhoneAdmissionProof struct {
	AccountID     uuid.UUID `json:"account_id"`
	Cycle         uint64    `json:"cycle"`
	Initial       bool      `json:"initial"`
	WebhookSHA256 string    `json:"webhook_sha256"`
}

func decodeCoexistencePhoneAdmissionProof(value any) (coexistencePhoneAdmissionProof, bool) {
	var proof coexistencePhoneAdmissionProof
	text, ok := value.(string)
	if !ok || json.Unmarshal([]byte(text), &proof) != nil || proof.AccountID == uuid.Nil ||
		proof.Cycle == 0 || !isSHA256Hex(proof.WebhookSHA256) {
		return proof, false
	}
	canonical, err := json.Marshal(proof)
	return proof, err == nil && string(canonical) == text
}

// A sidecar is optional; when supplied, exactly one entry must identify this
// raw phone, without introducing a user ID, parent ID, or username. Unmatched
// entries may belong to other messages in the same webhook batch.
func (m IncomingTextMessage) withPhoneOnlyContactEvidence(contacts []CoexistenceWebhookContact) IncomingTextMessage {
	if m.FromUserID != "" || len(contacts) == 0 {
		return m
	}
	matches := 0
	for _, contact := range contacts {
		if contact.WaID != m.From {
			continue
		}
		matches++
		if contact.UserID != "" || contact.ParentUserID != "" || strings.TrimSpace(contact.Profile.Username) != "" {
			m.phoneOnlySenderConflict = true
		}
	}
	m.phoneOnlySenderConflict = m.phoneOnlySenderConflict || matches != 1
	return m
}

// Called only after the authenticated wrapper owns the existing WAMID,
// organization policy/selector, account and current-cycle locks. A nil policy
// leaves every existing review rule intact.
func (a *App) admitCoexistencePhoneOnlySender(
	account *models.WhatsAppAccount, claim WhatsAppIdentityReviewClaim,
	admission *WhatsAppIdentityReviewAdmission, message IncomingTextMessage,
	profileName, webhookSHA256 string,
) (*incomingMessageAdmissionPolicy, error) {
	phone := message.From
	if account == nil || admission == nil || !admission.Blocked || !admission.NeedsReview ||
		admission.Reason != "direct_primary_missing" || !account.IsSMB ||
		claim.OrganizationID != account.OrganizationID || claim.WhatsAppAccountID != account.ID || claim.OnboardingCycle == 0 ||
		claim.VerifiedEventProvenance != WhatsAppIdentityReviewVerifiedMetaEvent ||
		claim.DirectPrimaryBSUID != "" || claim.ParentBSUID != "" ||
		message.FromUserID != "" || message.FromParentUserID != "" ||
		message.phoneOnlySenderConflict || strings.TrimSpace(message.senderUsername) != "" ||
		(message.senderWaID != "" && message.senderWaID != phone) ||
		!isPlausibleWhatsAppPhone(phone) || phone[0] == '0' || claim.Phone != phone || !isSHA256Hex(webhookSHA256) {
		return nil, nil
	}
	// Any review of this phone in the tenant, even an old or zero-member one,
	// is evidence that this is not a fresh, unambiguous phone-only identity.
	var reviews int64
	if err := a.DB.Model(&models.WhatsAppIdentityReviewHold{}).
		Where("organization_id = ? AND phone = ?", account.OrganizationID, phone).Count(&reviews).Error; err != nil {
		return nil, err
	}
	if reviews != 0 {
		return nil, nil
	}
	matches, err := a.coexistenceSenderSelectorMatches(account.OrganizationID,
		coexistenceSenderAdmissionSelectors(claim, message))
	if err != nil || len(matches) > 1 {
		return nil, err
	}
	initial := len(matches) == 0
	var contact *models.Contact
	if initial {
		// Fresh creation is all-or-nothing. A racing/aliased pre-existing contact
		// cannot be relabelled as freshly admitted.
		err = a.DB.Transaction(func(tx *gorm.DB) error {
			nested := a.scopedApp(tx, account.OrganizationID)
			var created bool
			var createErr error
			contact, created, createErr = nested.getOrCreateInboundContact(account, phone, profileName, "")
			if createErr != nil {
				return createErr
			}
			if !created {
				return errCoexistenceSenderAdmissionNotProven
			}
			a.adoptAfterCommit(nested)
			return nil
		})
		if errors.Is(err, errCoexistenceSenderAdmissionNotProven) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
	} else {
		contact = &matches[0]
		proven, proofErr := a.coexistencePhoneOnlyContinuation(account, &claim, contact, phone)
		if proofErr != nil || !proven {
			return nil, proofErr
		}
	}
	proof, err := json.Marshal(coexistencePhoneAdmissionProof{
		AccountID: account.ID, Cycle: claim.OnboardingCycle, Initial: initial, WebhookSHA256: webhookSHA256,
	})
	if err != nil {
		return nil, err
	}
	return &incomingMessageAdmissionPolicy{CanonicalContactID: contact.ID, PhoneOnlyProof: string(proof)}, nil
}

func (a *App) coexistencePhoneOnlyContinuation(account *models.WhatsAppAccount, claim *WhatsAppIdentityReviewClaim, contact *models.Contact, phone string) (bool, error) {
	if contact == nil || contact.OrganizationID != account.OrganizationID || contact.DeletedAt.Valid ||
		contact.MergedIntoID != nil || contact.PhoneNumber != phone || strings.TrimSpace(contact.BSUID) != "" {
		return false, nil
	}
	for _, key := range append(append([]string{}, coexistenceContactIdentityMetadataKeys...), "coexistence_username") {
		if _, exists := contact.Metadata[key]; exists {
			return false, nil
		}
	}
	var aliases, holds int64
	if err := a.DB.Unscoped().Model(&models.Contact{}).Where("organization_id = ? AND merged_into_id = ?", account.OrganizationID, contact.ID).Count(&aliases).Error; err != nil {
		return false, err
	}
	if err := a.DB.Model(&models.WhatsAppIdentityReviewMember{}).Where("organization_id = ? AND contact_id = ?", account.OrganizationID, contact.ID).Count(&holds).Error; err != nil {
		return false, err
	}
	if aliases != 0 || holds != 0 {
		return false, nil
	}

	// Every incoming row must carry this path's same-account/cycle evidence.
	// This refuses imported history, another account, deleted evidence, or an
	// intervening BSUID-bearing admission. There is no recency window which
	// could hide an older contradiction or eventually strand a busy sender.
	pattern := `^` + regexp.QuoteMeta(fmt.Sprintf(`{"account_id":"%s","cycle":%d,"initial":`, account.ID, claim.OnboardingCycle)) +
		`(true|false)` + regexp.QuoteMeta(`,"webhook_sha256":"`) + `[0-9a-f]{64}` + regexp.QuoteMeta(`"}`) + `$`
	base := a.DB.Unscoped().Model(&models.Message{}).Where("organization_id = ? AND contact_id = ? AND direction = ?", account.OrganizationID, contact.ID, models.DirectionIncoming)
	var contradictions []models.Message
	if err := base.Session(&gorm.Session{}).Select("id").Where("deleted_at IS NOT NULL OR COALESCE(metadata ->> ?, '') !~ ?", coexistencePhoneAdmissionKey, pattern).
		Limit(1).Find(&contradictions).Error; err != nil {
		return false, err
	}
	if len(contradictions) != 0 {
		return false, nil
	}
	var rows []models.Message
	if err := base.Session(&gorm.Session{}).Select("id", "organization_id", "contact_id", "whats_app_message_id", "metadata", "deleted_at").
		Where("metadata ->> ? LIKE ?", coexistencePhoneAdmissionKey, `%"initial":true,%`).Limit(2).Find(&rows).Error; err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	initials := 0
	for _, row := range rows {
		proof, valid := decodeCoexistencePhoneAdmissionProof(row.Metadata[coexistencePhoneAdmissionKey])
		if !valid || row.DeletedAt.Valid || proof.AccountID != account.ID || proof.Cycle != claim.OnboardingCycle || row.WhatsAppMessageID == "" {
			return false, nil
		}
		if !proof.Initial {
			continue
		}
		initials++
		var job models.ScheduledJob
		key := "inbound-message-continuation:" + uuid.NewSHA1(account.ID, []byte(row.WhatsAppMessageID)).String()
		err := a.DB.Where("organization_id = ? AND idempotency_key = ?", account.OrganizationID, key).First(&job).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read phone-only admission proof: %w", err)
		}
		inbound, proofErr := validateInboundContinuationJobProof(&job, account, row.ID, row.WhatsAppMessageID)
		if proofErr != nil || job.Payload[coexistencePhoneAdmissionKey] != row.Metadata[coexistencePhoneAdmissionKey] ||
			inbound.From != phone || inbound.FromUserID != "" || inbound.FromParentUserID != "" {
			return false, nil
		}
	}
	return initials == 1, nil
}
