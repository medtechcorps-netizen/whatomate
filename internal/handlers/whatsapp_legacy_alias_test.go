package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// This fixture uses the real MergeContact route, including its message move,
// source soft-delete and projection handling. Only the initial phone=BSUID
// shape is written directly, as in the prior-release regression fixture.
func mergedLegacyAliasFixture(t *testing.T) (*App, *models.WhatsAppAccount, *models.Contact, models.Contact, models.Message, string) {
	return mergedLegacyAliasFixtureWithMerge(t, mergeContactsForTest)
}

func mergedLegacyAliasFixtureWithMerge(t *testing.T, merge func(*testing.T, *App, uuid.UUID, uuid.UUID, uuid.UUID)) (*App, *models.WhatsAppAccount, *models.Contact, models.Contact, models.Message, string) {
	t.Helper()
	app, account, target := whatsappIdentityFixture(t)
	app.Config = &config.Config{WhatsApp: config.WhatsAppConfig{AppSecret: webhookTestAppSecret}}
	require.NoError(t, app.DB.Model(account).Update("app_secret", webhookTestAppSecret).Error)
	app.InvalidateWhatsAppAccountCache(account.PhoneID)
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, server.URL)
	t.Cleanup(func() {
		app.WaitForBackgroundTasks()
		server.Close()
		assert.Zero(t, providerCalls.Load(), "stored replay/edit/revoke must not send to the provider")
	})
	bsuid := "US." + testutil.NewTestGraphObjectID()[:17]
	wamid := "wamid.merged-legacy-" + uuid.NewString()
	persistCoexistenceAdmissionEcho(t, app, account, wamid, coexistenceAdmissionTestPhone(), "", "original legacy body")
	app.WaitForBackgroundTasks()
	var original models.Message
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, wamid).First(&original).Error)
	// HTTP/persistence success cannot stand in for the best-effort initial mirror.
	// This must be a real linked fixture, with the original S keys, before merge.
	beforeProjection := loadMergedLegacyProjection(t, app.DB, account, original.ID, original.ContactID, original.ContactID)
	require.Equal(t, original.Content, beforeProjection.Conversation.LastMessagePreview)
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ? AND organization_id = ?", original.ContactID, account.OrganizationID).
		Update("phone_number", bsuid).Error)
	legacyID := original.ContactID
	merge(t, app, account.OrganizationID, target.ID, legacyID)
	app.WaitForBackgroundTasks()
	var legacy models.Contact
	require.NoError(t, app.DB.Unscoped().First(&legacy, legacyID).Error)
	require.True(t, legacy.DeletedAt.Valid)
	require.NotNil(t, legacy.MergedIntoID)
	require.Equal(t, target.ID, *legacy.MergedIntoID)
	require.Empty(t, legacy.BSUID)
	require.NoError(t, app.DB.First(&original, original.ID).Error)
	require.Equal(t, target.ID, original.ContactID)
	afterProjection := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacyID)
	assertMergedLegacyProjectionIdentity(t, beforeProjection, afterProjection)
	targetValue := loadCoexistenceAdmissionContact(t, app.DB, account.OrganizationID, target.ID)
	require.Empty(t, targetValue.BSUID)
	return app, account, &targetValue, legacy, original, bsuid
}

type mergedLegacyProjectionSnapshot struct {
	Message      models.Message
	Account      models.ChannelAccount
	Conversation models.InboxConversation
	Identity     models.ContactIdentity
	Participant  models.ConversationParticipant
}

func loadMergedLegacyProjection(t *testing.T, db *gorm.DB, account *models.WhatsAppAccount, messageID, canonicalID, legacyID uuid.UUID) mergedLegacyProjectionSnapshot {
	t.Helper()
	var result mergedLegacyProjectionSnapshot
	require.NoError(t, db.Where("id = ? AND organization_id = ?", messageID, account.OrganizationID).First(&result.Message).Error)
	require.NotNil(t, result.Message.InboxConversationID, "fixture must have completed the actual initial mirror")
	require.Equal(t, canonicalID, result.Message.ContactID)
	require.NoError(t, db.Where("id = ? AND organization_id = ?", *result.Message.InboxConversationID, account.OrganizationID).First(&result.Conversation).Error)
	require.NotNil(t, result.Conversation.ContactIdentityID)
	require.NoError(t, db.Where("id = ? AND organization_id = ?", *result.Conversation.ContactIdentityID, account.OrganizationID).First(&result.Identity).Error)
	require.NoError(t, db.Where("id = ? AND organization_id = ?", result.Conversation.ChannelAccountID, account.OrganizationID).First(&result.Account).Error)
	nativeID, err := channelapi.LegacyMetaWhatsAppAccountID(&result.Account)
	require.NoError(t, err)
	require.Equal(t, account.ID, nativeID)
	require.Equal(t, models.ChannelWhatsApp, result.Conversation.Channel)
	require.Equal(t, models.ChannelWhatsApp, result.Identity.Channel)
	require.Equal(t, result.Account.ID, result.Identity.ChannelAccountID)
	require.Equal(t, canonicalID, result.Conversation.ContactID)
	require.Equal(t, canonicalID, result.Identity.ContactID)
	key := "legacy-contact:" + legacyID.String()
	require.Equal(t, key, result.Conversation.ExternalConversationID)
	require.Equal(t, key, result.Identity.ExternalID)
	require.Equal(t, legacyID.String(), result.Conversation.Metadata["legacy_contact_id"])
	require.Equal(t, legacyID.String(), result.Identity.Metadata["legacy_contact_id"])
	var participants []models.ConversationParticipant
	require.NoError(t, db.Where("organization_id = ? AND conversation_id = ? AND role = ?", account.OrganizationID, result.Conversation.ID, models.ConversationParticipantRoleCustomer).Limit(2).Find(&participants).Error)
	require.Len(t, participants, 1)
	result.Participant = participants[0]
	require.NotNil(t, result.Participant.ContactIdentityID)
	require.Equal(t, result.Identity.ID, *result.Participant.ContactIdentityID)
	require.Equal(t, key, result.Participant.ParticipantKey)
	require.Equal(t, key, result.Participant.ExternalID)
	require.Equal(t, legacyID.String(), result.Participant.Metadata["legacy_contact_id"])
	require.Nil(t, result.Participant.LeftAt)
	return result
}

func assertMergedLegacyProjectionIdentity(t *testing.T, before, after mergedLegacyProjectionSnapshot) {
	t.Helper()
	assert.Equal(t, before.Message.ID, after.Message.ID)
	assert.Equal(t, before.Message.InboxConversationID, after.Message.InboxConversationID)
	assert.Equal(t, before.Message.EffectiveIngestedAt(), after.Message.EffectiveIngestedAt())
	assert.Equal(t, before.Account.ID, after.Account.ID)
	assert.Equal(t, before.Conversation.ID, after.Conversation.ID)
	assert.Equal(t, before.Conversation.ExternalConversationID, after.Conversation.ExternalConversationID)
	assert.Equal(t, before.Conversation.Metadata["legacy_contact_id"], after.Conversation.Metadata["legacy_contact_id"])
	assert.Equal(t, before.Identity.ID, after.Identity.ID)
	assert.Equal(t, before.Identity.ExternalID, after.Identity.ExternalID)
	assert.Equal(t, before.Identity.Metadata["legacy_contact_id"], after.Identity.Metadata["legacy_contact_id"])
	assert.Equal(t, before.Participant.ID, after.Participant.ID)
	assert.Equal(t, before.Participant.ParticipantKey, after.Participant.ParticipantKey)
	assert.Equal(t, before.Participant.ExternalID, after.Participant.ExternalID)
	assert.Equal(t, before.Participant.Metadata["legacy_contact_id"], after.Participant.Metadata["legacy_contact_id"])
}

func mergedLegacyProjectionCounts(t *testing.T, db *gorm.DB, organizationID uuid.UUID) []int64 {
	t.Helper()
	var counts []int64
	for _, model := range []any{&models.ContactIdentity{}, &models.InboxConversation{}, &models.ConversationParticipant{}, &models.Message{}, &models.Contact{}, &models.ScheduledJob{}} {
		counts = append(counts, countCoexistenceAdmissionRows(t, db, model, "organization_id = ?", organizationID))
	}
	return counts
}

func mergedLegacyProjectionEvidence(account *models.WhatsAppAccount, legacy models.Contact, projection mergedLegacyProjectionSnapshot, bsuid string) channelapi.LegacyMergedProjectionEvidence {
	return channelapi.LegacyMergedProjectionEvidence{
		OrganizationID: account.OrganizationID, AccountID: account.ID, MessageID: projection.Message.ID,
		CanonicalContactID: projection.Message.ContactID, LegacyContactID: legacy.ID,
		ConversationID: projection.Conversation.ID, IdentityID: projection.Identity.ID, ParticipantID: projection.Participant.ID,
		WAMID: projection.Message.WhatsAppMessageID, DirectBSUID: bsuid,
	}
}

func mergedLegacyAccountRef(account *models.WhatsAppAccount) channelapi.LegacyMetaAccountRef {
	return channelapi.LegacyMetaAccountRef{ID: account.ID, OrganizationID: account.OrganizationID, Name: account.Name,
		Status: account.Status, PhoneID: account.PhoneID, BusinessID: account.BusinessID}
}

func mergedLegacyAliasEvent(t *testing.T, bsuid, wamid, kind string) CoexistenceMessage {
	t.Helper()
	switch kind {
	case "replay":
		return coexistenceAdmissionEchoEvent(t, `"to":"`+bsuid+`","id":"`+wamid+`","type":"text","text":{"body":"original legacy body"}`)
	case "edit":
		return coexistenceAdmissionEchoEvent(t, `"to":"`+bsuid+`","id":"wamid.legacy-edit-`+uuid.NewString()+`","type":"edit","edit":{"original_message_id":"`+wamid+`","message":{"type":"text","text":{"body":"edited legacy body"}}}`)
	case "revoke":
		return coexistenceAdmissionEchoEvent(t, `"to":"`+bsuid+`","id":"wamid.legacy-revoke-`+uuid.NewString()+`","type":"revoke","revoke":{"original_message_id":"`+wamid+`"}`)
	default:
		t.Fatalf("unknown test mutation %q", kind)
		return CoexistenceMessage{}
	}
}

func mergedLegacyAliasWebhookBody(t *testing.T, account *models.WhatsAppAccount, events ...CoexistenceMessage) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry": []any{map[string]any{"id": account.BusinessID, "changes": []any{map[string]any{
			"field": "smb_message_echoes", "value": map[string]any{
				"metadata": map[string]any{"phone_number_id": account.PhoneID}, "message_echoes": events,
			},
		}}}},
	})
	require.NoError(t, err)
	return body
}

func mergedLegacyAliasLookup(message models.Message, bsuid string) whatsAppMessageLookup {
	return whatsAppMessageLookup{WAMID: message.WhatsAppMessageID, MessageID: message.ID,
		Identity: coexistenceContactIdentity{UserID: bsuid}, Direction: models.DirectionOutgoing}
}

