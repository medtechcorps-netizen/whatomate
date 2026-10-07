package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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

const coexistenceAdmissionBusinessPhone = "15550783881"

// coexistenceAdmissionTestPhone is a plausible, tenant-unique E.164 number
// without "+" (12 digits).
func coexistenceAdmissionTestPhone() string {
	return "6011" + testutil.NewTestGraphObjectID()[:8]
}

func coexistenceAdmissionMessage(t *testing.T, wamid, from, userID, body string) IncomingTextMessage {
	t.Helper()
	message := inboundContinuationTextMessage(t, wamid, from, body)
	message.FromUserID = userID
	return message
}

func admitCoexistenceAdmissionMessage(
	t *testing.T,
	app *App,
	account *models.WhatsAppAccount,
	message IncomingTextMessage,
	profileName string,
) (*persistedIncomingMessage, bool) {
	t.Helper()
	work, duplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		profileName,
		strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)
	return work, duplicate
}

func persistCoexistenceAdmissionEcho(
	t *testing.T,
	app *App,
	account *models.WhatsAppAccount,
	wamid, to, toUserID, body string,
) {
	t.Helper()
	echo := CoexistenceMessage{IncomingTextMessage: inboundContinuationTextMessage(t, wamid, coexistenceAdmissionBusinessPhone, body)}
	echo.To = to
	echo.ToUserID = toUserID
	require.NoError(t, app.persistMessageEchoesBeforeAck(account.PhoneID, []CoexistenceMessage{echo}, nil))
}

func countCoexistenceAdmissionRows(t *testing.T, db *gorm.DB, model any, query string, args ...any) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Unscoped().Model(model).Where(query, args...).Count(&count).Error)
	return count
}

func countCoexistenceStagedReceipts(t *testing.T, app *App, organizationID uuid.UUID, wamid string) int64 {
	t.Helper()
	return countCoexistenceAdmissionRows(t, app.DB, &models.InboundEvent{},
		"organization_id = ? AND protocol = ? AND provider_event_id = ?",
		organizationID, models.WhatsAppIdentityReviewInboundProtocol, wamid)
}

func loadCoexistenceAdmissionContact(t *testing.T, db *gorm.DB, organizationID, contactID uuid.UUID) models.Contact {
	t.Helper()
	var contact models.Contact
	require.NoError(t, db.Unscoped().Where("organization_id = ? AND id = ?", organizationID, contactID).First(&contact).Error)
	return contact
}

func TestIsPlausibleWhatsAppPhone(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{
		"60123456789":      true,
		"1555078388":       true,
		"123456":           false,
		"1234567890123456": false,
		"6012-345-6789":    false,
		"US.1349120865530": false,
		"":                 false,
	} {
		assert.Equal(t, want, isPlausibleWhatsAppPhone(value), value)
	}
}

func TestCoexistenceSenderAdmissionSelectorsCoverEveryAuthenticatedValue(t *testing.T) {
	t.Parallel()
	sidecar := &CoexistenceWebhookContact{WaID: "60999999999"}
	sidecar.Profile.Name = "N"
	sidecar.Profile.Username = " handle "
	message := IncomingTextMessage{ID: "wamid.selectors", From: "+60 12-345 6789", FromUserID: "US.direct"}.
		withWebhookSenderContact(sidecar)
	claim := WhatsAppIdentityReviewClaim{DirectPrimaryBSUID: "US.direct", ParentBSUID: "US.parent", Phone: "60123456789"}
	selectors := coexistenceSenderAdmissionSelectors(claim, message)
	assert.Equal(t, []string{"US.direct", "US.parent"}, selectors.userIDs)
	// wa_id stands in only when "from" is absent (withWebhookSenderContact).
	assert.Equal(t, []string{"60123456789"}, selectors.phones)
	assert.Equal(t, "handle", selectors.username)
	assert.ElementsMatch(t, []string{
		coexistenceIdentityPlaceholder(coexistenceContactIdentity{UserID: "US.direct"}),
		coexistenceIdentityPlaceholder(coexistenceContactIdentity{ParentUserID: "US.parent"}),
		coexistenceIdentityPlaceholder(coexistenceContactIdentity{Username: "handle"}),
	}, selectors.placeholders)
	// The previous release stored a BSUID-addressed echo's BSUID as the phone.
	assert.Equal(t, []string{"US.direct", "US.parent"}, selectors.legacyPhones)

	withoutFrom := IncomingTextMessage{ID: "wamid.selectors-wa", FromUserID: "US.direct"}.
		withWebhookSenderContact(&CoexistenceWebhookContact{WaID: "60999999999"})
	selectors = coexistenceSenderAdmissionSelectors(WhatsAppIdentityReviewClaim{DirectPrimaryBSUID: "US.direct"}, withoutFrom)
	assert.Equal(t, []string{"60999999999"}, selectors.phones)
}

// The incident: a Coexistence number connected to an empty workspace. Every
// first message from a sender nobody knows must open a normal conversation.
func TestCoexistenceNewSenderIsAdmittedWithOneBoundContact(t *testing.T) {
	cases := []struct {
		name     string
		phone    bool
		parent   bool
		username bool
	}{
		{name: "phone_and_bsuid", phone: true},
		{name: "bsuid_only_username_user", username: true},
		{name: "bsuid_with_unowned_parent", phone: true, parent: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			organizationID := account.OrganizationID
			bsuid := "US.new-sender-" + uuid.NewString()
			phone := ""
			if tc.phone {
				phone = coexistenceAdmissionTestPhone()
			}
			first := coexistenceAdmissionMessage(t, "wamid.new-sender-first-"+uuid.NewString(), phone, bsuid, "Hi, is this available?")
			parent := ""
			if tc.parent {
				parent = "US.new-parent-" + uuid.NewString()
				first.FromParentUserID = parent
			}
			username := ""
			if tc.username {
				username = "handle_" + uuid.NewString()[:8]
				sidecar := &CoexistenceWebhookContact{UserID: bsuid}
				sidecar.Profile.Name = "Username Customer"
				sidecar.Profile.Username = username
				first = first.withWebhookSenderContact(sidecar)
			}
			contactsBefore := countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", organizationID)

			work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, first, "New Customer")
			require.False(t, duplicate)
			require.NotNil(t, work, "a genuinely new sender must not be held contact-free")

			contact := loadCoexistenceAdmissionContact(t, app.DB, organizationID, work.Persisted.ContactID)
			assert.Equal(t, bsuid, contact.BSUID)
			assert.Nil(t, contact.MergedIntoID)
			assert.False(t, contact.DeletedAt.Valid)
			assert.Equal(t, account.Name, contact.WhatsAppAccount)
			if tc.phone {
				assert.Equal(t, phone, contact.PhoneNumber)
			} else {
				assert.True(t, strings.HasPrefix(contact.PhoneNumber, "bsuid:"), contact.PhoneNumber)
				assert.Equal(t, username, contact.Metadata["coexistence_username"])
			}
			if tc.parent {
				assert.Equal(t, parent, contact.Metadata["coexistence_parent_user_id"])
			}
			assert.Equal(t, string(coexistenceSenderAdmissionNewSender), contact.Metadata[coexistenceIdentityAdmissionKey])
			assert.Equal(t, first.ID, contact.Metadata[coexistenceIdentityAdmissionWAMIDKey])
			assert.Equal(t, contactsBefore+1, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", organizationID))
			assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.CustomerActivityEvent{},
				"organization_id = ? AND contact_id = ? AND event_type = ?",
				organizationID, contact.ID, models.CustomerActivityContactCreated))

			// Normal inbox message: no staged receipt, no hold, no identity-review
			// suppression or route proof, and the regular continuation job.
			assert.Zero(t, countCoexistenceStagedReceipts(t, app, organizationID, first.ID))
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{}, "organization_id = ?", organizationID))
			var persisted models.Message
			require.NoError(t, app.DB.First(&persisted, work.Persisted.ID).Error)
			assert.Equal(t, contact.ID, persisted.ContactID)
			assert.Equal(t, models.DirectionIncoming, persisted.Direction)
			assert.NotContains(t, persisted.Metadata, incomingAutomaticAISuppressedKey)
			assert.NotContains(t, persisted.Metadata, incomingIdentityReviewHoldIDKey)
			assert.NotNil(t, persisted.InboxConversationID, "the omnichannel inbox projection is created")
			assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.InboxConversation{},
				"organization_id = ? AND contact_id = ? AND channel = ?", organizationID, contact.ID, models.ChannelWhatsApp))
			assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.ScheduledJob{},
				"organization_id = ? AND kind = ? AND aggregate_id = ?", organizationID, inboundContinuationJobKind, persisted.ID))

			// The continuation worker re-proves the sender against the bound contact.
			require.NoError(t, app.DB.Create(&models.ChatbotSettings{
				BaseModel:       models.BaseModel{ID: uuid.New()},
				OrganizationID:  organizationID,
				WhatsAppAccount: account.Name,
				IsEnabled:       false,
				AI:              models.AIConfig{Enabled: false},
			}).Error)
			processor := NewInboundContinuationProcessor(app, time.Hour)
			require.NoError(t, processor.ProcessMessage(context.Background(), organizationID, persisted.ID))
			assert.Equal(t, models.ScheduledJobStatusCompleted, loadInboundContinuationJob(t, app, organizationID, persisted.ID).Status)

			// The sender is now exactly one known contact.
			second := first
			second.ID = "wamid.new-sender-second-" + uuid.NewString()
			second.Text.Body = "Second message"
			secondWork, secondDuplicate := admitCoexistenceAdmissionMessage(t, app, account, second, "New Customer")
			require.False(t, secondDuplicate)
			require.NotNil(t, secondWork)
			assert.Equal(t, contact.ID, secondWork.Persisted.ContactID)
			assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND bs_uid = ?", organizationID, bsuid))
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{}, "organization_id = ?", organizationID))

			// A provider retry of the first WAMID is an accepted no-op.
			replay, replayDuplicate := admitCoexistenceAdmissionMessage(t, app, account, first, "Changed")
			assert.True(t, replayDuplicate)
			assert.Nil(t, replay)
			assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Message{},
				"organization_id = ? AND whats_app_message_id = ?", organizationID, first.ID))
		})
	}
}

