package handlers

import (
	"context"
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

func loadOnlyWhatsAppIdentityReviewHold(t *testing.T, app *App, account *models.WhatsAppAccount) models.WhatsAppIdentityReviewHold {
	t.Helper()
	var holds []models.WhatsAppIdentityReviewHold
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_account_id = ?", account.OrganizationID, account.ID).
		Find(&holds).Error)
	require.Len(t, holds, 1)
	return holds[0]
}

func countStagedWhatsAppIdentityReviewEvents(t *testing.T, app *App, organizationID uuid.UUID, wamid string) int64 {
	t.Helper()
	var staged int64
	require.NoError(t, app.DB.Model(&models.InboundEvent{}).Where(
		"organization_id = ? AND protocol = ? AND provider_event_id = ?",
		organizationID, models.WhatsAppIdentityReviewInboundProtocol, wamid,
	).Count(&staged).Error)
	return staged
}

// Neither the new-sender bind nor phone-app adoption may take an address-book
// contact, whose number may have been recycled, so the message stays
// contact-free. The review is a supported generation: the held list shows it as
// decidable, and no BSUID reaches the contact before a decision.
func TestWhatsAppIdentityReviewUnownedPrincipalStaysContactFreeButDecidable(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	phone := "60" + testutil.NewTestGraphObjectID()[:10]
	synced := syncPhoneOnlyCoexistenceContact(t, app, account, phone)
	inbound := inboundContinuationTextMessage(t, "wamid.unowned-held-"+uuid.NewString(), phone, "held for review")
	inbound.FromUserID = "US.unowned-held-" + uuid.NewString()

	work, duplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, inbound, "Unowned principal", strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)
	require.False(t, duplicate)
	assert.Nil(t, work)
	assert.EqualValues(t, 1, countStagedWhatsAppIdentityReviewEvents(t, app, account.OrganizationID, inbound.ID))
	var messages int64
	require.NoError(t, app.DB.Model(&models.Message{}).Where(
		"organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, inbound.ID,
	).Count(&messages).Error)
	assert.Zero(t, messages)

	hold := loadOnlyWhatsAppIdentityReviewHold(t, app, account)
	assert.True(t, hold.Supported)
	assert.Equal(t, uint64(1), hold.PrincipalGeneration)
	assert.Equal(t, inbound.FromUserID, hold.DirectPrimaryBSUID)
	var unchanged models.Contact
	require.NoError(t, app.DB.First(&unchanged, "id = ?", synced.ID).Error)
	assert.Empty(t, unchanged.BSUID, "an undecided review must not teach the contact the principal")
	assert.NotContains(t, unchanged.Metadata, coexistenceIdentityAdmissionKey)

	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, account.OrganizationID)
	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, account.OrganizationID, resolver.ID)
	require.NoError(t, app.ListStagedContactIdentityReviews(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request), string(testutil.GetResponseBody(request)))
	var page struct {
		Reviews       []stagedWhatsAppIdentityReviewItem `json:"reviews"`
		Total         int64                              `json:"total"`
		ReadOnlyTotal int64                              `json:"read_only_total"`
	}
	testutil.ParseEnvelopeResponse(t, request, &page)
	require.Len(t, page.Reviews, 1)
	assert.Equal(t, hold.ID, page.Reviews[0].HoldID)
	assert.False(t, page.Reviews[0].ReadOnly, "a decision can now resolve the held message")
	assert.Zero(t, page.ReadOnlyTotal)
}

// The new-sender bind still runs first: a contact created by this number's own
// Business app echo is adopted on its first reply, without any review hold.
func TestWhatsAppIdentityReviewPhoneAppRecipientIsAdoptedBeforeUnownedReview(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	phone := "60" + testutil.NewTestGraphObjectID()[:10]
	echo := CoexistenceMessage{IncomingTextMessage: inboundContinuationTextMessage(
		t, "wamid.adopt-echo-"+uuid.NewString(), "15550783881", "message from the app",
	)}
	echo.To = phone
	require.NoError(t, app.persistMessageEchoesBeforeAck(account.PhoneID, []CoexistenceMessage{echo}, nil))
	var recipient models.Contact
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND regexp_replace(phone_number, '[^0-9]', '', 'g') = ?", account.OrganizationID, phone,
	).First(&recipient).Error)
	require.Empty(t, recipient.BSUID)

	reply := inboundContinuationTextMessage(t, "wamid.adopt-reply-"+uuid.NewString(), phone, "reply to the app")
	reply.FromUserID = "US.adopted-" + uuid.NewString()
	work, _, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, reply, "Adopted", strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)
	require.NotNil(t, work)
	assert.Equal(t, recipient.ID, work.Persisted.ContactID)
	require.NoError(t, app.DB.First(&recipient, "id = ?", recipient.ID).Error)
	assert.Equal(t, reply.FromUserID, recipient.BSUID)
	assert.Equal(t, string(coexistenceSenderAdmissionPhoneAppRecipient), recipient.Metadata[coexistenceIdentityAdmissionKey])
	var holds int64
	require.NoError(t, app.DB.Model(&models.WhatsAppIdentityReviewHold{}).Where(
		"organization_id = ?", account.OrganizationID,
	).Count(&holds).Error)
	assert.Zero(t, holds)
}