func TestMergedLegacyAliasSignedReplayEditRevokeRemainIdempotent(t *testing.T) {
	app, account, target, legacy, original, bsuid := mergedLegacyAliasFixture(t)
	projectionBefore := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	projectionCounts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
	participantID, messageID := projectionBefore.Participant.ID, original.ID
	ingested := original.EffectiveIngestedAt()
	require.False(t, ingested.IsZero())
	read := models.ConversationRead{OrganizationID: account.OrganizationID, ConversationID: projectionBefore.Conversation.ID,
		ParticipantID: &participantID, ReaderKey: "external:" + participantID.String(), LastReadMessageID: &messageID,
		LastReadExternalID: original.WhatsAppMessageID, LastReadIngestedAt: &ingested, LastReadAt: time.Now().UTC(), Metadata: models.JSONB{}}
	require.NoError(t, app.DB.Create(&read).Error)
	require.NoError(t, app.DB.First(&read, read.ID).Error)
	contactsBefore := countCoexistenceAdmissionRows(t, app.DB.Unscoped(), &models.Contact{}, "organization_id = ?", account.OrganizationID)
	jobsBefore := countCoexistenceAdmissionRows(t, app.DB, &models.ScheduledJob{}, "organization_id = ?", account.OrganizationID)
	require.NoError(t, app.DB.Model(&models.Message{}).Where("id = ?", original.ID).
		Updates(map[string]any{"media_url": "private/legacy-object", "media_mime_type": "image/png"}).Error)
	for _, kind := range []string{"replay", "edit", "revoke"} {
		event := mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, kind)
		body := mergedLegacyAliasWebhookBody(t, account, event)
		for repeat := 0; repeat < 2; repeat++ {
			require.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, body), kind)
			app.WaitForBackgroundTasks()
		}
		var current models.Message
		require.NoError(t, app.DB.First(&current, original.ID).Error)
		assert.Equal(t, target.ID, current.ContactID)
		projectionAfter := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
		assertMergedLegacyProjectionIdentity(t, projectionBefore, projectionAfter)
		assert.Equal(t, current.Content, projectionAfter.Conversation.LastMessagePreview, "actual post-commit mirror must update the old conversation")
		assert.Equal(t, projectionCounts, mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID), "no target-key or duplicate projections")
		var readAfter models.ConversationRead
		require.NoError(t, app.DB.First(&readAfter, read.ID).Error)
		assert.Equal(t, read, readAfter, "replay/edit/revoke must not move or advance the read cursor")
		switch kind {
		case "edit":
			assert.Equal(t, "edited legacy body", current.Content)
		case "revoke":
			assert.Equal(t, "[Message deleted from WhatsApp Business App]", current.Content)
			assert.Equal(t, true, current.Metadata[coexistenceMediaRevokedMetadataKey])
			assert.Empty(t, current.MediaURL)
		}
	}
	// A replay after the tombstone must not resurrect either content or media.
	require.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app,
		mergedLegacyAliasWebhookBody(t, account, mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "replay"))))
	app.WaitForBackgroundTasks()
	var after models.Message
	require.NoError(t, app.DB.First(&after, original.ID).Error)
	assert.Equal(t, "[Message deleted from WhatsApp Business App]", after.Content)
	assert.Empty(t, after.MediaURL)
	projectionAfter := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	assertMergedLegacyProjectionIdentity(t, projectionBefore, projectionAfter)
	assert.Equal(t, after.Content, projectionAfter.Conversation.LastMessagePreview)
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Message{}, "organization_id = ?", account.OrganizationID))
	assert.Equal(t, contactsBefore, countCoexistenceAdmissionRows(t, app.DB.Unscoped(), &models.Contact{}, "organization_id = ?", account.OrganizationID))
	assert.Equal(t, jobsBefore, countCoexistenceAdmissionRows(t, app.DB, &models.ScheduledJob{}, "organization_id = ?", account.OrganizationID))
	assert.Empty(t, loadCoexistenceAdmissionContact(t, app.DB, account.OrganizationID, target.ID).BSUID)
	var legacyAfter models.Contact
	require.NoError(t, app.DB.Unscoped().First(&legacyAfter, legacy.ID).Error)
	assert.Equal(t, legacy, legacyAfter, "corroboration must not rewrite the retained alias")
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.InboundEvent{}, "organization_id = ?", account.OrganizationID))
	assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{}, "organization_id = ?", account.OrganizationID))
}

func TestMergedLegacyAliasRefusesCompetingIdentityWithoutMutation(t *testing.T) {
	for _, variant := range []string{"direct_holder", "parent_holder", "foreign_family_bsuid", "foreign_legacy_phone", "foreign_metadata", "conflict_metadata", "malformed_metadata", "alias_other_family", "alias_cycle", "deleted_terminal", "parent_only", "malformed_parent"} {
		t.Run(variant, func(t *testing.T) {
			app, account, target, legacy, original, bsuid := mergedLegacyAliasFixture(t)
			lookup := mergedLegacyAliasLookup(original, bsuid)
			switch variant {
			case "direct_holder", "parent_holder":
				holder := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
				id := bsuid
				if variant == "parent_holder" {
					id = "US." + testutil.NewTestGraphObjectID()[:17]
					lookup.Identity.ParentUserID = id
				}
				require.NoError(t, app.DB.Model(holder).Update("bs_uid", id).Error)
			case "foreign_family_bsuid", "foreign_legacy_phone":
				member := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
				values := map[string]any{"merged_into_id": target.ID, "deleted_at": time.Now().UTC()}
				key := "bs_uid"
				if variant == "foreign_legacy_phone" {
					key = "phone_number"
				}
				values[key] = "US." + testutil.NewTestGraphObjectID()[:17]
				require.NoError(t, app.DB.Model(member).Updates(values).Error)
			case "foreign_metadata", "conflict_metadata", "malformed_metadata":
				metadata := models.JSONB{"coexistence_user_id": "US." + testutil.NewTestGraphObjectID()[:17]}
				if variant == "conflict_metadata" {
					metadata = models.JSONB{"coexistence_phone_conflict": false}
				}
				if variant == "malformed_metadata" {
					metadata = models.JSONB{"coexistence_user_id": []any{bsuid}}
				}
				require.NoError(t, app.DB.Unscoped().Model(&models.Contact{}).Where("id = ?", legacy.ID).Update("metadata", metadata).Error)
			case "alias_other_family":
				other := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
				require.NoError(t, app.DB.Unscoped().Model(&models.Contact{}).Where("id = ?", legacy.ID).Update("merged_into_id", other.ID).Error)
			case "alias_cycle":
				require.NoError(t, app.DB.Unscoped().Model(&models.Contact{}).Where("id = ?", legacy.ID).Update("merged_into_id", legacy.ID).Error)
			case "deleted_terminal":
				require.NoError(t, app.DB.Model(target).Update("deleted_at", time.Now().UTC()).Error)
			case "parent_only":
				lookup.Identity = coexistenceContactIdentity{ParentUserID: bsuid}
			case "malformed_parent":
				lookup.Identity.ParentUserID = "not-a-bsuid"
			}
			_, err := app.resolveWhatsAppMessage(account, lookup)
			require.Error(t, err)
			if variant == "direct_holder" || variant == "parent_holder" {
				for _, kind := range []string{"replay", "edit", "revoke"} {
					event := mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, kind)
					event.ToParentUserID = lookup.Identity.ParentUserID
					require.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, mergedLegacyAliasWebhookBody(t, account, event)))
				}
			}
			var after models.Message
			require.NoError(t, app.DB.First(&after, original.ID).Error)
			assert.Equal(t, original, after)
			assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Message{}, "organization_id = ?", account.OrganizationID))
		})
	}
}

func TestMergedLegacyAliasCorroborationDoesNotAdmitANewSender(t *testing.T) {
	app, account, target, _, original, bsuid := mergedLegacyAliasFixture(t)
	require.NoError(t, app.persistMessageEchoesBeforeAck(account.PhoneID,
		[]CoexistenceMessage{mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "replay")}, nil))
	before := countCoexistenceAdmissionRows(t, app.DB.Unscoped(), &models.Contact{}, "organization_id = ?", account.OrganizationID)
	incoming := coexistenceAdmissionMessage(t, "wamid.legacy-still-held-"+uuid.NewString(), "", bsuid, "new sender message")
	work, _ := admitCoexistenceAdmissionMessage(t, app, account, incoming, "Unbound principal")
	assert.Nil(t, work, "stored alias corroboration is not a new incoming route grant")
	assert.EqualValues(t, 1, countCoexistenceStagedReceipts(t, app, account.OrganizationID, incoming.ID))
	assert.EqualValues(t, 1, countCoexistenceAdmissionRows(t, app.DB, &models.Message{}, "organization_id = ?", account.OrganizationID))
	assert.Equal(t, before, countCoexistenceAdmissionRows(t, app.DB.Unscoped(), &models.Contact{}, "organization_id = ?", account.OrganizationID))
	assert.Empty(t, loadCoexistenceAdmissionContact(t, app.DB, account.OrganizationID, target.ID).BSUID)
}

func TestMergedLegacyAliasDoesNotExpandIncomingOrContinuationProof(t *testing.T) {
	for _, variant := range []string{"incoming", "incoming_continuation", "outgoing_continuation_validation"} {
		t.Run(variant, func(t *testing.T) {
			app, account, target, _, original, bsuid := mergedLegacyAliasFixture(t)
			// Establish that the retained alias itself is sufficient, so refusal
			// below is caused by the trusted resolver's direction/purpose gate.
			matched, err := app.proveMergedLegacyWhatsAppMessageAlias(account.OrganizationID, target, coexistenceContactIdentity{UserID: bsuid})
			require.NoError(t, err)
			require.True(t, matched)
			lookup := mergedLegacyAliasLookup(original, bsuid)
			if variant != "outgoing_continuation_validation" {
				require.NoError(t, app.DB.Model(&models.Message{}).Where("id = ?", original.ID).Update("direction", models.DirectionIncoming).Error)
				lookup.Direction = models.DirectionIncoming
			}
			lookup.continuationValidation = variant != "incoming"
			_, err = app.resolveWhatsAppMessage(account, lookup)
			require.ErrorContains(t, err, "sender user identity mismatch")
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.ScheduledJob{}, "organization_id = ? AND kind = ?", account.OrganizationID, inboundContinuationJobKind))
			assert.Empty(t, loadCoexistenceAdmissionContact(t, app.DB, account.OrganizationID, target.ID).BSUID)
		})
	}
}

func TestMergedLegacyAliasIgnoresAnotherTenantsIdenticalAlias(t *testing.T) {
	app, account, _, _, original, bsuid := mergedLegacyAliasFixture(t)
	otherOrg := testutil.CreateTestOrganization(t, app.DB)
	other := testutil.CreateTestContactWith(t, app.DB, otherOrg.ID, testutil.WithPhoneNumber(bsuid))
	require.NoError(t, app.DB.Model(other).Update("bs_uid", bsuid).Error)
	_, err := app.resolveWhatsAppMessage(account, mergedLegacyAliasLookup(original, bsuid))
	require.NoError(t, err, "another tenant is neither corroboration nor a competing holder")
	var after models.Contact
	require.NoError(t, app.DB.First(&after, other.ID).Error)
	assert.Equal(t, bsuid, after.BSUID)
	assert.Nil(t, after.MergedIntoID)
}