func coexistenceUngrantedAdvisoryLocks(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var waiting int64
	require.NoError(t, db.Raw("SELECT count(*) FROM pg_catalog.pg_locks WHERE locktype = 'advisory' AND NOT granted").Scan(&waiting).Error)
	return waiting
}

// Concurrent first messages from one new sender (plus a provider retry racing
// its original) are proven to wait on the same organization admission fence
// before any of them may create a contact. Exactly one contact results, and
// every message is projected into the Omnichannel Inbox. Several rounds make
// the mirror/admission deadlock likely enough to exercise its retry.
func TestCoexistenceNewSenderConcurrentFirstMessagesBindOneContact(t *testing.T) {
	for round := range 3 {
		t.Run(fmt.Sprintf("round_%d", round), runCoexistenceNewSenderConcurrentRound)
	}
}

func runCoexistenceNewSenderConcurrentRound(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	bsuid := "US.race-" + uuid.NewString()
	phone := coexistenceAdmissionTestPhone()
	const distinct = 5
	messages := make([]IncomingTextMessage, 0, distinct+1)
	for index := range distinct {
		messages = append(messages, coexistenceAdmissionMessage(
			t, fmt.Sprintf("wamid.race-%d-%s", index, uuid.NewString()), phone, bsuid, "race",
		))
	}
	messages = append(messages, messages[0])

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
			// /api/webhook answers a retryable PostgreSQL abort with 503 and Meta
			// redelivers the same body; model that redelivery. The post-commit
			// legacy inbox mirror can deadlock with the next admission (40P01);
			// the mirror retries itself, which the projection check below proves.
			var work *persistedIncomingMessage
			var duplicate bool
			var err error
			for attempt := 0; attempt < 8; attempt++ {
				work, duplicate, err = app.persistAuthenticatedIncomingMessageBeforeAck(
					account.PhoneID, message, "Race Customer", strings.Repeat("b", sha256.Size*2),
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
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND bs_uid = ?", organizationID, bsuid))
	require.NoError(t, blocker.Rollback().Error)
	blockerOpen = false
	wg.Wait()
	close(results)

	contactIDs := map[uuid.UUID]bool{}
	admitted, duplicates := 0, 0
	for result := range results {
		require.NoError(t, result.err)
		if result.duplicate {
			duplicates++
			assert.Nil(t, result.work)
			continue
		}
		require.NotNil(t, result.work)
		admitted++
		contactIDs[result.work.Persisted.ContactID] = true
	}
	assert.Equal(t, distinct, admitted)
	assert.Equal(t, 1, duplicates)
	require.Len(t, contactIDs, 1, "two first messages must never create two contacts")
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND bs_uid = ?", organizationID, bsuid))
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND phone_number = ?", organizationID, phone))
	for contactID := range contactIDs {
		assert.EqualValues(t, distinct, countCoexistenceAdmissionRows(t, app.DB, &models.Message{},
			"organization_id = ? AND contact_id = ? AND direction = ?", organizationID, contactID, models.DirectionIncoming))
		// Every admitted message reaches the Omnichannel Inbox, which lists
		// messages by inbox_conversation_id.
		assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Message{},
			"organization_id = ? AND contact_id = ? AND inbox_conversation_id IS NULL", organizationID, contactID),
			"a message whose after-commit inbox mirror failed is missing from the Omnichannel Inbox")
		assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.InboxConversation{},
			"organization_id = ? AND contact_id = ?", organizationID, contactID))
	}
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{}, "organization_id = ?", organizationID))
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.InboundEvent{},
		"organization_id = ? AND protocol = ?", organizationID, models.WhatsAppIdentityReviewInboundProtocol))
}

// Any identity evidence, however indirect, keeps today's contact-free review.
func TestCoexistenceSenderWithIdentityEvidenceStaysHeld(t *testing.T) {
	type setup struct {
		app     *App
		account *models.WhatsAppAccount
		bsuid   string
		phone   string
	}
	createContact := func(t *testing.T, s setup, phone, bsuid string, metadata models.JSONB) *models.Contact {
		t.Helper()
		contact := newWhatsAppIdentityReviewSelectorContact(s.account.OrganizationID, phone)
		contact.BSUID = bsuid
		if metadata != nil {
			contact.Metadata = metadata
		}
		require.NoError(t, s.app.DB.Create(&contact).Error)
		return &contact
	}
	cases := []struct {
		name    string
		arrange func(*testing.T, setup) IncomingTextMessage
	}{
		{name: "phone_owner_with_inbound_history", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			owner := createContact(t, s, s.phone, "", nil)
			require.NoError(t, s.app.DB.Create(&models.Message{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: s.account.OrganizationID,
				WhatsAppAccount: s.account.Name, ContactID: owner.ID, WhatsAppMessageID: "wamid.prior-" + uuid.NewString(),
				Direction: models.DirectionIncoming, MessageType: models.MessageTypeText, Content: "earlier",
				Status: models.MessageStatusReceived, Metadata: models.JSONB{},
			}).Error)
			return coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), s.phone, s.bsuid, "held")
		}},
		{name: "phone_only_address_book_contact", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			createContact(t, s, s.phone, "", models.JSONB{"coexistence_app_contact": true})
			return coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), s.phone, s.bsuid, "held")
		}},
		{name: "phone_owner_with_other_bsuid", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			createContact(t, s, s.phone, "US.other-"+uuid.NewString(), nil)
			return coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), s.phone, s.bsuid, "held")
		}},
		{name: "soft_deleted_bsuid_owner", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			owner := createContact(t, s, coexistenceAdmissionTestPhone(), s.bsuid, nil)
			require.NoError(t, s.app.DB.Delete(&models.Contact{}, "id = ?", owner.ID).Error)
			return coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), s.phone, s.bsuid, "held")
		}},
		{name: "reconciled_bsuid_placeholder", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			createContact(t, s, coexistenceIdentityPlaceholder(coexistenceContactIdentity{UserID: s.bsuid}), "", nil)
			return coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), "", s.bsuid, "held")
		}},
		{name: "username_owner", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			username := "known_" + uuid.NewString()[:8]
			createContact(t, s, coexistenceAdmissionTestPhone(), "US.other-"+uuid.NewString(),
				models.JSONB{"coexistence_username": strings.ToUpper(username)})
			sidecar := &CoexistenceWebhookContact{UserID: s.bsuid}
			sidecar.Profile.Username = username
			return coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), "", s.bsuid, "held").
				withWebhookSenderContact(sidecar)
		}},
		{name: "conflicting_user_id_metadata", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			createContact(t, s, coexistenceAdmissionTestPhone(), "US.other-"+uuid.NewString(),
				models.JSONB{"coexistence_conflicting_user_id": s.bsuid})
			return coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), s.phone, s.bsuid, "held")
		}},
		{name: "contacts_wa_id_owner", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			createContact(t, s, s.phone, "", nil)
			return coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), "", s.bsuid, "held").
				withWebhookSenderContact(&CoexistenceWebhookContact{UserID: s.bsuid, WaID: s.phone})
		}},
		{name: "parent_owned_by_another_contact", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			parent := "US.parent-" + uuid.NewString()
			createContact(t, s, coexistenceAdmissionTestPhone(), parent, nil)
			message := coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), s.phone, s.bsuid, "held")
			message.FromParentUserID = parent
			return message
		}},
		{name: "earlier_review_had_candidates", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			owner := createContact(t, s, s.phone, "US.other-"+uuid.NewString(), nil)
			earlier := coexistenceAdmissionMessage(t, "wamid.earlier-"+uuid.NewString(), s.phone, s.bsuid, "earlier")
			work, _ := admitCoexistenceAdmissionMessage(t, s.app, s.account, earlier, "")
			require.Nil(t, work)
			// The phone owner later moves away; the earlier hold still names it.
			require.NoError(t, s.app.DB.Model(&models.Contact{}).Where("id = ?", owner.ID).
				Update("phone_number", coexistenceAdmissionTestPhone()).Error)
			return coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), "", s.bsuid, "held")
		}},
		{name: "missing_bsuid_existing_phone_owner", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			// Fresh phone-only senders have their own admission proof tests;
			// an existing phone owner still cannot be adopted by that fallback.
			createContact(t, s, s.phone, "", nil)
			return coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), s.phone, "", "held")
		}},
		{name: "non_phone_from_value", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			return coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), "US.1349120865530274", s.bsuid, "held")
		}},
		{name: "overlong_digit_from_value", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			return coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), "601"+testutil.NewTestGraphObjectID()[:17], s.bsuid, "held")
		}},
		{name: "bsuid_longer_than_contact_column", arrange: func(t *testing.T, s setup) IncomingTextMessage {
			return coexistenceAdmissionMessage(t, "wamid.held-"+uuid.NewString(), s.phone, "US."+strings.Repeat("9", 160), "held")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			s := setup{app: app, account: account, bsuid: "US.held-" + uuid.NewString(), phone: coexistenceAdmissionTestPhone()}
			message := tc.arrange(t, s)
			organizationID := account.OrganizationID
			contactsBefore := countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", organizationID)
			boundBefore := countCoexistenceAdmissionRows(t, app.DB, &models.Contact{},
				"organization_id = ? AND bs_uid = ?", organizationID, message.FromUserID)

			work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, message, "Held Customer")
			assert.False(t, duplicate)
			assert.Nil(t, work, "identity evidence must keep the contact-free review path")
			assert.EqualValues(t, 1, countCoexistenceStagedReceipts(t, app, organizationID, message.ID))
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Message{},
				"organization_id = ? AND whats_app_message_id = ?", organizationID, message.ID))
			assert.Equal(t, contactsBefore, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", organizationID))
			assert.Equal(t, boundBefore, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{},
				"organization_id = ? AND bs_uid = ?", organizationID, message.FromUserID))
		})
	}
}

