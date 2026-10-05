package handlers

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setWhatsAppAccountCoexistenceForTest(t *testing.T, app *App, account *models.WhatsAppAccount, coexistence bool) {
	t.Helper()
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where(
		"organization_id = ? AND id = ?", account.OrganizationID, account.ID,
	).Update("is_smb", coexistence).Error)
	account.IsSMB = coexistence
}

// deliverClassicInboundForTest admits one inbound message through the classic
// Cloud API path, as this number's traffic was recorded before it joined
// Coexistence, and leaves the account in Coexistence afterwards.
func deliverClassicInboundForTest(
	t *testing.T,
	app *App,
	account *models.WhatsAppAccount,
	phone, userID string,
) *models.Message {
	t.Helper()
	wasCoexistence := account.IsSMB
	setWhatsAppAccountCoexistenceForTest(t, app, account, false)
	defer setWhatsAppAccountCoexistenceForTest(t, app, account, wasCoexistence)
	message := coexistenceAdmissionMessage(t, "wamid.classic-"+uuid.NewString(), phone, userID, "classic order")
	work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, message, "Classic Customer")
	require.False(t, duplicate)
	require.NotNil(t, work)
	return &work.Persisted
}

func createLegacyUnsupportedHoldForTest(t *testing.T, app *App, account *models.WhatsAppAccount, phone string) uuid.UUID {
	t.Helper()
	var cycle uint64
	require.NoError(t, app.DB.Model(&models.WhatsAppCoexistenceState{}).Select("onboarding_cycle").Where(
		"organization_id = ? AND whats_app_account_id = ?", account.OrganizationID, account.ID,
	).Scan(&cycle).Error)
	// A BSUID-less claim matching the phone owner reproduces the unsupported,
	// member-bearing hold that earlier releases created.
	claim := WhatsAppIdentityReviewClaim{
		OrganizationID: account.OrganizationID, WhatsAppAccountID: account.ID, OnboardingCycle: cycle,
		Phone: phone, VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
		VerifiedEventDigest: strings.Repeat("c", 64), SelectorBodyDigest: strings.Repeat("d", 64),
	}
	var snapshot *WhatsAppIdentityReviewSnapshot
	require.NoError(t, app.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		snapshot, _, err = app.CreateOrReuseWhatsAppIdentityReviewHold(tx, &claim)
		return err
	}))
	require.False(t, snapshot.Supported)
	require.EqualValues(t, 1, snapshot.MemberCount)
	return snapshot.HoldID
}

