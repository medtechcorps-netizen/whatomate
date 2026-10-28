package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

type observationFixture struct {
	app       *App
	account   *models.WhatsAppAccount
	contact   *models.Contact
	resolver  *models.User
	hold      models.WhatsAppIdentityReviewHold
	event     models.InboundEvent
	phone     string
	principal string
}

func newObservationFixture(t *testing.T) observationFixture {
	t.Helper()
	app, account, _ := whatsappIdentityFixture(t)
	phone := "60" + testutil.NewTestGraphObjectID()[:10]
	contact := syncPhoneOnlyCoexistenceContact(t, app, account, phone)
	principal := "US.observer-" + uuid.NewString()
	work, _ := admitCoexistenceAdmissionMessage(t, app, account,
		coexistenceAdmissionMessage(t, "wamid.observer-"+uuid.NewString(), phone, principal, "authorized held copy"), "Held")
	require.Nil(t, work)
	hold := latestWhatsAppIdentityReviewHoldForBSUID(t, app, account.OrganizationID, principal)
	require.True(t, hold.Supported)
	var event models.InboundEvent
	require.NoError(t, app.DB.Where("organization_id = ? AND review_hold_id = ?", account.OrganizationID, hold.ID).First(&event).Error)
	return observationFixture{app, account, contact, createWhatsAppIdentityReviewResolver(t, app.DB, account.OrganizationID), hold, event, phone, principal}
}

func (f observationFixture) list(t *testing.T, page int) (*fastglue.Request, stagedReviewObservationPage) {
	t.Helper()
	r := testutil.NewGETRequest(t)
	testutil.SetAuthContext(r, f.account.OrganizationID, f.resolver.ID)
	testutil.SetQueryParam(r, "page", fmt.Sprint(page))
	testutil.SetQueryParam(r, "limit", "100")
	require.NoError(t, f.app.ListStagedContactIdentityReviews(r))
	var out stagedReviewObservationPage
	if testutil.GetResponseStatusCode(r) == fasthttp.StatusOK {
		testutil.ParseEnvelopeResponse(t, r, &out)
	}
	return r, out
}

type stagedReviewObservationPage struct {
	Reviews    []stagedWhatsAppIdentityReviewItem `json:"reviews"`
	Known      bool                               `json:"eligibility_known"`
	Total      int64                              `json:"total"`
	Actionable int64                              `json:"actionable_total"`
	Chains     int64                              `json:"actionable_chain_total"`
	ReadOnly   int64                              `json:"read_only_total"`
}

func TestStagedObservationTracksCompetingDecisionWithoutChangingHoldOrCopies(t *testing.T) {
	f := newObservationFixture(t)
	r, before := f.list(t, 1)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
	require.True(t, before.Known)
	require.EqualValues(t, 1, before.Actionable)
	require.EqualValues(t, 1, before.Chains)
	other := "US.observer-other-" + uuid.NewString()
	work, _ := admitCoexistenceAdmissionMessage(t, f.app, f.account,
		coexistenceAdmissionMessage(t, "wamid.other-"+uuid.NewString(), f.phone, other, "other held copy"), "Other")
	require.Nil(t, work)
	otherHold := latestWhatsAppIdentityReviewHoldForBSUID(t, f.app, f.account.OrganizationID, other)
	decideWhatsAppIdentityReviewForTest(t, f.app, f.app.DB, f.account.OrganizationID, f.resolver.ID, otherHold.ID, f.contact.ID)
	r, after := f.list(t, 1)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
	require.True(t, after.Known)
	assert.EqualValues(t, 1, after.Total)
	assert.Zero(t, after.Actionable)
	assert.Zero(t, after.Chains)
	assert.EqualValues(t, 1, after.ReadOnly)
	require.Len(t, after.Reviews, 1)
	assert.Equal(t, "no_eligible_member", after.Reviews[0].EligibilityReason)
	var unchanged models.WhatsAppIdentityReviewHold
	require.NoError(t, f.app.DB.First(&unchanged, "id = ?", f.hold.ID).Error)
	assert.True(t, unchanged.Supported)
	assert.Equal(t, models.WhatsAppIdentityReviewDispositionOpen, unchanged.Disposition)
	assert.Equal(t, f.hold.MemberDigest, unchanged.MemberDigest)
	assert.Equal(t, f.hold.Version, unchanged.Version)
	detail := testutil.NewGETRequest(t)
	testutil.SetAuthContext(detail, f.account.OrganizationID, f.resolver.ID)
	detail.RequestCtx.SetUserValue("id", f.event.ID.String())
	require.NoError(t, f.app.GetStagedContactIdentityReview(detail))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(detail))
	var copy stagedWhatsAppIdentityReviewDetail
	testutil.ParseEnvelopeResponse(t, detail, &copy)
	assert.True(t, copy.ReadOnly)
	assert.True(t, copy.EligibilityKnown)
	assert.False(t, copy.DecisionAvailable)
	assert.Equal(t, "authorized held copy", copy.Content)
	err := f.app.DB.Transaction(func(tx *gorm.DB) error {
		_, err := f.app.PreviewWhatsAppIdentityReviewDecision(tx, WhatsAppIdentityReviewPreviewInput{OrganizationID: f.account.OrganizationID, HoldID: f.hold.ID, ResolverUserID: f.resolver.ID})
		return err
	})
	assert.ErrorIs(t, err, ErrWhatsAppIdentityReviewReadOnly)
}