// If the bound contact does not re-evaluate as the unique direct owner, the
// savepoint is rolled back and the message is held exactly as before.
func TestCoexistenceSenderAdmissionFallsBackToReviewWhenBindingIsNotProven(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	phone := coexistenceAdmissionTestPhone()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	functionName := "drop_bsuid_" + suffix
	triggerName := "drop_bsuid_trigger_" + suffix
	require.NoError(t, app.DB.Exec(fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			IF NEW.phone_number = '%s' THEN
				NEW.bs_uid := '';
			END IF;
			RETURN NEW;
		END;
		$$`, functionName, phone)).Error)
	require.NoError(t, app.DB.Exec(fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE INSERT OR UPDATE ON contacts
		FOR EACH ROW EXECUTE FUNCTION %s()`, triggerName, functionName)).Error)
	t.Cleanup(func() {
		_ = app.DB.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON contacts", triggerName)).Error
		_ = app.DB.Exec(fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", functionName)).Error
	})

	contactsBefore := countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", organizationID)
	message := coexistenceAdmissionMessage(t, "wamid.unproven-"+uuid.NewString(), phone, "US.unproven-"+uuid.NewString(), "held")
	work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, message, "Unproven")
	assert.False(t, duplicate)
	assert.Nil(t, work)
	assert.EqualValues(t, 1, countCoexistenceStagedReceipts(t, app, organizationID, message.ID))
	assert.Equal(t, contactsBefore, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", organizationID),
		"the rolled-back savepoint leaves no contact behind")
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.CustomerActivityEvent{},
		"organization_id = ? AND event_type = ?", organizationID, models.CustomerActivityContactCreated))
}

// smb_message_echoes carries only the recipient phone in Meta's documented
// shape. A reply from that phone must land on the echo's contact.
func TestCoexistencePhoneAppEchoThenCustomerReplyEndsInOneContact(t *testing.T) {
	for _, withRecipientBSUID := range []bool{false, true} {
		t.Run(fmt.Sprintf("echo_has_recipient_bsuid=%t", withRecipientBSUID), func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			organizationID := account.OrganizationID
			phone := coexistenceAdmissionTestPhone()
			bsuid := "US.echo-recipient-" + uuid.NewString()
			echoWAMID := "wamid.echo-" + uuid.NewString()
			echoBSUID := ""
			if withRecipientBSUID {
				echoBSUID = bsuid
			}
			persistCoexistenceAdmissionEcho(t, app, account, echoWAMID, phone, echoBSUID, "Hello from our phone")

			var echoed models.Message
			require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", organizationID, echoWAMID).First(&echoed).Error)
			require.Equal(t, models.DirectionOutgoing, echoed.Direction)

			reply := coexistenceAdmissionMessage(t, "wamid.echo-reply-"+uuid.NewString(), phone, bsuid, "Thanks!")
			work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, reply, "Echo Customer")
			require.False(t, duplicate)
			require.NotNil(t, work, "the reply to a phone-app message must be visible")
			assert.Equal(t, echoed.ContactID, work.Persisted.ContactID)

			contact := loadCoexistenceAdmissionContact(t, app.DB, organizationID, echoed.ContactID)
			assert.Equal(t, bsuid, contact.BSUID)
			assert.Equal(t, phone, contact.PhoneNumber)
			if withRecipientBSUID {
				assert.NotContains(t, contact.Metadata, coexistenceIdentityAdmissionKey, "already bound by the echo itself")
			} else {
				assert.Equal(t, string(coexistenceSenderAdmissionPhoneAppRecipient), contact.Metadata[coexistenceIdentityAdmissionKey])
			}
			assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND phone_number = ?", organizationID, phone))
			assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND bs_uid = ?", organizationID, bsuid))
			assert.EqualValues(t, 2, countCoexistenceAdmissionRows(t, app.DB, &models.Message{}, "organization_id = ? AND contact_id = ?", organizationID, contact.ID))
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{}, "organization_id = ?", organizationID))
			assert.Zero(t, countCoexistenceStagedReceipts(t, app, organizationID, reply.ID))

			// A different WhatsApp user now writing from the same phone is a real
			// conflict and is held without touching the bound contact.
			other := coexistenceAdmissionMessage(t, "wamid.echo-other-"+uuid.NewString(), phone, "US.other-user-"+uuid.NewString(), "who is this")
			otherWork, otherDuplicate := admitCoexistenceAdmissionMessage(t, app, account, other, "Someone Else")
			assert.False(t, otherDuplicate)
			assert.Nil(t, otherWork)
			assert.EqualValues(t, 1, countCoexistenceStagedReceipts(t, app, organizationID, other.ID))
			assert.Equal(t, bsuid, loadCoexistenceAdmissionContact(t, app.DB, organizationID, contact.ID).BSUID)
		})
	}
}

func TestCoexistenceCustomerReplyThenPhoneAppEchoEndsInOneContact(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	phone := coexistenceAdmissionTestPhone()
	bsuid := "US.reply-first-" + uuid.NewString()

	first := coexistenceAdmissionMessage(t, "wamid.reply-first-"+uuid.NewString(), phone, bsuid, "Hi")
	work, _ := admitCoexistenceAdmissionMessage(t, app, account, first, "Reply First")
	require.NotNil(t, work)
	contactID := work.Persisted.ContactID

	echoWAMID := "wamid.reply-echo-" + uuid.NewString()
	persistCoexistenceAdmissionEcho(t, app, account, echoWAMID, phone, "", "Answer from our phone")
	var echoed models.Message
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", organizationID, echoWAMID).First(&echoed).Error)
	assert.Equal(t, contactID, echoed.ContactID)

	second := coexistenceAdmissionMessage(t, "wamid.reply-second-"+uuid.NewString(), phone, bsuid, "Great")
	secondWork, _ := admitCoexistenceAdmissionMessage(t, app, account, second, "Reply First")
	require.NotNil(t, secondWork)
	assert.Equal(t, contactID, secondWork.Persisted.ContactID)
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND phone_number = ?", organizationID, phone))
	assert.EqualValues(t, 3, countCoexistenceAdmissionRows(t, app.DB, &models.Message{}, "organization_id = ? AND contact_id = ?", organizationID, contactID))
}

// createCoexistenceAdmissionOtherSMBAccount adds a second Coexistence number to
// the same workspace.
func createCoexistenceAdmissionOtherSMBAccount(t *testing.T, app *App, account *models.WhatsAppAccount) *models.WhatsAppAccount {
	t.Helper()
	other := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: account.OrganizationID,
		Name:           "other-smb-" + uuid.NewString()[:8],
		PhoneID:        testutil.NewTestGraphObjectID(),
		BusinessID:     account.BusinessID,
		AccessToken:    "other-token",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(&other).Error)
	require.NoError(t, app.DB.Create(&models.WhatsAppCoexistenceState{
		ID: uuid.New(), OrganizationID: account.OrganizationID, WhatsAppAccountID: other.ID,
		OnboardingStatus: models.CoexistenceOnboardingStatusConnected, OnboardingCycle: 1,
		LifecycleStatus: models.CoexistenceLifecycleStatusConnected, LifecycleMetadata: models.JSONB{}, Version: 1,
	}).Error)
	return &other
}

func createCoexistenceAdmissionLead(t *testing.T, app *App, organizationID, contactID uuid.UUID) {
	t.Helper()
	pipeline := models.CRMPipeline{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationID,
		Name: "Ads " + uuid.NewString()[:8], IsActive: true, Version: 1,
	}
	require.NoError(t, app.DB.Create(&pipeline).Error)
	stage := models.CRMPipelineStage{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationID, PipelineID: pipeline.ID,
		Name: "New", Kind: models.CRMPipelineStageKindOpen, IsActive: true, Version: 1,
	}
	require.NoError(t, app.DB.Create(&stage).Error)
	require.NoError(t, app.DB.Create(&models.CRMLead{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationID, ContactID: contactID,
		PipelineID: pipeline.ID, StageID: stage.ID, Title: "Earlier enquiry", Status: models.CRMLeadStatusOpen,
		Source: models.CRMLeadSourceOther, Currency: "MYR", Metadata: models.JSONB{}, Version: 1,
	}).Error)
}

