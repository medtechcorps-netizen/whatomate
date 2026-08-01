package demodata

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestBuildKlinikReliveSalesFixtures(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 31, 16, 45, 0, 0, time.FixedZone("MYT", 8*60*60))
	refs := testFixtureReferences()
	fixtures, err := BuildKlinikReliveSalesFixtures(now, refs)
	if err != nil {
		t.Fatalf("BuildKlinikReliveSalesFixtures() error = %v", err)
	}

	if got := len(fixtures.CannedResponses); got != 12 {
		t.Fatalf("canned response count = %d, want 12", got)
	}
	if got := len(fixtures.IVRFlows); got != 5 {
		t.Fatalf("IVR flow count = %d, want 5", got)
	}
	if got := len(fixtures.CallLogs); got != 36 {
		t.Fatalf("call log count = %d, want 36", got)
	}
	if got := len(fixtures.CallTransfers); got != 12 {
		t.Fatalf("call transfer count = %d, want 12", got)
	}
	if got := len(fixtures.MetaSnapshots); got != 4 {
		t.Fatalf("Meta snapshot count = %d, want 4", got)
	}
	if err := ValidateKlinikReliveSalesFixtures(fixtures, refs); err != nil {
		t.Fatalf("ValidateKlinikReliveSalesFixtures() error = %v", err)
	}

	allowedCategories := map[string]bool{
		"greeting": true,
		"support":  true,
		"sales":    true,
		"closing":  true,
		"general":  true,
	}
	for _, row := range fixtures.CannedResponses {
		if row.IsActive {
			t.Fatalf("canned response %q is active", row.Name)
		}
		if !allowedCategories[row.Category] {
			t.Fatalf("canned response %q has unsupported category %q", row.Name, row.Category)
		}
		if strings.HasPrefix(row.Shortcut, "/") || !strings.HasPrefix(row.Shortcut, "mock-") {
			t.Fatalf("canned response %q has UI-incompatible shortcut %q", row.Name, row.Shortcut)
		}
	}

	anchor := utcDay(now)
	recentCalls := 0
	priorCalls := 0
	for _, row := range fixtures.CallLogs {
		if row.StartedAt == nil {
			t.Fatalf("call log %s has no start time", row.ID)
		}
		if !row.StartedAt.Before(anchor.AddDate(0, 0, -30)) {
			recentCalls++
		} else {
			priorCalls++
		}
	}
	if recentCalls != 30 || priorCalls != 6 {
		t.Fatalf("call time coverage recent=%d prior=%d, want recent=30 prior=6", recentCalls, priorCalls)
	}

	callsByID := make(map[uuid.UUID]models.CallLog, len(fixtures.CallLogs))
	for i, row := range fixtures.CallLogs {
		wantContact := refs.Contacts[i%crmContactReferenceCount]
		if row.ContactID != wantContact.ID || row.CallerPhone != wantContact.PhoneNumber {
			t.Fatalf(
				"call %d contact=(%s, %q), want (%s, %q)",
				i,
				row.ContactID,
				row.CallerPhone,
				wantContact.ID,
				wantContact.PhoneNumber,
			)
		}
		callsByID[row.ID] = row
	}
	for i, row := range fixtures.CallTransfers {
		sourceCall, ok := callsByID[row.CallLogID]
		if !ok {
			t.Fatalf("transfer %d references unknown call %s", i, row.CallLogID)
		}
		if row.ContactID != sourceCall.ContactID ||
			row.CallerPhone != sourceCall.CallerPhone ||
			row.WhatsAppCallID != sourceCall.WhatsAppCallID {
			t.Fatalf("transfer %d does not preserve its source call identity", i)
		}
	}
}

func TestBuildKlinikReliveSalesFixturesRejectsIncompleteCanonicalContacts(t *testing.T) {
	t.Parallel()

	refs := testFixtureReferences()
	refs.Contacts = refs.Contacts[:crmContactReferenceCount-1]

	_, err := BuildKlinikReliveSalesFixtures(time.Now().UTC(), refs)
	if err == nil || !strings.Contains(err.Error(), "expected 28 CRM mock contacts, got 27") {
		t.Fatalf("BuildKlinikReliveSalesFixtures() error = %v, want exact canonical contact count rejection", err)
	}
}

