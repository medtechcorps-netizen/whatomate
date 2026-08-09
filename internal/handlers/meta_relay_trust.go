package handlers

import (
	"errors"
	"time"

	"github.com/shridarpatil/whatomate/internal/metatrust"
	"github.com/shridarpatil/whatomate/internal/models"
)

func (a *App) trustedMetaRelayBinding(account *models.ChannelAccount) (metatrust.Binding, error) {
	if !isMetaRelayChannelAccount(account) {
		return metatrust.Binding{}, nil
	}
	if a == nil || a.Config == nil {
		return metatrust.Binding{}, errors.New("trusted Meta relay configuration is unavailable")
	}
	return metatrust.Resolve(a.Config.MetaRelay, a.Config.App.Environment, account)
}

func (a *App) trustedMetaRelayOutboundBinding(
	account *models.ChannelAccount,
	now time.Time,
) (metatrust.Binding, error) {
	if !isMetaRelayChannelAccount(account) {
		return metatrust.Binding{}, nil
	}
	if a == nil || a.Config == nil {
		return metatrust.Binding{}, errors.New("trusted Meta relay configuration is unavailable")
	}
	return metatrust.ValidateOutbound(
		a.Config.MetaRelay,
		a.Config.App.Environment,
		account,
		now,
	)
}