// A phone owner that already carries a different BSUID belongs to another
// WhatsApp user (a recycled number, typically). No decision may route the new
// principal into that contact: with no other member the hold stays read-only,
// exactly as before this review became decidable.
func TestWhatsAppIdentityReviewUnownedPrincipalOtherUsersContactStaysReadOnly(t *testing.T) {
	app, account, owner := whatsappIdentityFixture(t)
	require.NoError(t, app.DB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ?", account.OrganizationID, owner.ID,
	).Update("bs_uid", "US.previous-owner-"+uuid.NewString()).Error)
	inbound := inboundContinuationTextMessage(t, "wamid.contradicted-"+uuid.NewString(), owner.PhoneNumber, "recycled number?")
	inbound.FromUserID = "US.new-holder-" + uuid.NewString()

	work, _, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, inbound, "New holder", strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)
	assert.Nil(t, work)
	assert.EqualValues(t, 1, countStagedWhatsAppIdentityReviewEvents(t, app, account.OrganizationID, inbound.ID))
	hold := loadOnlyWhatsAppIdentityReviewHold(t, app, account)
	assert.False(t, hold.Supported)
	assert.Zero(t, hold.PrincipalGeneration)

	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, account.OrganizationID)
	status, body, _ := previewContactIdentityReviewOverHTTP(t, app, account.OrganizationID, resolver.ID, owner.ID)
	assert.Equal(t, fasthttp.StatusConflict, status, body)
	assert.Contains(t, body, "identity review is read-only")
	assert.Contains(t, body, "identity_review_read_only")
	probeErr := probeWhatsAppIdentityReviewDecisionTransition(t, app.DB, hold, owner.ID, resolver.ID)
	assert.Error(t, probeErr, "the database guard keeps the read-only hold closed to decisions")
}

// When the direct BSUID has its own contact, a phone owner bound to another
// BSUID is offered as a member but never as a target: the preview lists only
// the direct owner as routable, and Decide refuses the other user's contact.
func TestWhatsAppIdentityReviewDecisionRefusesOtherUsersContact(t *testing.T) {
	app, account, direct := whatsappIdentityFixture(t)
	principal := "US.direct-" + uuid.NewString()
	require.NoError(t, app.DB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ?", account.OrganizationID, direct.ID,
	).Update("bs_uid", principal).Error)
	phoneOwner := testutil.CreateTestContactWith(t, app.DB, account.OrganizationID,
		testutil.WithPhoneNumber("60"+testutil.NewTestGraphObjectID()[:10]))
	require.NoError(t, app.DB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ?", account.OrganizationID, phoneOwner.ID,
	).Update("bs_uid", "US.previous-owner-"+uuid.NewString()).Error)
	inbound := inboundContinuationTextMessage(t, "wamid.refuse-"+uuid.NewString(), phoneOwner.PhoneNumber, "phone conflict")
	inbound.FromUserID = principal
	work, _, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, inbound, "Direct owner", strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)
	require.NotNil(t, work, "the proven direct owner still receives the held message")
	assert.Equal(t, direct.ID, work.Persisted.ContactID)

	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, account.OrganizationID)
	status, body, preview := previewContactIdentityReviewOverHTTP(t, app, account.OrganizationID, resolver.ID, direct.ID)
	require.Equal(t, fasthttp.StatusOK, status, body)
	require.True(t, preview.Snapshot.Supported)
	require.Len(t, preview.Snapshot.Candidates, 2)
	assert.Equal(t, []uuid.UUID{direct.ID}, preview.RoutableContactIDs)

	status, body, _ = decideContactIdentityReviewOverHTTP(t, app, account.OrganizationID, resolver.ID, direct.ID, phoneOwner.ID, preview)
	assert.Equal(t, fasthttp.StatusUnprocessableEntity, status, body)
	status, body, decision := decideContactIdentityReviewOverHTTP(t, app, account.OrganizationID, resolver.ID, direct.ID, direct.ID, preview)
	require.Equal(t, fasthttp.StatusOK, status, body)
	assert.Equal(t, models.WhatsAppIdentityReviewDispositionFutureRouting, decision.Snapshot.Disposition)
}

