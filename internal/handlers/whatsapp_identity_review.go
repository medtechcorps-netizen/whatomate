package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const WhatsAppIdentityReviewVerifiedMetaEvent = "meta_signed_webhook"

var (
	ErrWhatsAppIdentityReviewConflict     = errors.New("WhatsApp identity review state changed")
	ErrWhatsAppIdentityReviewInvalid      = errors.New("WhatsApp identity review input is invalid")
	ErrWhatsAppIdentityReviewUnauthorized = errors.New("WhatsApp identity review is not authorized")
)

// WhatsAppIdentityReviewClaim is the account-bound input from the authenticated
// WhatsApp admission path. It intentionally accepts no candidate IDs or routing
// target: those facts are derived again from tenant-owned rows under the common
// organization fence.
type WhatsAppIdentityReviewClaim struct {
	OrganizationID          uuid.UUID
	WhatsAppAccountID       uuid.UUID
	OnboardingCycle         uint64
	DirectPrimaryBSUID      string
	ParentBSUID             string
	Phone                   string
	VerifiedEventProvenance string
	VerifiedEventDigest     string
	SelectorBodyDigest      string
}

// WhatsAppIdentityReviewCandidate is a sanitized complete-set projection. No
// selector value, profile, message history, or provider data is exposed.
type WhatsAppIdentityReviewCandidate struct {
	ContactID       uuid.UUID                                   `json:"contact_id"`
	SelectorReasons models.WhatsAppIdentityReviewSelectorReason `json:"selector_reasons"`
}

// WhatsAppIdentityReviewSnapshot is the sanitized durable authority returned
// to webhook and HTTP layers. StoredHold/StoredMembers are excluded from JSON
// and allow internal callers to bind a staged event without reloading the rows.
type WhatsAppIdentityReviewSnapshot struct {
	HoldID                  uuid.UUID                                `json:"hold_id"`
	WhatsAppAccountID       uuid.UUID                                `json:"whatsapp_account_id"`
	OnboardingCycle         uint64                                   `json:"onboarding_cycle"`
	ProtocolVersion         uint16                                   `json:"protocol_version"`
	Supported               bool                                     `json:"supported"`
	PrincipalGeneration     uint64                                   `json:"principal_generation"`
	Disposition             models.WhatsAppIdentityReviewDisposition `json:"disposition"`
	Version                 uint64                                   `json:"version"`
	MemberCount             uint32                                   `json:"member_count"`
	MemberDigest            string                                   `json:"member_digest"`
	Candidates              []WhatsAppIdentityReviewCandidate        `json:"candidates"`
	DecisionTargetContactID *uuid.UUID                               `json:"decision_target_contact_id,omitempty"`
	DecisionRequestID       *uuid.UUID                               `json:"decision_request_id,omitempty"`
	DecisionChainDigest     string                                   `json:"decision_chain_digest,omitempty"`
	StoredHold              models.WhatsAppIdentityReviewHold        `json:"-"`
	StoredMembers           []models.WhatsAppIdentityReviewMember    `json:"-"`
}

type WhatsAppIdentityReviewAdmission struct {
	Candidates           []WhatsAppIdentityReviewCandidate `json:"candidates"`
	DirectPrimaryOwnerID *uuid.UUID                        `json:"direct_primary_owner_id,omitempty"`
	LatestHoldID         *uuid.UUID                        `json:"latest_hold_id,omitempty"`
	LatestGeneration     uint64                            `json:"latest_generation,omitempty"`
	RouteContactID       *uuid.UUID                        `json:"route_contact_id,omitempty"`
	Blocked              bool                              `json:"blocked"`
	NeedsReview          bool                              `json:"needs_review"`
	Reason               string                            `json:"reason"`
}

type WhatsAppIdentityReviewEffectiveState struct {
	Known            bool       `json:"known"`
	AIAllowed        bool       `json:"ai_allowed"`
	Blocked          bool       `json:"blocked"`
	OpenHoldCount    int64      `json:"open_hold_count"`
	LatestHoldID     *uuid.UUID `json:"latest_hold_id,omitempty"`
	LatestGeneration uint64     `json:"latest_generation,omitempty"`
	Reason           string     `json:"reason"`
}

type WhatsAppIdentityReviewPreviewInput struct {
	OrganizationID uuid.UUID `json:"-"`
	HoldID         uuid.UUID `json:"hold_id"`
	ResolverUserID uuid.UUID `json:"-"`
}

type WhatsAppIdentityReviewPreview struct {
	Snapshot        WhatsAppIdentityReviewSnapshot    `json:"snapshot"`
	ChainDigest     string                            `json:"chain_digest"`
	UnionCandidates []WhatsAppIdentityReviewCandidate `json:"union_candidates"`
	OpenGenerations []uint64                          `json:"open_generations"`
}

type WhatsAppIdentityReviewDecisionInput struct {
	OrganizationID       uuid.UUID `json:"-"`
	HoldID               uuid.UUID `json:"hold_id"`
	ResolverUserID       uuid.UUID `json:"-"`
	TargetContactID      uuid.UUID `json:"target_contact_id"`
	ExpectedVersion      uint64    `json:"expected_version"`
	ExpectedMemberDigest string    `json:"expected_member_digest"`
	ExpectedChainDigest  string    `json:"expected_chain_digest"`
	RequestID            uuid.UUID `json:"request_id"`
	RequestDigest        string    `json:"request_digest"`
}

type WhatsAppIdentityReviewDecisionResult struct {
	Snapshot              WhatsAppIdentityReviewSnapshot `json:"snapshot"`
	SupersededGenerations []uint64                       `json:"superseded_generations"`
	IdempotentReplay      bool                           `json:"idempotent_replay"`
}

type identityReviewCandidateSet struct {
	items               []WhatsAppIdentityReviewCandidate
	digest              string
	supported           bool
	directPrimaryOwners []uuid.UUID
	multipleOwners      bool
}

