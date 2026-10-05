package handlers

import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

// syncPhoneOnlyCoexistenceContact models an address-book contact that Meta
// synced without a user_id, so the canonical Contact carries a phone and no BSUID.
func syncPhoneOnlyCoexistenceContact(t *testing.T, app *App, account *models.WhatsAppAccount, phone string) *models.Contact {
	t.Helper()
	item := CoexistenceStateSyncItem{Type: "contact", Action: "add"}
	item.Contact.FullName = "Address Book Contact"
	item.Contact.PhoneNumber = phone
	require.NoError(t, app.persistContactStateSyncBeforeAck(account.PhoneID, []CoexistenceStateSyncItem{item}))
	var contact models.Contact
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND regexp_replace(phone_number, '[^0-9]', '', 'g') = ?",
		account.OrganizationID, normalizeIdentityReviewPhone(phone),
	).First(&contact).Error)
	require.Empty(t, contact.BSUID, "the synced address-book contact must not own a BSUID")
	return &contact
}

func previewContactIdentityReviewOverHTTP(
	t *testing.T,
	app *App,
	organizationID, userID, contactID uuid.UUID,
) (int, string, *WhatsAppIdentityReviewPreview) {
	t.Helper()
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, organizationID, userID)
	testutil.SetPathParam(req, "id", contactID.String())
	require.NoError(t, app.PreviewContactIdentityReview(req))
	status := testutil.GetResponseStatusCode(req)
	body := string(testutil.GetResponseBody(req))
	if status != fasthttp.StatusOK {
		return status, body, nil
	}
	var envelope struct {
		Data WhatsAppIdentityReviewPreview `json:"data"`
	}
	testutil.ParseJSONResponse(t, req, &envelope)
	return status, body, &envelope.Data
}

func decideContactIdentityReviewOverHTTP(
	t *testing.T,
	app *App,
	organizationID, userID, contactID, targetID uuid.UUID,
	preview *WhatsAppIdentityReviewPreview,
) (int, string, *WhatsAppIdentityReviewDecisionResult) {
	t.Helper()
	input := WhatsAppIdentityReviewDecisionInput{
		HoldID:               preview.Snapshot.HoldID,
		TargetContactID:      targetID,
		ExpectedVersion:      preview.Snapshot.Version,
		ExpectedMemberDigest: preview.Snapshot.MemberDigest,
		ExpectedChainDigest:  preview.ChainDigest,
		RequestID:            uuid.New(),
	}
	req := testutil.NewJSONRequest(t, whatsAppIdentityReviewDecisionRequest{
		HoldID:               input.HoldID,
		TargetContactID:      input.TargetContactID,
		ExpectedVersion:      input.ExpectedVersion,
		ExpectedMemberDigest: input.ExpectedMemberDigest,
		ExpectedChainDigest:  input.ExpectedChainDigest,
		RequestID:            input.RequestID,
		RequestDigest:        WhatsAppIdentityReviewDecisionRequestDigest(input),
	})
	testutil.SetAuthContext(req, organizationID, userID)
	testutil.SetPathParam(req, "id", contactID.String())
	require.NoError(t, app.DecideContactIdentityReview(req))
	status := testutil.GetResponseStatusCode(req)
	body := string(testutil.GetResponseBody(req))
	if status != fasthttp.StatusOK {
		return status, body, nil
	}
	var envelope struct {
		Data WhatsAppIdentityReviewDecisionResult `json:"data"`
	}
	testutil.ParseJSONResponse(t, req, &envelope)
	return status, body, &envelope.Data
}

// probeWhatsAppIdentityReviewDecisionTransition attempts the exact write-once
// decision transition against the stored hold and always rolls it back. It
// isolates the database guard from the handler preview checks.
func probeWhatsAppIdentityReviewDecisionTransition(
	t *testing.T,
	db *gorm.DB,
	hold models.WhatsAppIdentityReviewHold,
	targetID, resolverID uuid.UUID,
) error {
	t.Helper()
	tx := db.Begin()
	require.NoError(t, tx.Error)
	defer tx.Rollback()
	return tx.Model(&models.WhatsAppIdentityReviewHold{}).
		Where("organization_id = ? AND id = ? AND version = 1 AND disposition = ?",
			hold.OrganizationID, hold.ID, models.WhatsAppIdentityReviewDispositionOpen).
		Updates(map[string]any{
			"version":                    2,
			"disposition":                models.WhatsAppIdentityReviewDispositionFutureRouting,
			"decision_target_contact_id": targetID,
			"decision_resolved_by_id":    resolverID,
			"decision_resolved_at":       time.Now().UTC(),
			"decision_request_id":        uuid.New(),
			"decision_request_digest":    strings.Repeat("a", sha256.Size*2),
			"decision_chain_digest":      strings.Repeat("b", sha256.Size*2),
		}).Error
}

