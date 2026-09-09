package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestNormalizeWhatsAppIdentityReviewClaimNeverFallsBackToParent(t *testing.T) {
	t.Parallel()

	claim, err := normalizeWhatsAppIdentityReviewClaim(WhatsAppIdentityReviewClaim{
		OrganizationID: uuid.New(), WhatsAppAccountID: uuid.New(), OnboardingCycle: 4,
		DirectPrimaryBSUID: "   ", ParentBSUID: " parent-only ", Phone: "+60 (12) 345-6789",
		VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
		VerifiedEventDigest:     strings.Repeat("a", 64), SelectorBodyDigest: strings.Repeat("b", 64),
	})
	require.NoError(t, err)
	assert.Empty(t, claim.DirectPrimaryBSUID)
	assert.Equal(t, "parent-only", claim.ParentBSUID)
	assert.Equal(t, "60123456789", claim.Phone)

	claim.VerifiedEventProvenance = "caller_asserted"
	_, err = normalizeWhatsAppIdentityReviewClaim(claim)
	require.ErrorIs(t, err, ErrWhatsAppIdentityReviewInvalid)
}

func TestIdentityReviewCandidateReasonsAndDirectRouteClassification(t *testing.T) {
	t.Parallel()

	directID := uuid.New()
	phoneID := uuid.New()
	phoneConflict := []WhatsAppIdentityReviewCandidate{
		{ContactID: directID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID | models.WhatsAppIdentityReviewSelectorParentBSUID},
		{ContactID: phoneID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPhone},
	}
	allowed, conflict := identityReviewDirectRouteAllowed(phoneConflict, directID)
	assert.True(t, allowed, "phone-only disagreement may preserve proven direct owner")
	assert.True(t, conflict)
	assert.False(t, identityReviewHasMultipleOwners(phoneConflict))

	parentConflict := append([]WhatsAppIdentityReviewCandidate(nil), phoneConflict...)
	parentConflict[1].SelectorReasons |= models.WhatsAppIdentityReviewSelectorParentBSUID
	allowed, conflict = identityReviewDirectRouteAllowed(parentConflict, directID)
	assert.False(t, allowed, "a contradictory authenticated parent must remain contact-free")
	assert.True(t, conflict)
	assert.True(t, identityReviewHasMultipleOwners(parentConflict))
}

func TestIdentityReviewDedicatedPermissionIsRequiredForPhoneOnlyConflict(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	organization := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"identity-review-without-dedicated-authority",
		[]string{"contacts:read", "contacts:write"},
	)
	user := testutil.CreateTestUser(
		t,
		db,
		organization.ID,
		testutil.WithRoleID(&role.ID),
	)
	app := &App{DB: db, Log: testutil.NopLogger()}
	candidates := []WhatsAppIdentityReviewCandidate{
		{
			ContactID:       uuid.New(),
			SelectorReasons: models.WhatsAppIdentityReviewSelectorPrimaryBSUID,
		},
		{
			ContactID:       uuid.New(),
			SelectorReasons: models.WhatsAppIdentityReviewSelectorPhone,
		},
	}
	require.False(t, identityReviewHasMultipleOwners(candidates))
	require.ErrorIs(
		t,
		app.authorizeWhatsAppIdentityReviewUnion(
			db,
			organization.ID,
			user.ID,
			candidates,
		),
		ErrWhatsAppIdentityReviewUnauthorized,
	)

	roleWithAuthority := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organization.ID,
		"identity-review-with-dedicated-authority",
		[]string{
			"contacts:read",
			"contacts:write",
			"contacts.identity_review:write",
		},
	)
	authorized := testutil.CreateTestUser(
		t,
		db,
		organization.ID,
		testutil.WithRoleID(&roleWithAuthority.ID),
	)
	require.NoError(t, app.authorizeWhatsAppIdentityReviewUnion(
		db,
		organization.ID,
		authorized.ID,
		candidates,
	))
}

func TestIdentityReviewCanonicalMemberDigestIsOrderIndependentAfterMapProjection(t *testing.T) {
	t.Parallel()

	firstID := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	secondID := uuid.MustParse("00000000-0000-4000-8000-000000000002")
	reasons := map[uuid.UUID]models.WhatsAppIdentityReviewSelectorReason{
		secondID: models.WhatsAppIdentityReviewSelectorPhone,
		firstID:  models.WhatsAppIdentityReviewSelectorPrimaryBSUID | models.WhatsAppIdentityReviewSelectorParentBSUID,
	}
	items := identityReviewCandidateMapSlice(reasons)
	require.Len(t, items, 2)
	assert.Equal(t, firstID, items[0].ContactID)
	assert.Equal(t, secondID, items[1].ContactID)
	assert.Equal(t, identityReviewMemberDigest(items), identityReviewMemberDigest(identityReviewCandidateMapSlice(reasons)))
}