// A customer who already wrote to this number before it joined Coexistence is
// bound to their existing contact on the first message that carries a BSUID,
// and the message is admitted with the normal AI policy.
func TestCoexistenceClassicHistoryCustomerIsBoundToTheirContact(t *testing.T) {
	for _, variant := range []string{"phone_owner", "parent_bsuid_owner", "wa_id_only_sender"} {
		t.Run(variant, func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			organizationID := account.OrganizationID
			phone := coexistenceAdmissionTestPhone()
			classic := deliverClassicInboundForTest(t, app, account, phone, "")
			customer := loadCoexistenceAdmissionContact(t, app.DB, organizationID, classic.ContactID)
			require.Empty(t, customer.BSUID)

			bsuid := "US.classic-" + uuid.NewString()
			message := coexistenceAdmissionMessage(t, "wamid.switch-"+uuid.NewString(), phone, bsuid, "first message after the switch")
			switch variant {
			case "parent_bsuid_owner":
				parent := "US.ENT.classic-" + uuid.NewString()[:8]
				require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", customer.ID).Update("bs_uid", parent).Error)
				message.FromParentUserID = parent
			case "wa_id_only_sender":
				sidecar := &CoexistenceWebhookContact{UserID: bsuid, WaID: phone}
				sidecar.Profile.Name = "Classic Customer"
				message = coexistenceAdmissionMessage(t, message.ID, "", bsuid, "first message after the switch").
					withWebhookSenderContact(sidecar)
			}
			contactsBefore := countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", organizationID)

			work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, message, "Classic Customer")
			require.False(t, duplicate)
			require.NotNil(t, work, "a classic-history customer must not be held")
			assert.Equal(t, customer.ID, work.Persisted.ContactID)
			assert.False(t, incomingMessageAutomaticAISuppressed(&work.Persisted))
			assert.NotContains(t, work.Persisted.Metadata, incomingIdentityReviewHoldIDKey)

			bound := loadCoexistenceAdmissionContact(t, app.DB, organizationID, customer.ID)
			assert.Equal(t, bsuid, bound.BSUID)
			assert.Equal(t, string(coexistenceSenderAdmissionClassicHistory), bound.Metadata[coexistenceIdentityAdmissionKey])
			assert.Equal(t, message.ID, bound.Metadata[coexistenceIdentityAdmissionWAMIDKey])
			assert.Equal(t, contactsBefore, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", organizationID))
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{}, "organization_id = ?", organizationID))
			assert.Zero(t, countCoexistenceStagedReceipts(t, app, organizationID, message.ID))
			state, err := app.GetWhatsAppIdentityReviewEffectiveState(app.DB, organizationID, customer.ID)
			require.NoError(t, err)
			assert.True(t, state.AIAllowed)

			later := coexistenceAdmissionMessage(t, "wamid.switch-later-"+uuid.NewString(), phone, bsuid, "and again")
			later.FromParentUserID = message.FromParentUserID
			work, _ = admitCoexistenceAdmissionMessage(t, app, account, later, "Classic Customer")
			require.NotNil(t, work)
			assert.Equal(t, customer.ID, work.Persisted.ContactID)
			assert.False(t, incomingMessageAutomaticAISuppressed(&work.Persisted))
		})
	}
}

// Concurrent first messages after the switch (plus a provider retry) wait on
// the same admission fence and bind the BSUID to the one classic contact.
func TestCoexistenceClassicHistoryConcurrentFirstMessagesBindOneContact(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	phone := coexistenceAdmissionTestPhone()
	classic := deliverClassicInboundForTest(t, app, account, phone, "")
	bsuid := "US.classic-race-" + uuid.NewString()
	const distinct = 4
	messages := make([]IncomingTextMessage, 0, distinct+1)
	for index := range distinct {
		messages = append(messages, coexistenceAdmissionMessage(
			t, fmt.Sprintf("wamid.classic-race-%d-%s", index, uuid.NewString()), phone, bsuid, "race",
		))
	}
	messages = append(messages, messages[0])
	contactsBefore := countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", organizationID)

	baseline := coexistenceUngrantedAdvisoryLocks(t, app.DB)
	blocker := app.DB.Begin()
	require.NoError(t, blocker.Error)
	blockerOpen := true
	defer func() {
		if blockerOpen {
			blocker.Rollback()
		}
	}()
	require.NoError(t, database.LockOrganizationPolicyScope(blocker, organizationID))

	type result struct {
		work      *persistedIncomingMessage
		duplicate bool
		err       error
	}
	results := make(chan result, len(messages))
	var wg sync.WaitGroup
	for _, message := range messages {
		wg.Add(1)
		go func(message IncomingTextMessage) {
			defer wg.Done()
			var work *persistedIncomingMessage
			var duplicate bool
			var err error
			for attempt := 0; attempt < 8; attempt++ {
				work, duplicate, err = app.persistAuthenticatedIncomingMessageBeforeAck(
					account.PhoneID, message, "Classic Customer", strings.Repeat("b", sha256.Size*2),
				)
				if !isRetryableCanonicalContactWrite(err) {
					break
				}
			}
			results <- result{work: work, duplicate: duplicate, err: err}
		}(message)
	}
	deadline := time.Now().Add(20 * time.Second)
	for coexistenceUngrantedAdvisoryLocks(t, app.DB) < baseline+int64(len(messages)) {
		require.True(t, time.Now().Before(deadline), "every admission must be waiting on the shared fence")
		time.Sleep(20 * time.Millisecond)
	}
	require.NoError(t, blocker.Rollback().Error)
	blockerOpen = false
	wg.Wait()
	close(results)

	admitted, duplicates := 0, 0
	for result := range results {
		require.NoError(t, result.err)
		if result.duplicate {
			duplicates++
			continue
		}
		require.NotNil(t, result.work)
		admitted++
		assert.Equal(t, classic.ContactID, result.work.Persisted.ContactID)
	}
	assert.Equal(t, distinct, admitted)
	assert.Equal(t, 1, duplicates)
	assert.Equal(t, bsuid, loadCoexistenceAdmissionContact(t, app.DB, organizationID, classic.ContactID).BSUID)
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND bs_uid = ?", organizationID, bsuid))
	assert.Equal(t, contactsBefore, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", organizationID))
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{}, "organization_id = ?", organizationID))
}