// A message whose direct BSUID has no owner, but whose phone matches an
// address-book contact synced without user_id, must not leave that contact
// behind an identity-review hold that nobody can ever close: (1) the open hold
// blocks the member's AI, so (2) the contact dialog must be able to preview it
// and (3) the database guard must admit the reviewed decision that closes it.
func TestWhatsAppIdentityReviewUnownedPrincipalHoldOnSyncedPhoneContactIsResolvable(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	phone := "60" + testutil.NewTestGraphObjectID()[:10]
	synced := syncPhoneOnlyCoexistenceContact(t, app, account, phone)
	principal := "US.unowned-" + uuid.NewString()

	first := inboundContinuationTextMessage(t, "wamid.unowned-first-"+uuid.NewString(), phone, "first message from an unowned BSUID")
	first.FromUserID = principal
	_, duplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, first, "Unowned principal", strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)
	require.False(t, duplicate)

	var holds []models.WhatsAppIdentityReviewHold
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_account_id = ?", account.OrganizationID, account.ID).
		Find(&holds).Error)
	require.Len(t, holds, 1)
	hold := holds[0]
	require.Equal(t, models.WhatsAppIdentityReviewDispositionOpen, hold.Disposition)
	var members []models.WhatsAppIdentityReviewMember
	require.NoError(t, app.DB.Where("organization_id = ? AND hold_id = ?", account.OrganizationID, hold.ID).
		Find(&members).Error)
	require.Len(t, members, 1)
	require.Equal(t, synced.ID, members[0].ContactID)
	require.Equal(t, models.WhatsAppIdentityReviewSelectorPhone, members[0].SelectorReasons)

	// (1) While the review is open, the member's AI is blocked and the dialog is
	// pointed at exactly this hold.
	state, err := app.GetWhatsAppIdentityReviewEffectiveState(app.DB, account.OrganizationID, synced.ID)
	require.NoError(t, err)
	require.True(t, state.Blocked)
	require.False(t, state.AIAllowed)
	require.Equal(t, "identity_review_open", state.Reason)
	require.NotNil(t, state.LatestHoldID)
	require.Equal(t, hold.ID, *state.LatestHoldID)

	// (2) The contact dialog must be able to preview the hold that blocks it.
	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, account.OrganizationID)
	previewStatus, previewBody, preview := previewContactIdentityReviewOverHTTP(
		t, app, account.OrganizationID, resolver.ID, synced.ID,
	)
	assert.Equal(t, fasthttp.StatusOK, previewStatus,
		"the contact dialog cannot preview the only hold blocking the contact: %s", previewBody)

	// (3) The database guard must admit a reviewed decision for that hold.
	probeErr := probeWhatsAppIdentityReviewDecisionTransition(t, app.DB, hold, synced.ID, resolver.ID)
	assert.NoError(t, probeErr, "no reviewed decision can ever close the hold blocking the contact")
	require.True(t, previewStatus == fasthttp.StatusOK && probeErr == nil,
		"the member contact stays AI-blocked until the account reconnects")

	require.True(t, preview.Snapshot.Supported)
	require.Equal(t, hold.ID, preview.Snapshot.HoldID)
	require.True(t, identityReviewContainsContact(preview.UnionCandidates, synced.ID))
	decisionStatus, decisionBody, decision := decideContactIdentityReviewOverHTTP(
		t, app, account.OrganizationID, resolver.ID, synced.ID, synced.ID, preview,
	)
	require.Equal(t, fasthttp.StatusOK, decisionStatus, decisionBody)
	require.Equal(t, models.WhatsAppIdentityReviewDispositionFutureRouting, decision.Snapshot.Disposition)
	require.NotNil(t, decision.Snapshot.DecisionTargetContactID)
	require.Equal(t, synced.ID, *decision.Snapshot.DecisionTargetContactID)

	state, err = app.GetWhatsAppIdentityReviewEffectiveState(app.DB, account.OrganizationID, synced.ID)
	require.NoError(t, err)
	assert.False(t, state.Blocked)
	assert.True(t, state.AIAllowed)
	assert.Equal(t, "no_open_identity_review", state.Reason)

	// The reviewed decision is effective for the next message of the principal.
	next := first
	next.ID = "wamid.unowned-next-" + uuid.NewString()
	next.Text.Body = "reviewed follow-up"
	work, duplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, next, "Unowned principal", strings.Repeat("b", sha256.Size*2),
	)
	require.NoError(t, err)
	require.False(t, duplicate)
	require.NotNil(t, work, "a reviewed future route must not stage the next message")
	assert.Equal(t, synced.ID, work.Persisted.ContactID)
	assert.Equal(t, hold.ID.String(), work.Persisted.Metadata[incomingIdentityReviewHoldIDKey])
	assert.Equal(t, "reviewed_future_route", work.Persisted.Metadata[incomingIdentityReviewReasonKey])
	assert.Equal(t, incomingIdentityReviewRouteReviewedFuture, work.Persisted.Metadata[incomingIdentityReviewRouteModeKey])
	assert.False(t, incomingMessageAutomaticAISuppressed(&work.Persisted))
}