// CreateOrReuseWhatsAppIdentityReviewHold captures one complete immutable
// selector membership. tx must be the admission transaction; this method takes
// the shared organization fence before account, state, or contact rows.
func (a *App) CreateOrReuseWhatsAppIdentityReviewHold(
	tx *gorm.DB,
	claim *WhatsAppIdentityReviewClaim,
) (*WhatsAppIdentityReviewSnapshot, bool, error) {
	if a == nil || tx == nil || claim == nil {
		return nil, false, ErrWhatsAppIdentityReviewInvalid
	}
	normalized, err := normalizeWhatsAppIdentityReviewClaim(*claim)
	if err != nil {
		return nil, false, err
	}
	if err := database.LockOrganizationPolicyScope(tx, normalized.OrganizationID); err != nil {
		return nil, false, fmt.Errorf("lock identity-review hold policy fence: %w", err)
	}
	if err := lockAndVerifyWhatsAppIdentityReviewAuthority(tx, normalized); err != nil {
		return nil, false, err
	}
	candidates, err := enumerateWhatsAppIdentityReviewCandidates(tx, normalized)
	if err != nil {
		return nil, false, err
	}
	semanticDigest := identityReviewSemanticDigest(normalized, candidates.digest)
	supported := candidates.supported && normalized.DirectPrimaryBSUID != ""
	var generation uint64
	if supported {
		var latest models.WhatsAppIdentityReviewHold
		err = tx.Where(
			"organization_id = ? AND whats_app_account_id = ? AND onboarding_cycle = ? AND direct_primary_bsuid = ? AND supported",
			normalized.OrganizationID,
			normalized.WhatsAppAccountID,
			normalized.OnboardingCycle,
			normalized.DirectPrimaryBSUID,
		).Order("principal_generation DESC").First(&latest).Error
		if err == nil {
			if latest.SemanticClaimDigest == semanticDigest {
				snapshot, loadErr := loadWhatsAppIdentityReviewSnapshot(tx, &latest)
				if loadErr != nil {
					return nil, false, loadErr
				}
				if snapshot.MemberDigest != candidates.digest || !sameIdentityReviewCandidates(snapshot.Candidates, candidates.items) {
					return nil, false, ErrWhatsAppIdentityReviewConflict
				}
				return snapshot, false, nil
			}
			generation = latest.PrincipalGeneration + 1
		} else if errors.Is(err, gorm.ErrRecordNotFound) {
			generation = 1
		} else {
			return nil, false, fmt.Errorf("load latest identity-review generation: %w", err)
		}
		if generation == 0 {
			return nil, false, ErrWhatsAppIdentityReviewConflict
		}
	} else {
		// Unsupported holds have no routing principal or generation. Retain a
		// narrow semantic replay identity for repeated copies of the same held
		// claim, while supported A->B->A transitions remain distinct G1/G2/G3.
		var existing models.WhatsAppIdentityReviewHold
		err = tx.Where(
			"organization_id = ? AND whats_app_account_id = ? AND onboarding_cycle = ? AND protocol_version = ? AND semantic_claim_digest = ? AND NOT supported",
			normalized.OrganizationID,
			normalized.WhatsAppAccountID,
			normalized.OnboardingCycle,
			models.WhatsAppIdentityReviewProtocolVersion,
			semanticDigest,
		).First(&existing).Error
		if err == nil {
			snapshot, loadErr := loadWhatsAppIdentityReviewSnapshot(tx, &existing)
			if loadErr != nil {
				return nil, false, loadErr
			}
			if snapshot.MemberDigest != candidates.digest || !sameIdentityReviewCandidates(snapshot.Candidates, candidates.items) {
				return nil, false, ErrWhatsAppIdentityReviewConflict
			}
			return snapshot, false, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, fmt.Errorf("load unsupported semantic identity-review replay: %w", err)
		}
		generation = 0
	}
	// Generation allocation is serialized by the organization row lock above.
	// The database's exact principal/generation unique index remains the final
	// fail-closed guard against an accidental lock-order regression.

	hold := models.WhatsAppIdentityReviewHold{
		ID:                      uuid.New(),
		OrganizationID:          normalized.OrganizationID,
		WhatsAppAccountID:       normalized.WhatsAppAccountID,
		OnboardingCycle:         normalized.OnboardingCycle,
		ProtocolVersion:         models.WhatsAppIdentityReviewProtocolVersion,
		Supported:               supported,
		DirectPrimaryBSUID:      normalized.DirectPrimaryBSUID,
		ParentBSUID:             normalized.ParentBSUID,
		Phone:                   normalized.Phone,
		PrincipalGeneration:     generation,
		SemanticClaimDigest:     semanticDigest,
		SelectorBodyDigest:      normalized.SelectorBodyDigest,
		VerifiedEventDigest:     normalized.VerifiedEventDigest,
		VerifiedEventProvenance: normalized.VerifiedEventProvenance,
		MemberCount:             uint32(len(candidates.items)),
		MemberDigest:            candidates.digest,
		Version:                 1,
		Disposition:             models.WhatsAppIdentityReviewDispositionOpen,
	}
	if err := tx.Create(&hold).Error; err != nil {
		return nil, false, fmt.Errorf("create identity-review hold: %w", err)
	}
	members := make([]models.WhatsAppIdentityReviewMember, 0, len(candidates.items))
	for _, candidate := range candidates.items {
		members = append(members, models.WhatsAppIdentityReviewMember{
			OrganizationID:  normalized.OrganizationID,
			HoldID:          hold.ID,
			ContactID:       candidate.ContactID,
			SelectorReasons: candidate.SelectorReasons,
		})
	}
	if len(members) > 0 {
		if err := tx.Create(&members).Error; err != nil {
			return nil, false, fmt.Errorf("create identity-review membership: %w", err)
		}
	}
	snapshot, err := loadWhatsAppIdentityReviewSnapshot(tx, &hold)
	if err != nil {
		return nil, false, err
	}
	return snapshot, true, nil
}