func TestMergedLegacyAliasRequiresCompleteBoundedFamily(t *testing.T) {
	for _, variant := range []string{"64_members", "65_members", "16_levels", "17_levels"} {
		t.Run(variant, func(t *testing.T) {
			app, account, target, _, original, bsuid := mergedLegacyAliasFixture(t)
			count, chain := 62, false // root + legacy alias already exist.
			switch variant {
			case "65_members":
				count = 63
			case "16_levels":
				count, chain = 15, true
			case "17_levels":
				count, chain = 16, true
			}
			parent := target.ID
			for i := 0; i < count; i++ {
				member := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
				require.NoError(t, app.DB.Model(member).Updates(map[string]any{"merged_into_id": parent, "deleted_at": time.Now().UTC()}).Error)
				if chain {
					parent = member.ID
				}
			}
			_, err := app.resolveWhatsAppMessage(account, mergedLegacyAliasLookup(original, bsuid))
			if variant == "64_members" || variant == "16_levels" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "proof bound")
			}
			assert.Empty(t, loadCoexistenceAdmissionContact(t, app.DB, account.OrganizationID, target.ID).BSUID)
		})
	}
}

func TestMergedLegacyAliasRespectsOpenAndForeignDecidedFamilies(t *testing.T) {
	for _, variant := range []string{"open", "decided", "decided_other_account_and_cycle"} {
		t.Run(variant, func(t *testing.T) {
			app, account, target, _, original, bsuid := mergedLegacyAliasFixture(t)
			reviewAccount := account
			if variant == "decided_other_account_and_cycle" {
				reviewAccount = testutil.CreateTestWhatsAppAccount(t, app.DB, account.OrganizationID)
				reviewAccount.IsSMB = true
				reviewAccount.BusinessID = testutil.NewTestGraphObjectID()
				require.NoError(t, app.DB.Save(reviewAccount).Error)
				require.NoError(t, app.DB.Create(&models.WhatsAppCoexistenceState{
					ID: uuid.New(), OrganizationID: account.OrganizationID, WhatsAppAccountID: reviewAccount.ID,
					OnboardingStatus: models.CoexistenceOnboardingStatusConnected, OnboardingCycle: 3,
					LifecycleStatus: models.CoexistenceLifecycleStatusConnected, LifecycleMetadata: models.JSONB{}, Version: 1,
				}).Error)
			}
			foreign := "US." + testutil.NewTestGraphObjectID()[:17]
			work, _ := admitCoexistenceAdmissionMessage(t, app, reviewAccount,
				coexistenceAdmissionMessage(t, "wamid.legacy-review-"+uuid.NewString(), target.PhoneNumber, foreign, "held"), "Foreign principal")
			require.Nil(t, work)
			hold := latestWhatsAppIdentityReviewHoldForBSUID(t, app, account.OrganizationID, foreign)
			require.Equal(t, models.WhatsAppIdentityReviewDispositionOpen, hold.Disposition)
			if variant != "open" {
				resolver := createWhatsAppIdentityReviewResolver(t, app.DB, account.OrganizationID)
				resolveLatestContactIdentityReviewForTest(t, app, account.OrganizationID, resolver.ID, target.ID, target.ID)
				hold = latestWhatsAppIdentityReviewHoldForBSUID(t, app, account.OrganizationID, foreign)
				require.Equal(t, models.WhatsAppIdentityReviewDispositionFutureRouting, hold.Disposition)
				if variant == "decided_other_account_and_cycle" {
					require.Equal(t, reviewAccount.ID, hold.WhatsAppAccountID)
					require.EqualValues(t, 3, hold.OnboardingCycle)
				}
			}
			body := mergedLegacyAliasWebhookBody(t, account, mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "revoke"))
			require.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, body))
			app.WaitForBackgroundTasks()
			var after models.Message
			require.NoError(t, app.DB.First(&after, original.ID).Error)
			assert.Equal(t, original, after)
			assert.Empty(t, loadCoexistenceAdmissionContact(t, app.DB, account.OrganizationID, target.ID).BSUID)
		})
	}
}

func TestMergedLegacyAliasCannotSupplyMissingAccountProof(t *testing.T) {
	app, account, target, _, _, bsuid := mergedLegacyAliasFixture(t)
	message := models.Message{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: account.OrganizationID,
		ContactID: target.ID, WhatsAppAccount: account.Name, WhatsAppMessageID: "wamid.no-account-proof-" + uuid.NewString(),
		Direction: models.DirectionOutgoing, MessageType: models.MessageTypeText, Content: "unproven", Status: models.MessageStatusSent, Metadata: models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&message).Error)
	_, err := app.resolveWhatsAppMessage(account, mergedLegacyAliasLookup(message, bsuid))
	require.ErrorContains(t, err, "lacks durable account provenance")
	var after models.Message
	require.NoError(t, app.DB.First(&after, message.ID).Error)
	assert.Equal(t, message.ID, after.ID)
	assert.Equal(t, "unproven", after.Content)
	assert.Nil(t, after.InboxConversationID)
}

func TestMergedLegacyAliasDoesNotReplaceAccountOrWebhookAuthority(t *testing.T) {
	for _, variant := range []string{"unsigned", "invalid_signature", "wrong_account", "wrong_organization", "wrong_direction", "missing_wamid", "conflicting_activity"} {
		t.Run(variant, func(t *testing.T) {
			app, account, _, _, original, bsuid := mergedLegacyAliasFixture(t)
			lookup := mergedLegacyAliasLookup(original, bsuid)
			if variant == "unsigned" || variant == "invalid_signature" {
				body := mergedLegacyAliasWebhookBody(t, account, mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "edit"))
				req := testutil.NewRequest(t)
				req.RequestCtx.Request.Header.SetMethod("POST")
				req.RequestCtx.Request.Header.SetContentType("application/json")
				if variant == "invalid_signature" {
					req.RequestCtx.Request.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
				}
				req.RequestCtx.Request.SetBody(body)
				require.NoError(t, app.WebhookHandler(req))
				require.Equal(t, http.StatusForbidden, req.RequestCtx.Response.StatusCode())
			} else {
				selected := account
				switch variant {
				case "wrong_account":
					selected = testutil.CreateTestWhatsAppAccount(t, app.DB, account.OrganizationID)
				case "wrong_organization":
					org := testutil.CreateTestOrganization(t, app.DB)
					selected = testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
				case "wrong_direction":
					lookup.Direction = models.DirectionIncoming
				case "missing_wamid":
					lookup.WAMID = "wamid.absent-" + uuid.NewString()
				case "conflicting_activity":
					createWhatsAppIdentityActivity(t, app.DB, account, &original)
				}
				_, err := app.resolveWhatsAppMessage(selected, lookup)
				require.Error(t, err)
			}
			var after models.Message
			require.NoError(t, app.DB.First(&after, original.ID).Error)
			assert.Equal(t, original, after)
		})
	}
}

func TestMergedLegacyAliasSelectiveQueryFailureRollsBackBatch(t *testing.T) {
	for _, query := range []string{"legacy", "bsuid", "family", "decision", "hold"} {
		t.Run(query, func(t *testing.T) {
			app, account, _, _, original, bsuid := mergedLegacyAliasFixture(t)
			phone, firstWAMID := coexistenceAdmissionTestPhone(), "wamid.before-failed-legacy-"+uuid.NewString()
			persistCoexistenceAdmissionEcho(t, app, account, firstWAMID, phone, "", "must roll back")
			app.WaitForBackgroundTasks()
			var first models.Message
			require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, firstWAMID).First(&first).Error)
			pool, err := app.DB.DB()
			require.NoError(t, err)
			isolated, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			var hits atomic.Int32
			require.NoError(t, isolated.Callback().Query().Before("gorm:query").Register("test:legacy-selective-query", func(tx *gorm.DB) {
				callbacks.BuildQuerySQL(tx)
				sql := tx.Statement.SQL.String()
				matched := false
				switch query {
				case "legacy":
					matched = strings.Contains(sql, "phone_number =") && strings.Contains(sql, "COALESCE(bs_uid, '') = ''")
				case "bsuid":
					matched = strings.Contains(sql, "BTRIM(bs_uid) IN")
				case "family":
					matched = strings.Contains(sql, "merged_into_id IN")
				case "decision":
					matched = strings.Contains(sql, "direct_primary_bsuid NOT IN")
				case "hold":
					matched = strings.Contains(sql, "whatsapp_identity_review_members AS member") && strings.Contains(sql, "member.contact_id IN")
				}
				if !matched {
					return
				}
				hits.Add(1)
				// Actual PostgreSQL statement failure, scoped to this test's GORM
				// instance. SELECT1 remains healthy before and after rollback.
				tx.Statement.SQL.Reset()
				tx.Statement.SQL.WriteString("SELECT 1 / 0")
				tx.Statement.Vars = nil
			}))
			live := app.DB
			app.DB = isolated
			defer func() { app.DB = live }()
			var one int
			require.NoError(t, app.DB.Raw("SELECT 1").Scan(&one).Error)
			body := mergedLegacyAliasWebhookBody(t, account,
				mergedLegacyAliasEvent(t, phone, firstWAMID, "edit"), mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "edit"))
			require.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, body))
			require.Positive(t, hits.Load(), "the targeted query must actually execute")
			app.WaitForBackgroundTasks()
			var afterFirst, afterOriginal models.Message
			require.NoError(t, live.First(&afterFirst, first.ID).Error)
			require.NoError(t, live.First(&afterOriginal, original.ID).Error)
			assert.Equal(t, first, afterFirst, "first edit cannot escape the failed batch transaction")
			assert.Equal(t, original, afterOriginal)
			require.NoError(t, app.DB.Raw("SELECT 1").Scan(&one).Error)
		})
	}
}

func TestMergedLegacyAliasPropagatesCancelledProof(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", expired), func(t *testing.T) {
			app, account, target, _, _, bsuid := mergedLegacyAliasFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			want := error(context.Canceled)
			if expired {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			} else {
				cancel()
			}
			defer cancel()
			scoped := app.scopedApp(app.DB.WithContext(ctx), account.OrganizationID)
			matched, err := scoped.proveMergedLegacyWhatsAppMessageAlias(account.OrganizationID, target, coexistenceContactIdentity{UserID: bsuid})
			require.ErrorIs(t, err, want)
			assert.False(t, matched)
		})
	}
}