func TestStagedObservationReceiptWeightsAndStablePaginationShareOneChain(t *testing.T) {
	f := newObservationFixture(t)
	for index := 0; index < 101; index++ {
		copy := f.event
		copy.BaseModel = models.BaseModel{ID: uuid.New()}
		copy.ProviderEventID = "wamid.page-" + uuid.NewString()
		copy.DedupeKey = "identity-review:" + copy.ProviderEventID
		copy.ReceivedAt = f.event.ReceivedAt
		require.NoError(t, f.app.DB.Create(&copy).Error)
	}
	reads := 0
	callback := "test:observation-count:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Row().Before("gorm:row").Register(callback, func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "receipt_scope AS NOT MATERIALIZED") {
			reads++
		}
	}))
	t.Cleanup(func() { require.NoError(t, f.app.DB.Callback().Row().Remove(callback)) })
	r, first := f.list(t, 1)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
	r, last := f.list(t, 2)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
	require.Len(t, first.Reviews, 100)
	require.Len(t, last.Reviews, 2)
	assert.EqualValues(t, 102, first.Total)
	assert.Equal(t, first.Total, last.Total)
	assert.EqualValues(t, 102, first.Actionable)
	assert.EqualValues(t, 1, first.Chains)
	assert.Equal(t, 2, reads, "one data snapshot per page, regardless of receipt count")
	seen := map[uuid.UUID]bool{}
	previous := ""
	for _, item := range append(first.Reviews, last.Reviews...) {
		assert.False(t, seen[item.ID])
		seen[item.ID] = true
		if previous != "" {
			assert.Greater(t, previous, item.ID.String())
		}
		previous = item.ID.String()
	}
}

func TestStagedObservationOlderHoldFilterUsesCompleteLatestClosingChain(t *testing.T) {
	f := newObservationFixture(t)
	testutil.CreateTestContactWith(t, f.app.DB, f.account.OrganizationID, testutil.WithPhoneNumber("+"+f.phone))
	work, _ := admitCoexistenceAdmissionMessage(t, f.app, f.account,
		coexistenceAdmissionMessage(t, "wamid.new-generation-"+uuid.NewString(), f.phone, f.principal, "new generation"), "Held")
	require.Nil(t, work)
	latest := latestWhatsAppIdentityReviewHoldForBSUID(t, f.app, f.account.OrganizationID, f.principal)
	require.Greater(t, latest.PrincipalGeneration, f.hold.PrincipalGeneration)
	r := testutil.NewGETRequest(t)
	testutil.SetAuthContext(r, f.account.OrganizationID, f.resolver.ID)
	r.RequestCtx.QueryArgs().Add("hold_id", f.hold.ID.String())
	require.NoError(t, f.app.ListStagedContactIdentityReviews(r))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
	var out stagedReviewObservationPage
	testutil.ParseEnvelopeResponse(t, r, &out)
	assert.EqualValues(t, 1, out.Total)
	assert.EqualValues(t, 1, out.Actionable)
	assert.EqualValues(t, 1, out.Chains)
	require.Len(t, out.Reviews, 1)
	assert.Equal(t, f.hold.ID, out.Reviews[0].HoldID)
	decision := decideWhatsAppIdentityReviewForTest(t, f.app, f.app.DB, f.account.OrganizationID, f.resolver.ID, latest.ID, f.contact.ID)
	assert.Len(t, decision.SupersededGenerations, 1)
	r, empty := f.list(t, 1)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r))
	assert.True(t, empty.Known)
	assert.Zero(t, empty.Total)
}

