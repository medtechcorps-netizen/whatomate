package handlers

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

var errMarketingPreferenceIdentityConflict = errors.New("marketing preference sender identity is ambiguous")

// A selector with more owners is unsafe to interpret, not permission to take
// the first row. Bound the census without silently accepting a partial set.
const marketingPreferenceSelectorLimit = 256

// resolveMarketingPreferenceContact must run under LockOrganizationPolicyScope,
// before any contact creation, restore or consent write. Both selectors are
// independent evidence: a phone match cannot hide a different BSUID owner.
// The bool authorizes only an empty-to-proven BSUID enrichment, never replacement.
func resolveMarketingPreferenceContact(
	tx *gorm.DB,
	organizationID uuid.UUID,
	userPhone, bsuid string,
) (*models.Contact, bool, error) {
	userPhone, bsuid = normalizeCoexistencePhone(userPhone), strings.TrimSpace(bsuid)
	rows := make(map[uuid.UUID]models.Contact)
	load := func(query *gorm.DB) (bool, error) {
		var selected []models.Contact
		if err := query.Order("id").Limit(marketingPreferenceSelectorLimit + 1).Find(&selected).Error; err != nil {
			return false, err
		}
		if len(selected) > marketingPreferenceSelectorLimit {
			return false, fmt.Errorf("%w: selector census exceeds limit", errMarketingPreferenceIdentityConflict)
		}
		for _, row := range selected {
			rows[row.ID] = row
		}
		return len(selected) > 0, nil
	}
	phoneMatched, bsuidMatched := false, false
	if userPhone != "" {
		query := tx.Unscoped().Where("organization_id = ?", organizationID)
		forms := []string{userPhone, "+" + userPhone}
		if digits := normalizeIdentityReviewPhone(userPhone); digits != "" && digits == userPhone {
			// Include manually formatted dialable owners, without treating the
			// digits embedded in a BSUID/placeholder as a phone-number proof.
			query = query.Where(`(phone_number IN ? OR
				(phone_number ~ '^[+0-9() .-]+$' AND regexp_replace(phone_number, '[^0-9]', '', 'g') = ?))`, forms, digits)
		} else {
			query = query.Where("phone_number IN ?", forms)
		}
		var err error
		phoneMatched, err = load(query)
		if err != nil {
			return nil, false, err
		}
	}
	if bsuid != "" {
		var err error
		bsuidMatched, err = load(tx.Unscoped().Where(
			"organization_id = ? AND BTRIM(bs_uid) = ?", organizationID, bsuid,
		))
		if err != nil {
			return nil, false, err
		}
	}
	if len(rows) == 0 {
		return nil, false, gorm.ErrRecordNotFound
	}

	ids := make([]uuid.UUID, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	var canonical *models.Contact
	for _, id := range ids {
		// The organization selector fence stabilizes merge/identity writers.
		// The caller locks the resolved canonical path before the final update.
		owner, err := contactutil.ResolveCanonicalContact(tx, organizationID, id)
		if err != nil {
			// Do not return ErrRecordNotFound for a selected deleted/corrupt
			// owner: only a completely empty census may authorize creation.
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, false, fmt.Errorf("%w: selector has no active canonical owner", errMarketingPreferenceIdentityConflict)
			}
			return nil, false, err
		}
		if canonical != nil && canonical.ID != owner.ID {
			return nil, false, errMarketingPreferenceIdentityConflict
		}
		canonical = owner
	}
	if userPhone != "" && !phoneMatched {
		// A known BSUID does not prove a newly supplied phone belongs to it.
		return nil, false, errMarketingPreferenceIdentityConflict
	}
	if bsuid == "" {
		return canonical, false, nil
	}
	if bsuidMatched {
		// Existing merge aliases are legitimate consent selectors. They may
		// fill an empty identity, but never rename an established canonical one.
		if strings.TrimSpace(canonical.BSUID) != "" {
			return canonical, false, nil
		}
	}
	if !bsuidMatched && (strings.TrimSpace(canonical.BSUID) != "" ||
		!contactHasDialablePhone(canonical) || isCoexistenceBSUIDAddress(userPhone) ||
		userPhone != normalizeCoexistencePhone(canonical.PhoneNumber)) {
		return nil, false, errMarketingPreferenceIdentityConflict
	}

	// An old phone alias or an apparently empty canonical row is insufficient
	// when its merged family, metadata or existing reviewed route belongs to
	// another principal. This is a read-only ownership census, not a new
	// message, onboarding-cycle claim, hold or approval.
	foreign, err := identityReviewMembersOfOtherUsers(tx, WhatsAppIdentityReviewClaim{
		OrganizationID: organizationID, DirectPrimaryBSUID: bsuid,
	}, []uuid.UUID{canonical.ID}, map[uuid.UUID]*models.Contact{canonical.ID: canonical})
	if err != nil {
		return nil, false, err
	}
	if foreign[canonical.ID] {
		if bsuidMatched {
			// Its existing alias proves consent ownership, but a mixed family
			// must not acquire a new canonical identity from a preference event.
			return canonical, false, nil
		}
		return nil, false, errMarketingPreferenceIdentityConflict
	}
	return canonical, true, nil
}