func TestMergedLegacyAliasPreAdmissionCommitRefusesSecondHolder(t *testing.T) {
	app, account, target, legacy, original, bsuid := mergedLegacyAliasFixture(t)
	// A pre-existing unrelated contact can acquire the selector while admission
	// has not acquired its organization fence. No trigger or lock is bypassed.
	holder := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	resolved, err := app.resolveWhatsAppMessage(account, mergedLegacyAliasLookup(original, bsuid))
	require.NoError(t, err)
	require.NotNil(t, resolved.LegacyMergedProjection, "the unchanged fixture must initially prove the stored winner")
	projectionBefore := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
	live := app.DB
	pool, err := live.DB()
	require.NoError(t, err)
	isolated, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	atFence, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var discovered, reached atomic.Bool
	require.NoError(t, isolated.Callback().Query().After("gorm:query").Register("test:merged-legacy-discovery", func(tx *gorm.DB) {
		if tx.Error != nil || tx.RowsAffected != 1 || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "whatsapp_accounts" ||
			len(tx.Statement.Selects) != 1 || tx.Statement.Selects[0] != "organization_id" {
			return
		}
		selected, ok := tx.Statement.Dest.(*models.WhatsAppAccount)
		if !ok || selected.OrganizationID != account.OrganizationID {
			return
		}
		for _, value := range tx.Statement.Vars {
			if fmt.Sprint(value) == account.PhoneID {
				discovered.Store(true)
			}
		}
	}))
	require.NoError(t, isolated.Callback().Raw().Before("gorm:raw").Register("test:merged-legacy-before-admission", func(tx *gorm.DB) {
		// Exec uses the Raw callback. Pause before the actual selector advisory
		// acquisition in LockOrganizationPolicyScope, after tenant discovery;
		// the earlier WAMID advisory lock does not fence contact selector writes.
		if tx.Statement.SQL.String() != "SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1, 0))" ||
			len(tx.Statement.Vars) != 1 || fmt.Sprint(tx.Statement.Vars[0]) != "rereply:whatsapp_identity_review_selector:v1:"+account.OrganizationID.String() ||
			!reached.CompareAndSwap(false, true) {
			return
		}
		if _, inTransaction := tx.Statement.ConnPool.(gorm.TxCommitter); !inTransaction || !discovered.Load() {
			_ = tx.AddError(fmt.Errorf("admission barrier did not follow the actual tenant read in a transaction"))
			return
		}
		close(atFence)
		select {
		case <-release:
		case <-ctx.Done():
			_ = tx.AddError(ctx.Err())
		}
	}))
	event := mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "revoke")
	app.DB = isolated.WithContext(ctx)
	result := make(chan error, 1)
	joined := false
	defer func() {
		unblock()
		cancel()
		if !joined {
			drain := time.NewTimer(2 * time.Second)
			defer drain.Stop()
			select {
			case <-result:
				joined = true
			case <-drain.C:
				t.Error("owned pre-admission goroutine did not stop within the bounded cancellation drain")
			}
		}
		if joined {
			app.DB = live
		}
	}()
	go func() { result <- app.persistMessageEchoesBeforeAck(account.PhoneID, []CoexistenceMessage{event}, nil) }()
	select {
	case <-atFence:
	case err := <-result:
		joined = true
		t.Fatalf("persistence ended before the pre-admission barrier: %v", err)
	case <-ctx.Done():
		t.Fatal("persistence never reached its pre-admission barrier")
	}
	require.True(t, discovered.Load(), "the request completed its real tenant lookup before the concurrent commit")
	require.NoError(t, live.WithContext(ctx).Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", holder.ID, account.OrganizationID).Update("bs_uid", bsuid).Error)
	var committedHolder models.Contact
	require.NoError(t, live.WithContext(ctx).First(&committedHolder, holder.ID).Error)
	require.Equal(t, bsuid, committedHolder.BSUID, "the independent selector write must actually commit before admission resumes")
	unblock()
	select {
	case err := <-result:
		joined = true
		require.Error(t, err, "fresh admission proof must refuse a second holder committed before its fence")
	case <-ctx.Done():
		t.Fatal("admission proof failed to finish after release")
	}
	app.WaitForBackgroundTasks()
	var after models.Message
	require.NoError(t, live.First(&after, original.ID).Error)
	assert.Equal(t, original, after, "the rejected revoke cannot mutate the stored winner")
	projectionAfter := loadMergedLegacyProjection(t, live, account, original.ID, target.ID, legacy.ID)
	assert.Equal(t, projectionBefore, projectionAfter, "refusal must not change any linked projection")
	assert.Equal(t, counts, mergedLegacyProjectionCounts(t, live, account.OrganizationID), "refusal cannot create replacement rows or jobs")
	var holderAfter models.Contact
	require.NoError(t, live.First(&holderAfter, holder.ID).Error)
	assert.Equal(t, committedHolder, holderAfter, "refusal cannot undo the independent committed selector")
}

func TestMergedLegacyAliasAdmissionFenceRejectsSecondHolderAndMirrorSucceeds(t *testing.T) {
	app, account, target, legacy, original, bsuid := mergedLegacyAliasFixture(t)
	holder := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	var holderBefore models.Contact
	require.NoError(t, app.DB.First(&holderBefore, holder.ID).Error)
	require.Empty(t, holderBefore.BSUID)
	projectionBefore := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
	live := app.DB
	pool, err := live.DB()
	require.NoError(t, err)
	isolated, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	atLock, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var reached atomic.Bool
	require.NoError(t, isolated.Callback().Query().Before("gorm:query").Register("test:merged-legacy-after-admission", func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "contacts" {
			return
		}
		if _, locking := tx.Statement.Clauses["FOR"]; !locking || !reached.CompareAndSwap(false, true) {
			return
		}
		close(atLock)
		select {
		case <-release:
		case <-ctx.Done():
			_ = tx.AddError(ctx.Err())
		}
	}))
	event := mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "revoke")
	app.DB = isolated.WithContext(ctx)
	result := make(chan error, 1)
	joined := false
	defer func() {
		unblock()
		cancel()
		if !joined {
			drain := time.NewTimer(2 * time.Second)
			defer drain.Stop()
			select {
			case <-result:
				joined = true
			case <-drain.C:
				t.Error("owned locked-proof goroutine did not stop within the bounded cancellation drain")
			}
		}
		if joined {
			app.DB = live
		}
	}()
	go func() { result <- app.persistMessageEchoesBeforeAck(account.PhoneID, []CoexistenceMessage{event}, nil) }()
	select {
	case <-atLock:
	case err := <-result:
		joined = true
		t.Fatalf("resolver ended before the post-admission contact lock: %v", err)
	case <-ctx.Done():
		t.Fatal("resolver never reached the contact lock barrier")
	}
	// The real selector trigger cannot admit a competing selector while this
	// transaction owns the organization fence. Keep its observed 55P03 contract.
	writeErr := live.WithContext(ctx).Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", holder.ID, account.OrganizationID).Update("bs_uid", bsuid).Error
	require.Error(t, writeErr)
	require.Equal(t, "55P03", postgresErrorCode(writeErr))
	var holderAfter models.Contact
	require.NoError(t, live.WithContext(ctx).First(&holderAfter, holder.ID).Error)
	require.Equal(t, holderBefore, holderAfter, "the rejected independent transaction cannot change the selector")
	unblock()
	select {
	case err := <-result:
		joined = true
		require.NoError(t, err, "unchanged authority and provenance must complete the revoke")
	case <-ctx.Done():
		t.Fatal("locked proof failed to finish after release")
	}
	app.WaitForBackgroundTasks()
	var after models.Message
	require.NoError(t, live.First(&after, original.ID).Error)
	assert.Equal(t, "[Message deleted from WhatsApp Business App]", after.Content)
	assert.Equal(t, true, after.Metadata[coexistenceMediaRevokedMetadataKey])
	assert.Empty(t, after.MediaURL)
	projectionAfter := loadMergedLegacyProjection(t, live, account, original.ID, target.ID, legacy.ID)
	assertMergedLegacyProjectionIdentity(t, projectionBefore, projectionAfter)
	assert.Equal(t, after.Content, projectionAfter.Conversation.LastMessagePreview, "actual post-commit mirror must succeed under unchanged facts")
	assert.Equal(t, counts, mergedLegacyProjectionCounts(t, live, account.OrganizationID))
	assert.Equal(t, *target, loadCoexistenceAdmissionContact(t, live, account.OrganizationID, target.ID))
	var legacyAfter models.Contact
	require.NoError(t, live.Unscoped().First(&legacyAfter, legacy.ID).Error)
	assert.Equal(t, legacy, legacyAfter, "the proof cannot rewrite the retained alias")
}

func TestMergedLegacyProjectionTargetOwnedIdentityStillUsesHistoricalProof(t *testing.T) {
	for _, owned := range []string{"direct", "accepted_parent"} {
		t.Run(owned, func(t *testing.T) {
			app, account, target, legacy, original, direct := mergedLegacyAliasFixture(t)
			parent, stored := "", direct
			if owned == "accepted_parent" {
				parent = "US." + testutil.NewTestGraphObjectID()[:17]
				require.NotEqual(t, direct, parent)
				stored = parent
			}
			require.NoError(t, app.DB.Model(target).Update("bs_uid", stored).Error)
			targetBefore := loadCoexistenceAdmissionContact(t, app.DB, account.OrganizationID, target.ID)
			identity := coexistenceContactIdentity{UserID: direct, ParentUserID: parent}
			needsAlias, err := app.validateWhatsAppMessageContactIdentity(account.OrganizationID, &targetBefore, identity, true)
			require.NoError(t, err)
			require.False(t, needsAlias, "ordinary direct/parent ownership already matches; the old joined keys must independently trigger full historical proof")
			before := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
			require.NotEqual(t, "legacy-contact:"+target.ID.String(), before.Conversation.ExternalConversationID)
			lookup := mergedLegacyAliasLookup(original, direct)
			lookup.Identity.ParentUserID = parent
			resolved, err := app.resolveWhatsAppMessage(account, lookup)
			require.NoError(t, err)
			require.True(t, resolved.LegacyAliasMatched)
			require.NotNil(t, resolved.LegacyMergedProjection)
			assert.Equal(t, before.Conversation.ID, resolved.LegacyMergedProjection.ConversationID)
			assert.Equal(t, before.Identity.ID, resolved.LegacyMergedProjection.IdentityID)
			assert.Equal(t, before.Participant.ID, resolved.LegacyMergedProjection.ParticipantID)
			assert.Equal(t, direct, resolved.LegacyMergedProjection.DirectBSUID)
			assert.Equal(t, parent, resolved.LegacyMergedProjection.ParentBSUID)
			counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
			for _, kind := range []string{"replay", "edit", "revoke"} {
				event := mergedLegacyAliasEvent(t, direct, original.WhatsAppMessageID, kind)
				event.ToParentUserID = parent
				body := mergedLegacyAliasWebhookBody(t, account, event)
				for repeat := 0; repeat < 2; repeat++ {
					require.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, body), kind)
					app.WaitForBackgroundTasks()
				}
				after := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
				assertMergedLegacyProjectionIdentity(t, before, after)
				assert.Equal(t, after.Message.Content, after.Conversation.LastMessagePreview, "post-commit historical mirror must actually succeed")
				if kind == "edit" {
					assert.Equal(t, "edited legacy body", after.Message.Content)
				}
				if kind == "revoke" {
					assert.Equal(t, "[Message deleted from WhatsApp Business App]", after.Message.Content)
				}
				assert.Equal(t, counts, mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID))
				targetAfter := loadCoexistenceAdmissionContact(t, app.DB, account.OrganizationID, target.ID)
				assert.Equal(t, targetBefore.BSUID, targetAfter.BSUID, "historical proof must not overwrite the accepted direct or parent identity")
				assert.Equal(t, targetBefore.PhoneNumber, targetAfter.PhoneNumber)
				assert.Equal(t, targetBefore.Metadata, targetAfter.Metadata)
			}
			var legacyAfter models.Contact
			require.NoError(t, app.DB.Unscoped().First(&legacyAfter, legacy.ID).Error)
			assert.Equal(t, legacy, legacyAfter)
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.WhatsAppIdentityReviewHold{}, "organization_id = ?", account.OrganizationID))
			assert.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.InboundEvent{}, "organization_id = ?", account.OrganizationID))
		})
	}
}

