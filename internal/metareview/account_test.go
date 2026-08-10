package metareview

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"gorm.io/gorm"
)

func TestDeploymentIdentityMatchesDoesNotDependOnReviewMetadata(t *testing.T) {
	tuple := validProvisionRequest(time.Now().UTC()).Tuple
	account := readyReviewAccount(tuple)
	account.Metadata = nil

	assert.True(t, DeploymentIdentityMatches(tuple, account))
	account.OrganizationID = uuid.New()
	assert.False(t, DeploymentIdentityMatches(tuple, account))
	account.OrganizationID = uuid.MustParse(tuple.OrganizationID)
	account.ExternalAccountID = "999999999999999"
	assert.False(t, DeploymentIdentityMatches(tuple, account))
}

func TestReadyInboundOnlyBindingMatchesRequiresEveryTrustFactAndFuse(t *testing.T) {
	tuple := validProvisionRequest(time.Now().UTC()).Tuple
	assert.True(t, ReadyInboundOnlyBindingMatches(tuple, readyReviewAccount(tuple)))

	tests := []struct {
		name   string
		mutate func(*models.ChannelAccount)
	}{
		{name: "deleted", mutate: func(value *models.ChannelAccount) { value.DeletedAt = gorm.DeletedAt{Time: time.Now(), Valid: true} }},
		{name: "provider case is not canonical", mutate: func(value *models.ChannelAccount) { value.Provider = "Relay" }},
		{name: "active is not pending", mutate: func(value *models.ChannelAccount) { value.Status = models.ChannelAccountStatusActive }},
		{name: "connected", mutate: func(value *models.ChannelAccount) { now := time.Now(); value.ConnectedAt = &now }},
		{name: "default outgoing", mutate: func(value *models.ChannelAccount) { value.IsDefaultOutgoing = true }},
		{name: "marker missing", mutate: func(value *models.ChannelAccount) { delete(value.Metadata, "review_relay_mode") }},
		{name: "generation changed", mutate: func(value *models.ChannelAccount) { value.Metadata["review_generation"] = uuid.NewString() }},
		{name: "expiry missing", mutate: func(value *models.ChannelAccount) { delete(value.Metadata, "review_expires_at") }},
		{name: "expiry changed", mutate: func(value *models.ChannelAccount) {
			value.Metadata["review_expires_at"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		}},
		{name: "expiry is not a string", mutate: func(value *models.ChannelAccount) { value.Metadata["review_expires_at"] = true }},
		{name: "business changed", mutate: func(value *models.ChannelAccount) { value.Metadata["meta_business_id"] = "999999999999999" }},
		{name: "app changed", mutate: func(value *models.ChannelAccount) { value.Metadata["meta_app_id"] = "999999999999999" }},
		{name: "management mode missing", mutate: func(value *models.ChannelAccount) { delete(value.Metadata, "management_mode") }},
		{name: "review not ready", mutate: func(value *models.ChannelAccount) { value.Metadata["review_ready"] = false }},
		{name: "subscription not verified", mutate: func(value *models.ChannelAccount) { value.Metadata["subscription_verified"] = false }},
		{name: "outbound true", mutate: func(value *models.ChannelAccount) { value.Config["outbound_enabled"] = true }},
		{name: "outbound fuse missing", mutate: func(value *models.ChannelAccount) { delete(value.Config, "outbound_enabled") }},
		{name: "AI true", mutate: func(value *models.ChannelAccount) { value.Config["ai_reply_enabled"] = true }},
		{name: "AI fuse missing", mutate: func(value *models.ChannelAccount) { delete(value.Config, "ai_reply_enabled") }},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			account := readyReviewAccount(tuple)
			testCase.mutate(account)
			assert.False(t, ReadyInboundOnlyBindingMatches(tuple, account))
		})
	}
}

func TestReadyInboundOnlyBindingRejectsExpiredDeploymentAuthority(t *testing.T) {
	tuple := validProvisionRequest(time.Now().UTC()).Tuple
	account := readyReviewAccount(tuple)
	tuple.ExpiresAt = time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	assert.True(t, DeploymentIdentityMatches(tuple, account))
	assert.False(t, ReadyInboundOnlyBindingMatches(tuple, account))
}

func readyReviewAccount(tuple ProvisionTuple) *models.ChannelAccount {
	return &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.MustParse(tuple.ChannelAccountID)},
		OrganizationID:    uuid.MustParse(tuple.OrganizationID),
		Channel:           models.ChannelMessenger,
		Provider:          "relay",
		ExternalAccountID: tuple.PageID,
		Status:            models.ChannelAccountStatusPending,
		Config: models.JSONB{
			"outbound_enabled": false,
			"ai_reply_enabled": false,
		},
		Metadata: models.JSONB{
			"management_mode":       reviewManagementMode,
			"review_relay_mode":     Marker,
			"review_generation":     tuple.Generation,
			"review_expires_at":     tuple.ExpiresAt,
			"meta_business_id":      tuple.MetaBusinessID,
			"meta_app_id":           tuple.MetaAppID,
			"review_ready":          true,
			"subscription_verified": true,
		},
	}
}
