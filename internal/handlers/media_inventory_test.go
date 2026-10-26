package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
)

func inventoryTestRow(t *testing.T, kind, id string, value any) database.MediaInventoryRow {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return database.MediaInventoryRow{Kind: kind, ID: id, Data: raw}
}

func inventoryTestBase(t *testing.T) (database.MediaInventorySnapshot, models.WhatsAppAccount, models.Message, models.ScheduledJob) {
	t.Helper()
	now := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	org, accountID, messageID, contactID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	account := models.WhatsAppAccount{BaseModel: models.BaseModel{ID: accountID}, OrganizationID: org, Name: "private-account", PhoneID: "private-phone", IsSMB: true, Status: "active"}
	message := models.Message{BaseModel: models.BaseModel{ID: messageID}, OrganizationID: org, ContactID: contactID, WhatsAppMessageID: "private-wamid", WhatsAppAccount: "old-account-name",
		Direction: models.DirectionIncoming, MessageType: models.MessageTypeImage, IngestedAt: &now, Content: "private-body", MediaFilename: "private-filename"}
	job := models.ScheduledJob{BaseModel: models.BaseModel{ID: uuid.New(), CreatedAt: now}, OrganizationID: org, Kind: inboundContinuationJobKind, AggregateType: "message", AggregateID: &messageID,
		Status: models.ScheduledJobStatusCompleted, Version: 1, IdempotencyKey: "inbound-message-continuation:" + uuid.NewSHA1(accountID, []byte(message.WhatsAppMessageID)).String(),
		Payload: models.JSONB{"message_id": messageID.String(), "wamid": message.WhatsAppMessageID, "phone_number_id": account.PhoneID,
			"message": map[string]any{"id": message.WhatsAppMessageID, "from": "601122334455", "type": "image", "image": map[string]any{"id": "private-media-id", "caption": "private-body"}}}}
	snapshot := database.MediaInventorySnapshot{Scope: database.MediaInventoryScope{OrganizationID: org.String(), AccountID: accountID.String(), From: now.Add(-time.Hour), To: now.Add(time.Hour), Limit: 100}, ObservedAt: now,
		Candidates: []database.MediaInventoryCandidate{{Kind: "message", ID: messageID.String(), Arrival: now}}}
	snapshot.Rows = []database.MediaInventoryRow{
		inventoryTestRow(t, "account", account.ID.String(), account), inventoryTestRow(t, "message", message.ID.String(), message), inventoryTestRow(t, "job", job.ID.String(), job),
		inventoryTestRow(t, "contact", contactID.String(), models.Contact{BaseModel: models.BaseModel{ID: contactID}, OrganizationID: org, PhoneNumber: "601122334455"}),
	}
	return snapshot, account, message, job
}

func inventoryTestReplace(t *testing.T, snapshot *database.MediaInventorySnapshot, kind, id string, value any) {
	t.Helper()
	for i, row := range snapshot.Rows {
		if row.Kind == kind && row.ID == id {
			snapshot.Rows[i] = inventoryTestRow(t, kind, id, value)
			return
		}
	}
	t.Fatal("fixture row not found")
}

func inventoryTestEntry(t *testing.T, snapshot database.MediaInventorySnapshot) mediaInventoryEntry {
	t.Helper()
	public, private, err := ClassifyMediaInventory(context.Background(), snapshot, "fixture-source", "fixture-build")
	require.NoError(t, err)
	for _, secret := range []string{"private-account", "private-phone", "private-wamid", "private-body", "private-filename", "private-media-id", "601122334455"} {
		require.NotContains(t, string(public), secret)
		require.NotContains(t, string(private), secret)
	}
	require.NotContains(t, string(public), snapshot.Scope.OrganizationID)
	require.NotContains(t, string(public), snapshot.Scope.AccountID)
	require.Contains(t, string(public), `"historical_token_and_cycle_proof":"unknown"`)
	require.Contains(t, string(public), `"object_existence":"unverified"`)
	require.Contains(t, string(private), `"apply_authorized":false`)
	var manifest struct {
		Entries []mediaInventoryEntry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(private, &manifest))
	require.Len(t, manifest.Entries, 1)
	return manifest.Entries[0]
}