func TestMergedLegacyProjectionFollowsRealMultiHopMergeHistory(t *testing.T) {
	app, account, middle, legacy, original, bsuid := mergedLegacyAliasFixture(t)
	before := loadMergedLegacyProjection(t, app.DB, account, original.ID, middle.ID, legacy.ID)
	target := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	mergeContactsForTest(t, app, account.OrganizationID, target.ID, middle.ID)
	app.WaitForBackgroundTasks()
	var sourceAfter, middleAfter models.Contact
	require.NoError(t, app.DB.Unscoped().First(&sourceAfter, legacy.ID).Error)
	require.NoError(t, app.DB.Unscoped().First(&middleAfter, middle.ID).Error)
	require.Equal(t, &target.ID, sourceAfter.MergedIntoID, "current pointer is compressed by the second real merge")
	require.Equal(t, &target.ID, middleAfter.MergedIntoID)
	for _, edge := range []struct {
		source models.Contact
		target uuid.UUID
	}{{sourceAfter, middle.ID}, {middleAfter, target.ID}} {
		var facts []models.CustomerActivityEvent
		require.NoError(t, app.DB.Where("organization_id = ? AND source_object_id = ? AND event_type = ?", account.OrganizationID, edge.source.ID, models.CustomerActivityContactMerged).Find(&facts).Error)
		require.Len(t, facts, 1)
		require.Equal(t, edge.source.ID.String(), facts[0].Metadata["source_contact_id"])
		require.Equal(t, edge.target, facts[0].ContactID)
		require.Equal(t, edge.target.String(), facts[0].Metadata["target_contact_id"])
		require.Equal(t, edge.source.MergedByID, facts[0].ActorUserID)
		require.NotNil(t, edge.source.MergedAt)
		require.True(t, facts[0].OccurredAt.Equal(*edge.source.MergedAt))
	}
	counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
	for _, kind := range []string{"replay", "edit", "revoke"} {
		body := mergedLegacyAliasWebhookBody(t, account, mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, kind))
		for i := 0; i < 2; i++ {
			require.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, body))
			app.WaitForBackgroundTasks()
		}
		after := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
		assertMergedLegacyProjectionIdentity(t, before, after)
		assert.Equal(t, after.Message.Content, after.Conversation.LastMessagePreview)
		assert.Equal(t, counts, mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID))
	}
}

func TestMergedLegacyProjectionRefusesAlteredJoinedRows(t *testing.T) {
	variants := []string{"conversation_key", "identity_key", "conversation_metadata", "identity_metadata_type", "participant_key", "participant_external", "participant_metadata", "participant_identity_nil", "participant_left", "participant_deleted", "conversation_deleted", "identity_deleted", "duplicate_customer", "same_family_wrong_root", "wrong_native_binding", "wrong_provider"}
	for _, variant := range variants {
		t.Run(variant, func(t *testing.T) {
			app, account, target, legacy, original, bsuid := mergedLegacyAliasFixture(t)
			projection := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
			wrongKey := "legacy-contact:" + target.ID.String()
			switch variant {
			case "conversation_key":
				require.NoError(t, app.DB.Model(&models.InboxConversation{}).Where("id = ?", projection.Conversation.ID).Update("external_conversation_id", wrongKey).Error)
			case "identity_key":
				require.NoError(t, app.DB.Model(&models.ContactIdentity{}).Where("id = ?", projection.Identity.ID).Update("external_id", wrongKey).Error)
			case "conversation_metadata":
				require.NoError(t, app.DB.Model(&models.InboxConversation{}).Where("id = ?", projection.Conversation.ID).Update("metadata", models.JSONB{"legacy_contact_id": target.ID.String()}).Error)
			case "identity_metadata_type":
				require.NoError(t, app.DB.Model(&models.ContactIdentity{}).Where("id = ?", projection.Identity.ID).Update("metadata", models.JSONB{"legacy_contact_id": []string{legacy.ID.String()}}).Error)
			case "participant_key", "participant_external", "participant_metadata", "participant_identity_nil", "participant_left", "participant_deleted":
				values := map[string]any{}
				switch variant {
				case "participant_key":
					values["participant_key"] = wrongKey
				case "participant_external":
					values["external_id"] = wrongKey
				case "participant_metadata":
					values["metadata"] = models.JSONB{"legacy_contact_id": target.ID.String()}
				case "participant_identity_nil":
					values["contact_identity_id"] = nil
				case "participant_left":
					values["left_at"] = time.Now().UTC()
				case "participant_deleted":
					values["deleted_at"] = time.Now().UTC()
				}
				require.NoError(t, app.DB.Model(&models.ConversationParticipant{}).Where("id = ?", projection.Participant.ID).Updates(values).Error)
			case "conversation_deleted":
				require.NoError(t, app.DB.Delete(&models.InboxConversation{}, projection.Conversation.ID).Error)
			case "identity_deleted":
				require.NoError(t, app.DB.Delete(&models.ContactIdentity{}, projection.Identity.ID).Error)
			case "duplicate_customer":
				duplicate := projection.Participant
				duplicate.BaseModel = models.BaseModel{ID: uuid.New()}
				duplicate.ParticipantKey = "different-key:" + uuid.NewString()
				require.NoError(t, app.DB.Create(&duplicate).Error, "distinct participant key is schema-valid; customer ambiguity still refuses")
			case "same_family_wrong_root":
				other := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
				mergeContactsForTest(t, app, account.OrganizationID, target.ID, other.ID)
				key := "legacy-contact:" + other.ID.String()
				metadata := models.JSONB{"legacy_contact_id": other.ID.String()}
				require.NoError(t, app.DB.Model(&models.InboxConversation{}).Where("id = ?", projection.Conversation.ID).Updates(map[string]any{"external_conversation_id": key, "metadata": metadata}).Error)
				require.NoError(t, app.DB.Model(&models.ContactIdentity{}).Where("id = ?", projection.Identity.ID).Updates(map[string]any{"external_id": key, "metadata": metadata}).Error)
				require.NoError(t, app.DB.Model(&models.ConversationParticipant{}).Where("id = ?", projection.Participant.ID).Updates(map[string]any{"external_id": key, "participant_key": key, "metadata": metadata}).Error)
			case "wrong_native_binding":
				other := testutil.CreateTestWhatsAppAccount(t, app.DB, account.OrganizationID)
				require.NoError(t, app.DB.Model(&models.ChannelAccount{}).Where("id = ?", projection.Account.ID).Update("metadata", models.JSONB{"legacy_account_id": other.ID.String()}).Error)
			case "wrong_provider":
				require.NoError(t, app.DB.Model(&models.ChannelAccount{}).Where("id = ?", projection.Account.ID).Update("provider", "different_provider").Error)
			}
			counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
			body := mergedLegacyAliasWebhookBody(t, account, mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "revoke"))
			require.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, body))
			app.WaitForBackgroundTasks()
			var after models.Message
			require.NoError(t, app.DB.First(&after, original.ID).Error)
			assert.Equal(t, original, after)
			assert.Equal(t, counts, mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID))
		})
	}
}

func TestMergedLegacyProjectionTreatsEvidenceAsUntrusted(t *testing.T) {
	for _, variant := range []string{"org", "account", "message", "canonical", "legacy", "conversation", "identity", "participant", "wamid", "direct", "parent"} {
		t.Run(variant, func(t *testing.T) {
			app, account, target, legacy, original, bsuid := mergedLegacyAliasFixture(t)
			projection := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
			evidence := mergedLegacyProjectionEvidence(account, legacy, projection, bsuid)
			var budget channelapi.LegacyMergedProjectionBudget
			bounded, cancel, err := budget.Scope(app.DB)
			require.NoError(t, err)
			defer cancel()
			valid, err := channelapi.ValidateLegacyMergedProjection(bounded, mergedLegacyAccountRef(account), &original, &evidence)
			require.NoError(t, err, "control: valid evidence must pass before a single selector is altered")
			require.NotNil(t, valid)
			switch variant {
			case "org":
				evidence.OrganizationID = uuid.New()
			case "account":
				evidence.AccountID = uuid.New()
			case "message":
				evidence.MessageID = uuid.New()
			case "canonical":
				evidence.CanonicalContactID = uuid.New()
			case "legacy":
				evidence.LegacyContactID = uuid.New()
			case "conversation":
				evidence.ConversationID = uuid.New()
			case "identity":
				evidence.IdentityID = uuid.New()
			case "participant":
				evidence.ParticipantID = uuid.New()
			case "wamid":
				evidence.WAMID = "wamid.foreign-" + uuid.NewString()
			case "direct":
				evidence.DirectBSUID = "US." + testutil.NewTestGraphObjectID()[:17]
			case "parent":
				evidence.ParentBSUID = "malformed-parent"
			}
			counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
			proved, err := channelapi.ValidateLegacyMergedProjection(bounded, mergedLegacyAccountRef(account), &original, &evidence)
			require.Error(t, err)
			assert.Nil(t, proved)
			_, err = channelapi.MirrorLegacyMergedWhatsAppMessage(app.DB, mergedLegacyAccountRef(account), original.ID, &evidence)
			require.Error(t, err)
			after := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
			assertMergedLegacyProjectionIdentity(t, projection, after)
			assert.Equal(t, original, after.Message)
			assert.Equal(t, counts, mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID))
		})
	}
}

func TestMergedLegacyProjectionRefusesContradictoryAppendOnlyHistory(t *testing.T) {
	for _, variant := range []string{"duplicate", "wrong_type", "wrong_category", "wrong_source", "wrong_target", "wrong_actor", "wrong_time", "wrong_key", "metadata_type"} {
		t.Run(variant, func(t *testing.T) {
			app, account, target, legacy, original, bsuid := mergedLegacyAliasFixture(t)
			var fact models.CustomerActivityEvent
			require.NoError(t, app.DB.Where("organization_id = ? AND source_object_id = ? AND event_type = ?", account.OrganizationID, legacy.ID, models.CustomerActivityContactMerged).First(&fact).Error)
			// Append a distinct claim; never rewrite/delete/disable the immutable history.
			fact.ID, fact.CreatedAt, fact.IdempotencyKey = uuid.New(), time.Time{}, "contact-merge:test-conflict-"+uuid.NewString()
			fact.Metadata = models.JSONB{"source_contact_id": legacy.ID.String(), "target_contact_id": target.ID.String()}
			switch variant {
			case "wrong_type":
				fact.EventType = models.CustomerActivityMessageIncoming
			case "wrong_category":
				fact.Category = models.CustomerActivityCategoryMessage
			case "wrong_source":
				fact.SourceObjectType = "message"
			case "wrong_target":
				other := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
				fact.ContactID, fact.Metadata["target_contact_id"] = other.ID, other.ID.String()
			case "wrong_actor":
				fact.ActorType, fact.ActorUserID = models.CustomerActivityActorSystem, nil
			case "wrong_time":
				fact.OccurredAt = fact.OccurredAt.Add(time.Second)
			case "wrong_key":
				fact.IdempotencyKey = "unrelated-key:" + uuid.NewString()
			case "metadata_type":
				fact.Metadata["source_contact_id"] = []string{legacy.ID.String()}
			}
			require.NoError(t, app.DB.Create(&fact).Error)
			counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
			require.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app,
				mergedLegacyAliasWebhookBody(t, account, mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "edit"))))
			app.WaitForBackgroundTasks()
			var after models.Message
			require.NoError(t, app.DB.First(&after, original.ID).Error)
			assert.Equal(t, original, after)
			assert.Equal(t, counts, mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID))
		})
	}
}