// A contact holding the parent BSUID of the claim is not adopted either: the
// message stays contact-free, and the review is decidable for that contact.
func TestWhatsAppIdentityReviewUnownedPrincipalParentOwnerIsDecidable(t *testing.T) {
	app, account, parentOwner := whatsappIdentityFixture(t)
	parent := "US.parent-" + uuid.NewString()
	require.NoError(t, app.DB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ?", account.OrganizationID, parentOwner.ID,
	).Update("bs_uid", parent).Error)
	inbound := inboundContinuationTextMessage(t, "wamid.parent-only-"+uuid.NewString(), "", "parent match only")
	inbound.FromUserID = "US.child-" + uuid.NewString()
	inbound.FromParentUserID = parent

	work, _, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, inbound, "Child principal", strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)
	assert.Nil(t, work)
	assert.EqualValues(t, 1, countStagedWhatsAppIdentityReviewEvents(t, app, account.OrganizationID, inbound.ID))
	hold := loadOnlyWhatsAppIdentityReviewHold(t, app, account)
	require.True(t, hold.Supported)

	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, account.OrganizationID)
	resolveLatestContactIdentityReviewForTest(t, app, account.OrganizationID, resolver.ID, parentOwner.ID, parentOwner.ID)
	next := inbound
	next.ID = "wamid.parent-only-next-" + uuid.NewString()
	work, _, err = app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, next, "Child principal", strings.Repeat("b", sha256.Size*2),
	)
	require.NoError(t, err)
	require.NotNil(t, work)
	assert.Equal(t, parentOwner.ID, work.Persisted.ContactID)
	assert.Equal(t, "reviewed_future_route", work.Persisted.Metadata[incomingIdentityReviewReasonKey])
	assert.False(t, incomingMessageAutomaticAISuppressed(&work.Persisted))
}

// Two different selector owners and no direct owner remain a genuine conflict:
// contact-free and blocking both members until a reviewer picks one, after
// which only the chosen member receives the principal and the other resumes.
func TestWhatsAppIdentityReviewUnownedPrincipalWithTwoSelectorOwnersNeedsReview(t *testing.T) {
	app, account, phoneOwner := whatsappIdentityFixture(t)
	parentOwner := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	parent := "US.parent-" + uuid.NewString()
	require.NoError(t, app.DB.Model(&models.Contact{}).Where(
		"organization_id = ? AND id = ?", account.OrganizationID, parentOwner.ID,
	).Update("bs_uid", parent).Error)
	inbound := inboundContinuationTextMessage(t, "wamid.two-owners-"+uuid.NewString(), phoneOwner.PhoneNumber, "phone and parent disagree")
	inbound.FromUserID = "US.child-" + uuid.NewString()
	inbound.FromParentUserID = parent

	work, _, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, inbound, "Child principal", strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)
	assert.Nil(t, work)
	assert.EqualValues(t, 1, countStagedWhatsAppIdentityReviewEvents(t, app, account.OrganizationID, inbound.ID))
	hold := loadOnlyWhatsAppIdentityReviewHold(t, app, account)
	require.True(t, hold.Supported)
	require.EqualValues(t, 2, hold.MemberCount)
	for _, contactID := range []uuid.UUID{phoneOwner.ID, parentOwner.ID} {
		state, stateErr := app.GetWhatsAppIdentityReviewEffectiveState(app.DB, account.OrganizationID, contactID)
		require.NoError(t, stateErr)
		require.True(t, state.Blocked)
	}

	replay := inbound
	replay.ID = "wamid.two-owners-replay-" + uuid.NewString()
	work, _, err = app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, replay, "Child principal", strings.Repeat("b", sha256.Size*2),
	)
	require.NoError(t, err)
	assert.Nil(t, work, "an open review of a genuine conflict keeps later messages contact-free")
	assert.Equal(t, hold.ID, loadOnlyWhatsAppIdentityReviewHold(t, app, account).ID)

	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, account.OrganizationID)
	resolveLatestContactIdentityReviewForTest(t, app, account.OrganizationID, resolver.ID, phoneOwner.ID, parentOwner.ID)
	for _, contactID := range []uuid.UUID{phoneOwner.ID, parentOwner.ID} {
		state, stateErr := app.GetWhatsAppIdentityReviewEffectiveState(app.DB, account.OrganizationID, contactID)
		require.NoError(t, stateErr)
		assert.True(t, state.AIAllowed)
	}

	next := inbound
	next.ID = "wamid.two-owners-next-" + uuid.NewString()
	work, _, err = app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, next, "Child principal", strings.Repeat("c", sha256.Size*2),
	)
	require.NoError(t, err)
	require.NotNil(t, work)
	assert.Equal(t, parentOwner.ID, work.Persisted.ContactID)
	assert.Equal(t, "reviewed_future_route", work.Persisted.Metadata[incomingIdentityReviewReasonKey])
}