func TestMediaInventoryOrdinaryProofAndPrivacy(t *testing.T) {
	for _, test := range []struct {
		name     string
		mutate   func(*database.MediaInventorySnapshot, *models.WhatsAppAccount, *models.Message, *models.ScheduledJob)
		positive bool
	}{
		{"exact_renamed_account", func(_ *database.MediaInventorySnapshot, a *models.WhatsAppAccount, _ *models.Message, _ *models.ScheduledJob) {
			a.Name = "new-private-account"
		}, true},
		{"wrong_raw_identity", func(_ *database.MediaInventorySnapshot, _ *models.WhatsAppAccount, _ *models.Message, j *models.ScheduledJob) {
			j.Payload["message"].(map[string]any)["id"] = "another"
		}, false},
		{"wrong_account_key", func(_ *database.MediaInventorySnapshot, _ *models.WhatsAppAccount, _ *models.Message, j *models.ScheduledJob) {
			j.IdempotencyKey = "inbound-message-continuation:" + uuid.New().String()
		}, false},
		{"wrong_phone", func(_ *database.MediaInventorySnapshot, _ *models.WhatsAppAccount, _ *models.Message, j *models.ScheduledJob) {
			j.Payload["phone_number_id"] = "other"
		}, false},
		{"wrong_sender", func(_ *database.MediaInventorySnapshot, _ *models.WhatsAppAccount, _ *models.Message, j *models.ScheduledJob) {
			j.Payload["message"].(map[string]any)["from"] = "609999"
		}, false},
		{"empty_normalized_sender", func(s *database.MediaInventorySnapshot, a *models.WhatsAppAccount, m *models.Message, j *models.ScheduledJob) {
			j.Payload["message"].(map[string]any)["from"] = "not-a-phone"
			inventoryTestReplace(t, s, "contact", m.ContactID.String(), models.Contact{BaseModel: models.BaseModel{ID: m.ContactID}, OrganizationID: a.OrganizationID})
		}, false},
		{"wrong_media_type", func(_ *database.MediaInventorySnapshot, _ *models.WhatsAppAccount, _ *models.Message, j *models.ScheduledJob) {
			j.Payload["message"].(map[string]any)["type"] = "video"
		}, false},
		{"missing_media_id", func(_ *database.MediaInventorySnapshot, _ *models.WhatsAppAccount, _ *models.Message, j *models.ScheduledJob) {
			j.Payload["message"].(map[string]any)["image"] = map[string]any{}
		}, false},
		{"review_route", func(_ *database.MediaInventorySnapshot, _ *models.WhatsAppAccount, m *models.Message, _ *models.ScheduledJob) {
			m.Metadata = models.JSONB{incomingIdentityReviewHoldIDKey: uuid.New().String()}
		}, false},
		{"missing_projection", func(_ *database.MediaInventorySnapshot, _ *models.WhatsAppAccount, m *models.Message, _ *models.ScheduledJob) {
			id := uuid.New()
			m.InboxConversationID = &id
		}, false},
		{"inactive_account", func(_ *database.MediaInventorySnapshot, a *models.WhatsAppAccount, _ *models.Message, _ *models.ScheduledJob) {
			a.Status = "inactive"
		}, false},
		{"nullable_ingestion", func(_ *database.MediaInventorySnapshot, _ *models.WhatsAppAccount, m *models.Message, _ *models.ScheduledJob) {
			m.IngestedAt = nil
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, a, m, j := inventoryTestBase(t)
			test.mutate(&s, &a, &m, &j)
			inventoryTestReplace(t, &s, "account", a.ID.String(), a)
			inventoryTestReplace(t, &s, "message", m.ID.String(), m)
			inventoryTestReplace(t, &s, "job", j.ID.String(), j)
			entry := inventoryTestEntry(t, s)
			require.Equal(t, test.positive, entry.Class == "ordinary_blank_url_exact_continuation_proof")
			require.Equal(t, m.IngestedAt == nil, entry.TimeUnknown)
		})
	}
	for _, mode := range []string{"zero_jobs", "two_jobs", "competing_wamid", "deleted_message", "deleted_account", "merged_contact", "conflicting_activity"} {
		t.Run(mode, func(t *testing.T) {
			s, a, m, j := inventoryTestBase(t)
			switch mode {
			case "zero_jobs":
				s.Rows = append(s.Rows[:2], s.Rows[3:]...)
			case "two_jobs":
				j.ID = uuid.New()
				s.Rows = append(s.Rows, inventoryTestRow(t, "job", j.ID.String(), j))
			case "competing_wamid":
				m.ID = uuid.New()
				s.Rows = append(s.Rows, inventoryTestRow(t, "message", m.ID.String(), m))
			case "deleted_message":
				s.Rows[1].Deleted = true
			case "deleted_account":
				s.Rows[0].Deleted = true
			case "merged_contact":
				id := uuid.New()
				inventoryTestReplace(t, &s, "contact", m.ContactID.String(), models.Contact{BaseModel: models.BaseModel{ID: m.ContactID}, OrganizationID: a.OrganizationID, PhoneNumber: "601122334455", MergedIntoID: &id})
			case "conflicting_activity":
				activity := models.CustomerActivityEvent{ID: uuid.New(), OrganizationID: a.OrganizationID, SourceObjectID: &m.ID, SourceObjectType: "message", IdempotencyKey: "wrong", EventType: models.CustomerActivityMessageIncoming}
				s.Rows = append(s.Rows, inventoryTestRow(t, "activity", activity.ID.String(), activity))
			}
			require.NotEqual(t, "ordinary_blank_url_exact_continuation_proof", inventoryTestEntry(t, s).Class)
		})
	}
}