func TestMergedLegacyProjectionRequiresInstalledHistoryAndUniqueKeyConstraints(t *testing.T) {
	app, account, target, legacy, original, _ := mergedLegacyAliasFixture(t)
	projection := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	var fact models.CustomerActivityEvent
	require.NoError(t, app.DB.Where("organization_id = ? AND source_object_id = ? AND event_type = ?", account.OrganizationID, legacy.ID, models.CustomerActivityContactMerged).First(&fact).Error)
	for _, operation := range []string{"update", "delete"} {
		err := app.DB.Transaction(func(tx *gorm.DB) error {
			if operation == "update" {
				return tx.Model(&models.CustomerActivityEvent{}).Where("id = ?", fact.ID).Update("title", "must refuse").Error
			}
			return tx.Delete(&models.CustomerActivityEvent{}, fact.ID).Error
		})
		require.ErrorContains(t, err, "append-only")
	}
	// Exact-key duplicates cannot be manufactured on the supported schema.
	// Exercise its real refusal rather than dropping an index to create a case.
	identity := projection.Identity
	identity.BaseModel = models.BaseModel{ID: uuid.New()}
	require.Error(t, app.DB.Create(&identity).Error)
	conversation := projection.Conversation
	conversation.BaseModel = models.BaseModel{ID: uuid.New()}
	require.Error(t, app.DB.Omit("Participants").Create(&conversation).Error)
	participant := projection.Participant
	participant.BaseModel = models.BaseModel{ID: uuid.New()}
	require.Error(t, app.DB.Create(&participant).Error)
	var after models.CustomerActivityEvent
	require.NoError(t, app.DB.First(&after, fact.ID).Error)
	assert.Equal(t, fact, after)
}

func TestMergedLegacyProjectionRejectsRawMergeWithoutTypedHistory(t *testing.T) {
	app, account, target, legacy, original, bsuid := mergedLegacyAliasFixtureWithMerge(t,
		func(t *testing.T, app *App, organizationID, targetID, sourceID uuid.UUID) {
			// This deliberately models a historical raw SQL shape, not a legitimate
			// MergeContact. No history trigger/index is disabled or existing fact edited.
			actor := testutil.CreateTestUser(t, app.DB, organizationID)
			now := time.Now().UTC()
			require.NoError(t, app.DB.Transaction(func(tx *gorm.DB) error {
				for _, model := range []any{&models.Message{}, &models.ContactIdentity{}, &models.InboxConversation{}} {
					if err := tx.Model(model).Where("organization_id = ? AND contact_id = ?", organizationID, sourceID).Update("contact_id", targetID).Error; err != nil {
						return err
					}
				}
				return tx.Model(&models.Contact{}).Where("id = ? AND organization_id = ?", sourceID, organizationID).
					Updates(map[string]any{"merged_into_id": targetID, "merged_at": now, "merged_by_id": actor.ID, "deleted_at": now}).Error
			}))
		})
	require.Zero(t, countCoexistenceAdmissionRows(t, app.DB, &models.CustomerActivityEvent{}, "organization_id = ? AND source_object_id = ? AND event_type = ?", account.OrganizationID, legacy.ID, models.CustomerActivityContactMerged))
	before := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
	require.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app,
		mergedLegacyAliasWebhookBody(t, account, mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "edit"))))
	app.WaitForBackgroundTasks()
	after := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	assert.Equal(t, before.Message, after.Message)
	assert.Equal(t, before.Conversation, after.Conversation)
	assert.Equal(t, counts, mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID))
}

func TestMergedLegacyProjectionRejectsChangedAliasMergeFacts(t *testing.T) {
	for _, variant := range []string{"missing_time", "changed_time", "missing_actor", "changed_actor"} {
		t.Run(variant, func(t *testing.T) {
			app, account, _, legacy, original, bsuid := mergedLegacyAliasFixture(t)
			values := map[string]any{}
			switch variant {
			case "missing_time":
				values["merged_at"] = nil
			case "changed_time":
				require.NotNil(t, legacy.MergedAt)
				values["merged_at"] = legacy.MergedAt.Add(time.Second)
			case "missing_actor":
				values["merged_by_id"] = nil
			case "changed_actor":
				values["merged_by_id"] = testutil.CreateTestUser(t, app.DB, account.OrganizationID).ID
			}
			require.NoError(t, app.DB.Unscoped().Model(&models.Contact{}).Where("id = ?", legacy.ID).Updates(values).Error)
			require.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app,
				mergedLegacyAliasWebhookBody(t, account, mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "revoke"))))
			app.WaitForBackgroundTasks()
			var after models.Message
			require.NoError(t, app.DB.First(&after, original.ID).Error)
			assert.Equal(t, original, after)
		})
	}
}

func TestMergedLegacyProjectionHistoricalDepthUsesImmutableEdges(t *testing.T) {
	for _, totalNodes := range []int{16, 17} {
		t.Run(fmt.Sprintf("nodes=%d", totalNodes), func(t *testing.T) {
			app, account, target, legacy, original, bsuid := mergedLegacyAliasFixture(t)
			before := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
			for nodes := 2; nodes < totalNodes; nodes++ {
				next := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
				mergeContactsForTest(t, app, account.OrganizationID, next.ID, target.ID)
				target = next
			}
			app.WaitForBackgroundTasks()
			// All current pointers are flat; only the typed historical chain is long.
			var source models.Contact
			require.NoError(t, app.DB.Unscoped().First(&source, legacy.ID).Error)
			require.Equal(t, &target.ID, source.MergedIntoID)
			counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
			body := mergedLegacyAliasWebhookBody(t, account, mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "edit"))
			status := signedWebhookStatusCode(t, app, body)
			app.WaitForBackgroundTasks()
			after := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
			if totalNodes == 16 {
				require.Equal(t, http.StatusOK, status)
				assert.Equal(t, "edited legacy body", after.Message.Content)
				assert.Equal(t, after.Message.Content, after.Conversation.LastMessagePreview)
			} else {
				require.Equal(t, http.StatusServiceUnavailable, status)
				assert.Equal(t, original.Content, after.Message.Content)
			}
			assertMergedLegacyProjectionIdentity(t, before, after)
			assert.Equal(t, counts, mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID))
		})
	}
}

func TestMergedLegacyProjectionSelectiveJoinedQueryFailureRollsBackBatch(t *testing.T) {
	for _, table := range []string{"customer_activity_events", "inbox_conversations", "contact_identities", "conversation_participants"} {
		t.Run(table, func(t *testing.T) {
			app, account, _, _, original, bsuid := mergedLegacyAliasFixture(t)
			phone, firstWAMID := coexistenceAdmissionTestPhone(), "wamid.projection-fault-"+uuid.NewString()
			persistCoexistenceAdmissionEcho(t, app, account, firstWAMID, phone, "", "earlier accepted body")
			app.WaitForBackgroundTasks()
			var first models.Message
			require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, firstWAMID).First(&first).Error)
			require.NotNil(t, first.InboxConversationID)
			live := app.DB
			pool, err := live.DB()
			require.NoError(t, err)
			isolated, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			var hits atomic.Int32
			require.NoError(t, isolated.Callback().Query().Before("gorm:query").Register("test:merged-projection-selective-query", func(tx *gorm.DB) {
				callbacks.BuildQuerySQL(tx)
				sql := tx.Statement.SQL.String()
				if !strings.Contains(sql, table) || !strings.Contains(sql, "legacy_proof_oversize") {
					return
				}
				// Fail only when the second message's historical proof is being
				// resolved; a prior same-batch edit must already be staged.
				var staged models.Message
				probe := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true})
				if err := probe.Raw("SELECT * FROM messages WHERE id = ?", first.ID).Scan(&staged).Error; err != nil {
					_ = tx.AddError(err)
					return
				}
				if staged.Content != "edited legacy body" {
					return
				}
				hits.Add(1)
				tx.Statement.SQL.Reset()
				tx.Statement.SQL.WriteString("SELECT 1 / 0")
				tx.Statement.Vars = nil
			}))
			app.DB = isolated
			defer func() { app.DB = live }()
			var one int
			require.NoError(t, isolated.Raw("SELECT 1").Scan(&one).Error)
			body := mergedLegacyAliasWebhookBody(t, account,
				mergedLegacyAliasEvent(t, phone, firstWAMID, "edit"), mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "edit"))
			require.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, body))
			require.Positive(t, hits.Load(), "must fail the selected historical/projection query after the earlier edit")
			app.WaitForBackgroundTasks()
			var afterFirst, afterOriginal models.Message
			require.NoError(t, live.First(&afterFirst, first.ID).Error)
			require.NoError(t, live.First(&afterOriginal, original.ID).Error)
			assert.Equal(t, first, afterFirst)
			assert.Equal(t, original, afterOriginal)
			require.NoError(t, live.Raw("SELECT 1").Scan(&one).Error)
		})
	}
}

func TestMergedLegacyProjectionBudgetIsSharedAndClipped(t *testing.T) {
	app, account, target, legacy, original, bsuid := mergedLegacyAliasFixture(t)
	var budget channelapi.LegacyMergedProjectionBudget
	first, cancelFirst, err := budget.Scope(app.DB)
	require.NoError(t, err)
	defer cancelFirst()
	firstDeadline, ok := first.Statement.Context.Deadline()
	require.True(t, ok)
	assert.False(t, firstDeadline.After(time.Now().Add(5*time.Second)), "five seconds covers the whole proof, not each graph node")
	second, cancelSecond, err := budget.Scope(app.DB)
	require.NoError(t, err)
	defer cancelSecond()
	secondDeadline, ok := second.Statement.Context.Deadline()
	require.True(t, ok)
	assert.Equal(t, firstDeadline, secondDeadline, "recursive/locked proof cannot reset the deadline")
	ctx, cancelParent := context.WithDeadline(context.Background(), time.Now().Add(2*time.Second))
	defer cancelParent()
	var clipped channelapi.LegacyMergedProjectionBudget
	db, cancelClipped, err := clipped.Scope(app.DB.WithContext(ctx))
	require.NoError(t, err)
	defer cancelClipped()
	deadline, ok := db.Statement.Context.Deadline()
	require.True(t, ok)
	want, _ := ctx.Deadline()
	assert.Equal(t, want, deadline)
	cancelParent()
	projection := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	evidence := mergedLegacyProjectionEvidence(account, legacy, projection, bsuid)
	proved, err := channelapi.ValidateLegacyMergedProjection(db, mergedLegacyAccountRef(account), &original, &evidence)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, proved)
}