func TestFixtureIDsAreDeterministicAcrossBuilds(t *testing.T) {
	t.Parallel()

	refs := testFixtureReferences()
	first, err := BuildKlinikReliveSalesFixtures(time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC), refs)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildKlinikReliveSalesFixtures(time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC), refs)
	if err != nil {
		t.Fatal(err)
	}

	for i := range first.CannedResponses {
		if first.CannedResponses[i].ID != second.CannedResponses[i].ID {
			t.Fatalf("canned response ID %d changed across builds", i)
		}
	}
	for i := range first.IVRFlows {
		if first.IVRFlows[i].ID != second.IVRFlows[i].ID {
			t.Fatalf("IVR ID %d changed across builds", i)
		}
	}
	for i := range first.CallLogs {
		if first.CallLogs[i].ID != second.CallLogs[i].ID {
			t.Fatalf("call ID %d changed across builds", i)
		}
	}
	for i := range first.CallTransfers {
		if first.CallTransfers[i].ID != second.CallTransfers[i].ID {
			t.Fatalf("transfer ID %d changed across builds", i)
		}
	}
	for i := range first.MetaSnapshots {
		if first.MetaSnapshots[i].ID != second.MetaSnapshots[i].ID ||
			first.MetaSnapshots[i].AccountID != second.MetaSnapshots[i].AccountID {
			t.Fatalf("Meta snapshot identity %d changed across builds", i)
		}
	}
}

func TestFixtureSafetyRejectsOperationalRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*FixtureSet)
	}{
		{
			name: "active canned response",
			mutate: func(fixtures *FixtureSet) {
				fixtures.CannedResponses[0].IsActive = true
			},
		},
		{
			name: "unsupported canned category",
			mutate: func(fixtures *FixtureSet) {
				fixtures.CannedResponses[0].Category = "Appointments"
			},
		},
		{
			name: "shortcut with UI slash",
			mutate: func(fixtures *FixtureSet) {
				fixtures.CannedResponses[0].Shortcut = "/mock-welcome"
			},
		},
		{
			name: "active IVR",
			mutate: func(fixtures *FixtureSet) {
				fixtures.IVRFlows[0].IsActive = true
			},
		},
		{
			name: "IVR callback URL",
			mutate: func(fixtures *FixtureSet) {
				nodes := fixtures.IVRFlows[0].Menu["nodes"].([]any)
				node := nodes[0].(map[string]any)
				node["config"].(map[string]any)["url"] = "https://example.invalid/callback"
			},
		},
		{
			name: "live call status",
			mutate: func(fixtures *FixtureSet) {
				fixtures.CallLogs[0].Status = models.CallStatusRinging
			},
		},
		{
			name: "recording object",
			mutate: func(fixtures *FixtureSet) {
				fixtures.CallLogs[0].RecordingS3Key = "unsafe/object"
			},
		},
		{
			name: "waiting transfer",
			mutate: func(fixtures *FixtureSet) {
				fixtures.CallTransfers[0].Status = models.CallTransferStatusWaiting
			},
		},
		{
			name: "connected non-completed transfer",
			mutate: func(fixtures *FixtureSet) {
				fixtures.CallTransfers[6].ConnectedAt = timePtr(time.Now().UTC())
			},
		},
		{
			name: "provider-looking Meta phone ID",
			mutate: func(fixtures *FixtureSet) {
				fixtures.MetaSnapshots[0].PhoneID = "123456789"
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			refs := testFixtureReferences()
			fixtures, err := BuildKlinikReliveSalesFixtures(time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), refs)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&fixtures)
			if err := ValidateKlinikReliveSalesFixtures(fixtures, refs); err == nil {
				t.Fatal("ValidateKlinikReliveSalesFixtures() accepted an unsafe mutation")
			}
		})
	}
}