// Without this account's own Meta-attested inbound history from the same
// phone, or with anything tying the contact to someone else, the sender keeps
// today's contact-free review.
func TestCoexistenceClassicHistoryRequiresThisAccountsOwnAttestedHistory(t *testing.T) {
	type setup struct {
		app     *App
		account *models.WhatsAppAccount
		phone   string
		bsuid   string
	}
	cases := []struct {
		name string
		// arrange returns the contact that must stay unbound.
		arrange  func(t *testing.T, s setup) uuid.UUID
		readOnly bool
	}{
		{name: "inbound_only_on_another_account", arrange: func(t *testing.T, s setup) uuid.UUID {
			other := testutil.CreateTestWhatsAppAccount(t, s.app.DB, s.account.OrganizationID)
			setWhatsAppAccountCoexistenceForTest(t, s.app, other, false)
			return deliverClassicInboundForTest(t, s.app, other, s.phone, "").ContactID
		}},
		{name: "address_book_only", arrange: func(t *testing.T, s setup) uuid.UUID {
			return syncPhoneOnlyCoexistenceContact(t, s.app, s.account, s.phone).ID
		}},
		{name: "chat_history_only", arrange: func(t *testing.T, s setup) uuid.UUID {
			contact := testutil.CreateTestContactWith(t, s.app.DB, s.account.OrganizationID, testutil.WithPhoneNumber(s.phone))
			history := inboundContinuationTextMessage(t, "wamid.history-"+uuid.NewString(), s.phone, "imported history")
			persistWhatsAppIdentityHistory(t, s.app, s.account, contact, history)
			return contact.ID
		}},
		{name: "bound_to_another_bsuid", readOnly: true, arrange: func(t *testing.T, s setup) uuid.UUID {
			contactID := deliverClassicInboundForTest(t, s.app, s.account, s.phone, "").ContactID
			require.NoError(t, s.app.DB.Model(&models.Contact{}).Where("id = ?", contactID).
				Update("bs_uid", "US.previous-holder-"+uuid.NewString()).Error)
			return contactID
		}},
		{name: "earlier_sender_was_another_user", arrange: func(t *testing.T, s setup) uuid.UUID {
			contactID := deliverClassicInboundForTest(t, s.app, s.account, s.phone, "US.previous-holder-"+uuid.NewString()).ContactID
			// The classic path learned that BSUID; clear it to prove the attested
			// history alone keeps the contact from a different sender.
			require.NoError(t, s.app.DB.Model(&models.Contact{}).Where("id = ?", contactID).Update("bs_uid", "").Error)
			return contactID
		}},
		{name: "two_phone_matches", arrange: func(t *testing.T, s setup) uuid.UUID {
			contactID := deliverClassicInboundForTest(t, s.app, s.account, s.phone, "").ContactID
			testutil.CreateTestContactWith(t, s.app.DB, s.account.OrganizationID, testutil.WithPhoneNumber("+"+s.phone))
			return contactID
		}},
		{name: "merged_into_another_contact", arrange: func(t *testing.T, s setup) uuid.UUID {
			contactID := deliverClassicInboundForTest(t, s.app, s.account, s.phone, "").ContactID
			target := testutil.CreateTestContact(t, s.app.DB, s.account.OrganizationID)
			require.NoError(t, s.app.DB.Model(&models.Contact{}).Where("id = ?", contactID).Updates(map[string]any{
				"merged_into_id": target.ID, "merged_at": time.Now().UTC(),
			}).Error)
			require.NoError(t, s.app.DB.Delete(&models.Contact{}, "id = ?", contactID).Error)
			return contactID
		}},
		{name: "soft_deleted", arrange: func(t *testing.T, s setup) uuid.UUID {
			contactID := deliverClassicInboundForTest(t, s.app, s.account, s.phone, "").ContactID
			require.NoError(t, s.app.DB.Delete(&models.Contact{}, "id = ?", contactID).Error)
			return contactID
		}},
		{name: "open_review_of_this_phone", arrange: func(t *testing.T, s setup) uuid.UUID {
			contactID := deliverClassicInboundForTest(t, s.app, s.account, s.phone, "").ContactID
			createLegacyUnsupportedHoldForTest(t, s.app, s.account, s.phone)
			return contactID
		}},
		{name: "decided_review_of_this_phone", arrange: func(t *testing.T, s setup) uuid.UUID {
			contactID := deliverClassicInboundForTest(t, s.app, s.account, s.phone, "").ContactID
			// Another BSUID with its own contact wrote from this phone, and a
			// reviewer routed it to that contact: a person decided this phone.
			owner := testutil.CreateTestContact(t, s.app.DB, s.account.OrganizationID)
			ownerBSUID := "US.reviewed-" + uuid.NewString()
			require.NoError(t, s.app.DB.Model(&models.Contact{}).Where("id = ?", owner.ID).Update("bs_uid", ownerBSUID).Error)
			reviewed := coexistenceAdmissionMessage(t, "wamid.reviewed-"+uuid.NewString(), s.phone, ownerBSUID, "reviewed")
			work, _ := admitCoexistenceAdmissionMessage(t, s.app, s.account, reviewed, "Reviewed")
			require.NotNil(t, work)
			var hold models.WhatsAppIdentityReviewHold
			require.NoError(t, s.app.DB.Where("organization_id = ? AND direct_primary_bsuid = ?", s.account.OrganizationID, ownerBSUID).
				First(&hold).Error)
			resolver := createWhatsAppIdentityReviewResolver(t, s.app.DB, s.account.OrganizationID)
			decideWhatsAppIdentityReviewForTest(t, s.app, s.app.DB, s.account.OrganizationID, resolver.ID, hold.ID, owner.ID)
			require.False(t, loadCoexistenceAdmissionContact(t, s.app.DB, s.account.OrganizationID, contactID).DeletedAt.Valid)
			return contactID
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			s := setup{app: app, account: account, phone: coexistenceAdmissionTestPhone(), bsuid: "US.switch-" + uuid.NewString()}
			contactID := tc.arrange(t, s)
			organizationID := account.OrganizationID
			before := loadCoexistenceAdmissionContact(t, app.DB, organizationID, contactID)

			message := coexistenceAdmissionMessage(t, "wamid.switch-"+uuid.NewString(), s.phone, s.bsuid, "after the switch")
			work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, message, "Switch Customer")
			assert.False(t, duplicate)
			assert.Nil(t, work, "the sender must keep the contact-free review path")
			assert.EqualValues(t, 1, countCoexistenceStagedReceipts(t, app, organizationID, message.ID))
			after := loadCoexistenceAdmissionContact(t, app.DB, organizationID, contactID)
			assert.Equal(t, before.BSUID, after.BSUID, "the contact must not receive the sender's BSUID")
			assert.NotEqual(t, string(coexistenceSenderAdmissionClassicHistory), after.Metadata[coexistenceIdentityAdmissionKey])
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{},
				"organization_id = ? AND bs_uid = ?", organizationID, s.bsuid))
			if tc.readOnly {
				var hold models.WhatsAppIdentityReviewHold
				require.NoError(t, app.DB.Where(
					"organization_id = ? AND direct_primary_bsuid = ?", organizationID, s.bsuid,
				).First(&hold).Error)
				assert.False(t, hold.Supported, "another user's contact leaves the review read-only")
			}
		})
	}
}