func TestWhatsAppIdentityReviewDecisionRequestDigestHasExactDomainSeparatedEncoding(t *testing.T) {
	t.Parallel()

	input := WhatsAppIdentityReviewDecisionInput{
		HoldID:          uuid.MustParse("00000000-0000-4000-8000-000000000010"),
		TargetContactID: uuid.MustParse("00000000-0000-4000-8000-000000000011"),
		ExpectedVersion: 7, ExpectedMemberDigest: strings.Repeat("a", 64),
		ExpectedChainDigest: strings.Repeat("b", 64),
		RequestID:           uuid.MustParse("00000000-0000-4000-8000-000000000012"),
	}
	canonical := strings.Join([]string{
		"whatsapp_identity_review_decision_v1",
		input.HoldID.String(), input.TargetContactID.String(), strconv.FormatUint(input.ExpectedVersion, 10),
		input.ExpectedMemberDigest, input.ExpectedChainDigest, input.RequestID.String(),
	}, "\n")
	sum := sha256.Sum256([]byte(canonical))
	assert.Equal(t, hex.EncodeToString(sum[:]), WhatsAppIdentityReviewDecisionRequestDigest(input))

	changed := input
	changed.ExpectedVersion++
	assert.NotEqual(t, WhatsAppIdentityReviewDecisionRequestDigest(input), WhatsAppIdentityReviewDecisionRequestDigest(changed))
}

func TestWhatsAppIdentityReviewSafeProjectionsFailClosedAndDoNotLeakCounterpart(t *testing.T) {
	t.Parallel()

	state := FailClosedWhatsAppIdentityReviewEffectiveState("")
	assert.False(t, state.Known)
	assert.False(t, state.AIAllowed)
	assert.True(t, state.Blocked)
	assert.Equal(t, "identity_review_unknown", state.Reason)

	encoded, err := json.Marshal(state)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "future_route_contact_id")
	assert.NotContains(t, string(encoded), "decision_target_contact_id")
}

func setupWhatsAppIdentityReviewAdmissionTest(
	t *testing.T,
) (*App, *gorm.DB, *models.Organization, *models.WhatsAppAccount) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	state := models.WhatsAppCoexistenceState{
		ID: uuid.New(), OrganizationID: organization.ID, WhatsAppAccountID: account.ID,
		OnboardingStatus: models.CoexistenceOnboardingStatusConnected, OnboardingCycle: 1,
		SyncStatus:        models.CoexistenceSyncStatusNotRequested,
		ContactSyncStatus: models.CoexistenceSyncStatusNotRequested,
		HistoryConsent:    models.CoexistenceHistoryConsentUnknown,
		HistorySyncStatus: models.CoexistenceSyncStatusNotRequested,
		LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata: models.JSONB{}, Version: 1,
	}
	require.NoError(t, db.Create(&state).Error)
	return &App{DB: db, Log: testutil.NopLogger()}, db, organization, account
}

func newWhatsAppIdentityReviewSelectorContact(organizationID uuid.UUID, phone string) models.Contact {
	return models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		PhoneNumber:    phone,
		ProfileName:    "Identity Review Selector",
		IsRead:         true,
		Tags:           models.JSONBArray{},
		Metadata:       models.JSONB{},
	}
}

func requireWhatsAppIdentityReviewSelectorBusy(
	t *testing.T,
	db *gorm.DB,
	mutate func(*gorm.DB) error,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	started := time.Now()
	err := db.WithContext(ctx).Transaction(mutate)
	elapsed := time.Since(started)
	require.Error(t, err)
	assert.Less(t, elapsed, 1500*time.Millisecond, "selector contention must fail promptly")
	code := postgresErrorCode(err)
	require.NotEqual(t, "40P01", code, "selector contention must not depend on deadlock detection")
	require.Equal(t, "55P03", code)
}

func createWhatsAppIdentityReviewResolver(t *testing.T, db *gorm.DB, organizationID uuid.UUID) *models.User {
	t.Helper()
	role := testutil.CreateTestRoleWithKeys(
		t,
		db,
		organizationID,
		"identity-review-resolver-"+uuid.NewString(),
		[]string{"contacts:read", "contacts:write", "contacts.identity_review:write"},
	)
	return testutil.CreateTestUser(t, db, organizationID, testutil.WithRoleID(&role.ID))
}

func decideWhatsAppIdentityReviewForTest(
	t *testing.T,
	app *App,
	db *gorm.DB,
	organizationID, resolverID, holdID, targetID uuid.UUID,
) *WhatsAppIdentityReviewDecisionResult {
	t.Helper()
	var preview *WhatsAppIdentityReviewPreview
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		preview, err = app.PreviewWhatsAppIdentityReviewDecision(tx, WhatsAppIdentityReviewPreviewInput{
			OrganizationID: organizationID,
			HoldID:         holdID,
			ResolverUserID: resolverID,
		})
		return err
	}))
	input := WhatsAppIdentityReviewDecisionInput{
		OrganizationID:       organizationID,
		HoldID:               holdID,
		ResolverUserID:       resolverID,
		TargetContactID:      targetID,
		ExpectedVersion:      preview.Snapshot.Version,
		ExpectedMemberDigest: preview.Snapshot.MemberDigest,
		ExpectedChainDigest:  preview.ChainDigest,
		RequestID:            uuid.New(),
	}
	input.RequestDigest = WhatsAppIdentityReviewDecisionRequestDigest(input)
	var result *WhatsAppIdentityReviewDecisionResult
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		result, err = app.DecideWhatsAppIdentityReview(tx, input)
		return err
	}))
	return result
}