// EvaluateWhatsAppIdentityReviewAdmission returns the only future contact that
// may be selected for a new durable admission. Before review, a missing or
// ambiguous principal, an open hold, or an authenticated selector contradiction
// blocks admission. An exact current future-routing decision may select only a
// member of the unchanged complete candidate set.
func (a *App) EvaluateWhatsAppIdentityReviewAdmission(
	tx *gorm.DB,
	claim *WhatsAppIdentityReviewClaim,
) (*WhatsAppIdentityReviewAdmission, error) {
	if a == nil || tx == nil || claim == nil {
		return nil, ErrWhatsAppIdentityReviewInvalid
	}
	normalized, err := normalizeWhatsAppIdentityReviewClaim(*claim)
	if err != nil {
		return nil, err
	}
	if err := lockAndVerifyWhatsAppIdentityReviewAuthority(tx, normalized); err != nil {
		return nil, err
	}
	candidates, err := enumerateWhatsAppIdentityReviewCandidates(tx, normalized)
	if err != nil {
		return nil, err
	}
	result := &WhatsAppIdentityReviewAdmission{
		Candidates:  candidates.items,
		Blocked:     true,
		NeedsReview: true,
	}
	if len(candidates.directPrimaryOwners) == 1 {
		owner := candidates.directPrimaryOwners[0]
		result.DirectPrimaryOwnerID = &owner
	}
	if normalized.DirectPrimaryBSUID == "" {
		result.Reason = "direct_primary_missing"
		return result, nil
	}
	if !candidates.supported || len(candidates.directPrimaryOwners) != 1 || candidates.multipleOwners {
		result.Reason = "direct_primary_ambiguous"
		return result, nil
	}
	directOwner := candidates.directPrimaryOwners[0]
	directRouteAllowed, selectorConflict := identityReviewDirectRouteAllowed(candidates.items, directOwner)

	var latest models.WhatsAppIdentityReviewHold
	err = tx.Where(
		"organization_id = ? AND whats_app_account_id = ? AND onboarding_cycle = ? AND direct_primary_bsuid = ? AND supported",
		normalized.OrganizationID,
		normalized.WhatsAppAccountID,
		normalized.OnboardingCycle,
		normalized.DirectPrimaryBSUID,
	).Order("principal_generation DESC").First(&latest).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if !directRouteAllowed {
			result.Reason = "primary_parent_conflict"
			return result, nil
		}
		result.RouteContactID = &directOwner
		if selectorConflict {
			result.Reason = "phone_selector_conflict"
		} else {
			blocked, blockErr := database.ContactHasBlockingIdentityReviewHold(tx, normalized.OrganizationID, directOwner)
			if blockErr != nil {
				return nil, blockErr
			}
			if blocked {
				result.Reason = "another_review_open"
				return result, nil
			}
			result.Blocked = false
			result.NeedsReview = false
			result.Reason = "unique_direct_primary"
		}
		return result, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load latest identity-review generation: %w", err)
	}
	result.LatestHoldID = &latest.ID
	result.LatestGeneration = latest.PrincipalGeneration
	latestSnapshot, err := loadWhatsAppIdentityReviewSnapshot(tx, &latest)
	if err != nil {
		return nil, err
	}
	semanticDigest := identityReviewSemanticDigest(normalized, candidates.digest)
	if latest.SemanticClaimDigest != semanticDigest || latestSnapshot.MemberDigest != candidates.digest ||
		!sameIdentityReviewCandidates(latestSnapshot.Candidates, candidates.items) {
		// Any changed selector or reason map is a new review generation. A
		// unique direct-primary owner may still receive this one new event (with
		// sticky suppression installed by admission), but drift must never fall
		// through as an unreviewed allow decision.
		if !directRouteAllowed {
			result.Reason = "primary_parent_conflict"
			return result, nil
		}
		result.RouteContactID = &directOwner
		if selectorConflict {
			result.Reason = "phone_selector_drift"
		} else {
			result.Reason = "unique_direct_primary_drift"
		}
		return result, nil
	}
	if latest.Disposition != models.WhatsAppIdentityReviewDispositionFutureRouting {
		if !directRouteAllowed {
			result.Reason = "primary_parent_conflict"
			return result, nil
		}
		result.RouteContactID = &directOwner
		result.Reason = "review_open"
		return result, nil
	}
	if latest.DecisionTargetContactID == nil {
		result.Reason = "decision_target_missing"
		return result, nil
	}
	if !identityReviewContainsContact(candidates.items, *latest.DecisionTargetContactID) {
		result.Reason = "decision_target_missing"
		return result, nil
	}
	target := *latest.DecisionTargetContactID
	result.RouteContactID = &target
	blocked, err := database.ContactHasBlockingIdentityReviewHold(tx, normalized.OrganizationID, *latest.DecisionTargetContactID)
	if err != nil {
		return nil, err
	}
	if blocked {
		result.Reason = "another_review_open"
		return result, nil
	}
	result.Blocked = false
	result.NeedsReview = false
	result.Reason = "reviewed_future_route"
	return result, nil
}

func (a *App) LoadWhatsAppIdentityReviewSnapshot(
	db *gorm.DB,
	organizationID, holdID uuid.UUID,
) (*WhatsAppIdentityReviewSnapshot, error) {
	if db == nil || organizationID == uuid.Nil || holdID == uuid.Nil {
		return nil, ErrWhatsAppIdentityReviewInvalid
	}
	var hold models.WhatsAppIdentityReviewHold
	if err := db.Where("organization_id = ? AND id = ?", organizationID, holdID).
		First(&hold).Error; err != nil {
		return nil, err
	}
	return loadWhatsAppIdentityReviewSnapshot(db, &hold)
}

func (a *App) GetWhatsAppIdentityReviewEffectiveState(
	db *gorm.DB,
	organizationID, contactID uuid.UUID,
) (WhatsAppIdentityReviewEffectiveState, error) {
	states, err := a.GetWhatsAppIdentityReviewEffectiveStates(db, organizationID, []uuid.UUID{contactID})
	if err != nil {
		return FailClosedWhatsAppIdentityReviewEffectiveState("identity_review_read_failed"), err
	}
	state, ok := states[contactID]
	if !ok {
		return FailClosedWhatsAppIdentityReviewEffectiveState("identity_review_unknown"), ErrWhatsAppIdentityReviewConflict
	}
	return state, nil
}

// FailClosedWhatsAppIdentityReviewEffectiveState is the only safe projection
// default. Callers must never let a zero value or a failed read become an AI
// allow decision.
func FailClosedWhatsAppIdentityReviewEffectiveState(reason string) WhatsAppIdentityReviewEffectiveState {
	if strings.TrimSpace(reason) == "" {
		reason = "identity_review_unknown"
	}
	return WhatsAppIdentityReviewEffectiveState{Known: false, AIAllowed: false, Blocked: true, Reason: reason}
}