// Only a contact that this number's own phone-app echo created, that nobody
// has written from and that holds no CRM record, adopts the replying BSUID.
// An older CRM record, an address-book contact or an imported history thread
// that merely received a later echo stays held: its number may have been
// recycled to a different person (the "Patient A" record must never receive
// person B's BSUID).
func TestCoexistencePhoneAppRecipientAdoptionRequiresAContactTheEchoCreated(t *testing.T) {
	type setup struct {
		app       *App
		account   *models.WhatsAppAccount
		phone     string
		echoWAMID string
	}
	echoContact := func(t *testing.T, s setup) models.Message {
		t.Helper()
		var echoed models.Message
		require.NoError(t, s.app.DB.Where("organization_id = ? AND whats_app_message_id = ?",
			s.account.OrganizationID, s.echoWAMID).First(&echoed).Error)
		return echoed
	}
	cases := []struct {
		name    string
		adopted bool
		// before runs ahead of the echo; after runs once the echo is stored.
		before func(*testing.T, setup) *models.WhatsAppAccount
		after  func(*testing.T, setup)
	}{
		{name: "fresh_echo_contact_is_adopted", adopted: true},
		{name: "echo_from_other_account", before: func(t *testing.T, s setup) *models.WhatsAppAccount {
			return createCoexistenceAdmissionOtherSMBAccount(t, s.app, s.account)
		}},
		{name: "preexisting_manual_contact_then_echo", before: func(t *testing.T, s setup) *models.WhatsAppAccount {
			contact := newWhatsAppIdentityReviewSelectorContact(s.account.OrganizationID, s.phone)
			contact.ProfileName = "Patient A"
			require.NoError(t, s.app.DB.Create(&contact).Error)
			require.NoError(t, s.app.DB.Model(&models.Contact{}).Where("id = ?", contact.ID).
				Update("created_at", time.Now().UTC().Add(-30*24*time.Hour)).Error)
			return nil
		}},
		{name: "address_book_contact_then_echo", before: func(t *testing.T, s setup) *models.WhatsAppAccount {
			// smb_app_state_sync created it moments before the echo.
			contact := newWhatsAppIdentityReviewSelectorContact(s.account.OrganizationID, s.phone)
			contact.Metadata = models.JSONB{"coexistence_app_contact": true}
			require.NoError(t, s.app.DB.Create(&contact).Error)
			return nil
		}},
		{name: "recipient_has_inbound_history", after: func(t *testing.T, s setup) {
			require.NoError(t, s.app.DB.Create(&models.Message{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: s.account.OrganizationID,
				WhatsAppAccount: s.account.Name, ContactID: echoContact(t, s).ContactID, WhatsAppMessageID: "wamid.history-" + uuid.NewString(),
				Direction: models.DirectionIncoming, MessageType: models.MessageTypeText, Content: "imported",
				Status: models.MessageStatusReceived, Metadata: models.JSONB{"coexistence_source": "history"},
			}).Error)
		}},
		{name: "outgoing_history_dated_before_the_echo", after: func(t *testing.T, s setup) {
			echoed := echoContact(t, s)
			earlier := echoed.CreatedAt.Add(-24 * time.Hour)
			require.NoError(t, s.app.DB.Create(&models.Message{
				BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: earlier, UpdatedAt: earlier},
				OrganizationID: s.account.OrganizationID, WhatsAppAccount: s.account.Name, ContactID: echoed.ContactID,
				WhatsAppMessageID: "wamid.history-out-" + uuid.NewString(), Direction: models.DirectionOutgoing,
				MessageType: models.MessageTypeText, Content: "imported", Status: models.MessageStatusSent,
				Metadata: models.JSONB{"coexistence_source": "history"},
			}).Error)
		}},
		{name: "recipient_has_merge_alias", after: func(t *testing.T, s setup) {
			alias := newWhatsAppIdentityReviewSelectorContact(s.account.OrganizationID, coexistenceAdmissionTestPhone())
			require.NoError(t, s.app.DB.Create(&alias).Error)
			require.NoError(t, s.app.DB.Model(&models.Contact{}).Where("id = ?", alias.ID).
				Updates(map[string]any{"merged_into_id": echoContact(t, s).ContactID, "deleted_at": time.Now().UTC()}).Error)
		}},
		{name: "recipient_is_assigned", after: func(t *testing.T, s setup) {
			agent := testutil.CreateTestUser(t, s.app.DB, s.account.OrganizationID)
			require.NoError(t, s.app.DB.Model(&models.Contact{}).Where("id = ?", echoContact(t, s).ContactID).
				Update("assigned_user_id", agent.ID).Error)
		}},
		{name: "recipient_has_crm_lead", after: func(t *testing.T, s setup) {
			createCoexistenceAdmissionLead(t, s.app, s.account.OrganizationID, echoContact(t, s).ContactID)
		}},
		{name: "manual_contact_created_30s_before_echo", before: func(t *testing.T, s setup) *models.WhatsAppAccount {
			// Staff saved "Patient A" moments before messaging the number from the
			// Business app; the echo then lands on that record.
			contact := newWhatsAppIdentityReviewSelectorContact(s.account.OrganizationID, s.phone)
			contact.ProfileName = "Patient A"
			require.NoError(t, s.app.DB.Create(&contact).Error)
			require.NoError(t, s.app.DB.Model(&models.Contact{}).Where("id = ?", contact.ID).
				Update("created_at", time.Now().UTC().Add(-30*time.Second)).Error)
			return nil
		}},
		{name: "old_echo_contact_refreshed_by_a_new_echo", after: func(t *testing.T, s setup) {
			// The echo created the contact 30 days ago; today's echo to the same
			// number does not make that older thread adoptable.
			echoed := echoContact(t, s)
			require.NoError(t, s.app.DB.Model(&models.Contact{}).Where("id = ?", echoed.ContactID).
				Update("created_at", gorm.Expr("created_at - interval '30 days'")).Error)
			require.NoError(t, s.app.DB.Model(&models.Message{}).Where("id = ?", echoed.ID).
				Updates(map[string]any{
					"created_at":  gorm.Expr("created_at - interval '30 days'"),
					"ingested_at": gorm.Expr("ingested_at - interval '30 days'"),
				}).Error)
			persistCoexistenceAdmissionEcho(t, s.app, s.account, "wamid.adopt-fresh-echo-"+uuid.NewString(), s.phone, "", "Hello again")
		}},
		{name: "echo_older_than_seven_days", after: func(t *testing.T, s setup) {
			echoed := echoContact(t, s)
			require.NoError(t, s.app.DB.Model(&models.Contact{}).Where("id = ?", echoed.ContactID).
				Update("created_at", gorm.Expr("created_at - interval '8 days'")).Error)
			require.NoError(t, s.app.DB.Model(&models.Message{}).Where("id = ?", echoed.ID).
				Update("ingested_at", gorm.Expr("ingested_at - interval '8 days'")).Error)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			organizationID := account.OrganizationID
			s := setup{app: app, account: account, phone: coexistenceAdmissionTestPhone(), echoWAMID: "wamid.adopt-echo-" + uuid.NewString()}
			echoAccount := account
			if tc.before != nil {
				if other := tc.before(t, s); other != nil {
					echoAccount = other
				}
			}
			persistCoexistenceAdmissionEcho(t, app, echoAccount, s.echoWAMID, s.phone, "", "Hello")
			echoed := echoContact(t, s)
			if tc.after != nil {
				tc.after(t, s)
			}

			bsuid := "US.adopt-" + uuid.NewString()
			reply := coexistenceAdmissionMessage(t, "wamid.adopt-reply-"+uuid.NewString(), s.phone, bsuid, "wrong number")
			work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, reply, "")
			assert.False(t, duplicate)
			recipient := loadCoexistenceAdmissionContact(t, app.DB, organizationID, echoed.ContactID)
			if tc.adopted {
				require.NotNil(t, work)
				assert.Equal(t, echoed.ContactID, work.Persisted.ContactID)
				assert.Equal(t, bsuid, recipient.BSUID)
				assert.Equal(t, string(coexistenceSenderAdmissionPhoneAppRecipient), recipient.Metadata[coexistenceIdentityAdmissionKey])
				assert.Zero(t, countCoexistenceStagedReceipts(t, app, organizationID, reply.ID))
				return
			}
			assert.Nil(t, work, "the reply must stay held")
			assert.EqualValues(t, 1, countCoexistenceStagedReceipts(t, app, organizationID, reply.ID))
			assert.Empty(t, recipient.BSUID, "the existing record must not receive the replying BSUID")
			assert.NotContains(t, recipient.Metadata, coexistenceIdentityAdmissionKey)
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{},
				"organization_id = ? AND bs_uid = ?", organizationID, bsuid))
		})
	}
}

// stageCoexistenceReceiptAsBeforeThisChange reproduces the receipt that the
// previous admission path committed for a new sender: a zero-member,
// unsupported hold and its contact-free staged event.
func stageCoexistenceReceiptAsBeforeThisChange(
	t *testing.T,
	app *App,
	account *models.WhatsAppAccount,
	message IncomingTextMessage,
) uuid.UUID {
	t.Helper()
	var holdID uuid.UUID
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		if err := database.LockOrganizationPolicyScope(scoped.DB, account.OrganizationID); err != nil {
			return err
		}
		channelAccount, err := ensureFencedLegacyMetaAccountForTest(scoped.DB, channelapi.LegacyMetaAccountRef{
			ID: account.ID, OrganizationID: account.OrganizationID, Name: account.Name, Status: account.Status,
		})
		if err != nil {
			return err
		}
		current := *account
		if err := scoped.prepareWhatsAppMessageAuthority(&current); err != nil {
			return err
		}
		claim := WhatsAppIdentityReviewClaim{
			OrganizationID: account.OrganizationID, WhatsAppAccountID: account.ID, OnboardingCycle: 1,
			DirectPrimaryBSUID: message.FromUserID, ParentBSUID: message.FromParentUserID,
			Phone:                   normalizeIdentityReviewPhone(message.From),
			VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent,
			VerifiedEventDigest:     strings.Repeat("c", 64), SelectorBodyDigest: strings.Repeat("d", 64),
		}
		hold, _, err := scoped.CreateOrReuseWhatsAppIdentityReviewHold(scoped.DB, &claim)
		if err != nil {
			return err
		}
		if hold.Supported || hold.MemberCount != 0 {
			return fmt.Errorf("expected the pre-change zero-member hold, got supported=%t members=%d", hold.Supported, hold.MemberCount)
		}
		holdID = hold.HoldID
		_, _, err = scoped.persistWhatsAppIdentityReviewEvent(&current, channelAccount, hold.HoldID, message)
		return err
	}))
	return holdID
}