func TestWhatsAppIdentityReviewAdmissionCapturesCompleteSetAndAllocatesFreshDriftGeneration(t *testing.T) {
	app, db, organization, account := setupWhatsAppIdentityReviewAdmissionTest(t)
	direct := testutil.CreateTestContact(t, db, organization.ID)
	phoneOwner := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithPhoneNumber("+60123456789"))
	require.NoError(t, db.Model(direct).Update("bs_uid", "direct-principal").Error)

	claim := WhatsAppIdentityReviewClaim{
		OrganizationID: organization.ID, WhatsAppAccountID: account.ID, OnboardingCycle: 1,
		DirectPrimaryBSUID: "direct-principal", Phone: "60123456789",
		VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
		VerifiedEventDigest:     strings.Repeat("a", 64), SelectorBodyDigest: strings.Repeat("b", 64),
	}
	var admission *WhatsAppIdentityReviewAdmission
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		admission, err = app.EvaluateWhatsAppIdentityReviewAdmission(tx, &claim)
		return err
	}))
	require.NotNil(t, admission.RouteContactID)
	assert.Equal(t, direct.ID, *admission.RouteContactID)
	assert.True(t, admission.Blocked)
	assert.True(t, admission.NeedsReview)
	assert.Equal(t, "phone_selector_conflict", admission.Reason)
	require.Len(t, admission.Candidates, 2)
	assert.True(t, identityReviewContainsContact(admission.Candidates, direct.ID))
	assert.True(t, identityReviewContainsContact(admission.Candidates, phoneOwner.ID))

	var first *WhatsAppIdentityReviewSnapshot
	var created bool
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		first, created, err = app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &claim)
		return err
	}))
	assert.True(t, created)
	assert.Equal(t, uint64(1), first.PrincipalGeneration)
	assert.Equal(t, uint32(2), first.MemberCount)

	var replay *WhatsAppIdentityReviewSnapshot
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		replay, created, err = app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &claim)
		return err
	}))
	assert.False(t, created)
	assert.Equal(t, first.HoldID, replay.HoldID)
	assert.Equal(t, first.MemberDigest, replay.MemberDigest)

	drift := claim
	drift.Phone = ""
	drift.SelectorBodyDigest = strings.Repeat("c", 64)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		admission, err = app.EvaluateWhatsAppIdentityReviewAdmission(tx, &drift)
		return err
	}))
	require.NotNil(t, admission.RouteContactID)
	assert.Equal(t, direct.ID, *admission.RouteContactID)
	assert.True(t, admission.Blocked, "reasons-only drift must not become an allow")
	assert.True(t, admission.NeedsReview)
	assert.Equal(t, "unique_direct_primary_drift", admission.Reason)

	var second *WhatsAppIdentityReviewSnapshot
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		second, created, err = app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &drift)
		return err
	}))
	assert.True(t, created)
	assert.Equal(t, uint64(2), second.PrincipalGeneration)
	assert.NotEqual(t, first.HoldID, second.HoldID)
	assert.NotEqual(t, first.MemberDigest, second.MemberDigest)
	assert.Equal(t, uint32(1), second.MemberCount)

	returnToFirst := claim
	returnToFirst.SelectorBodyDigest = strings.Repeat("f", 64)
	var third *WhatsAppIdentityReviewSnapshot
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		third, created, err = app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &returnToFirst)
		return err
	}))
	assert.True(t, created, "A->B->A is fresh drift from the latest generation, not replay of historical G1")
	assert.Equal(t, uint64(3), third.PrincipalGeneration)
	assert.NotEqual(t, first.HoldID, third.HoldID)
	assert.Equal(t, first.MemberDigest, third.MemberDigest)

	var thirdReplay *WhatsAppIdentityReviewSnapshot
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		thirdReplay, created, err = app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &returnToFirst)
		return err
	}))
	assert.False(t, created)
	assert.Equal(t, third.HoldID, thirdReplay.HoldID)

	var holds int64
	require.NoError(t, db.Model(&models.WhatsAppIdentityReviewHold{}).
		Where("organization_id = ? AND whats_app_account_id = ? AND onboarding_cycle = ? AND direct_primary_bsuid = ?",
			organization.ID, account.ID, 1, "direct-principal").Count(&holds).Error)
	assert.Equal(t, int64(3), holds)
}

func TestWhatsAppIdentityReviewSelectorFenceCoversEveryCandidateMutation(t *testing.T) {
	_, db, organization, _ := setupWhatsAppIdentityReviewAdmissionTest(t)
	otherOrganization := testutil.CreateTestOrganization(t, db)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	mergeTarget := testutil.CreateTestContact(t, db, organization.ID)

	fence := db.Begin()
	require.NoError(t, fence.Error)
	defer fence.Rollback()
	require.NoError(t, fence.Exec(
		"SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))",
		database.WhatsAppIdentityReviewContactSelectorFenceKey(organization.ID),
	).Error)

	attempt := func(mutate func(*gorm.DB) error) error {
		return db.Transaction(mutate)
	}
	now := time.Now().UTC()
	mutations := []struct {
		name   string
		mutate func(*gorm.DB) error
	}{
		{
			name: "insert",
			mutate: func(tx *gorm.DB) error {
				candidate := newWhatsAppIdentityReviewSelectorContact(organization.ID, "+60120000001")
				return tx.Create(&candidate).Error
			},
		},
		{name: "id", mutate: func(tx *gorm.DB) error {
			return tx.Model(&models.Contact{}).Where("id = ?", contact.ID).UpdateColumn("id", uuid.New()).Error
		}},
		{name: "organization_id", mutate: func(tx *gorm.DB) error {
			return tx.Model(&models.Contact{}).Where("id = ?", contact.ID).UpdateColumn("organization_id", otherOrganization.ID).Error
		}},
		{name: "phone_number", mutate: func(tx *gorm.DB) error {
			return tx.Model(&models.Contact{}).Where("id = ?", contact.ID).UpdateColumn("phone_number", "+60120000002").Error
		}},
		{name: "bs_uid", mutate: func(tx *gorm.DB) error {
			return tx.Model(&models.Contact{}).Where("id = ?", contact.ID).UpdateColumn("bs_uid", "selector-fence-bsuid").Error
		}},
		{name: "merged_into_id", mutate: func(tx *gorm.DB) error {
			return tx.Model(&models.Contact{}).Where("id = ?", contact.ID).UpdateColumn("merged_into_id", mergeTarget.ID).Error
		}},
		{name: "deleted_at", mutate: func(tx *gorm.DB) error {
			return tx.Model(&models.Contact{}).Where("id = ?", contact.ID).UpdateColumn("deleted_at", now).Error
		}},
		{name: "hard_delete", mutate: func(tx *gorm.DB) error {
			return tx.Unscoped().Delete(&models.Contact{}, "id = ?", contact.ID).Error
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			requireWhatsAppIdentityReviewSelectorBusy(t, db, mutation.mutate)
		})
	}

	require.NoError(t, attempt(func(tx *gorm.DB) error {
		return tx.Model(&models.Contact{}).Where("id = ?", contact.ID).
			UpdateColumn("profile_name", "Unrelated profile update").Error
	}), "non-selector updates must not take the selector fence")
	require.NoError(t, attempt(func(tx *gorm.DB) error {
		candidate := newWhatsAppIdentityReviewSelectorContact(otherOrganization.ID, "+60120000003")
		return tx.Create(&candidate).Error
	}), "a different tenant has an independent selector fence")
}