func TestFixtureSafetyRejectsChangedMetaTotals(t *testing.T) {
	t.Parallel()

	refs := testFixtureReferences()
	fixtures, err := BuildKlinikReliveSalesFixtures(time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), refs)
	if err != nil {
		t.Fatal(err)
	}
	section := fixtures.MetaSnapshots[0].Payload["analytics"].(map[string]any)
	point := section["data_points"].([]any)[0].(map[string]any)
	point["sent"] = 109
	if err := ValidateKlinikReliveSalesFixtures(fixtures, refs); err == nil {
		t.Fatal("ValidateKlinikReliveSalesFixtures() accepted changed analytics totals")
	}
}

func TestCompareForbiddenState(t *testing.T) {
	t.Parallel()

	before := map[string]invariantValue{
		"outbox_jobs":       {Count: 4, Digest: "one"},
		"whatsapp_accounts": {Count: 1, Digest: "two"},
	}
	unchanged := map[string]invariantValue{
		"outbox_jobs":       {Count: 4, Digest: "one"},
		"whatsapp_accounts": {Count: 1, Digest: "two"},
	}
	if err := compareForbiddenState(before, unchanged); err != nil {
		t.Fatalf("compareForbiddenState() rejected unchanged state: %v", err)
	}

	changed := map[string]invariantValue{
		"outbox_jobs":       {Count: 5, Digest: "changed"},
		"whatsapp_accounts": {Count: 1, Digest: "two"},
	}
	if err := compareForbiddenState(before, changed); err == nil {
		t.Fatal("compareForbiddenState() accepted changed queue state")
	}
}

func TestSeedKlinikReliveSalesRejectsWrongTenantBeforeOpeningTransaction(t *testing.T) {
	// The tenant identity guard runs before any database operation. A non-nil
	// zero-value handle makes that ordering part of the regression contract
	// without sharing the integration-test database across Go package workers.
	db := &gorm.DB{}

	_, err := SeedKlinikReliveSales(context.Background(), db, SeedOptions{
		OrganizationID:   uuid.New(),
		OrganizationName: KlinikReliveOrganizationName,
		Apply:            true,
	})
	require.ErrorContains(t, err, "organization ID must exactly equal "+KlinikReliveOrganizationID)

	_, err = SeedKlinikReliveSales(context.Background(), db, SeedOptions{
		OrganizationID:   expectedOrganizationID,
		OrganizationName: "Similar but unsafe tenant name",
		Apply:            true,
	})
	require.ErrorContains(t, err, `organization name must exactly equal "Klinik Relive"`)
}

func TestFixtureReplayBuildIsIdempotentAndOperationalRowsStayInactive(t *testing.T) {
	t.Parallel()

	refs := testFixtureReferences()
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	first, err := BuildKlinikReliveSalesFixtures(now, refs)
	require.NoError(t, err)
	second, err := BuildKlinikReliveSalesFixtures(now, refs)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(first, second), "same fixture inputs must produce the same replay payload")
	for _, row := range second.CannedResponses {
		require.False(t, row.IsActive)
	}
	for _, row := range second.IVRFlows {
		require.False(t, row.IsActive)
	}
}

func testFixtureReferences() FixtureReferences {
	contacts := make([]ContactReference, 0, crmContactReferenceCount)
	for i := crmContactFirstSequence; i <= crmContactLastSequence; i++ {
		contacts = append(contacts, ContactReference{
			ID:          deterministicUUID(fmt.Sprintf("test-contact-%d", i)),
			PhoneNumber: fmt.Sprintf("6000000000%02d", i),
		})
	}
	agents := []uuid.UUID{
		deterministicUUID("test-agent-1"),
		deterministicUUID("test-agent-2"),
		deterministicUUID("test-agent-3"),
	}
	return FixtureReferences{
		OrganizationID: expectedOrganizationID,
		OwnerID:        deterministicUUID("test-owner"),
		TeamID:         deterministicUUID("test-team"),
		Contacts:       contacts,
		AgentIDs:       agents,
	}
}