func TestMergedLegacyUnlinkedReplayDoesNotCreateHistoricalProjection(t *testing.T) {
	app, account, target := whatsappIdentityFixture(t)
	phone, bsuid := coexistenceAdmissionTestPhone(), "US."+testutil.NewTestGraphObjectID()[:17]
	wamid := "wamid.legacy-unlinked-" + uuid.NewString()
	var stored *models.Message
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		if err := scoped.prepareWhatsAppMessageAuthority(account); err != nil {
			return err
		}
		var err error
		stored, _, err = scoped.persistCoexistenceMessage(account, coexistenceContactIdentity{Phone: phone},
			CoexistenceMessage{IncomingTextMessage: inboundContinuationTextMessage(t, wamid, coexistenceAdmissionBusinessPhone, "original legacy body")},
			models.DirectionOutgoing, models.MessageStatusSent, models.JSONB{"coexistence_source": "echo"}, false)
		return err
	}))
	require.NotNil(t, stored)
	app.WaitForBackgroundTasks()
	var original models.Message
	require.NoError(t, app.DB.First(&original, stored.ID).Error)
	require.Nil(t, original.InboxConversationID, "emit=false must actually produce the historical unlinked shape")
	legacyID := original.ContactID
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", legacyID).Update("phone_number", bsuid).Error)
	mergeContactsForTest(t, app, account.OrganizationID, target.ID, legacyID)
	counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
	for _, kind := range []string{"replay", "edit", "revoke"} {
		event := mergedLegacyAliasEvent(t, bsuid, wamid, kind)
		for repeat := 0; repeat < 2; repeat++ {
			require.NoError(t, app.persistMessageEchoesBeforeAck(account.PhoneID, []CoexistenceMessage{event}, nil))
			app.WaitForBackgroundTasks()
		}
		var after models.Message
		require.NoError(t, app.DB.First(&after, original.ID).Error)
		assert.Nil(t, after.InboxConversationID, "corroboration cannot establish a historical link")
		assert.Equal(t, target.ID, after.ContactID)
		assert.Equal(t, original.EffectiveIngestedAt(), after.EffectiveIngestedAt())
		if kind == "edit" {
			assert.Equal(t, "edited legacy body", after.Content)
		}
		if kind == "revoke" {
			assert.Equal(t, "[Message deleted from WhatsApp Business App]", after.Content)
		}
		assert.Equal(t, counts, mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID))
	}
}

func TestMergedLegacyProjectionAfterCommitDriftDoesNotReplayBusinessMutation(t *testing.T) {
	app, account, target, legacy, original, bsuid := mergedLegacyAliasFixture(t)
	projection := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	evidence := mergedLegacyProjectionEvidence(account, legacy, projection, bsuid)
	counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
	reader := testutil.CreateTestUser(t, app.DB, account.OrganizationID)
	var advancedRead models.ConversationRead
	var readAdvanced bool
	live := app.DB
	pool, err := live.DB()
	require.NoError(t, err)
	isolated, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var armed, reached atomic.Bool
	atMirror, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	require.NoError(t, isolated.Callback().Update().After("gorm:update").Register("test:arm-after-business-edit", func(tx *gorm.DB) {
		if tx.Error == nil && tx.RowsAffected > 0 && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "messages" {
			armed.Store(true)
		}
	}))
	require.NoError(t, isolated.Callback().Row().Before("gorm:row").Register("test:before-fresh-merged-mirror-fence", func(tx *gorm.DB) {
		if !armed.Load() || tx.Statement.Table != "whatsapp_accounts" || tx.Statement.Context == nil ||
			tx.Statement.Context.Value(whatsAppMessageAuthorityKey{}) != nil {
			return
		}
		callbacks.BuildQuerySQL(tx)
		matchesAccount := false
		for _, value := range tx.Statement.Vars {
			if fmt.Sprint(value) == account.ID.String() {
				matchesAccount = true
			}
		}
		if !matchesAccount || !reached.CompareAndSwap(false, true) {
			return
		}
		// verifiedLegacyMetaAccountRef reads the account before the fresh
		// mirror takes the org/shadow fence. Its actual Table(...).Scan runs
		// the Row callback, not the Query callback. The business commit is over.
		close(atMirror)
		select {
		case <-release:
		case <-ctx.Done():
			_ = tx.AddError(ctx.Err())
		}
	}))
	event := mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "edit")
	app.DB = isolated.WithContext(ctx)
	result := make(chan error, 1)
	joined := false
	defer func() {
		// Fatal assertions must not restore the shared receiver while this
		// test's owned persistence goroutine is still using it.
		unblock()
		cancel()
		if !joined {
			drain := time.NewTimer(2 * time.Second)
			defer drain.Stop()
			select {
			case <-result:
				joined = true
			case <-drain.C:
				t.Error("owned persistence goroutine did not stop within the bounded cancellation drain")
			}
		}
		if joined {
			app.DB = live
		}
	}()
	go func() { result <- app.persistMessageEchoesBeforeAck(account.PhoneID, []CoexistenceMessage{event}, nil) }()
	select {
	case <-atMirror:
	case err := <-result:
		joined = true
		t.Fatalf("missing post-commit mirror barrier: %v", err)
	case <-ctx.Done():
		t.Fatal("mirror did not reach its pre-fence read")
	}
	var committed models.Message
	require.NoError(t, live.WithContext(ctx).First(&committed, original.ID).Error)
	require.Equal(t, "edited legacy body", committed.Content, "business edit must be committed before concurrent bridge write")
	require.Equal(t, original.InboxConversationID, committed.InboxConversationID)
	require.NoError(t, live.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := channelapi.LockLegacyMetaOrganizationAndShadow(tx, account.OrganizationID, projection.Account.ID); err != nil {
			return err
		}
		var native models.WhatsAppAccount
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ? AND organization_id = ?", account.ID, account.OrganizationID).First(&native).Error; err != nil {
			return err
		}
		var identity models.ContactIdentity
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ?", projection.Identity.ID, account.OrganizationID).First(&identity).Error; err != nil {
			return err
		}
		var conversation models.InboxConversation
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ?", projection.Conversation.ID, account.OrganizationID).First(&conversation).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.ConversationParticipant{}).Where("id = ? AND organization_id = ?", projection.Participant.ID, account.OrganizationID).
			Update("metadata", models.JSONB{"legacy_contact_id": target.ID.String()}).Error; err != nil {
			return err
		}
		var err error
		advancedRead, readAdvanced, err = advanceConversationReadCursor(tx, account.OrganizationID, reader.ID, projection.Conversation.ID, &committed, time.Now().UTC())
		return err
	}))
	require.True(t, readAdvanced, "concurrent reader must actually advance under the existing conversation/message lock order")
	require.NoError(t, live.WithContext(ctx).First(&advancedRead, advancedRead.ID).Error)
	unblock()
	select {
	case err := <-result:
		joined = true
		require.NoError(t, err, "best-effort mirror failure does not undo accepted business truth")
	case <-ctx.Done():
		t.Fatal("mirror failed to release after the concurrent bridge commit")
	}
	app.WaitForBackgroundTasks()
	var after models.Message
	var conversation models.InboxConversation
	require.NoError(t, live.First(&after, original.ID).Error)
	require.NoError(t, live.First(&conversation, projection.Conversation.ID).Error)
	assert.Equal(t, committed, after, "no replay, rollback, relink or additional content mutation")
	assert.Equal(t, projection.Conversation.LastMessagePreview, conversation.LastMessagePreview, "unproven mirror must refuse its preview write")
	assert.Equal(t, counts, mergedLegacyProjectionCounts(t, live, account.OrganizationID))
	var readAfter models.ConversationRead
	require.NoError(t, live.First(&readAfter, advancedRead.ID).Error)
	assert.Equal(t, advancedRead, readAfter, "mirror refusal cannot reset or move the concurrent read cursor")
	_, err = channelapi.MirrorLegacyMergedWhatsAppMessage(live.WithContext(ctx), mergedLegacyAccountRef(account), original.ID, &evidence)
	require.Error(t, err, "the unchanged captured hint must independently refuse the drift, not authorize repair")
}

func TestMergedLegacyProjectionInFlightDeadlineRollsBackBatch(t *testing.T) {
	app, account, _, _, original, bsuid := mergedLegacyAliasFixture(t)
	phone, firstWAMID := coexistenceAdmissionTestPhone(), "wamid.projection-deadline-"+uuid.NewString()
	persistCoexistenceAdmissionEcho(t, app, account, firstWAMID, phone, "", "earlier accepted body")
	app.WaitForBackgroundTasks()
	var first models.Message
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, firstWAMID).First(&first).Error)
	live := app.DB
	pool, err := live.DB()
	require.NoError(t, err)
	isolated, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	var hits atomic.Int32
	var deadlineObserved atomic.Bool
	var queryStarted atomic.Int64
	require.NoError(t, isolated.Callback().Query().Before("gorm:query").Register("test:merged-history-real-driver-deadline", func(tx *gorm.DB) {
		callbacks.BuildQuerySQL(tx)
		sql := tx.Statement.SQL.String()
		if !strings.Contains(sql, "customer_activity_events") || !strings.Contains(sql, "contact-merge:%") {
			return
		}
		var staged models.Message
		if err := tx.Session(&gorm.Session{NewDB: true}).Raw("SELECT * FROM messages WHERE id = ?", first.ID).Scan(&staged).Error; err != nil {
			_ = tx.AddError(err)
			return
		}
		if staged.Content != "edited legacy body" {
			return
		}
		hits.Add(1)
		deadline, ok := tx.Statement.Context.Deadline()
		now := time.Now()
		outerDeadline, _ := ctx.Deadline()
		deadlineObserved.Store(ok && !deadline.After(now.Add(5*time.Second)) && deadline.Before(outerDeadline))
		queryStarted.Store(now.UnixNano())
		// The statement actually enters PostgreSQL and outlives the complete
		// five-second proof budget. This is not only a pre-cancelled context.
		tx.Statement.SQL.Reset()
		tx.Statement.SQL.WriteString("SELECT pg_sleep(20)")
		tx.Statement.Vars = nil
	}))
	app.DB = isolated.WithContext(ctx)
	defer func() { app.DB = live }()
	body := mergedLegacyAliasWebhookBody(t, account,
		mergedLegacyAliasEvent(t, phone, firstWAMID, "edit"), mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "edit"))
	require.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app, body))
	require.Positive(t, hits.Load(), "the sleeping history query must execute after the earlier edit")
	require.True(t, deadlineObserved.Load(), "query must inherit the proof ceiling instead of only the longer outer request")
	assert.Less(t, time.Since(time.Unix(0, queryStarted.Load())), 8*time.Second, "one proof deadline must cancel the driver before the outer12-second bound")
	app.WaitForBackgroundTasks()
	var afterFirst, afterOriginal models.Message
	require.NoError(t, live.First(&afterFirst, first.ID).Error)
	require.NoError(t, live.First(&afterOriginal, original.ID).Error)
	assert.Equal(t, first, afterFirst)
	assert.Equal(t, original, afterOriginal)
	var one int
	require.NoError(t, live.Raw("SELECT 1").Scan(&one).Error, "rollback must leave the pool usable")
}