func TestWhatsAppIdentityReviewAdmissionWaitsForPriorSelectorWriterAndSeesItsCommit(t *testing.T) {
	app, db, organization, account := setupWhatsAppIdentityReviewAdmissionTest(t)
	direct := testutil.CreateTestContact(t, db, organization.ID)
	require.NoError(t, db.Model(direct).UpdateColumn("bs_uid", "direct-prior-writer").Error)

	writer := db.Begin()
	require.NoError(t, writer.Error)
	late := newWhatsAppIdentityReviewSelectorContact(organization.ID, "+60123456789")
	require.NoError(t, writer.Session(&gorm.Session{SkipDefaultTransaction: true}).Create(&late).Error)

	claim := WhatsAppIdentityReviewClaim{
		OrganizationID: organization.ID, WhatsAppAccountID: account.ID, OnboardingCycle: 1,
		DirectPrimaryBSUID: "direct-prior-writer", Phone: "60123456789",
		VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
		VerifiedEventDigest:     strings.Repeat("7", 64), SelectorBodyDigest: strings.Repeat("8", 64),
	}
	type admissionResult struct {
		admission *WhatsAppIdentityReviewAdmission
		err       error
	}
	done := make(chan admissionResult, 1)
	go func() {
		var admission *WhatsAppIdentityReviewAdmission
		err := db.Transaction(func(tx *gorm.DB) error {
			var err error
			admission, err = app.EvaluateWhatsAppIdentityReviewAdmission(tx, &claim)
			return err
		})
		done <- admissionResult{admission: admission, err: err}
	}()
	select {
	case result := <-done:
		require.Failf(t, "admission did not wait", "completed before selector writer commit: %v", result.err)
	case <-time.After(150 * time.Millisecond):
	}
	require.NoError(t, writer.Commit().Error)

	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.NotNil(t, result.admission)
		require.Len(t, result.admission.Candidates, 2)
		assert.True(t, identityReviewContainsContact(result.admission.Candidates, direct.ID))
		assert.True(t, identityReviewContainsContact(result.admission.Candidates, late.ID))
		assert.Equal(t, "phone_selector_conflict", result.admission.Reason)
	case <-time.After(5 * time.Second):
		require.Fail(t, "admission remained blocked after selector writer commit")
	}
}

func TestWhatsAppIdentityReviewSelectorInsertFailsFastForPriorAdmissionAndRetriesInFreshTransaction(t *testing.T) {
	app, db, organization, account := setupWhatsAppIdentityReviewAdmissionTest(t)
	direct := testutil.CreateTestContact(t, db, organization.ID)
	require.NoError(t, db.Model(direct).UpdateColumn("bs_uid", "direct-prior-admission").Error)
	claim := WhatsAppIdentityReviewClaim{
		OrganizationID: organization.ID, WhatsAppAccountID: account.ID, OnboardingCycle: 1,
		DirectPrimaryBSUID: "direct-prior-admission", Phone: "60123456789",
		VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
		VerifiedEventDigest:     strings.Repeat("9", 64), SelectorBodyDigest: strings.Repeat("a", 64),
	}

	admissionTx := db.Begin()
	require.NoError(t, admissionTx.Error)
	defer admissionTx.Rollback()
	first, err := app.EvaluateWhatsAppIdentityReviewAdmission(admissionTx, &claim)
	require.NoError(t, err)
	require.Len(t, first.Candidates, 1)
	assert.Equal(t, direct.ID, first.Candidates[0].ContactID)

	late := newWhatsAppIdentityReviewSelectorContact(organization.ID, "+60123456789")
	requireWhatsAppIdentityReviewSelectorBusy(t, db, func(tx *gorm.DB) error {
		return tx.Create(&late).Error
	})
	var insertedDuringAdmission int64
	require.NoError(t, db.Unscoped().Model(&models.Contact{}).
		Where("id = ?", late.ID).Count(&insertedDuringAdmission).Error)
	assert.Zero(t, insertedDuringAdmission, "the failed contender transaction must not insert the contact")

	require.NoError(t, admissionTx.Commit().Error)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return tx.Create(&late).Error
	}), "a fresh transaction must succeed after the admission commits")

	var after *WhatsAppIdentityReviewAdmission
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		after, err = app.EvaluateWhatsAppIdentityReviewAdmission(tx, &claim)
		return err
	}))
	require.Len(t, after.Candidates, 2)
	assert.True(t, identityReviewContainsContact(after.Candidates, direct.ID))
	assert.True(t, identityReviewContainsContact(after.Candidates, late.ID))
}

