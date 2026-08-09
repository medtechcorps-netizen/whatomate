package handlers

import (
	"testing"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
)

func TestManagedMessengerManualCreationPolicyFailsClosedInProduction(t *testing.T) {
	request := ChannelAccountRequest{
		Channel:  models.ChannelMessenger,
		Provider: channelapi.RelayProvider,
	}

	production := &App{Config: &configpkg.Config{
		App: configpkg.AppConfig{Environment: "production"},
	}}
	assert.True(t, production.managedMessengerManualCreationBlocked(request))

	development := &App{Config: &configpkg.Config{
		App: configpkg.AppConfig{Environment: "development"},
	}}
	assert.False(t, development.managedMessengerManualCreationBlocked(request))

	development.Config.MetaMessengerOnboarding.Enabled = true
	assert.True(t, development.managedMessengerManualCreationBlocked(request))

	request.Channel = models.ChannelInstagram
	assert.False(t, production.managedMessengerManualCreationBlocked(request))
}

func TestManagedMetaMessengerAccountUsesServerMetadata(t *testing.T) {
	account := &models.ChannelAccount{
		Channel:  models.ChannelMessenger,
		Provider: channelapi.RelayProvider,
		Metadata: models.JSONB{"management_mode": metaMessengerManagementMode},
	}
	assert.True(t, isManagedMetaMessengerAccount(account))

	account.Metadata = models.JSONB{}
	account.Config = models.JSONB{"management_mode": metaMessengerManagementMode}
	assert.False(t, isManagedMetaMessengerAccount(account), "tenant-editable config is not management authority")

	account.Metadata = models.JSONB{"management_mode": metaMessengerManagementMode}
	account.Channel = models.ChannelInstagram
	assert.False(t, isManagedMetaMessengerAccount(account))
}

func TestManagedMetaMessengerGenericUpdateProtectsProvisionedFields(t *testing.T) {
	name := "Display name"
	outbound := true
	aiReply := false
	defaultIncoming := true
	assert.False(t, managedMetaMessengerProtectedFieldsChanged(UpdateChannelAccountRequest{
		Name:              &name,
		OutboundEnabled:   &outbound,
		AIReplyEnabled:    &aiReply,
		IsDefaultIncoming: &defaultIncoming,
	}))

	config := models.JSONB{"relay_url": "https://attacker.invalid"}
	assert.True(t, managedMetaMessengerProtectedFieldsChanged(UpdateChannelAccountRequest{Config: &config}))
	capabilities := models.JSONB{"text": false}
	assert.True(t, managedMetaMessengerProtectedFieldsChanged(UpdateChannelAccountRequest{Capabilities: &capabilities}))
	assert.True(t, managedMetaMessengerProtectedFieldsChanged(UpdateChannelAccountRequest{
		OutboundSecret: "replacement-secret-that-must-not-be-accepted",
	}))
}