// insertMainShapeUnsupportedHoldForTest writes the hold the releases before
// #226 created for a sender whose direct BSUID had no owner but whose phone
// matched a contact: unsupported, generation 0, the sender's BSUID, and that
// contact as a phone-only member.
func insertMainShapeUnsupportedHoldForTest(
	t *testing.T,
	app *App,
	account *models.WhatsAppAccount,
	direct, phone string,
	memberID uuid.UUID,
) uuid.UUID {
	t.Helper()
	var cycle uint64
	require.NoError(t, app.DB.Model(&models.WhatsAppCoexistenceState{}).Select("onboarding_cycle").Where(
		"organization_id = ? AND whats_app_account_id = ?", account.OrganizationID, account.ID,
	).Scan(&cycle).Error)
	members := []WhatsAppIdentityReviewCandidate{{ContactID: memberID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPhone}}
	memberDigest := identityReviewMemberDigest(members)
	claim := WhatsAppIdentityReviewClaim{
		OrganizationID: account.OrganizationID, WhatsAppAccountID: account.ID, OnboardingCycle: cycle,
		DirectPrimaryBSUID: direct, Phone: phone,
	}
	hold := models.WhatsAppIdentityReviewHold{
		ID: uuid.New(), OrganizationID: account.OrganizationID, WhatsAppAccountID: account.ID,
		OnboardingCycle: cycle, ProtocolVersion: models.WhatsAppIdentityReviewProtocolVersion,
		DirectPrimaryBSUID: direct, Phone: phone,
		SemanticClaimDigest: identityReviewSemanticDigest(claim, memberDigest),
		SelectorBodyDigest:  strings.Repeat("e", 64), VerifiedEventDigest: strings.Repeat("f", 64),
		VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
		MemberCount:             1, MemberDigest: memberDigest, Version: 1,
		Disposition: models.WhatsAppIdentityReviewDispositionOpen,
	}
	require.NoError(t, app.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&hold).Error; err != nil {
			return err
		}
		return tx.Create(&models.WhatsAppIdentityReviewMember{
			OrganizationID: account.OrganizationID, HoldID: hold.ID, ContactID: memberID,
			SelectorReasons: models.WhatsAppIdentityReviewSelectorPhone,
		}).Error
	}))
	return hold.ID
}