// Requirement: a receipt held before this change is neither moved nor
// auto-admitted; it stays readable in the protected queue while the sender's
// next message opens a normal conversation.
func TestCoexistenceHeldNewSenderReceiptStaysInProtectedQueueAfterSenderIsAdmitted(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	phone := coexistenceAdmissionTestPhone()
	bsuid := "US.held-before-" + uuid.NewString()
	held := coexistenceAdmissionMessage(t, "wamid.held-before-"+uuid.NewString(), phone, bsuid, "Test message before the fix")
	holdID := stageCoexistenceReceiptAsBeforeThisChange(t, app, account, held)

	replay, replayDuplicate := admitCoexistenceAdmissionMessage(t, app, account, held, "")
	assert.True(t, replayDuplicate, "a provider retry of the held WAMID stays an accepted no-op")
	assert.Nil(t, replay)

	next := coexistenceAdmissionMessage(t, "wamid.held-next-"+uuid.NewString(), phone, bsuid, "Anyone there?")
	work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, next, "Held Customer")
	require.False(t, duplicate)
	require.NotNil(t, work)
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Message{},
		"organization_id = ? AND whats_app_message_id = ?", organizationID, held.ID), "held content is never promoted")

	var hold models.WhatsAppIdentityReviewHold
	require.NoError(t, app.DB.Where("organization_id = ? AND id = ?", organizationID, holdID).First(&hold).Error)
	assert.Equal(t, models.WhatsAppIdentityReviewDispositionOpen, hold.Disposition)

	reviewer := createWhatsAppIdentityReviewResolver(t, app.DB, organizationID)
	listRequest := testutil.NewGETRequest(t)
	testutil.SetAuthContext(listRequest, organizationID, reviewer.ID)
	require.NoError(t, app.ListStagedContactIdentityReviews(listRequest))
	require.Equal(t, 200, testutil.GetResponseStatusCode(listRequest), string(testutil.GetResponseBody(listRequest)))
	assert.Contains(t, string(testutil.GetResponseBody(listRequest)), holdID.String())
	assert.Contains(t, string(testutil.GetResponseBody(listRequest)), `"total":1`)

	var event models.InboundEvent
	require.NoError(t, app.DB.Where("organization_id = ? AND provider_event_id = ?", organizationID, held.ID).First(&event).Error)
	detailRequest := testutil.NewGETRequest(t)
	testutil.SetAuthContext(detailRequest, organizationID, reviewer.ID)
	testutil.SetPathParam(detailRequest, "id", event.ID.String())
	require.NoError(t, app.GetStagedContactIdentityReview(detailRequest))
	require.Equal(t, 200, testutil.GetResponseStatusCode(detailRequest), string(testutil.GetResponseBody(detailRequest)))
	assert.Contains(t, string(testutil.GetResponseBody(detailRequest)), "Test message before the fix")
	assert.NotContains(t, string(testutil.GetResponseBody(detailRequest)), bsuid, "the queue never exposes selectors")
	assert.NotContains(t, string(testutil.GetResponseBody(detailRequest)), phone)

	// The protected queue keeps its permission boundary.
	agentRole := testutil.CreateTestRoleWithKeys(t, app.DB, organizationID, "agent-"+uuid.NewString(), []string{"contacts:read", "contacts:write"})
	agent := testutil.CreateTestUser(t, app.DB, organizationID, testutil.WithRoleID(&agentRole.ID))
	forbidden := testutil.NewGETRequest(t)
	testutil.SetAuthContext(forbidden, organizationID, agent.ID)
	require.NoError(t, app.ListStagedContactIdentityReviews(forbidden))
	assert.Equal(t, 403, testutil.GetResponseStatusCode(forbidden))
}

// GET /api/identity-reviews/staged is the workspace-level entry point. It must
// count and page open receipts newest first, and mark the receipts that no
// decision can resolve (unsupported holds) as read-only.
func TestListStagedContactIdentityReviewsCountsAndPagesOpenReceipts(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	holds := make([]uuid.UUID, 0, 3)
	for index := range 3 {
		message := coexistenceAdmissionMessage(
			t,
			fmt.Sprintf("wamid.queue-page-%d-%s", index, uuid.NewString()),
			coexistenceAdmissionTestPhone(),
			"US.queue-page-"+uuid.NewString(),
			fmt.Sprintf("held %d", index),
		)
		holds = append(holds, stageCoexistenceReceiptAsBeforeThisChange(t, app, account, message))
		time.Sleep(5 * time.Millisecond)
	}
	// A real conflict between two existing contacts (the direct BSUID and its
	// parent belong to different contacts) is held under a supported hold,
	// which the flagged contact's Identity review can decide.
	direct := "US.queue-direct-" + uuid.NewString()
	parent := "US.queue-parent-" + uuid.NewString()
	for _, bsuid := range []string{direct, parent} {
		owner := newWhatsAppIdentityReviewSelectorContact(organizationID, coexistenceAdmissionTestPhone())
		owner.BSUID = bsuid
		require.NoError(t, app.DB.Create(&owner).Error)
	}
	conflict := coexistenceAdmissionMessage(t, "wamid.queue-conflict-"+uuid.NewString(), coexistenceAdmissionTestPhone(), direct, "conflict")
	conflict.FromParentUserID = parent
	conflictWork, _ := admitCoexistenceAdmissionMessage(t, app, account, conflict, "")
	require.Nil(t, conflictWork)
	var conflictEvent models.InboundEvent
	require.NoError(t, app.DB.Where("organization_id = ? AND provider_event_id = ?", organizationID, conflict.ID).First(&conflictEvent).Error)
	require.NotNil(t, conflictEvent.ReviewHoldID)
	var conflictHold models.WhatsAppIdentityReviewHold
	require.NoError(t, app.DB.Where("organization_id = ? AND id = ?", organizationID, *conflictEvent.ReviewHoldID).First(&conflictHold).Error)
	require.True(t, conflictHold.Supported, "a conflict between existing contacts has a supported hold")

	reviewer := createWhatsAppIdentityReviewResolver(t, app.DB, organizationID)
	page := func(number, limit int) (items []stagedWhatsAppIdentityReviewItem, total, readOnly int64) {
		t.Helper()
		request := testutil.NewGETRequest(t)
		testutil.SetAuthContext(request, organizationID, reviewer.ID)
		testutil.SetQueryParam(request, "page", number)
		testutil.SetQueryParam(request, "limit", limit)
		require.NoError(t, app.ListStagedContactIdentityReviews(request))
		require.Equal(t, 200, testutil.GetResponseStatusCode(request), string(testutil.GetResponseBody(request)))
		assert.Equal(t, "private, no-store", string(request.RequestCtx.Response.Header.Peek("Cache-Control")))
		var payload struct {
			Reviews       []stagedWhatsAppIdentityReviewItem `json:"reviews"`
			Total         int64                              `json:"total"`
			ReadOnlyTotal int64                              `json:"read_only_total"`
		}
		testutil.ParseEnvelopeResponse(t, request, &payload)
		return payload.Reviews, payload.Total, payload.ReadOnlyTotal
	}
	first, total, readOnly := page(1, 2)
	assert.EqualValues(t, 4, total)
	assert.EqualValues(t, 3, readOnly)
	require.Len(t, first, 2)
	assert.Equal(t, conflictHold.ID, first[0].HoldID, "newest first")
	assert.False(t, first[0].ReadOnly, "a supported hold can be decided")
	assert.Equal(t, holds[2], first[1].HoldID)
	second, total, readOnly := page(2, 2)
	assert.EqualValues(t, 4, total)
	assert.EqualValues(t, 3, readOnly)
	require.Len(t, second, 2)
	assert.Equal(t, holds[1], second[0].HoldID)
	assert.Equal(t, holds[0], second[1].HoldID)
	for _, item := range append(first[1:], second...) {
		assert.True(t, item.ReadOnly, "a zero-member, unsupported hold can never be decided")
	}
	for _, item := range append(first, second...) {
		assert.Equal(t, "text", item.MessageType)
		assert.Equal(t, models.InboundEventStatusPending, item.Status)
	}

	detail := func(eventID uuid.UUID) stagedWhatsAppIdentityReviewDetail {
		t.Helper()
		request := testutil.NewGETRequest(t)
		testutil.SetAuthContext(request, organizationID, reviewer.ID)
		testutil.SetPathParam(request, "id", eventID.String())
		require.NoError(t, app.GetStagedContactIdentityReview(request))
		require.Equal(t, 200, testutil.GetResponseStatusCode(request), string(testutil.GetResponseBody(request)))
		var payload stagedWhatsAppIdentityReviewDetail
		testutil.ParseEnvelopeResponse(t, request, &payload)
		return payload
	}
	assert.False(t, detail(conflictEvent.ID).ReadOnly)
	assert.True(t, detail(second[1].ID).ReadOnly)
}