func TestMediaInventoryCoexistenceExhaustionAndStoredReferences(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        models.ScheduledJobStatus
		attempts, max int
		mutate        string
		class, reason string
	}{
		{"default_exhausted", models.ScheduledJobStatusFailed, 8, 0, "", "coex_message_failed_exhausted", "failed_exhausted"},
		{"configured_exhausted", models.ScheduledJobStatusFailed, 3, 3, "", "coex_message_failed_exhausted", "failed_exhausted"},
		{"below_default", models.ScheduledJobStatusFailed, 7, 0, "", "coex_other_job_state", "failed_below_cap"},
		{"below_configured", models.ScheduledJobStatusFailed, 8, 10, "", "coex_other_job_state", "failed_below_cap"},
		{"pending", models.ScheduledJobStatusPending, 8, 8, "", "coex_other_job_state", "work_pending_or_processing"},
		{"completed", models.ScheduledJobStatusCompleted, 8, 8, "", "coex_other_job_state", "other_terminal_state"},
		{"cancelled", models.ScheduledJobStatusCancelled, 8, 8, "", "coex_other_job_state", "other_terminal_state"},
		{"cleanup", models.ScheduledJobStatusFailed, 8, 8, "cleanup", "coex_other_job_state", "cleanup_only"},
		{"changed_revision", models.ScheduledJobStatusFailed, 8, 8, "changed", "coex_other_job_state", "stale_revision_or_invalid_proof"},
		{"revoked", models.ScheduledJobStatusFailed, 8, 8, "revoked", "coex_other_job_state", "stale_revision_or_invalid_proof"},
		{"key_only_collision", models.ScheduledJobStatusFailed, 8, 8, "key_collision", "coex_other_job_state", "missing_or_ambiguous_jobs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a, m, j := inventoryTestBase(t)
			m.Metadata = models.JSONB{coexistenceMediaProviderIDMetadataKey: "private-media-id"}
			mediaID, revision, _ := coexistenceMediaRevision(&m)
			j.Kind = coexistenceMediaHydrationJobKind
			j.Status = tc.status
			j.Attempts = tc.attempts
			j.MaxAttempts = tc.max
			j.IdempotencyKey = coexistenceMediaJobKey(a.ID, m.WhatsAppMessageID, mediaID, revision)
			j.Payload = models.JSONB{"account_id": a.ID.String(), "message_id": m.ID.String(), "wamid": m.WhatsAppMessageID, "phone_number_id": a.PhoneID, "media_id": mediaID, "media_revision": revision}
			switch tc.mutate {
			case "cleanup":
				j.Payload["cleanup_only"] = true
			case "changed":
				m.Content = "changed-private-body"
			case "revoked":
				m.Metadata[coexistenceMediaRevokedMetadataKey] = true
			}
			inventoryTestReplace(t, &s, "message", m.ID.String(), m)
			inventoryTestReplace(t, &s, "job", j.ID.String(), j)
			if tc.mutate == "key_collision" {
				duplicate := j
				duplicate.ID, duplicate.Kind, duplicate.Payload = uuid.New(), "wrong.kind", models.JSONB{}
				other := uuid.New()
				duplicate.AggregateID = &other
				s.Rows = append(s.Rows, inventoryTestRow(t, "job", duplicate.ID.String(), duplicate))
			}
			e := inventoryTestEntry(t, s)
			require.Equal(t, tc.class, e.Class)
			require.Equal(t, tc.reason, e.Reason)
		})
	}
	for _, reference := range []string{"organizations/", "images/private-filename", "https://private.invalid/private?token=private", "../private"} {
		s, a, m, _ := inventoryTestBase(t)
		if reference == "organizations/" {
			reference += a.OrganizationID.String() + "/media/private-filename"
		}
		m.MediaURL = reference
		inventoryTestReplace(t, &s, "message", m.ID.String(), m)
		e := inventoryTestEntry(t, s)
		require.Equal(t, "stored_reference_present_existence_unverified", e.Class)
		require.True(t, strings.HasSuffix(e.Reference, "existence_unverified"))
	}
}