// GetWhatsAppIdentityReviewEffectiveStates loads a bounded contact projection
// with two set queries, avoiding an N+1 read for contact/conversation lists.
// Every requested contact is present in the result on success.
func (a *App) GetWhatsAppIdentityReviewEffectiveStates(
	db *gorm.DB,
	organizationID uuid.UUID,
	contactIDs []uuid.UUID,
) (map[uuid.UUID]WhatsAppIdentityReviewEffectiveState, error) {
	if a == nil || db == nil || organizationID == uuid.Nil || len(contactIDs) == 0 || len(contactIDs) > 250 {
		return nil, ErrWhatsAppIdentityReviewInvalid
	}
	uniqueIDs := make([]uuid.UUID, 0, len(contactIDs))
	seen := make(map[uuid.UUID]struct{}, len(contactIDs))
	states := make(map[uuid.UUID]WhatsAppIdentityReviewEffectiveState, len(contactIDs))
	for _, contactID := range contactIDs {
		if contactID == uuid.Nil {
			return nil, ErrWhatsAppIdentityReviewInvalid
		}
		if _, exists := seen[contactID]; exists {
			continue
		}
		seen[contactID] = struct{}{}
		uniqueIDs = append(uniqueIDs, contactID)
		states[contactID] = WhatsAppIdentityReviewEffectiveState{
			Known: true, AIAllowed: true, Blocked: false, Reason: "no_open_identity_review",
		}
	}

	type openCountRow struct {
		ContactID uuid.UUID
		OpenCount int64
	}
	var openRows []openCountRow
	if err := db.Table("whatsapp_identity_review_members AS member").
		Select("member.contact_id, COUNT(*) AS open_count").
		Joins("JOIN whatsapp_identity_review_holds AS hold ON hold.organization_id = member.organization_id AND hold.id = member.hold_id").
		Where("member.organization_id = ? AND member.contact_id IN ? AND hold.disposition = ?",
			organizationID, uniqueIDs, models.WhatsAppIdentityReviewDispositionOpen).
		Group("member.contact_id").Scan(&openRows).Error; err != nil {
		return nil, fmt.Errorf("read identity-review open policy: %w", err)
	}
	for _, row := range openRows {
		state := states[row.ContactID]
		state.AIAllowed = false
		state.Blocked = true
		state.OpenHoldCount = row.OpenCount
		state.Reason = "identity_review_open"
		states[row.ContactID] = state
	}

	type latestRow struct {
		ContactID           uuid.UUID
		HoldID              uuid.UUID
		PrincipalGeneration uint64
	}
	var latestRows []latestRow
	if err := db.Raw(`SELECT DISTINCT ON (member.contact_id)
		member.contact_id, hold.id AS hold_id, hold.principal_generation
		FROM whatsapp_identity_review_members AS member
		JOIN whatsapp_identity_review_holds AS hold
		  ON hold.organization_id = member.organization_id AND hold.id = member.hold_id
		WHERE member.organization_id = ? AND member.contact_id IN ?
		ORDER BY member.contact_id,
		  CASE WHEN hold.disposition = ? THEN 0 ELSE 1 END,
		  hold.principal_generation DESC, hold.created_at DESC, hold.id DESC`,
		organizationID, uniqueIDs, models.WhatsAppIdentityReviewDispositionOpen).Scan(&latestRows).Error; err != nil {
		return nil, fmt.Errorf("read latest identity-review policy: %w", err)
	}
	for _, row := range latestRows {
		state := states[row.ContactID]
		holdID := row.HoldID
		state.LatestHoldID = &holdID
		state.LatestGeneration = row.PrincipalGeneration
		states[row.ContactID] = state
	}
	return states, nil
}

// PreviewWhatsAppIdentityReviewDecision must run in a transaction. It performs
// the same current candidate, latest-generation, open-chain, and complete-union
// authorization reads that Decide repeats immediately before its write.
func (a *App) PreviewWhatsAppIdentityReviewDecision(
	tx *gorm.DB,
	input WhatsAppIdentityReviewPreviewInput,
) (*WhatsAppIdentityReviewPreview, error) {
	return a.previewWhatsAppIdentityReviewDecision(tx, input, false)
}