func createCoexistenceAdmissionWebhookAccount(t *testing.T, app *App) models.WhatsAppAccount {
	t.Helper()
	uid := uuid.NewString()[:8]
	organization := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "empty-workspace-" + uid,
		Slug:      "empty-workspace-" + uid,
	}
	require.NoError(t, app.DB.Create(&organization).Error)
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Name:           "empty-coexistence-" + uid,
		PhoneID:        testutil.NewTestGraphObjectID(),
		BusinessID:     testutil.NewTestGraphObjectID(),
		AccessToken:    "token",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(&account).Error)
	require.NoError(t, app.DB.Create(&models.WhatsAppCoexistenceState{
		ID:                uuid.New(),
		OrganizationID:    organization.ID,
		WhatsAppAccountID: account.ID,
		OnboardingStatus:  models.CoexistenceOnboardingStatusSyncing,
		OnboardingCycle:   1,
		LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata: models.JSONB{},
		Version:           1,
	}).Error)
	return account
}

func coexistenceAdmissionInboundBody(account models.WhatsAppAccount, wamid, phone, bsuid, body string) []byte {
	return []byte(`{"object":"whatsapp_business_account","entry":[{"id":"` + account.BusinessID + `","changes":[
		{"field":"messages","value":{"messaging_product":"whatsapp",
			"metadata":{"display_phone_number":"` + coexistenceAdmissionBusinessPhone + `","phone_number_id":"` + account.PhoneID + `"},
			"contacts":[{"profile":{"name":"Ad Lead"},"wa_id":"` + phone + `","user_id":"` + bsuid + `"}],
			"messages":[{"from":"` + phone + `","from_user_id":"` + bsuid + `","id":"` + wamid + `",
				"timestamp":"1739230980","type":"text","text":{"body":"` + body + `"}}]}}]}]}`)
}

func coexistenceAdmissionEchoBody(account models.WhatsAppAccount, wamid, phone, body string) []byte {
	return []byte(`{"object":"whatsapp_business_account","entry":[{"id":"` + account.BusinessID + `","changes":[
		{"field":"smb_message_echoes","value":{"messaging_product":"whatsapp",
			"metadata":{"display_phone_number":"` + coexistenceAdmissionBusinessPhone + `","phone_number_id":"` + account.PhoneID + `"},
			"message_echoes":[{"from":"` + coexistenceAdmissionBusinessPhone + `","to":"` + phone + `","id":"` + wamid + `",
				"timestamp":"1739230990","type":"text","text":{"body":"` + body + `"}}]}}]}]}`)
}

// The production incident end to end through the signed webhook: an empty
// Coexistence workspace receives a first message, and the owner answers from
// the WhatsApp Business app (or answers first and the customer replies).
func TestWebhookHandler_CoexistenceEmptyWorkspaceConversationIsVisible(t *testing.T) {
	for _, customerFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("customer_first=%t", customerFirst), func(t *testing.T) {
			app := webhookTestApp(t)
			account := createCoexistenceAdmissionWebhookAccount(t, app)
			organizationID := account.OrganizationID
			phone := coexistenceAdmissionTestPhone()
			bsuid := "US." + testutil.NewTestGraphObjectID()[:20]
			inboundWAMID := "wamid.empty-in-" + uuid.NewString()
			echoWAMID := "wamid.empty-echo-" + uuid.NewString()
			inbound := coexistenceAdmissionInboundBody(account, inboundWAMID, phone, bsuid, "Saw your ad, price?")
			echo := coexistenceAdmissionEchoBody(account, echoWAMID, phone, "Hi! It is RM99")
			if customerFirst {
				sendSignedWebhook(t, app, inbound)
				sendSignedWebhook(t, app, echo)
			} else {
				sendSignedWebhook(t, app, echo)
				sendSignedWebhook(t, app, inbound)
			}
			app.WaitForBackgroundTasks()

			var contacts []models.Contact
			require.NoError(t, app.DB.Where("organization_id = ?", organizationID).Find(&contacts).Error)
			require.Len(t, contacts, 1, "exactly one contact for the conversation")
			assert.Equal(t, phone, contacts[0].PhoneNumber)
			assert.Equal(t, bsuid, contacts[0].BSUID)
			assert.Equal(t, account.Name, contacts[0].WhatsAppAccount)

			var messages []models.Message
			require.NoError(t, app.DB.Where("organization_id = ?", organizationID).Order("direction").Find(&messages).Error)
			require.Len(t, messages, 2, "both the customer message and the phone-app reply are visible")
			for _, message := range messages {
				assert.Equal(t, contacts[0].ID, message.ContactID)
				assert.NotNil(t, message.InboxConversationID, "omnichannel inbox projection for %s", message.WhatsAppMessageID)
			}
			assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.InboxConversation{},
				"organization_id = ? AND contact_id = ?", organizationID, contacts[0].ID))
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.InboundEvent{},
				"organization_id = ? AND protocol = ?", organizationID, models.WhatsAppIdentityReviewInboundProtocol))
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{}, "organization_id = ?", organizationID))
		})
	}
}

func TestCoexistenceSenderAdmissionPhone(t *testing.T) {
	t.Parallel()
	withWaID := func(from, waID string) IncomingTextMessage {
		return IncomingTextMessage{ID: "wamid.phone", From: from, FromUserID: "US.direct"}.
			withWebhookSenderContact(&CoexistenceWebhookContact{UserID: "US.direct", WaID: waID})
	}
	cases := []struct {
		name      string
		message   IncomingTextMessage
		claim     string
		wantPhone string
		wantWaID  bool
		wantOK    bool
	}{
		{name: "from_phone", message: withWaID("60123456789", "60999999999"), claim: "60123456789", wantPhone: "60123456789", wantOK: true},
		{name: "from_differs_from_claim", message: withWaID("60123456789", ""), claim: "60120000000", wantPhone: "60123456789"},
		{name: "non_phone_from", message: withWaID("US.1349120865530274", ""), claim: "1349120865530274", wantPhone: "US.1349120865530274"},
		{name: "wa_id_without_from", message: withWaID("", "+60123456789"), wantPhone: "60123456789", wantWaID: true, wantOK: true},
		{name: "non_phone_wa_id_without_from", message: withWaID("", "US.1349120865530274"), wantOK: true},
		{name: "no_phone_at_all", message: withWaID("", ""), wantOK: true},
	}
	for _, tc := range cases {
		phone, fromWaID, ok := coexistenceSenderAdmissionPhone(tc.message, tc.claim)
		assert.Equal(t, tc.wantPhone, phone, tc.name)
		assert.Equal(t, tc.wantWaID, fromWaID, tc.name)
		assert.Equal(t, tc.wantOK, ok, tc.name)
	}
}

func TestCoexistenceEchoRecipientThatIsNotAPhoneIsABSUID(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{
		"US.14839120865530":           true,
		"US.ENT.11815799212886844830": true,
		"60123456789":                 false,
		"+60 12-345 6789":             false,
		"+1234567890ab12":             false,
		"bsuid:abc":                   false,
		"us.1483":                     false,
		"US.":                         false,
		"":                            false,
	} {
		assert.Equal(t, want, isCoexistenceBSUIDAddress(value), value)
	}
	echo := func(to, toUserID string) CoexistenceMessage {
		message := CoexistenceMessage{IncomingTextMessage: IncomingTextMessage{ID: "wamid.echo-identity", From: coexistenceAdmissionBusinessPhone}}
		message.To = to
		message.ToUserID = toUserID
		return message
	}
	identity := coexistenceMessageContactIdentity(echo("US.14839120865530", ""), models.DirectionOutgoing, coexistenceContactIdentity{}, nil)
	assert.Empty(t, identity.Phone, "a BSUID in to must not become a contact phone")
	assert.Equal(t, "US.14839120865530", identity.UserID)
	identity = coexistenceMessageContactIdentity(echo("US.14839120865530", "US.explicit"), models.DirectionOutgoing, coexistenceContactIdentity{}, nil)
	assert.Empty(t, identity.Phone)
	assert.Equal(t, "US.explicit", identity.UserID, "to_user_id wins")
	identity = coexistenceMessageContactIdentity(echo("60123456789", ""), models.DirectionOutgoing, coexistenceContactIdentity{}, nil)
	assert.Equal(t, "60123456789", identity.Phone)
	assert.Empty(t, identity.UserID)
}