func TestMediaInventoryHeldGenerationNeverAuthorizesRouting(t *testing.T) {
	for _, fault := range []string{"none", "old_cycle", "offboarded", "wrong_generation", "invalid_signature", "wrong_channel", "missing_member", "cleanup", "revoked", "key_collision"} {
		t.Run(fault, func(t *testing.T) {
			s, a, _, j := inventoryTestBase(t)
			eventID, holdID, channelID, stateID, contactID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
			member := models.WhatsAppIdentityReviewMember{OrganizationID: a.OrganizationID, HoldID: holdID, ContactID: contactID, SelectorReasons: models.WhatsAppIdentityReviewSelectorPhone}
			hold := models.WhatsAppIdentityReviewHold{ID: holdID, OrganizationID: a.OrganizationID, WhatsAppAccountID: a.ID, OnboardingCycle: 2, ProtocolVersion: models.WhatsAppIdentityReviewProtocolVersion, Disposition: models.WhatsAppIdentityReviewDispositionOpen, MemberCount: 1, MemberDigest: identityReviewMemberDigest([]WhatsAppIdentityReviewCandidate{{ContactID: contactID, SelectorReasons: member.SelectorReasons}})}
			event := models.InboundEvent{BaseModel: models.BaseModel{ID: eventID}, OrganizationID: a.OrganizationID, ChannelAccountID: channelID, ProviderEventID: "private-wamid", SignatureValid: true, Status: models.InboundEventStatusPending, Protocol: models.WhatsAppIdentityReviewInboundProtocol, EventType: models.WhatsAppIdentityReviewPendingEvent, ReviewHoldID: &holdID, ReceivedAt: s.ObservedAt,
				Payload: models.JSONB{"message_type": "image", "media_id": "private-media-id", "content": "private-body", "media_generation": uint64(2)}}
			revision := coexistenceStagedMediaRevisionDigest(event.Payload, 2)
			event.Payload["media_revision"] = revision
			j.Kind = coexistenceMediaHydrationJobKind
			j.AggregateType = coexistenceMediaReviewAggregate
			j.AggregateID = &eventID
			j.Payload = models.JSONB{"account_id": a.ID.String(), "event_id": eventID.String(), "phone_number_id": a.PhoneID, "wamid": event.ProviderEventID, "media_id": "private-media-id", "media_revision": revision, "media_generation": uint64(2)}
			j.IdempotencyKey = coexistenceStagedMediaJobKey(a.ID, event.ProviderEventID, "private-media-id", revision, 2)
			channel := models.ChannelAccount{BaseModel: models.BaseModel{ID: channelID}, OrganizationID: a.OrganizationID, Channel: models.ChannelWhatsApp, Provider: "meta_legacy", ExternalAccountID: "legacy-account:" + a.ID.String(), Metadata: models.JSONB{"legacy_account_id": a.ID.String()}}
			state := map[string]any{"id": stateID, "organization_id": a.OrganizationID, "whatsapp_account_id": a.ID, "onboarding_cycle": 2, "lifecycle_status": "connected"}
			switch fault {
			case "old_cycle":
				state["onboarding_cycle"] = 3
			case "offboarded":
				state["lifecycle_status"] = "offboarded"
			case "wrong_generation":
				event.Payload["media_generation"] = 3
			case "invalid_signature":
				event.SignatureValid = false
			case "wrong_channel":
				channel.ExternalAccountID = "legacy-account:" + uuid.New().String()
			case "cleanup":
				j.Payload["cleanup_only"] = true
			case "revoked":
				event.Payload["media_revoked"] = true
			}
			s.Candidates = []database.MediaInventoryCandidate{{Kind: "event", ID: eventID.String(), Arrival: s.ObservedAt}}
			s.Rows = []database.MediaInventoryRow{inventoryTestRow(t, "account", a.ID.String(), a), inventoryTestRow(t, "job", j.ID.String(), j), inventoryTestRow(t, "event", event.ID.String(), event), inventoryTestRow(t, "channel", channel.ID.String(), channel), inventoryTestRow(t, "hold", hold.ID.String(), hold), inventoryTestRow(t, "state", stateID.String(), state)}
			if fault != "missing_member" {
				s.Rows = append(s.Rows, inventoryTestRow(t, "member", holdID.String()+":"+contactID.String(), member))
			}
			if fault == "key_collision" {
				duplicate := j
				duplicate.ID, duplicate.Kind, duplicate.Payload = uuid.New(), "wrong.kind", models.JSONB{}
				other := uuid.New()
				duplicate.AggregateID = &other
				s.Rows = append(s.Rows, inventoryTestRow(t, "job", duplicate.ID.String(), duplicate))
			}
			e := inventoryTestEntry(t, s)
			require.Equal(t, fault == "none", e.Class == "held_stage_current_generation")
		})
	}
}