// Before any generation exists, an unowned direct BSUID keeps the exact
// admission shape the new-sender bind accepts (ambiguous, no route, no latest
// hold), so that bind is always tried first. With no selector owner the hold
// it falls back to stays unsupported and memberless, blocking nobody; with a
// selector owner it is a supported generation.
func TestWhatsAppIdentityReviewUnownedAdmissionKeepsNewSenderGateShape(t *testing.T) {
	app, db, organization, account := setupWhatsAppIdentityReviewAdmissionTest(t)
	bystander := testutil.CreateTestContact(t, db, organization.ID)
	phoneOwner := testutil.CreateTestContactWith(t, db, organization.ID,
		testutil.WithPhoneNumber("+60"+testutil.NewTestGraphObjectID()[:10]))

	for _, fixture := range []struct {
		name      string
		phone     string
		supported bool
		members   uint32
	}{
		{name: "no_selector_owner", phone: "60" + testutil.NewTestGraphObjectID()[:10]},
		{name: "phone_only_owner", phone: phoneOwner.PhoneNumber, supported: true, members: 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			claim := WhatsAppIdentityReviewClaim{
				OrganizationID: organization.ID, WhatsAppAccountID: account.ID, OnboardingCycle: 1,
				DirectPrimaryBSUID: "US.unowned-" + uuid.NewString(), Phone: fixture.phone,
				VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
				VerifiedEventDigest:     strings.Repeat("a", 64), SelectorBodyDigest: strings.Repeat("b", 64),
			}
			var admission *WhatsAppIdentityReviewAdmission
			var snapshot *WhatsAppIdentityReviewSnapshot
			require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
				var err error
				if admission, err = app.EvaluateWhatsAppIdentityReviewAdmission(tx, &claim); err != nil {
					return err
				}
				snapshot, _, err = app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &claim)
				return err
			}))
			assert.True(t, admission.Blocked)
			assert.True(t, admission.NeedsReview)
			assert.Equal(t, "direct_primary_ambiguous", admission.Reason)
			assert.Nil(t, admission.RouteContactID)
			assert.Nil(t, admission.LatestHoldID)
			assert.Nil(t, admission.DirectPrimaryOwnerID)
			assert.Equal(t, fixture.supported, snapshot.Supported)
			assert.Equal(t, fixture.members, snapshot.MemberCount)
			if !fixture.supported {
				assert.Zero(t, snapshot.PrincipalGeneration)
			}
		})
	}
	state, err := app.GetWhatsAppIdentityReviewEffectiveState(db, organization.ID, bystander.ID)
	require.NoError(t, err)
	assert.True(t, state.AIAllowed)
}