// resolveLatestContactIdentityReviewForTest follows the contact dialog: read the
// effective state, preview the hold it names, and decide for target.
func resolveLatestContactIdentityReviewForTest(
	t *testing.T,
	app *App,
	organizationID, resolverID, contactID, targetID uuid.UUID,
) *WhatsAppIdentityReviewDecisionResult {
	t.Helper()
	status, body, preview := previewContactIdentityReviewOverHTTP(t, app, organizationID, resolverID, contactID)
	require.Equal(t, fasthttp.StatusOK, status, body)
	status, body, decision := decideContactIdentityReviewOverHTTP(t, app, organizationID, resolverID, contactID, targetID, preview)
	require.Equal(t, fasthttp.StatusOK, status, body)
	return decision
}

// Once the Business App reply teaches the synced contact its BSUID, the next
// message opens a supported generation for the now-owned principal. Deciding
// that generation must also close the earlier unowned one, so the contact's AI
// resumes instead of staying behind a hold no decision can reach.
func TestWhatsAppIdentityReviewUnownedHoldClosesWithLaterBSUIDOwnerDecision(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	phone := "60" + testutil.NewTestGraphObjectID()[:10]
	synced := syncPhoneOnlyCoexistenceContact(t, app, account, phone)
	principal := "US.learned-" + uuid.NewString()

	first := inboundContinuationTextMessage(t, "wamid.learned-first-"+uuid.NewString(), phone, "before the app reply")
	first.FromUserID = principal
	_, _, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, first, "Learned principal", strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)

	// The business answers from the WhatsApp Business App; the echo carries the
	// recipient BSUID, which the coexistence contact resolver stores on the
	// phone-matched contact.
	echo := CoexistenceMessage{IncomingTextMessage: inboundContinuationTextMessage(
		t, "wamid.learned-echo-"+uuid.NewString(), "15550783881", "reply from the app",
	)}
	echo.To = phone
	echo.ToUserID = principal
	require.NoError(t, app.persistMessageEchoesBeforeAck(account.PhoneID, []CoexistenceMessage{echo}, nil))
	var learned models.Contact
	require.NoError(t, app.DB.First(&learned, "id = ?", synced.ID).Error)
	require.Equal(t, principal, learned.BSUID)

	owned := first
	owned.ID = "wamid.learned-owned-" + uuid.NewString()
	owned.Text.Body = "after the app reply"
	work, _, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, owned, "Learned principal", strings.Repeat("b", sha256.Size*2),
	)
	require.NoError(t, err)
	require.NotNil(t, work, "the now-owned principal is routed visibly while its review is open")
	require.Equal(t, synced.ID, work.Persisted.ContactID)
	require.True(t, incomingMessageAutomaticAISuppressed(&work.Persisted))

	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, account.OrganizationID)
	resolveLatestContactIdentityReviewForTest(t, app, account.OrganizationID, resolver.ID, synced.ID, synced.ID)

	state, err := app.GetWhatsAppIdentityReviewEffectiveState(app.DB, account.OrganizationID, synced.ID)
	require.NoError(t, err)
	assert.False(t, state.Blocked, "an earlier hold still blocks the reviewed contact: %+v", state)
	assert.True(t, state.AIAllowed)
	assert.Zero(t, state.OpenHoldCount)

	var open int64
	require.NoError(t, app.DB.Model(&models.WhatsAppIdentityReviewHold{}).Where(
		"organization_id = ? AND disposition = ?", account.OrganizationID, models.WhatsAppIdentityReviewDispositionOpen,
	).Count(&open).Error)
	assert.Zero(t, open)

	later := first
	later.ID = "wamid.learned-later-" + uuid.NewString()
	later.Text.Body = "reviewed"
	work, _, err = app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, later, "Learned principal", strings.Repeat("c", sha256.Size*2),
	)
	require.NoError(t, err)
	require.NotNil(t, work)
	assert.Equal(t, synced.ID, work.Persisted.ContactID)
	assert.Equal(t, "reviewed_future_route", work.Persisted.Metadata[incomingIdentityReviewReasonKey])
	assert.False(t, incomingMessageAutomaticAISuppressed(&work.Persisted))
}