func TestStagedObservationReadsCurrentPermissionDespiteWarmPermissionCache(t *testing.T) {
	f := newObservationFixture(t)
	cache := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: cache.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	f.app.Redis = client
	require.True(t, f.app.HasPermission(f.resolver.ID, models.ResourceContactsIdentityReview, models.ActionWrite, f.account.OrganizationID))
	require.True(t, cache.Exists(fmt.Sprintf("%s%s:%s", userPermissionsCachePrefix, f.resolver.ID, f.account.OrganizationID)))
	var membership models.UserOrganization
	require.NoError(t, f.app.DB.Where("user_id = ? AND organization_id = ?", f.resolver.ID, f.account.OrganizationID).First(&membership).Error)
	require.NotNil(t, membership.RoleID)
	require.NoError(t, f.app.DB.Exec(`DELETE FROM role_permissions WHERE custom_role_id = ? AND permission_id IN
		(SELECT id FROM permissions WHERE resource = ? AND action = ?)`, *membership.RoleID, models.ResourceContactsIdentityReview, models.ActionWrite).Error)
	r, _ := f.list(t, 1)
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(r))
	assert.NotContains(t, string(testutil.GetResponseBody(r)), "authorized held copy")
	assert.NotContains(t, string(testutil.GetResponseBody(r)), "actionable_total")
}

func TestStagedObservationAssignmentOnlyReviewerRequiresCompleteUnion(t *testing.T) {
	f := newObservationFixture(t)
	role := testutil.CreateTestRoleWithKeys(t, f.app.DB, f.account.OrganizationID, "observation-assigned-"+uuid.NewString(), []string{"contacts:write", "contacts.identity_review:write"})
	f.resolver = testutil.CreateTestUser(t, f.app.DB, f.account.OrganizationID, testutil.WithRoleID(&role.ID))
	r, before := f.list(t, 1)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r))
	assert.Zero(t, before.Actionable)
	require.Len(t, before.Reviews, 1)
	assert.Equal(t, "reviewer_scope", before.Reviews[0].EligibilityReason)
	require.NoError(t, f.app.DB.Model(&models.Contact{}).Where("id = ?", f.contact.ID).Update("assigned_user_id", f.resolver.ID).Error)
	r, after := f.list(t, 1)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r))
	assert.EqualValues(t, 1, after.Actionable)
}

func TestStagedObservationFamilyExpansionCapsBeforeRetentionAndDetailRequiresOptIn(t *testing.T) {
	f := newObservationFixture(t)
	for index := 0; index < 255; index++ {
		alias := testutil.CreateTestContact(t, f.app.DB, f.account.OrganizationID)
		require.NoError(t, f.app.DB.Model(alias).Update("merged_into_id", f.contact.ID).Error)
	}
	r, boundary := f.list(t, 1)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
	require.True(t, boundary.Known)
	require.EqualValues(t, 1, boundary.Actionable)
	alias := testutil.CreateTestContact(t, f.app.DB, f.account.OrganizationID)
	require.NoError(t, f.app.DB.Model(alias).Update("merged_into_id", f.contact.ID).Error)
	r, _ = f.list(t, 1)
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(r))
	assert.NotContains(t, string(testutil.GetResponseBody(r)), "actionable_total")
	for _, optIn := range []bool{false, true} {
		detail := testutil.NewGETRequest(t)
		testutil.SetAuthContext(detail, f.account.OrganizationID, f.resolver.ID)
		detail.RequestCtx.SetUserValue("id", f.event.ID.String())
		if optIn {
			testutil.SetQueryParam(detail, "eligibility", "current-v1")
		}
		require.NoError(t, f.app.GetStagedContactIdentityReview(detail))
		if !optIn {
			assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(detail))
			continue
		}
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(detail), string(testutil.GetResponseBody(detail)))
		var out map[string]any
		testutil.ParseEnvelopeResponse(t, detail, &out)
		assert.Equal(t, false, out["eligibility_known"])
		assert.Equal(t, false, out["decision_available"])
		assert.Nil(t, out["read_only"])
		assert.Equal(t, "unavailable", out["eligibility_reason"])
		assert.Equal(t, "authorized held copy", out["content"])
	}
}

func TestStagedObservationSelectiveQueryFailureNeverBecomesCopyOrZeroCounts(t *testing.T) {
	f := newObservationFixture(t)
	injected := errors.New("selective observation dependency failure")
	name := "test:observation:" + uuid.NewString()
	hits := 0
	require.NoError(t, f.app.DB.Callback().Row().Before("gorm:row").Register(name, func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "receipt_scope AS NOT MATERIALIZED") {
			hits++
			_ = tx.AddError(injected)
		}
	}))
	t.Cleanup(func() { require.NoError(t, f.app.DB.Callback().Row().Remove(name)) })
	var healthy int
	require.NoError(t, f.app.DB.Raw("SELECT 1").Scan(&healthy).Error)
	require.Equal(t, 1, healthy)
	r, _ := f.list(t, 1)
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(r))
	assert.NotContains(t, string(testutil.GetResponseBody(r)), "actionable_total")
	detail := testutil.NewGETRequest(t)
	testutil.SetAuthContext(detail, f.account.OrganizationID, f.resolver.ID)
	detail.RequestCtx.SetUserValue("id", f.event.ID.String())
	testutil.SetQueryParam(detail, "eligibility", "current-v1")
	require.NoError(t, f.app.GetStagedContactIdentityReview(detail))
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(detail))
	assert.NotContains(t, string(testutil.GetResponseBody(detail)), "authorized held copy")
	assert.Equal(t, 2, hits)
}