func TestWhatsAppIdentityReviewSelectorUpdateAndDeleteFailFastForPriorAdmissionAndRetryFresh(t *testing.T) {
	tests := []struct {
		name            string
		mutate          func(*gorm.DB, uuid.UUID) error
		assertUnchanged func(*testing.T, *gorm.DB, uuid.UUID)
		assertChanged   func(*testing.T, *gorm.DB, uuid.UUID)
	}{
		{
			name: "update_phone_number",
			mutate: func(tx *gorm.DB, contactID uuid.UUID) error {
				return tx.Model(&models.Contact{}).Where("id = ?", contactID).
					UpdateColumn("phone_number", "+60129990002").Error
			},
			assertUnchanged: func(t *testing.T, db *gorm.DB, contactID uuid.UUID) {
				var contact models.Contact
				require.NoError(t, db.Unscoped().Select("id", "phone_number").First(&contact, "id = ?", contactID).Error)
				assert.Equal(t, "+60129990001", contact.PhoneNumber)
			},
			assertChanged: func(t *testing.T, db *gorm.DB, contactID uuid.UUID) {
				var contact models.Contact
				require.NoError(t, db.Unscoped().Select("id", "phone_number").First(&contact, "id = ?", contactID).Error)
				assert.Equal(t, "+60129990002", contact.PhoneNumber)
			},
		},
		{
			name: "hard_delete",
			mutate: func(tx *gorm.DB, contactID uuid.UUID) error {
				return tx.Unscoped().Delete(&models.Contact{}, "id = ?", contactID).Error
			},
			assertUnchanged: func(t *testing.T, db *gorm.DB, contactID uuid.UUID) {
				var count int64
				require.NoError(t, db.Unscoped().Model(&models.Contact{}).Where("id = ?", contactID).Count(&count).Error)
				assert.Equal(t, int64(1), count, "the failed contender transaction must not delete the contact")
			},
			assertChanged: func(t *testing.T, db *gorm.DB, contactID uuid.UUID) {
				var count int64
				require.NoError(t, db.Unscoped().Model(&models.Contact{}).Where("id = ?", contactID).Count(&count).Error)
				assert.Zero(t, count)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, db, organization, account := setupWhatsAppIdentityReviewAdmissionTest(t)
			direct := testutil.CreateTestContact(t, db, organization.ID)
			require.NoError(t, db.Model(direct).UpdateColumn("bs_uid", "direct-"+test.name).Error)
			target := testutil.CreateTestContactWith(
				t,
				db,
				organization.ID,
				testutil.WithPhoneNumber("+60129990001"),
			)
			claim := WhatsAppIdentityReviewClaim{
				OrganizationID: organization.ID, WhatsAppAccountID: account.ID, OnboardingCycle: 1,
				DirectPrimaryBSUID:      "direct-" + test.name,
				VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
				VerifiedEventDigest:     strings.Repeat("b", 64), SelectorBodyDigest: strings.Repeat("c", 64),
			}

			admissionTx := db.Begin()
			require.NoError(t, admissionTx.Error)
			defer admissionTx.Rollback()
			admission, err := app.EvaluateWhatsAppIdentityReviewAdmission(admissionTx, &claim)
			require.NoError(t, err)
			require.Len(t, admission.Candidates, 1)
			assert.Equal(t, direct.ID, admission.Candidates[0].ContactID)

			requireWhatsAppIdentityReviewSelectorBusy(t, db, func(tx *gorm.DB) error {
				return test.mutate(tx, target.ID)
			})
			test.assertUnchanged(t, db, target.ID)

			require.NoError(t, admissionTx.Commit().Error)
			require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
				return test.mutate(tx, target.ID)
			}), "a fresh transaction must succeed after the admission commits")
			test.assertChanged(t, db, target.ID)
		})
	}
}

func TestWhatsAppIdentityReviewAdmissionWaitsForPolicyWriterWithoutLockInversion(t *testing.T) {
	app, db, organization, account := setupWhatsAppIdentityReviewAdmissionTest(t)
	direct := testutil.CreateTestContact(t, db, organization.ID)

	writer := db.Begin()
	require.NoError(t, writer.Error)
	defer writer.Rollback()
	require.NoError(t, database.LockOrganizationPolicyScope(writer, organization.ID))
	require.NoError(t, writer.Model(&models.Contact{}).Where("id = ?", direct.ID).
		UpdateColumn("bs_uid", "policy-writer-direct").Error)

	claim := WhatsAppIdentityReviewClaim{
		OrganizationID: organization.ID, WhatsAppAccountID: account.ID, OnboardingCycle: 1,
		DirectPrimaryBSUID:      "policy-writer-direct",
		VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
		VerifiedEventDigest:     strings.Repeat("d", 64), SelectorBodyDigest: strings.Repeat("e", 64),
	}
	type admissionResult struct {
		admission *WhatsAppIdentityReviewAdmission
		err       error
	}
	done := make(chan admissionResult, 1)
	go func() {
		var admission *WhatsAppIdentityReviewAdmission
		err := db.Transaction(func(tx *gorm.DB) error {
			var err error
			admission, err = app.EvaluateWhatsAppIdentityReviewAdmission(tx, &claim)
			return err
		})
		done <- admissionResult{admission: admission, err: err}
	}()
	select {
	case result := <-done:
		require.Failf(t, "admission bypassed policy writer", "result before commit: %+v", result)
	case <-time.After(150 * time.Millisecond):
	}
	require.NoError(t, writer.Commit().Error)
	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.NotNil(t, result.admission)
		require.Len(t, result.admission.Candidates, 1)
		assert.Equal(t, direct.ID, result.admission.Candidates[0].ContactID)
	case <-time.After(5 * time.Second):
		require.Fail(t, "admission did not resume after policy writer commit")
	}
}