// Several contacts owning the same direct BSUID stay a genuine conflict until a
// reviewer picks one. After that decision the next unchanged message must reach
// the chosen contact; staging it against the closed hold would hide it from
// both the conversation and the open-hold staged queue.
func TestWhatsAppIdentityReviewSeveralDirectOwnersDecisionRoutesNextMessage(t *testing.T) {
	app, account, first := whatsappIdentityFixture(t)
	principal := "US.shared-" + uuid.NewString()
	second := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	for _, contactID := range []uuid.UUID{first.ID, second.ID} {
		require.NoError(t, app.DB.Model(&models.Contact{}).Where(
			"organization_id = ? AND id = ?", account.OrganizationID, contactID,
		).Update("bs_uid", principal).Error)
	}

	inbound := inboundContinuationTextMessage(t, "wamid.shared-first-"+uuid.NewString(), "", "which owner?")
	inbound.FromUserID = principal
	work, _, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, inbound, "Shared principal", strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)
	require.Nil(t, work, "several direct owners must stay contact-free until review")
	for _, contactID := range []uuid.UUID{first.ID, second.ID} {
		state, stateErr := app.GetWhatsAppIdentityReviewEffectiveState(app.DB, account.OrganizationID, contactID)
		require.NoError(t, stateErr)
		require.True(t, state.Blocked)
	}

	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, account.OrganizationID)
	decision := resolveLatestContactIdentityReviewForTest(t, app, account.OrganizationID, resolver.ID, second.ID, second.ID)
	require.Equal(t, models.WhatsAppIdentityReviewDispositionFutureRouting, decision.Snapshot.Disposition)

	next := inbound
	next.ID = "wamid.shared-next-" + uuid.NewString()
	next.Text.Body = "reviewed owner"
	work, duplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, next, "Shared principal", strings.Repeat("b", sha256.Size*2),
	)
	require.NoError(t, err)
	require.False(t, duplicate)
	var staged int64
	require.NoError(t, app.DB.Model(&models.InboundEvent{}).Where(
		"organization_id = ? AND protocol = ? AND provider_event_id = ?",
		account.OrganizationID, models.WhatsAppIdentityReviewInboundProtocol, next.ID,
	).Count(&staged).Error)
	assert.Zero(t, staged, "the reviewed message was staged against a closed hold")
	require.NotNil(t, work, "the reviewed decision must route the next unchanged message")
	assert.Equal(t, second.ID, work.Persisted.ContactID)
	assert.Equal(t, decision.Snapshot.HoldID.String(), work.Persisted.Metadata[incomingIdentityReviewHoldIDKey])
	assert.Equal(t, "reviewed_future_route", work.Persisted.Metadata[incomingIdentityReviewReasonKey])
	assert.False(t, incomingMessageAutomaticAISuppressed(&work.Persisted))
}