func TestStagedObservationUsesHeldConnectionAndClipsCallerDeadline(t *testing.T) {
	observationHeldConnectionAndLockBudget(t)
}

func TestStagedObservationCancelledClassificationDiscardsAllCounts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := classifyStagedReviewSnapshot(ctx, uuid.New(), &stagedReviewObservationFacts{Allowed: true, Total: 5})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, result)
}

func TestStagedObservationExactOrganizationAdmissionPrecedesPolicyLock(t *testing.T) {
	f := newObservationFixture(t)
	other := testutil.CreateTestOrganization(t, f.app.DB)
	locks := 0
	callback := "test:observation-lock:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Raw().Before("gorm:raw").Register(callback, func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "pg_advisory_xact_lock") {
			locks++
		}
	}))
	t.Cleanup(func() { require.NoError(t, f.app.DB.Callback().Raw().Remove(callback)) })
	r := testutil.NewGETRequest(t)
	testutil.SetAuthContext(r, f.account.OrganizationID, f.resolver.ID)
	r.RequestCtx.Request.Header.Set("X-Organization-ID", other.ID.String())
	require.NoError(t, f.app.ListStagedContactIdentityReviews(r))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(r))
	assert.Zero(t, locks)
	assert.NotContains(t, string(testutil.GetResponseBody(r)), "authorized held copy")
	role := testutil.CreateTestRoleWithKeys(t, f.app.DB, other.ID, "observer-other-"+uuid.NewString(), []string{"contacts:read", "contacts:write", "contacts.identity_review:write"})
	require.NoError(t, f.app.DB.Create(&models.UserOrganization{UserID: f.resolver.ID, OrganizationID: other.ID, RoleID: &role.ID, Source: models.MembershipSourceDirect}).Error)
	locks = 0
	r = testutil.NewGETRequest(t)
	testutil.SetAuthContext(r, f.account.OrganizationID, f.resolver.ID)
	r.RequestCtx.Request.Header.Set("X-Organization-ID", other.ID.String())
	require.NoError(t, f.app.ListStagedContactIdentityReviews(r))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
	var out stagedReviewObservationPage
	testutil.ParseEnvelopeResponse(t, r, &out)
	assert.True(t, out.Known)
	assert.Zero(t, out.Total)
	assert.Equal(t, 1, locks)
}

func TestStagedObservationExcessPagePayloadCannotBecomeAnUnknownCopy(t *testing.T) {
	f := newObservationFixture(t)
	copy := f.event
	copy.BaseModel = models.BaseModel{ID: uuid.New()}
	copy.ProviderEventID = "wamid.large-copy-" + uuid.NewString()
	copy.DedupeKey = "identity-review:" + copy.ProviderEventID
	copy.Payload = models.JSONB{"schema_version": 1, "message_type": "text", "content": strings.Repeat("x", 65537)}
	require.NoError(t, f.app.DB.Create(&copy).Error)
	r, _ := f.list(t, 1)
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(r))
	detail := testutil.NewGETRequest(t)
	testutil.SetAuthContext(detail, f.account.OrganizationID, f.resolver.ID)
	detail.RequestCtx.SetUserValue("id", copy.ID.String())
	testutil.SetQueryParam(detail, "eligibility", "current-v1")
	require.NoError(t, f.app.GetStagedContactIdentityReview(detail))
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(detail))
	assert.NotContains(t, string(testutil.GetResponseBody(detail)), strings.Repeat("x", 100))
}

func TestStagedObservationPreservesMissingSelectedRoleDenialForSuperAdmin(t *testing.T) {
	f := newObservationFixture(t)
	var member models.UserOrganization
	require.NoError(t, f.app.DB.Where("user_id = ? AND organization_id = ?", f.resolver.ID, f.account.OrganizationID).First(&member).Error)
	require.NotNil(t, member.RoleID)
	require.NoError(t, f.app.DB.Model(&models.User{}).Where("id = ?", f.resolver.ID).Update("is_super_admin", true).Error)
	require.NoError(t, f.app.DB.Delete(&models.CustomRole{}, "id = ?", *member.RoleID).Error)
	require.False(t, f.app.HasPermission(f.resolver.ID, models.ResourceContactsIdentityReview, models.ActionWrite, f.account.OrganizationID))
	r, _ := f.list(t, 1)
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(r))
	assert.NotContains(t, string(testutil.GetResponseBody(r)), "actionable_total")
}