func TestWhatsAppIdentityReviewHoldCommitBlocksPhysicalAIAttemptBeforeCustomerQwenAndProvider(t *testing.T) {
	app, db, organization, account := setupWhatsAppIdentityReviewAdmissionTest(t)
	direct := testutil.CreateTestContact(t, db, organization.ID)
	require.NoError(t, db.Model(direct).UpdateColumn("bs_uid", "fenced-direct-principal").Error)

	claim := WhatsAppIdentityReviewClaim{
		OrganizationID: organization.ID, WhatsAppAccountID: account.ID, OnboardingCycle: 1,
		DirectPrimaryBSUID:      "fenced-direct-principal",
		VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
		VerifiedEventDigest:     strings.Repeat("7", 64),
		SelectorBodyDigest:      strings.Repeat("8", 64),
	}

	// Keep the newly-created hold uncommitted while the physical attempt starts.
	// Its policy UPDATE fence must stop the attempt before policy evaluation;
	// once the hold commits, the attempt may resume only to observe the block.
	holdTx := db.Begin()
	require.NoError(t, holdTx.Error)
	t.Cleanup(func() { _ = holdTx.Rollback().Error })
	hold, created, err := app.CreateOrReuseWhatsAppIdentityReviewHold(holdTx, &claim)
	require.NoError(t, err)
	require.True(t, created)
	require.True(t, identityReviewContainsContact(hold.Candidates, direct.ID))

	type physicalAttemptResult struct {
		policy         database.ContactAutomaticReplyPolicy
		customerCalled bool
		qwenCalled     bool
		providerCalled bool
		err            error
	}
	backendPID := make(chan int, 1)
	done := make(chan physicalAttemptResult, 1)
	go func() {
		result := physicalAttemptResult{}
		result.err = db.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var pid int
			if err := session.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				backendPID <- 0
				return err
			}
			backendPID <- pid
			return session.Transaction(func(tx *gorm.DB) error {
				if err := database.LockOrganizationAIAttemptScope(tx, organization.ID); err != nil {
					return err
				}
				policy, err := database.EvaluateContactAutomaticReplyPolicy(tx, organization.ID, direct.ID)
				result.policy = policy
				if err != nil || !policy.Allowed {
					return err
				}
				// These flags model the customer-context lookup, Qwen request, and
				// final delivery callback that follow the physical policy fence.
				result.customerCalled = true
				result.qwenCalled = true
				result.providerCalled = true
				return nil
			})
		})
		done <- result
	}()

	pid := <-backendPID
	require.Positive(t, pid)
	testutil.RequirePostgresBackendWaitingForLock(t, db, pid)
	select {
	case result := <-done:
		require.Failf(t, "physical attempt crossed the uncommitted hold", "result: %+v", result)
	default:
	}

	require.NoError(t, holdTx.Commit().Error)
	select {
	case result := <-done:
		require.NoError(t, result.err)
		assert.False(t, result.policy.Allowed)
		assert.Equal(t, database.AutomaticReplyBlockedIdentityHold, result.policy.Reason)
		assert.False(t, result.customerCalled, "customer context must not be read after a committed hold")
		assert.False(t, result.qwenCalled, "Qwen must not be called after a committed hold")
		assert.False(t, result.providerCalled, "delivery provider must not be called after a committed hold")
	case <-time.After(5 * time.Second):
		require.Fail(t, "physical attempt did not resume after the hold committed")
	}
}

func TestWhatsAppIdentityReviewAdmissionCanonicalizesAllRawOwnersAndORsReasons(t *testing.T) {
	app, db, organization, account := setupWhatsAppIdentityReviewAdmissionTest(t)
	canonical := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithPhoneNumber("+60119876543"))
	alias := testutil.CreateTestContact(t, db, organization.ID)
	require.NoError(t, db.Model(canonical).Update("bs_uid", "direct-canonical").Error)
	require.NoError(t, db.Model(alias).Updates(map[string]any{
		"bs_uid": "parent-alias", "merged_into_id": canonical.ID, "merged_at": time.Now().UTC(),
	}).Error)
	require.NoError(t, db.Delete(alias).Error)

	claim := WhatsAppIdentityReviewClaim{
		OrganizationID: organization.ID, WhatsAppAccountID: account.ID, OnboardingCycle: 1,
		DirectPrimaryBSUID: "direct-canonical", ParentBSUID: "parent-alias", Phone: "60119876543",
		VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
		VerifiedEventDigest:     strings.Repeat("d", 64), SelectorBodyDigest: strings.Repeat("e", 64),
	}
	var snapshot *WhatsAppIdentityReviewSnapshot
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		snapshot, _, err = app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &claim)
		return err
	}))
	require.Len(t, snapshot.Candidates, 1)
	assert.Equal(t, canonical.ID, snapshot.Candidates[0].ContactID)
	assert.Equal(t, models.WhatsAppIdentityReviewSelectorReasonMask, snapshot.Candidates[0].SelectorReasons)
	assert.True(t, snapshot.Supported)
	assert.Equal(t, uint64(1), snapshot.PrincipalGeneration)
}