func (a *App) previewWhatsAppIdentityReviewDecision(
	tx *gorm.DB,
	input WhatsAppIdentityReviewPreviewInput,
	lockHolds bool,
) (*WhatsAppIdentityReviewPreview, error) {
	if a == nil || tx == nil || input.OrganizationID == uuid.Nil || input.HoldID == uuid.Nil || input.ResolverUserID == uuid.Nil {
		return nil, ErrWhatsAppIdentityReviewInvalid
	}
	if err := lockWhatsAppIdentityReviewOrganization(tx, input.OrganizationID); err != nil {
		return nil, err
	}
	var hold models.WhatsAppIdentityReviewHold
	if err := tx.Where("organization_id = ? AND id = ?", input.OrganizationID, input.HoldID).
		First(&hold).Error; err != nil {
		return nil, err
	}
	normalized, err := normalizeWhatsAppIdentityReviewClaim(identityReviewClaimFromHold(hold))
	if err != nil {
		return nil, err
	}
	if err := lockAndVerifyWhatsAppIdentityReviewAuthority(tx, normalized); err != nil {
		return nil, err
	}
	if lockHolds {
		// The onboarding-cycle trigger owns State -> Hold order. Take the same
		// order here so a cycle change cannot deadlock with a decision. The hold
		// authority fields are immutable, but reload and compare them after the
		// lock so a concurrent terminal transition still fails closed below.
		var locked models.WhatsAppIdentityReviewHold
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND id = ?", input.OrganizationID, input.HoldID).
			First(&locked).Error; err != nil {
			return nil, err
		}
		lockedClaim, err := normalizeWhatsAppIdentityReviewClaim(identityReviewClaimFromHold(locked))
		if err != nil || lockedClaim != normalized {
			return nil, ErrWhatsAppIdentityReviewConflict
		}
		hold = locked
	}
	candidates, err := enumerateWhatsAppIdentityReviewCandidates(tx, normalized)
	if err != nil {
		return nil, err
	}

	if !hold.Supported || hold.Disposition != models.WhatsAppIdentityReviewDispositionOpen ||
		hold.WhatsAppAccountID != normalized.WhatsAppAccountID ||
		hold.OnboardingCycle != normalized.OnboardingCycle ||
		hold.DirectPrimaryBSUID != normalized.DirectPrimaryBSUID {
		return nil, ErrWhatsAppIdentityReviewConflict
	}
	var latestGeneration uint64
	if err := tx.Model(&models.WhatsAppIdentityReviewHold{}).
		Where("organization_id = ? AND whats_app_account_id = ? AND onboarding_cycle = ? AND direct_primary_bsuid = ? AND supported",
			normalized.OrganizationID, normalized.WhatsAppAccountID, normalized.OnboardingCycle, normalized.DirectPrimaryBSUID).
		Select("COALESCE(MAX(principal_generation), 0)").Scan(&latestGeneration).Error; err != nil {
		return nil, err
	}
	if latestGeneration != hold.PrincipalGeneration {
		return nil, ErrWhatsAppIdentityReviewConflict
	}
	snapshot, err := loadWhatsAppIdentityReviewSnapshot(tx, &hold)
	if err != nil {
		return nil, err
	}
	if snapshot.MemberDigest != candidates.digest || !sameIdentityReviewCandidates(snapshot.Candidates, candidates.items) {
		return nil, ErrWhatsAppIdentityReviewConflict
	}

	openQuery := tx.Where(
		"organization_id = ? AND whats_app_account_id = ? AND onboarding_cycle = ? AND direct_primary_bsuid = ? AND supported AND disposition = ? AND principal_generation <= ?",
		normalized.OrganizationID, normalized.WhatsAppAccountID, normalized.OnboardingCycle,
		normalized.DirectPrimaryBSUID, models.WhatsAppIdentityReviewDispositionOpen,
		hold.PrincipalGeneration,
	).Order("principal_generation, id")
	if lockHolds {
		openQuery = openQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var openHolds []models.WhatsAppIdentityReviewHold
	if err := openQuery.Find(&openHolds).Error; err != nil {
		return nil, err
	}
	if len(openHolds) == 0 || openHolds[len(openHolds)-1].ID != hold.ID {
		return nil, ErrWhatsAppIdentityReviewConflict
	}
	union := make(map[uuid.UUID]models.WhatsAppIdentityReviewSelectorReason)
	openGenerations := make([]uint64, 0, len(openHolds))
	chainParts := make([]string, 0, len(openHolds))
	for index := range openHolds {
		generationSnapshot, loadErr := loadWhatsAppIdentityReviewSnapshot(tx, &openHolds[index])
		if loadErr != nil {
			return nil, loadErr
		}
		openGenerations = append(openGenerations, openHolds[index].PrincipalGeneration)
		chainParts = append(chainParts, identityReviewChainPart(openHolds[index]))
		for _, member := range generationSnapshot.Candidates {
			union[member.ContactID] |= member.SelectorReasons
		}
	}
	unionCandidates := identityReviewCandidateMapSlice(union)
	if err := a.authorizeWhatsAppIdentityReviewUnion(tx, normalized.OrganizationID, input.ResolverUserID, unionCandidates); err != nil {
		return nil, err
	}
	return &WhatsAppIdentityReviewPreview{
		Snapshot:        *snapshot,
		ChainDigest:     sha256Hex(strings.Join(chainParts, "\n")),
		UnionCandidates: unionCandidates,
		OpenGenerations: openGenerations,
	}, nil
}

func (a *App) DecideWhatsAppIdentityReview(
	tx *gorm.DB,
	input WhatsAppIdentityReviewDecisionInput,
) (*WhatsAppIdentityReviewDecisionResult, error) {
	if a == nil || tx == nil || input.OrganizationID == uuid.Nil || input.HoldID == uuid.Nil ||
		input.ResolverUserID == uuid.Nil || input.RequestID == uuid.Nil || input.TargetContactID == uuid.Nil ||
		input.ExpectedVersion == 0 || !isSHA256Hex(input.RequestDigest) ||
		!isSHA256Hex(input.ExpectedMemberDigest) || !isSHA256Hex(input.ExpectedChainDigest) {
		return nil, ErrWhatsAppIdentityReviewInvalid
	}
	input.ExpectedMemberDigest = strings.ToLower(strings.TrimSpace(input.ExpectedMemberDigest))
	input.ExpectedChainDigest = strings.ToLower(strings.TrimSpace(input.ExpectedChainDigest))
	input.RequestDigest = strings.ToLower(strings.TrimSpace(input.RequestDigest))
	if input.RequestDigest != WhatsAppIdentityReviewDecisionRequestDigest(input) {
		return nil, ErrWhatsAppIdentityReviewInvalid
	}
	if err := database.LockOrganizationPolicyScope(tx, input.OrganizationID); err != nil {
		return nil, fmt.Errorf("lock identity-review decision policy fence: %w", err)
	}
	var replay models.WhatsAppIdentityReviewHold
	err := tx.Where(
		"organization_id = ? AND decision_request_id = ? AND disposition = ?",
		input.OrganizationID, input.RequestID, models.WhatsAppIdentityReviewDispositionFutureRouting,
	).First(&replay).Error
	if err == nil {
		if replay.ID != input.HoldID || replay.DecisionRequestDigest != strings.ToLower(input.RequestDigest) ||
			replay.DecisionTargetContactID == nil || *replay.DecisionTargetContactID != input.TargetContactID ||
			replay.DecisionResolvedByID == nil || *replay.DecisionResolvedByID != input.ResolverUserID ||
			replay.DecisionChainDigest != input.ExpectedChainDigest {
			return nil, ErrWhatsAppIdentityReviewConflict
		}
		return a.reauthorizeWhatsAppIdentityReviewReplay(tx, input, &replay)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	preview, err := a.previewWhatsAppIdentityReviewDecision(tx, WhatsAppIdentityReviewPreviewInput{
		OrganizationID: input.OrganizationID, HoldID: input.HoldID, ResolverUserID: input.ResolverUserID,
	}, true)
	if err != nil {
		return nil, err
	}
	if preview.Snapshot.Version != input.ExpectedVersion ||
		preview.Snapshot.MemberDigest != input.ExpectedMemberDigest ||
		preview.ChainDigest != input.ExpectedChainDigest ||
		!identityReviewContainsContact(preview.Snapshot.Candidates, input.TargetContactID) {
		return nil, ErrWhatsAppIdentityReviewConflict
	}

	now := time.Now().UTC()
	requestDigest := input.RequestDigest
	result := tx.Model(&models.WhatsAppIdentityReviewHold{}).
		Where("organization_id = ? AND id = ? AND version = ? AND disposition = ?",
			input.OrganizationID, input.HoldID, input.ExpectedVersion,
			models.WhatsAppIdentityReviewDispositionOpen).
		Updates(map[string]any{
			"version":                    input.ExpectedVersion + 1,
			"disposition":                models.WhatsAppIdentityReviewDispositionFutureRouting,
			"decision_target_contact_id": input.TargetContactID,
			"decision_resolved_by_id":    input.ResolverUserID,
			"decision_resolved_at":       now,
			"decision_request_id":        input.RequestID,
			"decision_request_digest":    requestDigest,
			"decision_chain_digest":      preview.ChainDigest,
		})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrWhatsAppIdentityReviewConflict
	}

	superseded := make([]uint64, 0, len(preview.OpenGenerations)-1)
	for _, generation := range preview.OpenGenerations {
		if generation == preview.Snapshot.PrincipalGeneration {
			continue
		}
		update := tx.Model(&models.WhatsAppIdentityReviewHold{}).
			Where(
				"organization_id = ? AND whats_app_account_id = ? AND onboarding_cycle = ? AND direct_primary_bsuid = ? AND supported AND principal_generation = ? AND disposition = ?",
				preview.Snapshot.StoredHold.OrganizationID, preview.Snapshot.WhatsAppAccountID, preview.Snapshot.OnboardingCycle,
				preview.Snapshot.StoredHold.DirectPrimaryBSUID, generation,
				models.WhatsAppIdentityReviewDispositionOpen,
			).
			Updates(map[string]any{
				"version":                  gorm.Expr("version + 1"),
				"disposition":              models.WhatsAppIdentityReviewDispositionSupersededByLatest,
				"decision_resolved_by_id":  input.ResolverUserID,
				"decision_resolved_at":     now,
				"decision_request_id":      input.RequestID,
				"decision_request_digest":  requestDigest,
				"decision_chain_digest":    preview.ChainDigest,
				"superseded_by_hold_id":    input.HoldID,
				"superseded_by_generation": preview.Snapshot.PrincipalGeneration,
			})
		if update.Error != nil {
			return nil, update.Error
		}
		if update.RowsAffected != 1 {
			return nil, ErrWhatsAppIdentityReviewConflict
		}
		superseded = append(superseded, generation)
	}
	updated, err := a.LoadWhatsAppIdentityReviewSnapshot(tx, input.OrganizationID, input.HoldID)
	if err != nil {
		return nil, err
	}
	return &WhatsAppIdentityReviewDecisionResult{
		Snapshot: *updated, SupersededGenerations: superseded,
	}, nil
}

// WhatsAppIdentityReviewDecisionRequestDigest is the canonical schema-1 body
// digest. HTTP code may use it to populate RequestDigest, while Decide always
// recomputes and verifies it before either a mutation or an idempotent replay.
func WhatsAppIdentityReviewDecisionRequestDigest(input WhatsAppIdentityReviewDecisionInput) string {
	return sha256Hex(strings.Join([]string{
		"whatsapp_identity_review_decision_v1",
		input.HoldID.String(),
		input.TargetContactID.String(),
		strconv.FormatUint(input.ExpectedVersion, 10),
		strings.ToLower(strings.TrimSpace(input.ExpectedMemberDigest)),
		strings.ToLower(strings.TrimSpace(input.ExpectedChainDigest)),
		input.RequestID.String(),
	}, "\n"))
}

func (a *App) reauthorizeWhatsAppIdentityReviewReplay(
	tx *gorm.DB,
	input WhatsAppIdentityReviewDecisionInput,
	latest *models.WhatsAppIdentityReviewHold,
) (*WhatsAppIdentityReviewDecisionResult, error) {
	if latest == nil || latest.Disposition != models.WhatsAppIdentityReviewDispositionFutureRouting ||
		latest.Version != input.ExpectedVersion+1 || latest.MemberDigest != input.ExpectedMemberDigest {
		return nil, ErrWhatsAppIdentityReviewConflict
	}
	var chain []models.WhatsAppIdentityReviewHold
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where(
		"organization_id = ? AND decision_request_id = ? AND decision_request_digest = ? AND decision_chain_digest = ?",
		input.OrganizationID, input.RequestID, input.RequestDigest, input.ExpectedChainDigest,
	).Order("principal_generation, id").Find(&chain).Error; err != nil {
		return nil, err
	}
	if len(chain) == 0 || chain[len(chain)-1].ID != latest.ID {
		return nil, ErrWhatsAppIdentityReviewConflict
	}
	chainParts := make([]string, 0, len(chain))
	union := make(map[uuid.UUID]models.WhatsAppIdentityReviewSelectorReason)
	superseded := make([]uint64, 0, len(chain)-1)
	for index := range chain {
		hold := &chain[index]
		if !hold.Supported || hold.Version != 2 || hold.WhatsAppAccountID != latest.WhatsAppAccountID ||
			hold.OnboardingCycle != latest.OnboardingCycle || hold.DirectPrimaryBSUID != latest.DirectPrimaryBSUID ||
			hold.DecisionResolvedByID == nil || *hold.DecisionResolvedByID != input.ResolverUserID {
			return nil, ErrWhatsAppIdentityReviewConflict
		}
		if hold.ID == latest.ID {
			if hold.Disposition != models.WhatsAppIdentityReviewDispositionFutureRouting {
				return nil, ErrWhatsAppIdentityReviewConflict
			}
		} else {
			if hold.Disposition != models.WhatsAppIdentityReviewDispositionSupersededByLatest ||
				hold.SupersededByHoldID == nil || *hold.SupersededByHoldID != latest.ID ||
				hold.SupersededByGeneration == nil || *hold.SupersededByGeneration != latest.PrincipalGeneration {
				return nil, ErrWhatsAppIdentityReviewConflict
			}
			superseded = append(superseded, hold.PrincipalGeneration)
		}
		snapshot, err := loadWhatsAppIdentityReviewSnapshot(tx, hold)
		if err != nil {
			return nil, err
		}
		for _, member := range snapshot.Candidates {
			union[member.ContactID] |= member.SelectorReasons
		}
		chainParts = append(chainParts, identityReviewChainPartAtVersion(*hold, hold.Version-1))
	}
	if sha256Hex(strings.Join(chainParts, "\n")) != input.ExpectedChainDigest {
		return nil, ErrWhatsAppIdentityReviewConflict
	}
	unionCandidates := identityReviewCandidateMapSlice(union)
	if !identityReviewContainsContact(unionCandidates, input.TargetContactID) {
		return nil, ErrWhatsAppIdentityReviewConflict
	}
	if err := a.authorizeWhatsAppIdentityReviewUnion(
		tx, input.OrganizationID, input.ResolverUserID, unionCandidates,
	); err != nil {
		return nil, err
	}
	snapshot, err := loadWhatsAppIdentityReviewSnapshot(tx, latest)
	if err != nil {
		return nil, err
	}
	return &WhatsAppIdentityReviewDecisionResult{
		Snapshot: *snapshot, SupersededGenerations: superseded, IdempotentReplay: true,
	}, nil
}

func lockAndVerifyWhatsAppIdentityReviewAuthority(tx *gorm.DB, claim WhatsAppIdentityReviewClaim) error {
	if err := lockWhatsAppIdentityReviewOrganization(tx, claim.OrganizationID); err != nil {
		return err
	}
	var account models.WhatsAppAccount
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id", "organization_id").
		Where("id = ? AND organization_id = ?", claim.WhatsAppAccountID, claim.OrganizationID).
		First(&account).Error; err != nil {
		return fmt.Errorf("verify identity-review account: %w", err)
	}
	var state models.WhatsAppCoexistenceState
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("organization_id = ? AND whats_app_account_id = ?", claim.OrganizationID, claim.WhatsAppAccountID).
		First(&state).Error; err != nil {
		return fmt.Errorf("verify identity-review onboarding state: %w", err)
	}
	if state.OnboardingCycle != claim.OnboardingCycle ||
		state.LifecycleStatus != models.CoexistenceLifecycleStatusConnected ||
		state.OnboardingStatus == models.CoexistenceOnboardingStatusOffboarded {
		return ErrWhatsAppIdentityReviewConflict
	}
	return nil
}

func lockWhatsAppIdentityReviewOrganization(tx *gorm.DB, organizationID uuid.UUID) error {
	if tx == nil || organizationID == uuid.Nil {
		return ErrWhatsAppIdentityReviewInvalid
	}
	if err := tx.Exec(
		"SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))",
		database.WhatsAppIdentityReviewContactSelectorFenceKey(organizationID),
	).Error; err != nil {
		return fmt.Errorf("lock identity-review selector fence: %w", err)
	}
	var organization models.Organization
	if err := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).
		Select("id").Where("id = ?", organizationID).First(&organization).Error; err != nil {
		return fmt.Errorf("lock identity-review organization fence: %w", err)
	}
	return nil
}

