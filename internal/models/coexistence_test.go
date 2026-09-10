package models_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWhatsAppCoexistenceStateContract(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "whatsapp_coexistence_states", (models.WhatsAppCoexistenceState{}).TableName())
	assert.Equal(t, 24*time.Hour, models.CoexistenceSyncWindow)
	assert.Equal(t, models.CoexistenceOnboardingStatus("ready"), models.CoexistenceOnboardingStatusReady)
	assert.Equal(t, models.CoexistenceSyncStatus("requesting"), models.CoexistenceSyncStatusRequesting)
	assert.Equal(t, models.CoexistenceSyncStatus("completed"), models.CoexistenceSyncStatusCompleted)
	assert.Equal(t, models.CoexistenceHistoryConsent("declined"), models.CoexistenceHistoryConsentDeclined)
	assert.Equal(t, models.CoexistenceLifecycleStatus("offboarded"), models.CoexistenceLifecycleStatusOffboarded)

	modelType := reflect.TypeOf(models.WhatsAppCoexistenceState{})
	organizationField, ok := modelType.FieldByName("OrganizationID")
	require.True(t, ok)
	assert.Contains(t, organizationField.Tag.Get("gorm"), "not null")
	assert.Contains(t, organizationField.Tag.Get("gorm"), "uniqueIndex:idx_whatsapp_coexistence_org_account")
	accountField, ok := modelType.FieldByName("WhatsAppAccountID")
	require.True(t, ok)
	assert.Contains(t, accountField.Tag.Get("gorm"), "not null")
	assert.Contains(t, accountField.Tag.Get("gorm"), "uniqueIndex:idx_whatsapp_coexistence_org_account")
}

func TestWhatsAppIdentityReviewStorageContract(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "whatsapp_identity_review_holds", (models.WhatsAppIdentityReviewHold{}).TableName())
	assert.Equal(t, "whatsapp_identity_review_members", (models.WhatsAppIdentityReviewMember{}).TableName())
	assert.Equal(t, uint16(1), models.WhatsAppIdentityReviewProtocolVersion)
	assert.Equal(t, models.WhatsAppIdentityReviewSelectorReason(7), models.WhatsAppIdentityReviewSelectorReasonMask)
	assert.True(t, (models.WhatsAppIdentityReviewSelectorPrimaryBSUID | models.WhatsAppIdentityReviewSelectorPhone).Valid())
	assert.False(t, models.WhatsAppIdentityReviewSelectorReason(0).Valid())
	assert.False(t, models.WhatsAppIdentityReviewSelectorReason(8).Valid())

	holdType := reflect.TypeOf(models.WhatsAppIdentityReviewHold{})
	require.NotContains(t, fieldNames(holdType), "DeletedAt")
	for _, name := range []string{
		"DirectPrimaryBSUID", "ParentBSUID", "Phone", "SelectorBodyDigest",
		"VerifiedEventDigest", "VerifiedEventProvenance",
	} {
		field, ok := holdType.FieldByName(name)
		require.True(t, ok)
		assert.Equal(t, "-", field.Tag.Get("json"), "%s must stay server-private", name)
	}

	memberType := reflect.TypeOf(models.WhatsAppIdentityReviewMember{})
	require.NotContains(t, fieldNames(memberType), "ID")
	require.NotContains(t, fieldNames(memberType), "UpdatedAt")
	require.NotContains(t, fieldNames(memberType), "DeletedAt")
	for _, name := range []string{"OrganizationID", "HoldID", "ContactID"} {
		field, ok := memberType.FieldByName(name)
		require.True(t, ok)
		assert.Contains(t, field.Tag.Get("gorm"), "primaryKey")
	}
}

func TestWhatsAppIdentityReviewHoldJSONDoesNotExposeClaimValues(t *testing.T) {
	t.Parallel()

	hold := models.WhatsAppIdentityReviewHold{
		DirectPrimaryBSUID:      "private-primary",
		ParentBSUID:             "private-parent",
		Phone:                   "60123456789",
		SelectorBodyDigest:      "private-selector-body",
		VerifiedEventDigest:     "private-event-digest",
		VerifiedEventProvenance: "private-provenance",
	}
	encoded, err := json.Marshal(hold)
	require.NoError(t, err)
	for _, privateValue := range []string{
		"private-primary", "private-parent", "60123456789", "private-selector-body",
		"private-event-digest", "private-provenance",
	} {
		assert.NotContains(t, string(encoded), privateValue)
	}
}

func TestWhatsAppIdentityReviewPermissionIsAdminOnlyByDefault(t *testing.T) {
	t.Parallel()

	const permission = "contacts.identity_review:write"
	roles := models.SystemRolePermissions()
	assert.Contains(t, roles["admin"], permission)
	assert.NotContains(t, roles["manager"], permission)
	assert.NotContains(t, roles["agent"], permission)

	var definitions int
	for _, candidate := range models.DefaultPermissions() {
		if candidate.Resource == models.ResourceContactsIdentityReview && candidate.Action == models.ActionWrite {
			definitions++
		}
	}
	assert.Equal(t, 1, definitions)
}

func fieldNames(modelType reflect.Type) []string {
	names := make([]string, 0, modelType.NumField())
	for index := 0; index < modelType.NumField(); index++ {
		names = append(names, modelType.Field(index).Name)
	}
	return names
}

func TestWhatsAppCoexistenceStateDoesNotSerializeInternalErrors(t *testing.T) {
	t.Parallel()

	modelType := reflect.TypeOf(models.WhatsAppCoexistenceState{})
	for _, name := range []string{
		"BusinessPhoneNumber",
		"OnboardingErrorCode",
		"OnboardingErrorMessage",
		"OnboardingCycle",
		"ContactSyncErrorCode",
		"ContactSyncErrorMessage",
		"HistorySyncErrorCode",
		"HistorySyncErrorMessage",
		"DisconnectReasonMessage",
	} {
		field, ok := modelType.FieldByName(name)
		require.True(t, ok)
		assert.Equal(t, "-", field.Tag.Get("json"), "%s must not be exposed through API JSON", name)
	}
}
