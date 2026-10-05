package handlers

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func latestWhatsAppIdentityReviewHoldForBSUID(t *testing.T, app *App, organizationID uuid.UUID, bsuid string) models.WhatsAppIdentityReviewHold {
	t.Helper()
	var hold models.WhatsAppIdentityReviewHold
	require.NoError(t, app.DB.Where("organization_id = ? AND direct_primary_bsuid = ?", organizationID, bsuid).
		Order("created_at DESC, id DESC").First(&hold).Error)
	return hold
}

func mergeContactsForTest(t *testing.T, app *App, organizationID, targetID, sourceID uuid.UUID) {
	t.Helper()
	adminRole := testutil.CreateAdminRole(t, app.DB, organizationID)
	admin := testutil.CreateTestUser(t, app.DB, organizationID, testutil.WithRoleID(&adminRole.ID))
	req := testutil.NewJSONRequest(t, MergeContactRequest{
		SourceContactID: sourceID, Confirm: true, IdempotencyKey: "identity-merge-" + uuid.NewString(),
	})
	testutil.SetAuthContext(req, organizationID, admin.ID)
	testutil.SetPathParam(req, "id", targetID.String())
	require.NoError(t, app.MergeContact(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
}

// A phone-matched contact whose canonical family already belongs to another
// WhatsApp user is not a routable target, even with an empty bs_uid: a staff
// merge left that user's BSUID on an alias, or its stored identity names that
// user. With no other member the review stays read-only.
func TestWhatsAppIdentityReviewOtherUsersFamilyIsNotRoutable(t *testing.T) {
	for _, variant := range []string{"merged_alias_with_other_bsuid", "metadata_names_other_user", "merged_with_other_users_metadata"} {
		t.Run(variant, func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			organizationID := account.OrganizationID
			phone := "60" + testutil.NewTestGraphObjectID()[:10]
			contact := syncPhoneOnlyCoexistenceContact(t, app, account, phone)
			otherUser := "US.other-user-" + uuid.NewString()
			switch variant {
			case "merged_alias_with_other_bsuid":
				source := testutil.CreateTestContact(t, app.DB, organizationID)
				require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", source.ID).Update("bs_uid", otherUser).Error)
				mergeContactsForTest(t, app, organizationID, contact.ID, source.ID)
			case "metadata_names_other_user":
				require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", contact.ID).
					Update("metadata", models.JSONB{"coexistence_user_id": otherUser}).Error)
			case "merged_with_other_users_metadata":
				source := testutil.CreateTestContact(t, app.DB, organizationID)
				require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", source.ID).
					Update("metadata", models.JSONB{"coexistence_user_id": otherUser}).Error)
				mergeContactsForTest(t, app, organizationID, contact.ID, source.ID)
			}
			merged := loadCoexistenceAdmissionContact(t, app.DB, organizationID, contact.ID)
			require.Empty(t, merged.BSUID, "the canonical contact itself carries no BSUID")

			principal := "US.new-holder-" + uuid.NewString()
			message := coexistenceAdmissionMessage(t, "wamid.family-"+uuid.NewString(), phone, principal, "recycled number?")
			work, _ := admitCoexistenceAdmissionMessage(t, app, account, message, "New holder")
			assert.Nil(t, work)
			hold := latestWhatsAppIdentityReviewHoldForBSUID(t, app, organizationID, principal)
			assert.False(t, hold.Supported, "another user's family leaves the review read-only")

			resolver := createWhatsAppIdentityReviewResolver(t, app.DB, organizationID)
			status, body, _ := previewContactIdentityReviewOverHTTP(t, app, organizationID, resolver.ID, contact.ID)
			assert.Equal(t, fasthttp.StatusConflict, status)
			assert.Contains(t, body, "read-only", "the dialog explains the read-only review instead of asking for a reload")
			assert.NotContains(t, body, "reload and try again")
		})
	}
}