func enumerateWhatsAppIdentityReviewCandidates(
	tx *gorm.DB,
	claim WhatsAppIdentityReviewClaim,
) (identityReviewCandidateSet, error) {
	reasonsByRawID := make(map[uuid.UUID]models.WhatsAppIdentityReviewSelectorReason)
	rawByID := make(map[uuid.UUID]models.Contact)
	load := func(column, value string, reason models.WhatsAppIdentityReviewSelectorReason) error {
		if value == "" {
			return nil
		}
		var rows []models.Contact
		query := tx.Unscoped().Where("organization_id = ?", claim.OrganizationID)
		if column == "phone_number" {
			// Existing manual and CSV-import records may retain presentation
			// punctuation. Compare the same ASCII-digit projection used for the
			// authenticated inbound claim so those owners cannot be omitted.
			query = query.Where("regexp_replace(phone_number, '[^0-9]', '', 'g') = ?", value)
		} else {
			query = query.Where(column+" = ?", value)
		}
		if err := query.Order("id").Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			rawByID[row.ID] = row
			reasonsByRawID[row.ID] |= reason
		}
		return nil
	}
	if err := load("bs_uid", claim.DirectPrimaryBSUID, models.WhatsAppIdentityReviewSelectorPrimaryBSUID); err != nil {
		return identityReviewCandidateSet{}, err
	}
	if err := load("bs_uid", claim.ParentBSUID, models.WhatsAppIdentityReviewSelectorParentBSUID); err != nil {
		return identityReviewCandidateSet{}, err
	}
	if err := load("phone_number", claim.Phone, models.WhatsAppIdentityReviewSelectorPhone); err != nil {
		return identityReviewCandidateSet{}, err
	}

	rawIDs := make([]uuid.UUID, 0, len(rawByID))
	for id := range rawByID {
		rawIDs = append(rawIDs, id)
	}
	sort.Slice(rawIDs, func(i, j int) bool { return rawIDs[i].String() < rawIDs[j].String() })
	reasons := make(map[uuid.UUID]models.WhatsAppIdentityReviewSelectorReason)
	supported := claim.DirectPrimaryBSUID != ""
	for _, rawID := range rawIDs {
		// The organization-scoped selector fence prevents every selector or merge
		// path mutation until this transaction completes, so a non-locking walk is
		// both stable and avoids a contact-row/selector-fence lock inversion.
		canonical, err := contactutil.ResolveCanonicalContact(tx, claim.OrganizationID, rawID)
		if err != nil {
			// Preserve a corrupt/deleted raw owner in immutable evidence but mark the
			// hold unsupported so it can never become routing authority.
			supported = false
			reasons[rawID] |= reasonsByRawID[rawID]
			continue
		}
		reasons[canonical.ID] |= reasonsByRawID[rawID]
	}
	items := identityReviewCandidateMapSlice(reasons)
	directOwners := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		if item.SelectorReasons.Has(models.WhatsAppIdentityReviewSelectorPrimaryBSUID) {
			directOwners = append(directOwners, item.ContactID)
		}
	}
	if len(directOwners) == 0 {
		supported = false
	}
	return identityReviewCandidateSet{
		items: items, digest: identityReviewMemberDigest(items), supported: supported,
		directPrimaryOwners: directOwners,
		multipleOwners:      identityReviewHasMultipleOwners(items),
	}, nil
}