// A zero-member hold is skipped only when it is an earlier held copy of this
// same sender. One left by a different BSUID on the same phone (number
// recycling, or two users held during the transition) is review evidence, so
// the second BSUID is neither bootstrapped nor adopted into a contact that may
// hold the conversation written for the first.
func TestCoexistenceOtherBSUIDHeldOnSamePhoneKeepsReview(t *testing.T) {
	cases := []struct {
		name string
		// arrange stages earlier receipts and returns the message under test.
		arrange func(t *testing.T, app *App, account *models.WhatsAppAccount, phone string) IncomingTextMessage
		// echoBetween answers the phone from the Business app after staging.
		echoBetween bool
	}{
		{name: "new_sender_after_other_bsuid_was_held", arrange: func(t *testing.T, app *App, account *models.WhatsAppAccount, phone string) IncomingTextMessage {
			stageCoexistenceReceiptAsBeforeThisChange(t, app, account,
				coexistenceAdmissionMessage(t, "wamid.held-y-"+uuid.NewString(), phone, "US.y-"+uuid.NewString(), "from Y"))
			return coexistenceAdmissionMessage(t, "wamid.x-"+uuid.NewString(), phone, "US.x-"+uuid.NewString(), "from X")
		}},
		{name: "phone_app_recipient_after_other_bsuid_was_held", echoBetween: true, arrange: func(t *testing.T, app *App, account *models.WhatsAppAccount, phone string) IncomingTextMessage {
			stageCoexistenceReceiptAsBeforeThisChange(t, app, account,
				coexistenceAdmissionMessage(t, "wamid.held-y-"+uuid.NewString(), phone, "US.y-"+uuid.NewString(), "from Y"))
			return coexistenceAdmissionMessage(t, "wamid.x-"+uuid.NewString(), phone, "US.x-"+uuid.NewString(), "from X")
		}},
		{name: "same_bsuid_held_with_another_parent", arrange: func(t *testing.T, app *App, account *models.WhatsAppAccount, phone string) IncomingTextMessage {
			bsuid := "US.same-" + uuid.NewString()
			earlier := coexistenceAdmissionMessage(t, "wamid.held-parent-"+uuid.NewString(), phone, bsuid, "earlier")
			earlier.FromParentUserID = "US.parent-one-" + uuid.NewString()
			stageCoexistenceReceiptAsBeforeThisChange(t, app, account, earlier)
			message := coexistenceAdmissionMessage(t, "wamid.parent-two-"+uuid.NewString(), phone, bsuid, "later")
			message.FromParentUserID = "US.parent-two-" + uuid.NewString()
			return message
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			organizationID := account.OrganizationID
			phone := coexistenceAdmissionTestPhone()
			message := tc.arrange(t, app, account, phone)
			var recipient *models.Message
			if tc.echoBetween {
				echoWAMID := "wamid.between-echo-" + uuid.NewString()
				persistCoexistenceAdmissionEcho(t, app, account, echoWAMID, phone, "", "Reply written for Y")
				var echoed models.Message
				require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", organizationID, echoWAMID).First(&echoed).Error)
				recipient = &echoed
			}
			contactsBefore := countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", organizationID)

			work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, message, "")
			assert.False(t, duplicate)
			assert.Nil(t, work, "another BSUID's earlier hold on these selectors keeps the review path")
			assert.EqualValues(t, 1, countCoexistenceStagedReceipts(t, app, organizationID, message.ID))
			assert.Equal(t, contactsBefore, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ?", organizationID))
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{},
				"organization_id = ? AND bs_uid = ?", organizationID, message.FromUserID))
			if recipient != nil {
				assert.Empty(t, loadCoexistenceAdmissionContact(t, app.DB, organizationID, recipient.ContactID).BSUID)
			}
		})
	}
}

// A sender identified by BSUID with contacts[].wa_id but no "from" gets wa_id
// as its contact phone, like the classic resolver, so the phone-app echo and
// the customer's messages share one contact in either order.
func TestCoexistenceWaIDOnlySenderAndPhoneAppEchoEndInOneContact(t *testing.T) {
	waIDOnly := func(t *testing.T, wamid, phone, bsuid string) IncomingTextMessage {
		t.Helper()
		sidecar := &CoexistenceWebhookContact{UserID: bsuid, WaID: phone}
		sidecar.Profile.Name = "Wa ID Customer"
		return coexistenceAdmissionMessage(t, wamid, "", bsuid, "hello").withWebhookSenderContact(sidecar)
	}
	for _, customerFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("customer_first=%t", customerFirst), func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			organizationID := account.OrganizationID
			phone := coexistenceAdmissionTestPhone()
			bsuid := "US.wa-id-" + uuid.NewString()
			echoWAMID := "wamid.wa-id-echo-" + uuid.NewString()
			first := waIDOnly(t, "wamid.wa-id-first-"+uuid.NewString(), phone, bsuid)

			if !customerFirst {
				persistCoexistenceAdmissionEcho(t, app, account, echoWAMID, phone, "", "Hello from our phone")
			}
			work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, first, "Wa ID Customer")
			require.False(t, duplicate)
			require.NotNil(t, work, "a wa_id-only sender must not be held")
			if customerFirst {
				persistCoexistenceAdmissionEcho(t, app, account, echoWAMID, phone, "", "Hello from our phone")
			}
			var echoed models.Message
			require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", organizationID, echoWAMID).First(&echoed).Error)
			assert.Equal(t, work.Persisted.ContactID, echoed.ContactID, "echo and customer message share one contact")

			contact := loadCoexistenceAdmissionContact(t, app.DB, organizationID, work.Persisted.ContactID)
			assert.Equal(t, phone, contact.PhoneNumber, "wa_id became the contact phone")
			assert.Equal(t, bsuid, contact.BSUID)
			wantKind := coexistenceSenderAdmissionNewSender
			if !customerFirst {
				wantKind = coexistenceSenderAdmissionPhoneAppRecipient
			}
			assert.Equal(t, string(wantKind), contact.Metadata[coexistenceIdentityAdmissionKey])
			assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{},
				"organization_id = ? AND (phone_number = ? OR bs_uid = ?)", organizationID, phone, bsuid))
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{}, "organization_id = ?", organizationID))

			// Later messages in either shape stay on the same contact.
			second := waIDOnly(t, "wamid.wa-id-second-"+uuid.NewString(), phone, bsuid)
			secondWork, _ := admitCoexistenceAdmissionMessage(t, app, account, second, "Wa ID Customer")
			require.NotNil(t, secondWork)
			assert.Equal(t, contact.ID, secondWork.Persisted.ContactID)
			withFrom := coexistenceAdmissionMessage(t, "wamid.wa-id-from-"+uuid.NewString(), phone, bsuid, "now with from")
			withFromWork, _ := admitCoexistenceAdmissionMessage(t, app, account, withFrom, "Wa ID Customer")
			require.NotNil(t, withFromWork)
			assert.Equal(t, contact.ID, withFromWork.Persisted.ContactID)
			assert.EqualValues(t, 4, countCoexistenceAdmissionRows(t, app.DB, &models.Message{},
				"organization_id = ? AND contact_id = ?", organizationID, contact.ID))
		})
	}
}

// An echo may name a username user's BSUID in "to" without to_user_id. It
// must land on that user's contact, not create a second contact whose phone
// number is the BSUID.
func TestCoexistenceEchoToUsernameRecipientBSUIDEndsInOneContact(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	bsuid := "US." + testutil.NewTestGraphObjectID()[:17]
	sidecar := &CoexistenceWebhookContact{UserID: bsuid}
	sidecar.Profile.Name = "Username Customer"
	sidecar.Profile.Username = "handle_" + uuid.NewString()[:8]
	first := coexistenceAdmissionMessage(t, "wamid.username-first-"+uuid.NewString(), "", bsuid, "hi").withWebhookSenderContact(sidecar)
	work, _ := admitCoexistenceAdmissionMessage(t, app, account, first, "Username Customer")
	require.NotNil(t, work)
	contactID := work.Persisted.ContactID
	require.True(t, isCoexistencePlaceholderPhone(loadCoexistenceAdmissionContact(t, app.DB, organizationID, contactID).PhoneNumber))

	echoWAMID := "wamid.username-echo-" + uuid.NewString()
	persistCoexistenceAdmissionEcho(t, app, account, echoWAMID, bsuid, "", "Answer from our phone")
	var echoed models.Message
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", organizationID, echoWAMID).First(&echoed).Error)
	assert.Equal(t, contactID, echoed.ContactID)
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{},
		"organization_id = ? AND phone_number = ?", organizationID, bsuid), "a BSUID is never stored as a phone")
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND bs_uid = ?", organizationID, bsuid))
	assert.EqualValues(t, 2, countCoexistenceAdmissionRows(t, app.DB, &models.Message{},
		"organization_id = ? AND contact_id = ?", organizationID, contactID))
}

// coexistenceAdmissionEchoEvent builds a smb_message_echoes event from JSON.
func coexistenceAdmissionEchoEvent(t *testing.T, fields string) CoexistenceMessage {
	t.Helper()
	var echo CoexistenceMessage
	require.NoError(t, json.Unmarshal([]byte(`{"from":"`+coexistenceAdmissionBusinessPhone+`","timestamp":"1722222222",`+fields+`}`), &echo))
	return echo
}

// An echo addressed by BSUID ("to" is the BSUID) never takes a phone from the
// contacts[] sidecar, whether the only entry names another user or the same
// one. Otherwise its wa_id would bind the BSUID onto whichever older record
// owns that phone, and every later message from that BSUID would route there.
func TestCoexistenceEchoAddressedByBSUIDTakesNoPhoneFromTheSidecar(t *testing.T) {
	for _, sidecarIsRecipient := range []bool{false, true} {
		t.Run(fmt.Sprintf("sidecar_names_the_recipient=%t", sidecarIsRecipient), func(t *testing.T) {
			app, account, _ := whatsappIdentityFixture(t)
			organizationID := account.OrganizationID
			phone := coexistenceAdmissionTestPhone()
			old := newWhatsAppIdentityReviewSelectorContact(organizationID, phone)
			old.ProfileName = "Patient M old"
			require.NoError(t, app.DB.Create(&old).Error)
			require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", old.ID).
				Update("created_at", time.Now().UTC().Add(-30*24*time.Hour)).Error)

			// Another person's record, holding the parent BSUID that the unrelated
			// sidecar entry names.
			otherParent := "US.ENT." + testutil.NewTestGraphObjectID()[:17]
			other := newWhatsAppIdentityReviewSelectorContact(organizationID, coexistenceAdmissionTestPhone())
			other.ProfileName = "Patient Z"
			other.BSUID = otherParent
			require.NoError(t, app.DB.Create(&other).Error)

			bsuid := "US." + testutil.NewTestGraphObjectID()[:17]
			sidecar := CoexistenceWebhookContact{UserID: "US." + testutil.NewTestGraphObjectID()[:16], ParentUserID: otherParent, WaID: phone}
			if sidecarIsRecipient {
				sidecar.UserID, sidecar.ParentUserID = bsuid, ""
			}
			sidecar.Profile.Name = "Sidecar Name"
			echoWAMID := "wamid.bsuid-to-echo-" + uuid.NewString()
			echo := coexistenceAdmissionEchoEvent(t, `"to":"`+bsuid+`","id":"`+echoWAMID+`","type":"text","text":{"body":"Hello"}`)
			for range 2 { // the second delivery is Meta's replay
				require.NoError(t, app.persistMessageEchoesBeforeAck(account.PhoneID, []CoexistenceMessage{echo}, []CoexistenceWebhookContact{sidecar}))
			}

			var echoed models.Message
			require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", organizationID, echoWAMID).First(&echoed).Error)
			assert.NotEqual(t, old.ID, echoed.ContactID, "the echo must not land on the phone's older record")
			assert.NotEqual(t, other.ID, echoed.ContactID, "the echo must not land on another user's record")
			assert.Equal(t, otherParent, loadCoexistenceAdmissionContact(t, app.DB, organizationID, other.ID).BSUID)
			stored := loadCoexistenceAdmissionContact(t, app.DB, organizationID, old.ID)
			assert.Empty(t, stored.BSUID, "the older record must not receive the echo's BSUID")
			assert.NotContains(t, stored.Metadata, "coexistence_user_id")
			recipient := loadCoexistenceAdmissionContact(t, app.DB, organizationID, echoed.ContactID)
			assert.Equal(t, bsuid, recipient.BSUID)
			assert.True(t, isCoexistencePlaceholderPhone(recipient.PhoneNumber), recipient.PhoneNumber)
			if sidecarIsRecipient {
				assert.Equal(t, "Sidecar Name", recipient.ProfileName, "names from the recipient's own entry are kept")
			} else {
				assert.NotEqual(t, "Sidecar Name", recipient.ProfileName, "an unrelated entry supplies nothing")
			}
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{},
				"organization_id = ? AND phone_number = ?", organizationID, bsuid), "a BSUID is never stored as a phone")
			assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Message{},
				"organization_id = ? AND whats_app_message_id = ?", organizationID, echoWAMID))
		})
	}
}