func TestMediaInventoryRefusesIncompleteOrCrossTenantInput(t *testing.T) {
	for _, fault := range []string{"cap", "oversize", "foreign", "duplicate", "arrival_outside", "unknown_kind"} {
		t.Run(fault, func(t *testing.T) {
			s, a, _, _ := inventoryTestBase(t)
			switch fault {
			case "cap":
				s.Scope.Limit = 0
			case "oversize":
				s.Rows[0].Data = json.RawMessage(strings.Repeat("x", 65537))
			case "foreign":
				a.OrganizationID = uuid.New()
				s.Rows[0] = inventoryTestRow(t, "account", a.ID.String(), a)
			case "duplicate":
				s.Rows = append(s.Rows, s.Rows[0])
			case "arrival_outside":
				s.Candidates[0].Arrival = s.Scope.To
			case "unknown_kind":
				s.Rows[0].Kind = "credentials"
			}
			public, private, err := ClassifyMediaInventory(context.Background(), s, "fixture", "fixture")
			require.Error(t, err)
			require.Nil(t, public)
			require.Nil(t, private)
		})
	}
}

type mediaInventoryCancelDuringScan struct {
	context.Context
	calls int
}

func (c *mediaInventoryCancelDuringScan) Err() error {
	c.calls++
	if c.calls >= 4 {
		return context.Canceled
	}
	return nil
}

func TestMediaInventoryCancellationAndManifestFanoutRefuseAllOutput(t *testing.T) {
	s, a, _, job := inventoryTestBase(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{cancelled, &mediaInventoryCancelDuringScan{Context: context.Background()}} {
		public, private, err := ClassifyMediaInventory(ctx, s, "fixture", "fixture")
		require.Error(t, err)
		require.Nil(t, public)
		require.Nil(t, private)
	}
	s.Scope.Limit = 2
	s.Rows = s.Rows[:1]
	s.Candidates = nil
	for n := 0; n < 2; n++ {
		eventID, holdID := uuid.New(), uuid.New()
		event := models.InboundEvent{BaseModel: models.BaseModel{ID: eventID}, OrganizationID: a.OrganizationID,
			ChannelAccountID: uuid.New(), ProviderEventID: "private-wamid", SignatureValid: true, Status: models.InboundEventStatusPending,
			Protocol: models.WhatsAppIdentityReviewInboundProtocol, EventType: models.WhatsAppIdentityReviewPendingEvent, ReviewHoldID: &holdID,
			Payload: models.JSONB{"message_type": "image", "media_id": "private-media-id", "media_generation": uint64(1)}}
		event.Payload["media_revision"] = coexistenceStagedMediaRevisionDigest(event.Payload, 1)
		s.Rows = append(s.Rows, inventoryTestRow(t, "event", event.ID.String(), event))
		s.Candidates = append(s.Candidates, database.MediaInventoryCandidate{Kind: "event", ID: event.ID.String(), Arrival: s.ObservedAt})
	}
	for n := 0; n < 20; n++ {
		job.ID, job.Kind, job.IdempotencyKey = uuid.New(), coexistenceMediaHydrationJobKind, uuid.New().String()
		s.Rows = append(s.Rows, inventoryTestRow(t, "job", job.ID.String(), job))
	}
	// Twenty small jobs fit the input budget, but repeating all twenty for two
	// aggregates exceeds the manifest's global (8*limit+16) job-reference cap.
	public, private, err := ClassifyMediaInventory(context.Background(), s, "fixture", "fixture")
	require.Error(t, err)
	require.Nil(t, public)
	require.Nil(t, private)
}