// A contact a reviewer already routed one WhatsApp ID to is not offered to a
// second WhatsApp ID writing from the same phone in this onboarding cycle.
func TestWhatsAppIdentityReviewContactDecidedForAnotherPrincipalIsNotRoutable(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	phone := "60" + testutil.NewTestGraphObjectID()[:10]
	contact := syncPhoneOnlyCoexistenceContact(t, app, account, phone)
	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, organizationID)

	first := "US.first-" + uuid.NewString()
	work, _ := admitCoexistenceAdmissionMessage(t, app, account,
		coexistenceAdmissionMessage(t, "wamid.first-"+uuid.NewString(), phone, first, "first user"), "First")
	require.Nil(t, work)
	resolveLatestContactIdentityReviewForTest(t, app, organizationID, resolver.ID, contact.ID, contact.ID)

	second := "US.second-" + uuid.NewString()
	work, _ = admitCoexistenceAdmissionMessage(t, app, account,
		coexistenceAdmissionMessage(t, "wamid.second-"+uuid.NewString(), phone, second, "second user"), "Second")
	assert.Nil(t, work)
	assert.False(t, latestWhatsAppIdentityReviewHoldForBSUID(t, app, organizationID, second).Supported,
		"the contact already decided for another WhatsApp ID cannot receive this one")
	assert.Empty(t, loadCoexistenceAdmissionContact(t, app.DB, organizationID, contact.ID).BSUID)
}

// When a decided target later comes to belong to another WhatsApp user, an
// owned principal's next message stays on its own direct owner, held with
// suppression, and a fresh generation asks again.
func TestWhatsAppIdentityReviewContradictedDecisionKeepsOwnedPrincipalOnItsContact(t *testing.T) {
	app, account, direct := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	principal := "US.owned-" + uuid.NewString()
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", direct.ID).Update("bs_uid", principal).Error)
	phone := "60" + testutil.NewTestGraphObjectID()[:10]
	phoneOwner := testutil.CreateTestContactWith(t, app.DB, organizationID, testutil.WithPhoneNumber(phone))

	first := coexistenceAdmissionMessage(t, "wamid.owned-first-"+uuid.NewString(), phone, principal, "phone conflict")
	work, _ := admitCoexistenceAdmissionMessage(t, app, account, first, "Owned")
	require.NotNil(t, work)
	require.Equal(t, direct.ID, work.Persisted.ContactID)
	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, organizationID)
	resolveLatestContactIdentityReviewForTest(t, app, organizationID, resolver.ID, direct.ID, phoneOwner.ID)

	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", phoneOwner.ID).
		Update("bs_uid", "US.later-holder-"+uuid.NewString()).Error)
	next := first
	next.ID = "wamid.owned-next-" + uuid.NewString()
	work, _ = admitCoexistenceAdmissionMessage(t, app, account, next, "Owned")
	require.NotNil(t, work, "the owned principal's message must stay on its direct owner")
	assert.Equal(t, direct.ID, work.Persisted.ContactID)
	assert.Equal(t, "decision_target_contradicted", work.Persisted.Metadata[incomingIdentityReviewReasonKey])
	assert.Equal(t, incomingIdentityReviewRouteHeldDirect, work.Persisted.Metadata[incomingIdentityReviewRouteModeKey])
	assert.True(t, incomingMessageAutomaticAISuppressed(&work.Persisted))
	assert.EqualValues(t, 2, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{},
		"organization_id = ? AND direct_primary_bsuid = ?", organizationID, principal))
	accepted, err := app.validatePersistedWhatsAppIdentityReviewRoute(account, &work.Persisted,
		coexistenceContactIdentity{Phone: next.From, UserID: next.FromUserID})
	require.NoError(t, err)
	assert.True(t, accepted)
}