// The previous release stored an echo addressed by BSUID on a contact whose
// phone_number is that BSUID and whose bs_uid is empty. Meta's replay of that
// echo, an edit or revoke of it, and a new echo to the same BSUID must still
// prove against that row instead of failing every retry (503) or starting a
// second contact for the same user.
func TestCoexistenceEchoStoredWithABSUIDPhoneByThePreviousReleaseStaysProvable(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	bsuid := "US." + testutil.NewTestGraphObjectID()[:17]

	// Reproduce the stored shape: the same echo row the previous release wrote,
	// on a contact whose phone_number is the BSUID and whose bs_uid is empty.
	originalWAMID := "wamid.legacy-bsuid-echo-" + uuid.NewString()
	persistCoexistenceAdmissionEcho(t, app, account, originalWAMID, coexistenceAdmissionTestPhone(), "", "Stored by the previous release")
	var original models.Message
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", organizationID, originalWAMID).First(&original).Error)
	legacyID := original.ContactID
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("organization_id = ? AND id = ?", organizationID, legacyID).
		Update("phone_number", bsuid).Error)
	legacy := loadCoexistenceAdmissionContact(t, app.DB, organizationID, legacyID)
	require.Empty(t, legacy.BSUID)
	require.Equal(t, bsuid, legacy.PhoneNumber)
	persist := func(events ...CoexistenceMessage) {
		t.Helper()
		require.NoError(t, app.persistMessageEchoesBeforeAck(account.PhoneID, events, nil))
	}

	// Meta replays the stored echo, still addressed by the BSUID.
	persist(coexistenceAdmissionEchoEvent(t, `"to":"`+bsuid+`","id":"`+originalWAMID+`","type":"text","text":{"body":"Stored by the previous release"}`))
	// The recipient (a username user) writes before any new echo. The legacy
	// row is that user's thread, so the sender is not new: the message is held
	// as before this change rather than starting a second contact.
	heldWAMID := "wamid.legacy-bsuid-inbound-" + uuid.NewString()
	held, _ := admitCoexistenceAdmissionMessage(t, app, account,
		coexistenceAdmissionMessage(t, heldWAMID, "", bsuid, "hi"), "Username Customer")
	assert.Nil(t, held)
	assert.EqualValues(t, 1, countCoexistenceStagedReceipts(t, app, organizationID, heldWAMID))
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{}, "organization_id = ? AND bs_uid = ?", organizationID, bsuid))

	// Staff edit the stored message.
	persist(coexistenceAdmissionEchoEvent(t, `"to":"`+bsuid+`","id":"wamid.legacy-edit-`+uuid.NewString()+`","type":"edit","edit":{"original_message_id":"`+originalWAMID+`","message":{"type":"text","text":{"body":"Edited after the release"}}}`))
	edited := models.Message{}
	require.NoError(t, app.DB.Where("id = ?", original.ID).First(&edited).Error)
	assert.Equal(t, "Edited after the release", edited.Content)
	assert.Equal(t, legacyID, edited.ContactID)

	// A new echo to the same BSUID continues the stored thread and binds it.
	nextWAMID := "wamid.legacy-bsuid-next-" + uuid.NewString()
	persist(coexistenceAdmissionEchoEvent(t, `"to":"`+bsuid+`","id":"`+nextWAMID+`","type":"text","text":{"body":"Another answer"}`))
	var next models.Message
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", organizationID, nextWAMID).First(&next).Error)
	assert.Equal(t, legacyID, next.ContactID, "a new echo must not split the thread")
	assert.Equal(t, bsuid, loadCoexistenceAdmissionContact(t, app.DB, organizationID, legacyID).BSUID)
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Contact{},
		"organization_id = ? AND (bs_uid = ? OR phone_number = ? OR phone_number = ?)",
		organizationID, bsuid, bsuid, coexistenceIdentityPlaceholder(coexistenceContactIdentity{UserID: bsuid})))
	// Now that the row holds the BSUID, the user's next message joins it.
	reply, _ := admitCoexistenceAdmissionMessage(t, app, account,
		coexistenceAdmissionMessage(t, "wamid.legacy-bsuid-reply-"+uuid.NewString(), "", bsuid, "thanks"), "Username Customer")
	require.NotNil(t, reply, "the user's message after the binding echo opens the stored thread")
	assert.Equal(t, legacyID, reply.Persisted.ContactID)

	// Staff delete the stored message for everyone; it is sanitized in place.
	persist(coexistenceAdmissionEchoEvent(t, `"to":"`+bsuid+`","id":"wamid.legacy-revoke-`+uuid.NewString()+`","type":"revoke","revoke":{"original_message_id":"`+originalWAMID+`"}`))
	revoked := models.Message{}
	require.NoError(t, app.DB.Where("id = ?", original.ID).First(&revoked).Error)
	assert.Equal(t, "[Message deleted from WhatsApp Business App]", revoked.Content)
	assert.Equal(t, true, revoked.Metadata[coexistenceMediaRevokedMetadataKey])

	// Replays still prove against the now-bound row.
	persist(coexistenceAdmissionEchoEvent(t, `"to":"`+bsuid+`","id":"`+originalWAMID+`","type":"text","text":{"body":"Stored by the previous release"}`))
	assert.EqualValues(t, 3, countCoexistenceAdmissionRows(t, app.DB, &models.Message{},
		"organization_id = ? AND contact_id = ?", organizationID, legacyID))
}

// The after-commit Omnichannel Inbox mirror is the only live projection of an
// inbound message. A retryable PostgreSQL abort (here a synthetic 40P01 on the
// first conversation insert, standing in for the mirror/admission deadlock)
// must be retried rather than leave inbox_conversation_id NULL until the next
// deploy's backfill.
func TestLegacyWhatsAppMirrorAfterCommitRetriesRetryableAbort(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	organizationID := account.OrganizationID
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	sequenceName := "mirror_abort_seq_" + suffix
	functionName := "mirror_abort_" + suffix
	triggerName := "mirror_abort_trigger_" + suffix
	require.NoError(t, app.DB.Exec("CREATE SEQUENCE "+sequenceName).Error)
	require.NoError(t, app.DB.Exec(fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			IF NEW.organization_id = '%s'::uuid THEN
				IF nextval('%s') = 1 THEN
					RAISE EXCEPTION 'synthetic mirror deadlock' USING ERRCODE = '40P01';
				END IF;
			END IF;
			RETURN NEW;
		END;
		$$`, functionName, organizationID, sequenceName)).Error)
	require.NoError(t, app.DB.Exec(fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE INSERT ON inbox_conversations
		FOR EACH ROW EXECUTE FUNCTION %s()`, triggerName, functionName)).Error)
	t.Cleanup(func() {
		_ = app.DB.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON inbox_conversations", triggerName)).Error
		_ = app.DB.Exec(fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", functionName)).Error
		_ = app.DB.Exec("DROP SEQUENCE IF EXISTS " + sequenceName).Error
	})

	message := coexistenceAdmissionMessage(t, "wamid.mirror-retry-"+uuid.NewString(), coexistenceAdmissionTestPhone(), "US.mirror-"+uuid.NewString(), "hi")
	work, duplicate := admitCoexistenceAdmissionMessage(t, app, account, message, "Mirror Customer")
	require.False(t, duplicate)
	require.NotNil(t, work)
	var calls int64
	require.NoError(t, app.DB.Raw("SELECT last_value FROM "+sequenceName).Scan(&calls).Error)
	require.GreaterOrEqual(t, calls, int64(2), "the first mirror attempt was aborted")
	assert.NotNil(t, work.Persisted.InboxConversationID, "the retried mirror projected the message")
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.InboxConversation{},
		"organization_id = ? AND contact_id = ?", organizationID, work.Persisted.ContactID))
}