func (a *App) authorizeWhatsAppIdentityReviewUnion(
	db *gorm.DB,
	organizationID, userID uuid.UUID,
	candidates []WhatsAppIdentityReviewCandidate,
) error {
	authority := a.scopedApp(db, organizationID)
	if !authority.HasPermission(userID, models.ResourceContacts, models.ActionWrite, organizationID) {
		return ErrWhatsAppIdentityReviewUnauthorized
	}
	if !authority.HasPermission(userID, models.ResourceContactsIdentityReview, models.ActionWrite, organizationID) {
		return ErrWhatsAppIdentityReviewUnauthorized
	}
	if authority.HasPermission(userID, models.ResourceContacts, models.ActionRead, organizationID) {
		return nil
	}
	for _, candidate := range candidates {
		var accessible int64
		if err := db.Model(&models.Contact{}).
			Where("organization_id = ? AND id = ? AND (assigned_user_id = ? OR id IN (?))",
				organizationID, candidate.ContactID, userID,
				db.Model(&models.AgentTransfer{}).Select("contact_id").Where(
					"organization_id = ? AND contact_id = ? AND agent_id = ? AND status = ?",
					organizationID, candidate.ContactID, userID, models.TransferStatusActive,
				)).Count(&accessible).Error; err != nil {
			return err
		}
		if accessible != 1 {
			return ErrWhatsAppIdentityReviewUnauthorized
		}
	}
	return nil
}

func loadWhatsAppIdentityReviewSnapshot(
	db *gorm.DB,
	hold *models.WhatsAppIdentityReviewHold,
) (*WhatsAppIdentityReviewSnapshot, error) {
	if db == nil || hold == nil || hold.ID == uuid.Nil || hold.OrganizationID == uuid.Nil {
		return nil, ErrWhatsAppIdentityReviewInvalid
	}
	var members []models.WhatsAppIdentityReviewMember
	if err := db.Where("organization_id = ? AND hold_id = ?", hold.OrganizationID, hold.ID).
		Order("contact_id").Find(&members).Error; err != nil {
		return nil, err
	}
	candidates := make([]WhatsAppIdentityReviewCandidate, 0, len(members))
	for _, member := range members {
		if !member.SelectorReasons.Valid() {
			return nil, ErrWhatsAppIdentityReviewConflict
		}
		candidates = append(candidates, WhatsAppIdentityReviewCandidate{
			ContactID: member.ContactID, SelectorReasons: member.SelectorReasons,
		})
	}
	if uint32(len(members)) != hold.MemberCount || identityReviewMemberDigest(candidates) != hold.MemberDigest {
		return nil, ErrWhatsAppIdentityReviewConflict
	}
	return &WhatsAppIdentityReviewSnapshot{
		HoldID: hold.ID, WhatsAppAccountID: hold.WhatsAppAccountID,
		OnboardingCycle: hold.OnboardingCycle, ProtocolVersion: hold.ProtocolVersion,
		Supported: hold.Supported, PrincipalGeneration: hold.PrincipalGeneration,
		Disposition: hold.Disposition, Version: hold.Version,
		MemberCount: hold.MemberCount, MemberDigest: hold.MemberDigest,
		Candidates: candidates, DecisionTargetContactID: hold.DecisionTargetContactID,
		DecisionRequestID: hold.DecisionRequestID, DecisionChainDigest: hold.DecisionChainDigest,
		StoredHold: *hold, StoredMembers: members,
	}, nil
}