func reconnectCoexistenceAccountForTest(t *testing.T, app *App, account *models.WhatsAppAccount, holdID uuid.UUID) {
	t.Helper()
	require.NoError(t, app.DB.Model(&models.WhatsAppCoexistenceState{}).Where(
		"organization_id = ? AND whats_app_account_id = ?", account.OrganizationID, account.ID,
	).Update("onboarding_cycle", gorm.Expr("onboarding_cycle + 1")).Error)
	var hold models.WhatsAppIdentityReviewHold
	require.NoError(t, app.DB.First(&hold, "id = ?", holdID).Error)
	require.Equal(t, models.WhatsAppIdentityReviewDispositionSupersededByCycle, hold.Disposition)
}

// A reconnect closes this sender's own pre-#226 review without recording a
// decision, so afterwards the classic-history customer is bound. While that
// review is still open, or when the closed review was left by a different
// BSUID on the same phone (a second WhatsApp user), the sender stays held.
func TestCoexistenceClassicHistoryAfterReconnect(t *testing.T) {
	for _, variant := range []string{"own_review_open", "own_review_closed_by_reconnect", "other_bsuid_review_closed_by_reconnect"} {
		t.Run(variant, func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			organizationID := account.OrganizationID
			phone := coexistenceAdmissionTestPhone()
			classic := deliverClassicInboundForTest(t, app, account, phone, "")
			bsuid := "US.reconnect-" + uuid.NewString()
			holdBSUID := bsuid
			if variant == "other_bsuid_review_closed_by_reconnect" {
				holdBSUID = "US.second-user-" + uuid.NewString()
			}
			legacyHold := insertMainShapeUnsupportedHoldForTest(t, app, account, holdBSUID, phone, classic.ContactID)
			if variant != "own_review_open" {
				reconnectCoexistenceAccountForTest(t, app, account, legacyHold)
			}

			message := coexistenceAdmissionMessage(t, "wamid.reconnect-"+uuid.NewString(), phone, bsuid, "after the reconnect")
			work, _ := admitCoexistenceAdmissionMessage(t, app, account, message, "Classic Customer")
			if variant != "own_review_closed_by_reconnect" {
				assert.Nil(t, work, "the sender must stay held")
				assert.Empty(t, loadCoexistenceAdmissionContact(t, app.DB, organizationID, classic.ContactID).BSUID)
				return
			}
			require.NotNil(t, work)
			assert.Equal(t, classic.ContactID, work.Persisted.ContactID)
			assert.Equal(t, bsuid, loadCoexistenceAdmissionContact(t, app.DB, organizationID, classic.ContactID).BSUID)
			state, err := app.GetWhatsAppIdentityReviewEffectiveState(app.DB, organizationID, classic.ContactID)
			require.NoError(t, err)
			assert.True(t, state.AIAllowed)
		})
	}
}