// A decided target that held the sender's parent BSUID keeps its decision
// when a Business app reply carrying both IDs replaces that parent BSUID with
// the sender's own.
func TestWhatsAppIdentityReviewDecisionSurvivesParentOwnerLearningTheBSUID(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	phone := "60" + testutil.NewTestGraphObjectID()[:10]
	parent := "US.ENT.parent-" + uuid.NewString()[:8]
	contact := testutil.CreateTestContactWith(t, app.DB, organizationID, testutil.WithPhoneNumber(phone))
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", contact.ID).Update("bs_uid", parent).Error)
	principal := "US.child-" + uuid.NewString()

	first := coexistenceAdmissionMessage(t, "wamid.parent-learn-"+uuid.NewString(), phone, principal, "held")
	first.FromParentUserID = parent
	work, _ := admitCoexistenceAdmissionMessage(t, app, account, first, "Child")
	require.Nil(t, work)
	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, organizationID)
	resolveLatestContactIdentityReviewForTest(t, app, organizationID, resolver.ID, contact.ID, contact.ID)

	echo := CoexistenceMessage{IncomingTextMessage: inboundContinuationTextMessage(
		t, "wamid.parent-learn-echo-"+uuid.NewString(), "15550783881", "reply from the app",
	)}
	echo.To = phone
	echo.ToUserID = principal
	echo.ToParentUserID = parent
	require.NoError(t, app.persistMessageEchoesBeforeAck(account.PhoneID, []CoexistenceMessage{echo}, nil))
	require.Equal(t, principal, loadCoexistenceAdmissionContact(t, app.DB, organizationID, contact.ID).BSUID)

	next := first
	next.ID = "wamid.parent-learn-next-" + uuid.NewString()
	work, _ = admitCoexistenceAdmissionMessage(t, app, account, next, "Child")
	require.NotNil(t, work)
	assert.Equal(t, contact.ID, work.Persisted.ContactID)
	assert.Equal(t, "reviewed_future_route", work.Persisted.Metadata[incomingIdentityReviewReasonKey])
	assert.False(t, incomingMessageAutomaticAISuppressed(&work.Persisted))
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{},
		"organization_id = ?", organizationID))
}

// Every held copy a decision would close is listable for the dialog: the
// preview names every open generation, and the held list returns all of
// their receipts oldest first.
func TestWhatsAppIdentityReviewPreviewListsEveryHeldCopyOfTheOpenChain(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	phone := "60" + testutil.NewTestGraphObjectID()[:10]
	contact := syncPhoneOnlyCoexistenceContact(t, app, account, phone)
	principal := "US.chain-" + uuid.NewString()
	send := func(body string, digest string) string {
		message := coexistenceAdmissionMessage(t, "wamid.chain-"+uuid.NewString(), phone, principal, body)
		work, _, err := app.persistAuthenticatedIncomingMessageBeforeAck(
			account.PhoneID, message, "Chain", strings.Repeat(digest, sha256.Size*2),
		)
		require.NoError(t, err)
		require.Nil(t, work)
		return message.ID
	}
	firstGeneration := []string{send("hi", "a"), send("my order is 42", "b")}
	// A second phone owner changes the set: the next copy opens generation 2.
	testutil.CreateTestContactWith(t, app.DB, organizationID, testutil.WithPhoneNumber("+"+phone))
	secondGeneration := send("thanks", "c")

	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, organizationID)
	status, body, preview := previewContactIdentityReviewOverHTTP(t, app, organizationID, resolver.ID, contact.ID)
	require.Equal(t, fasthttp.StatusOK, status, body)
	require.Len(t, preview.OpenHoldIDs, 2)
	assert.Equal(t, preview.Snapshot.HoldID, preview.OpenHoldIDs[1])

	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organizationID, resolver.ID)
	for _, holdID := range preview.OpenHoldIDs {
		request.RequestCtx.QueryArgs().Add("hold_id", holdID.String())
	}
	testutil.SetQueryParam(request, "order", "oldest")
	require.NoError(t, app.ListStagedContactIdentityReviews(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	var page struct {
		Reviews []stagedWhatsAppIdentityReviewItem `json:"reviews"`
		Total   int64                              `json:"total"`
	}
	testutil.ParseEnvelopeResponse(t, request, &page)
	assert.EqualValues(t, 3, page.Total)
	require.Len(t, page.Reviews, 3)
	var events []models.InboundEvent
	require.NoError(t, app.DB.Where("organization_id = ? AND protocol = ?", organizationID, models.WhatsAppIdentityReviewInboundProtocol).
		Find(&events).Error)
	wamidByID := map[uuid.UUID]string{}
	for _, event := range events {
		wamidByID[event.ID] = event.ProviderEventID
	}
	got := []string{wamidByID[page.Reviews[0].ID], wamidByID[page.Reviews[1].ID], wamidByID[page.Reviews[2].ID]}
	assert.Equal(t, append(firstGeneration, secondGeneration), got, "oldest first, across both generations")
}