func normalizeWhatsAppIdentityReviewClaim(claim WhatsAppIdentityReviewClaim) (WhatsAppIdentityReviewClaim, error) {
	if claim.OrganizationID == uuid.Nil || claim.WhatsAppAccountID == uuid.Nil || claim.OnboardingCycle == 0 {
		return WhatsAppIdentityReviewClaim{}, ErrWhatsAppIdentityReviewInvalid
	}
	claim.DirectPrimaryBSUID = strings.TrimSpace(claim.DirectPrimaryBSUID)
	claim.ParentBSUID = strings.TrimSpace(claim.ParentBSUID)
	claim.Phone = normalizeIdentityReviewPhone(claim.Phone)
	claim.VerifiedEventProvenance = strings.TrimSpace(claim.VerifiedEventProvenance)
	claim.VerifiedEventDigest = strings.ToLower(strings.TrimSpace(claim.VerifiedEventDigest))
	claim.SelectorBodyDigest = strings.ToLower(strings.TrimSpace(claim.SelectorBodyDigest))
	if len(claim.DirectPrimaryBSUID) > 255 || len(claim.ParentBSUID) > 255 || len(claim.Phone) > 50 ||
		claim.VerifiedEventProvenance != WhatsAppIdentityReviewVerifiedMetaEvent ||
		!isSHA256Hex(claim.VerifiedEventDigest) || !isSHA256Hex(claim.SelectorBodyDigest) {
		return WhatsAppIdentityReviewClaim{}, ErrWhatsAppIdentityReviewInvalid
	}
	return claim, nil
}

func identityReviewClaimFromHold(hold models.WhatsAppIdentityReviewHold) WhatsAppIdentityReviewClaim {
	return WhatsAppIdentityReviewClaim{
		OrganizationID:          hold.OrganizationID,
		WhatsAppAccountID:       hold.WhatsAppAccountID,
		OnboardingCycle:         hold.OnboardingCycle,
		DirectPrimaryBSUID:      hold.DirectPrimaryBSUID,
		ParentBSUID:             hold.ParentBSUID,
		Phone:                   hold.Phone,
		VerifiedEventProvenance: hold.VerifiedEventProvenance,
		VerifiedEventDigest:     hold.VerifiedEventDigest,
		SelectorBodyDigest:      hold.SelectorBodyDigest,
	}
}

func normalizeIdentityReviewPhone(value string) string {
	var normalized strings.Builder
	for _, char := range strings.TrimSpace(value) {
		if char >= '0' && char <= '9' {
			normalized.WriteRune(char)
		}
	}
	return normalized.String()
}

func identityReviewCandidateMapSlice(
	reasons map[uuid.UUID]models.WhatsAppIdentityReviewSelectorReason,
) []WhatsAppIdentityReviewCandidate {
	items := make([]WhatsAppIdentityReviewCandidate, 0, len(reasons))
	for contactID, selectorReasons := range reasons {
		items = append(items, WhatsAppIdentityReviewCandidate{ContactID: contactID, SelectorReasons: selectorReasons})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ContactID.String() < items[j].ContactID.String() })
	return items
}

func identityReviewMemberDigest(items []WhatsAppIdentityReviewCandidate) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, item.ContactID.String()+":"+strconv.FormatUint(uint64(item.SelectorReasons), 10))
	}
	return sha256Hex(strings.Join(parts, "\n"))
}

func identityReviewSemanticDigest(claim WhatsAppIdentityReviewClaim, memberDigest string) string {
	return sha256Hex(strings.Join([]string{
		"whatsapp_identity_review_v1",
		claim.OrganizationID.String(),
		claim.WhatsAppAccountID.String(),
		strconv.FormatUint(claim.OnboardingCycle, 10),
		claim.DirectPrimaryBSUID,
		claim.ParentBSUID,
		claim.Phone,
		memberDigest,
	}, "\n"))
}

func identityReviewChainPart(hold models.WhatsAppIdentityReviewHold) string {
	return identityReviewChainPartAtVersion(hold, hold.Version)
}

func identityReviewChainPartAtVersion(hold models.WhatsAppIdentityReviewHold, version uint64) string {
	return strings.Join([]string{
		strconv.FormatUint(hold.PrincipalGeneration, 10), hold.ID.String(),
		strconv.FormatUint(version, 10), hold.MemberDigest,
	}, ":")
}

func identityReviewHasMultipleOwners(items []WhatsAppIdentityReviewCandidate) bool {
	for _, flag := range []models.WhatsAppIdentityReviewSelectorReason{
		models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
		models.WhatsAppIdentityReviewSelectorParentBSUID,
		models.WhatsAppIdentityReviewSelectorPhone,
	} {
		count := 0
		for _, item := range items {
			if item.SelectorReasons.Has(flag) {
				count++
			}
		}
		if count > 1 {
			return true
		}
	}
	return false
}

// identityReviewDirectRouteAllowed distinguishes an ordinary phone-only
// disagreement from an unsafe authenticated primary/parent contradiction.
// The former may still be stored on the proven direct owner with sticky AI
// suppression; the latter must remain contact-free staged intake.
func identityReviewDirectRouteAllowed(items []WhatsAppIdentityReviewCandidate, directOwner uuid.UUID) (bool, bool) {
	selectorConflict := false
	for _, item := range items {
		if item.ContactID == directOwner {
			continue
		}
		selectorConflict = true
		if item.SelectorReasons.Has(models.WhatsAppIdentityReviewSelectorPrimaryBSUID) ||
			item.SelectorReasons.Has(models.WhatsAppIdentityReviewSelectorParentBSUID) {
			return false, true
		}
	}
	return true, selectorConflict
}

func identityReviewContainsContact(items []WhatsAppIdentityReviewCandidate, target uuid.UUID) bool {
	for _, item := range items {
		if item.ContactID == target {
			return true
		}
	}
	return false
}

func sameIdentityReviewCandidates(left, right []WhatsAppIdentityReviewCandidate) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func isSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