func TestMergedLegacyProjectionLockedRecheckDoesNotReuseParticipantProof(t *testing.T) {
	app, account, _, _, original, bsuid := mergedLegacyAliasFixture(t)
	live := app.DB
	pool, err := live.DB()
	require.NoError(t, err)
	isolated, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	var contactLocked atomic.Bool
	var initialParticipants, failedRechecks atomic.Int32
	require.NoError(t, isolated.Callback().Query().Before("gorm:query").Register("test:merged-participant-locked-recheck", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "contacts" {
			if _, locked := tx.Statement.Clauses["FOR"]; locked {
				contactLocked.Store(true)
			}
		}
		callbacks.BuildQuerySQL(tx)
		sql := tx.Statement.SQL.String()
		if !strings.Contains(sql, "conversation_participants") || !strings.Contains(sql, "legacy_proof_oversize") {
			return
		}
		if !contactLocked.Load() {
			initialParticipants.Add(1)
			return
		}
		failedRechecks.Add(1)
		tx.Statement.SQL.Reset()
		tx.Statement.SQL.WriteString("SELECT 1 / 0")
		tx.Statement.Vars = nil
	}))
	app.DB = isolated
	defer func() { app.DB = live }()
	require.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app,
		mergedLegacyAliasWebhookBody(t, account, mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "revoke"))))
	require.Positive(t, initialParticipants.Load(), "initial participant proof must have succeeded")
	require.Positive(t, failedRechecks.Load(), "participant authority must be reread after the canonical contact lock")
	app.WaitForBackgroundTasks()
	var after models.Message
	require.NoError(t, live.First(&after, original.ID).Error)
	assert.Equal(t, original, after)
}

func TestMergedLegacyProjectionRefusesMissingGuardObservation(t *testing.T) {
	app, account, _, _, original, bsuid := mergedLegacyAliasFixture(t)
	live := app.DB
	pool, err := live.DB()
	require.NoError(t, err)
	isolated, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	var hits atomic.Int32
	require.NoError(t, isolated.Callback().Row().Before("gorm:row").Register("test:merged-history-guard-observation", func(tx *gorm.DB) {
		if !strings.Contains(tx.Statement.SQL.String(), "trg_customer_activity_events_append_only") {
			return
		}
		hits.Add(1)
		// Simulate an absent/disabled observation without disabling the actual
		// shared append-only guard or changing any schema object.
		tx.Statement.SQL.Reset()
		tx.Statement.SQL.WriteString("SELECT false")
		tx.Statement.Vars = nil
	}))
	app.DB = isolated
	defer func() { app.DB = live }()
	require.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app,
		mergedLegacyAliasWebhookBody(t, account, mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "edit"))))
	require.Positive(t, hits.Load())
	app.WaitForBackgroundTasks()
	var after models.Message
	require.NoError(t, live.First(&after, original.ID).Error)
	assert.Equal(t, original, after)
	var guarded bool
	require.NoError(t, live.Raw("SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = to_regclass('customer_activity_events') AND tgname = 'trg_customer_activity_events_append_only' AND tgenabled IN ('O','A'))").Scan(&guarded).Error)
	require.True(t, guarded, "the real guard remains installed and enabled")
}

func TestMergedLegacyProjectionSignedMediaEditRetainsCurrentIdentityAndRevision(t *testing.T) {
	app, account, target, legacy, original, bsuid := mergedLegacyAliasFixture(t)
	before := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	mediaID := testutil.NewTestGraphObjectID()
	edit := coexistenceAdmissionEchoEvent(t, `"to":"`+bsuid+`","id":"wamid.legacy-image-edit-`+uuid.NewString()+`","type":"edit","edit":{"original_message_id":"`+original.WhatsAppMessageID+`","message":{"type":"image","image":{"id":"`+mediaID+`","mime_type":"image/png","caption":"edited legacy image"}}}`)
	editBody := mergedLegacyAliasWebhookBody(t, account, edit)
	for repeat := 0; repeat < 2; repeat++ {
		require.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, editBody))
		app.WaitForBackgroundTasks()
	}
	current := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	require.Equal(t, models.MessageTypeImage, current.Message.MessageType)
	require.Equal(t, "edited legacy image", current.Message.Content)
	require.Equal(t, mediaID, current.Message.Metadata[coexistenceMediaProviderIDMetadataKey])
	require.Equal(t, current.Message.Content, current.Conversation.LastMessagePreview)
	assertMergedLegacyProjectionIdentity(t, before, current)
	var jobs []models.ScheduledJob
	require.NoError(t, app.DB.Where("organization_id = ? AND kind = ? AND aggregate_id = ?", account.OrganizationID, coexistenceMediaHydrationJobKind, original.ID).Find(&jobs).Error)
	require.Len(t, jobs, 1, "the current media revision is queued once; no worker is executed by this fixture")
	jobID := jobs[0].ID
	counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
	mediaReplay := coexistenceAdmissionEchoEvent(t, `"to":"`+bsuid+`","id":"`+original.WhatsAppMessageID+`","type":"image","image":{"id":"`+mediaID+`","mime_type":"image/png","caption":"older replay caption"}`)
	for _, replay := range []CoexistenceMessage{mediaReplay, mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "replay")} {
		for repeat := 0; repeat < 2; repeat++ {
			require.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, mergedLegacyAliasWebhookBody(t, account, replay)))
			app.WaitForBackgroundTasks()
		}
		after := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
		assertMergedLegacyProjectionIdentity(t, before, after)
		assert.Equal(t, models.MessageTypeImage, after.Message.MessageType)
		assert.Equal(t, "edited legacy image", after.Message.Content, "old original/media replays have no revision authority")
		assert.Equal(t, mediaID, after.Message.Metadata[coexistenceMediaProviderIDMetadataKey])
		assert.Equal(t, after.Message.Content, after.Conversation.LastMessagePreview)
		assert.Equal(t, counts, mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID))
		jobs = nil
		require.NoError(t, app.DB.Where("organization_id = ? AND kind = ? AND aggregate_id = ?", account.OrganizationID, coexistenceMediaHydrationJobKind, original.ID).Find(&jobs).Error)
		require.Len(t, jobs, 1)
		assert.Equal(t, jobID, jobs[0].ID)
	}
	// Supplying current signed-event identity is not a blanket worker grant.
	// The generic no-identity helper and continuation resolver still refuse.
	require.Error(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		if err := scoped.prepareWhatsAppMessageAuthority(account); err != nil {
			return err
		}
		return scoped.ensureProvenCoexistenceMediaJob(account, &current.Message)
	}))
	lookup := mergedLegacyAliasLookup(current.Message, bsuid)
	lookup.continuationValidation = true
	_, err := app.resolveWhatsAppMessage(account, lookup)
	require.Error(t, err)
	assert.Equal(t, counts, mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID))
	revoke := mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "revoke")
	for repeat := 0; repeat < 2; repeat++ {
		require.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, mergedLegacyAliasWebhookBody(t, account, revoke)))
		app.WaitForBackgroundTasks()
	}
	after := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	assertMergedLegacyProjectionIdentity(t, before, after)
	assert.Equal(t, "[Message deleted from WhatsApp Business App]", after.Message.Content)
	assert.Equal(t, after.Message.Content, after.Conversation.LastMessagePreview)
	assert.Empty(t, after.Message.MediaURL)
}

func TestMergedLegacyProjectionMirrorNeverRevivesDeletedShadow(t *testing.T) {
	app, account, target, legacy, original, bsuid := mergedLegacyAliasFixture(t)
	projection := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	evidence := mergedLegacyProjectionEvidence(account, legacy, projection, bsuid)
	duplicate := projection.Account
	duplicate.BaseModel = models.BaseModel{ID: uuid.New()}
	duplicate.Name = "duplicate-shadow-" + uuid.NewString()
	require.ErrorContains(t, app.DB.Create(&duplicate).Error, "uq_channel_accounts_org_external_active", "exact shadow external-key uniqueness must remain installed")
	require.NoError(t, app.DB.Delete(&models.ChannelAccount{}, projection.Account.ID).Error)
	var deleted models.ChannelAccount
	require.NoError(t, app.DB.Unscoped().First(&deleted, projection.Account.ID).Error)
	require.True(t, deleted.DeletedAt.Valid)
	counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
	shadowCount := countCoexistenceAdmissionRows(t, app.DB, &models.ChannelAccount{}, "organization_id = ?", account.OrganizationID)
	_, err := channelapi.MirrorLegacyMergedWhatsAppMessage(app.DB, mergedLegacyAccountRef(account), original.ID, &evidence)
	require.Error(t, err, "historical mirror must not call the ordinary ensure/revive branch")
	var afterShadow models.ChannelAccount
	var after models.Message
	var conversation models.InboxConversation
	require.NoError(t, app.DB.Unscoped().First(&afterShadow, deleted.ID).Error)
	require.NoError(t, app.DB.First(&after, original.ID).Error)
	require.NoError(t, app.DB.First(&conversation, projection.Conversation.ID).Error)
	assert.Equal(t, deleted, afterShadow)
	assert.Equal(t, original, after)
	assert.Equal(t, projection.Conversation, conversation)
	assert.Equal(t, shadowCount, countCoexistenceAdmissionRows(t, app.DB, &models.ChannelAccount{}, "organization_id = ?", account.OrganizationID))
	assert.Equal(t, counts, mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID))
}

func TestMergedLegacyProjectionRefusesRetainedExactShadowAmbiguity(t *testing.T) {
	app, account, target, legacy, original, bsuid := mergedLegacyAliasFixture(t)
	projection := loadMergedLegacyProjection(t, app.DB, account, original.ID, target.ID, legacy.ID)
	evidence := mergedLegacyProjectionEvidence(account, legacy, projection, bsuid)
	retained := projection.Account
	retained.BaseModel = models.BaseModel{ID: uuid.New(), DeletedAt: gorm.DeletedAt{Time: time.Now().UTC(), Valid: true}}
	retained.Name = "retained-shadow-" + uuid.NewString()
	// The active-only unique index legitimately permits a deleted predecessor.
	// It must not be omitted from the bounded ambiguity observation.
	require.NoError(t, app.DB.Create(&retained).Error)
	counts := mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID)
	shadowCount := countCoexistenceAdmissionRows(t, app.DB, &models.ChannelAccount{}, "organization_id = ?", account.OrganizationID)
	require.Equal(t, http.StatusServiceUnavailable, signedWebhookStatusCode(t, app,
		mergedLegacyAliasWebhookBody(t, account, mergedLegacyAliasEvent(t, bsuid, original.WhatsAppMessageID, "edit"))))
	app.WaitForBackgroundTasks()
	_, err := channelapi.MirrorLegacyMergedWhatsAppMessage(app.DB, mergedLegacyAccountRef(account), original.ID, &evidence)
	require.Error(t, err)
	var after models.Message
	var retainedAfter models.ChannelAccount
	require.NoError(t, app.DB.First(&after, original.ID).Error)
	require.NoError(t, app.DB.Unscoped().First(&retainedAfter, retained.ID).Error)
	assert.Equal(t, original, after)
	assert.True(t, retainedAfter.DeletedAt.Valid)
	assert.Equal(t, shadowCount, countCoexistenceAdmissionRows(t, app.DB, &models.ChannelAccount{}, "organization_id = ?", account.OrganizationID))
	assert.Equal(t, counts, mergedLegacyProjectionCounts(t, app.DB, account.OrganizationID))
}