func TestWhatsAppIdentityReviewAdmissionIncludesFormattedStoredPhoneOwners(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		stored string
	}{
		{name: "manual formatting", stored: "+60 12-345 6789"},
		{name: "csv formatting", stored: "60 (12) 345-6789"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			app, db, organization, account := setupWhatsAppIdentityReviewAdmissionTest(t)
			direct := testutil.CreateTestContact(t, db, organization.ID)
			formattedPhoneOwner := testutil.CreateTestContactWith(
				t, db, organization.ID, testutil.WithPhoneNumber(fixture.stored),
			)
			require.NoError(t, db.Model(direct).UpdateColumn("bs_uid", "direct-formatted-phone").Error)
			claim := WhatsAppIdentityReviewClaim{
				OrganizationID: organization.ID, WhatsAppAccountID: account.ID, OnboardingCycle: 1,
				DirectPrimaryBSUID: "direct-formatted-phone", Phone: "+60 (12) 345-6789",
				VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
				VerifiedEventDigest:     strings.Repeat("b", 64), SelectorBodyDigest: strings.Repeat("c", 64),
			}
			var admission *WhatsAppIdentityReviewAdmission
			require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
				var err error
				admission, err = app.EvaluateWhatsAppIdentityReviewAdmission(tx, &claim)
				return err
			}))
			require.Len(t, admission.Candidates, 2)
			assert.True(t, identityReviewContainsContact(admission.Candidates, direct.ID))
			assert.True(t, identityReviewContainsContact(admission.Candidates, formattedPhoneOwner.ID))
			assert.Equal(t, "phone_selector_conflict", admission.Reason)
			assert.True(t, admission.Blocked)
			assert.True(t, admission.NeedsReview)
		})
	}
}

func TestWhatsAppIdentityReviewEffectiveStateSelectsRemainingOpenPrincipal(t *testing.T) {
	app, db, organization, account := setupWhatsAppIdentityReviewAdmissionTest(t)
	shared := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithPhoneNumber("+60123456789"))
	other := testutil.CreateTestContact(t, db, organization.ID)
	require.NoError(t, db.Model(shared).Update("bs_uid", "shared-principal").Error)
	require.NoError(t, db.Model(other).Update("bs_uid", "other-principal").Error)

	createHold := func(claim WhatsAppIdentityReviewClaim) *WhatsAppIdentityReviewSnapshot {
		t.Helper()
		var snapshot *WhatsAppIdentityReviewSnapshot
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			var err error
			snapshot, _, err = app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &claim)
			return err
		}))
		return snapshot
	}
	baseClaim := WhatsAppIdentityReviewClaim{
		OrganizationID: organization.ID, WhatsAppAccountID: account.ID, OnboardingCycle: 1,
		VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
		VerifiedEventDigest:     strings.Repeat("a", 64), SelectorBodyDigest: strings.Repeat("b", 64),
	}
	firstClaim := baseClaim
	firstClaim.DirectPrimaryBSUID = "shared-principal"
	first := createHold(firstClaim)
	secondClaim := baseClaim
	secondClaim.DirectPrimaryBSUID = "other-principal"
	secondClaim.Phone = "60123456789"
	second := createHold(secondClaim)
	require.NotEqual(t, first.HoldID, second.HoldID)

	resolver := createWhatsAppIdentityReviewResolver(t, db, organization.ID)
	resolved := decideWhatsAppIdentityReviewForTest(
		t, app, db, organization.ID, resolver.ID, second.HoldID, shared.ID,
	)
	require.Equal(t, models.WhatsAppIdentityReviewDispositionFutureRouting, resolved.Snapshot.Disposition)

	state, err := app.GetWhatsAppIdentityReviewEffectiveState(db, organization.ID, shared.ID)
	require.NoError(t, err)
	require.True(t, state.Blocked)
	require.False(t, state.AIAllowed)
	require.EqualValues(t, 1, state.OpenHoldCount)
	require.NotNil(t, state.LatestHoldID)
	require.Equal(t, first.HoldID, *state.LatestHoldID)

	remaining := decideWhatsAppIdentityReviewForTest(
		t, app, db, organization.ID, resolver.ID, first.HoldID, shared.ID,
	)
	require.Equal(t, models.WhatsAppIdentityReviewDispositionFutureRouting, remaining.Snapshot.Disposition)
	state, err = app.GetWhatsAppIdentityReviewEffectiveState(db, organization.ID, shared.ID)
	require.NoError(t, err)
	require.False(t, state.Blocked)
	require.True(t, state.AIAllowed)
	require.Zero(t, state.OpenHoldCount)
}