// Another account's newer inbound traffic on the same contact can neither
// crowd this account's attested history out of the evidence window nor stand
// in for it.
func TestCoexistenceClassicHistoryEvidenceWindowCoversOnlyThisAccount(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	phone := coexistenceAdmissionTestPhone()
	classic := deliverClassicInboundForTest(t, app, account, phone, "")
	other := testutil.CreateTestWhatsAppAccount(t, app.DB, organizationID)

	crowd := coexistenceClassicHistoryEvidenceLimit + 1
	base := time.Now().UTC().Add(time.Minute)
	messages := make([]models.Message, 0, crowd)
	jobs := make([]models.ScheduledJob, 0, crowd)
	for index := range crowd {
		wamid := fmt.Sprintf("wamid.other-account-%d-%s", index, uuid.NewString())
		messageID := uuid.New()
		messages = append(messages, models.Message{
			BaseModel:      models.BaseModel{ID: messageID, CreatedAt: base, UpdatedAt: base},
			OrganizationID: organizationID, WhatsAppAccount: other.Name, ContactID: classic.ContactID,
			WhatsAppMessageID: wamid, Direction: models.DirectionIncoming, MessageType: models.MessageTypeText,
			Content: "other account", Status: models.MessageStatusReceived, Metadata: models.JSONB{},
		})
		created := base.Add(time.Duration(index) * time.Millisecond)
		jobs = append(jobs, models.ScheduledJob{
			BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: created, UpdatedAt: created},
			OrganizationID: organizationID, Kind: inboundContinuationJobKind, AggregateType: "message",
			AggregateID: &messageID, RunAt: created, Status: models.ScheduledJobStatusCompleted,
			MaxAttempts:    defaultInboundContinuationMaxTry,
			IdempotencyKey: "inbound-message-continuation:" + uuid.NewSHA1(other.ID, []byte(wamid)).String(),
			Payload: models.JSONB{
				"phone_number_id": other.PhoneID, "wamid": wamid, "message_id": messageID.String(),
				"message": map[string]any{"from": phone, "id": wamid, "type": "text", "timestamp": "1722222222"},
			},
			Version: 1,
		})
	}
	require.NoError(t, app.DB.CreateInBatches(&messages, 100).Error)
	require.NoError(t, app.DB.CreateInBatches(&jobs, 100).Error)

	bsuid := "US.window-" + uuid.NewString()
	work, _ := admitCoexistenceAdmissionMessage(t, app, account,
		coexistenceAdmissionMessage(t, "wamid.window-"+uuid.NewString(), phone, bsuid, "after the switch"), "Classic Customer")
	require.NotNil(t, work, "this account's attested history must stay inside the evidence window")
	assert.Equal(t, classic.ContactID, work.Persisted.ContactID)
	assert.Equal(t, bsuid, loadCoexistenceAdmissionContact(t, app.DB, organizationID, classic.ContactID).BSUID)
}