// Once a reviewer routes an unowned principal to a contact, the first Business
// app reply that teaches that contact the principal's BSUID changes only the
// target's reason bits. The decision still covers that set: the next message
// is routed with AI and opens no second review. Any other change still asks
// again.
func TestWhatsAppIdentityReviewDecisionSurvivesTargetLearningTheBSUID(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	phone := "60" + testutil.NewTestGraphObjectID()[:10]
	synced := syncPhoneOnlyCoexistenceContact(t, app, account, phone)
	principal := "US.learn-" + uuid.NewString()

	first := inboundContinuationTextMessage(t, "wamid.learn-first-"+uuid.NewString(), phone, "held first")
	first.FromUserID = principal
	work, _, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, first, "Learner", strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)
	require.Nil(t, work)
	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, organizationID)
	decision := resolveLatestContactIdentityReviewForTest(t, app, organizationID, resolver.ID, synced.ID, synced.ID)

	echo := CoexistenceMessage{IncomingTextMessage: inboundContinuationTextMessage(
		t, "wamid.learn-echo-"+uuid.NewString(), "15550783881", "reply from the app",
	)}
	echo.To = phone
	echo.ToUserID = principal
	require.NoError(t, app.persistMessageEchoesBeforeAck(account.PhoneID, []CoexistenceMessage{echo}, nil))
	require.Equal(t, principal, loadCoexistenceAdmissionContact(t, app.DB, organizationID, synced.ID).BSUID)

	next := first
	next.ID = "wamid.learn-next-" + uuid.NewString()
	work, _, err = app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, next, "Learner", strings.Repeat("b", sha256.Size*2),
	)
	require.NoError(t, err)
	require.NotNil(t, work)
	assert.Equal(t, synced.ID, work.Persisted.ContactID)
	assert.Equal(t, "reviewed_future_route", work.Persisted.Metadata[incomingIdentityReviewReasonKey])
	assert.Equal(t, decision.Snapshot.HoldID.String(), work.Persisted.Metadata[incomingIdentityReviewHoldIDKey])
	assert.False(t, incomingMessageAutomaticAISuppressed(&work.Persisted))
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{}, "organization_id = ?", organizationID))
	state, err := app.GetWhatsAppIdentityReviewEffectiveState(app.DB, organizationID, synced.ID)
	require.NoError(t, err)
	assert.True(t, state.AIAllowed)

	require.NoError(t, app.DB.Create(&models.ChatbotSettings{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationID,
		WhatsAppAccount: account.Name, IsEnabled: false, AI: models.AIConfig{Enabled: false},
	}).Error)
	processor := NewInboundContinuationProcessor(app, time.Hour)
	require.NoError(t, processor.ProcessMessage(context.Background(), organizationID, work.Persisted.ID))
	assert.Equal(t, models.ScheduledJobStatusCompleted, loadInboundContinuationJob(t, app, organizationID, work.Persisted.ID).Status)

	// A second phone owner is a new person in the set: two contacts now own the
	// phone, so the message is held contact-free under a fresh generation.
	testutil.CreateTestContactWith(t, app.DB, organizationID, testutil.WithPhoneNumber("+"+phone))
	again := first
	again.ID = "wamid.learn-again-" + uuid.NewString()
	work, _, err = app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, again, "Learner", strings.Repeat("c", sha256.Size*2),
	)
	require.NoError(t, err)
	assert.Nil(t, work)
	assert.EqualValues(t, 1, countStagedWhatsAppIdentityReviewEvents(t, app, organizationID, again.ID))
	assert.EqualValues(t, 2, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{}, "organization_id = ?", organizationID))
	state, err = app.GetWhatsAppIdentityReviewEffectiveState(app.DB, organizationID, synced.ID)
	require.NoError(t, err)
	assert.True(t, state.Blocked)
}

// The held list can be narrowed to one hold, which lets the contact dialog
// show the held message beside the decision.
func TestListStagedContactIdentityReviewsFiltersByHold(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	heldFor := func(label string) uuid.UUID {
		phone := "60" + testutil.NewTestGraphObjectID()[:10]
		syncPhoneOnlyCoexistenceContact(t, app, account, phone)
		message := inboundContinuationTextMessage(t, "wamid.filter-"+label+"-"+uuid.NewString(), phone, "held "+label)
		message.FromUserID = "US.filter-" + uuid.NewString()
		work, _, err := app.persistAuthenticatedIncomingMessageBeforeAck(
			account.PhoneID, message, "Held", strings.Repeat("a", sha256.Size*2),
		)
		require.NoError(t, err)
		require.Nil(t, work)
		var hold models.WhatsAppIdentityReviewHold
		require.NoError(t, app.DB.Where("organization_id = ? AND direct_primary_bsuid = ?", organizationID, message.FromUserID).
			First(&hold).Error)
		return hold.ID
	}
	first := heldFor("first")
	heldFor("second")

	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, organizationID)
	list := func(holdID string) (int, []stagedWhatsAppIdentityReviewItem, int64) {
		request := testutil.NewGETRequest(t)
		testutil.SetAuthContext(request, organizationID, resolver.ID)
		if holdID != "" {
			testutil.SetQueryParam(request, "hold_id", holdID)
		}
		require.NoError(t, app.ListStagedContactIdentityReviews(request))
		status := testutil.GetResponseStatusCode(request)
		if status != fasthttp.StatusOK {
			return status, nil, 0
		}
		var page struct {
			Reviews []stagedWhatsAppIdentityReviewItem `json:"reviews"`
			Total   int64                              `json:"total"`
		}
		testutil.ParseEnvelopeResponse(t, request, &page)
		return status, page.Reviews, page.Total
	}
	_, all, total := list("")
	assert.Len(t, all, 2)
	assert.EqualValues(t, 2, total)
	_, narrowed, total := list(first.String())
	require.Len(t, narrowed, 1)
	assert.Equal(t, first, narrowed[0].HoldID)
	assert.EqualValues(t, 1, total)
	status, _, _ := list("not-a-uuid")
	assert.Equal(t, fasthttp.StatusBadRequest, status)
}