func TestStagedIdentityReviewQueueExposesOnlyOpenHoldReceipts(t *testing.T) {
	app, db, organization, account := setupWhatsAppIdentityReviewAdmissionTest(t)
	direct := testutil.CreateTestContactWith(t, db, organization.ID, testutil.WithPhoneNumber("+60123456789"))
	require.NoError(t, db.Model(&models.Contact{}).Where("organization_id = ? AND id = ?", organization.ID, direct.ID).
		Update("bs_uid", "queue-direct-principal").Error)

	channelAccount, err := channelapi.EnsureLegacyMetaWhatsAppAccount(db, channelapi.LegacyMetaAccountRef{
		ID: account.ID, OrganizationID: organization.ID, Name: account.Name, Status: account.Status,
	})
	require.NoError(t, err)

	createHold := func(onboardingCycle uint64, directBSUID, parentBSUID, phone, digest string) *WhatsAppIdentityReviewSnapshot {
		t.Helper()
		claim := WhatsAppIdentityReviewClaim{
			OrganizationID: organization.ID, WhatsAppAccountID: account.ID, OnboardingCycle: onboardingCycle,
			DirectPrimaryBSUID: directBSUID, ParentBSUID: parentBSUID, Phone: phone,
			VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
			VerifiedEventDigest:     strings.Repeat(digest, 64),
			SelectorBodyDigest:      strings.Repeat(digest, 64),
		}
		var snapshot *WhatsAppIdentityReviewSnapshot
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			var createErr error
			snapshot, _, createErr = app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &claim)
			return createErr
		}))
		return snapshot
	}
	createReceipt := func(holdID uuid.UUID, label string) *models.InboundEvent {
		t.Helper()
		now := time.Now().UTC()
		event := &models.InboundEvent{
			BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: now, UpdatedAt: now},
			OrganizationID: organization.ID, ChannelAccountID: channelAccount.ID,
			DedupeKey:       "identity-review:wamid.queue-" + label,
			ProviderEventID: "wamid.queue-" + label,
			EventType:       models.WhatsAppIdentityReviewPendingEvent, Status: models.InboundEventStatusPending,
			SignatureValid: true, ReceivedAt: now,
			Protocol: models.WhatsAppIdentityReviewInboundProtocol, ReviewHoldID: &holdID,
			Headers: models.JSONB{}, Payload: models.JSONB{
				"schema_version": 1, "message_type": "text", "content": "held " + label,
			},
		}
		require.NoError(t, db.Create(event).Error)
		return event
	}
	resolver := createWhatsAppIdentityReviewResolver(t, db, organization.ID)

	future := createHold(1, "queue-direct-principal", "", "", "a")
	futureReceipt := createReceipt(future.HoldID, "future-routing")
	futureDecision := decideWhatsAppIdentityReviewForTest(t, app, db, organization.ID, resolver.ID, future.HoldID, direct.ID)
	require.Equal(t, models.WhatsAppIdentityReviewDispositionFutureRouting, futureDecision.Snapshot.Disposition)

	latestOne := createHold(1, "queue-direct-principal", "queue-parent-one", "", "b")
	latestReceipt := createReceipt(latestOne.HoldID, "superseded-latest")
	latestTwo := createHold(1, "queue-direct-principal", "queue-parent-two", "", "c")
	latestDecision := decideWhatsAppIdentityReviewForTest(t, app, db, organization.ID, resolver.ID, latestTwo.HoldID, direct.ID)
	require.Equal(t, models.WhatsAppIdentityReviewDispositionFutureRouting, latestDecision.Snapshot.Disposition)
	var supersededLatest models.WhatsAppIdentityReviewHold
	require.NoError(t, db.Where("organization_id = ? AND id = ?", organization.ID, latestOne.HoldID).First(&supersededLatest).Error)
	require.Equal(t, models.WhatsAppIdentityReviewDispositionSupersededByLatest, supersededLatest.Disposition)

	cycle := createHold(1, "queue-cycle-principal", "", "", "d")
	cycleReceipt := createReceipt(cycle.HoldID, "superseded-cycle")
	require.NoError(t, db.Model(&models.WhatsAppCoexistenceState{}).Where(
		"organization_id = ? AND whats_app_account_id = ?", organization.ID, account.ID,
	).Update("onboarding_cycle", 2).Error)
	var supersededCycle models.WhatsAppIdentityReviewHold
	require.NoError(t, db.Where("organization_id = ? AND id = ?", organization.ID, cycle.HoldID).First(&supersededCycle).Error)
	require.Equal(t, models.WhatsAppIdentityReviewDispositionSupersededByCycle, supersededCycle.Disposition)

	unsupported := createHold(2, "", "queue-parent-unsupported", "60123456789", "e")
	unsupportedReceipt := createReceipt(unsupported.HoldID, "unsupported-open")

	var queue []models.InboundEvent
	require.NoError(t, app.stagedIdentityReviewEventsQuery(organization.ID).Order("inbound_events.id").Find(&queue).Error)
	require.Len(t, queue, 1)
	require.Equal(t, unsupportedReceipt.ID, queue[0].ID)

	for _, receipt := range []*models.InboundEvent{futureReceipt, latestReceipt, cycleReceipt} {
		_, loadErr := app.loadStagedIdentityReviewEvent(organization.ID, receipt.ID)
		require.ErrorIs(t, loadErr, gorm.ErrRecordNotFound, "closed hold receipt must be a detail/media 404")
		var retained models.InboundEvent
		require.NoError(t, db.Where("organization_id = ? AND id = ?", organization.ID, receipt.ID).First(&retained).Error)
		require.Equal(t, receipt.ReviewHoldID, retained.ReviewHoldID, "receipt remains immutable audit evidence")
	}

	loadedUnsupported, err := app.loadStagedIdentityReviewEvent(organization.ID, unsupportedReceipt.ID)
	require.NoError(t, err)
	require.Equal(t, unsupportedReceipt.ID, loadedUnsupported.ID)
}
