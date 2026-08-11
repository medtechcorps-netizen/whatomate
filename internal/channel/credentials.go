package channel

import (
	"sort"
	"strings"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
)

// ProviderRequiredCredentialKind returns the credential family that a
// provider must use for live delivery. Extra credentials from a separate
// control-plane flow (for example, a managed Messenger OAuth token) do not
// replace this provider-facing credential and must not make the account
// unusable merely by coexisting with it.
func ProviderRequiredCredentialKind(
	channel models.Channel,
	provider string,
) (models.ChannelCredentialKind, bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case RelayProvider:
		return models.ChannelCredentialKindWebhook, true
	case ThreadsProvider:
		if channel == models.ChannelThreads {
			return models.ChannelCredentialKindOAuth, true
		}
	}
	return "", false
}

// CredentialIndexesByPriority returns a stable newest-version-first ordering.
// Callers use indexes so a selected credential can still be updated in the
// original account slice.
func CredentialIndexesByPriority(
	credentials []models.ChannelCredential,
) []int {
	indexes := make([]int, len(credentials))
	for index := range credentials {
		indexes[index] = index
	}
	sort.Slice(indexes, func(i, j int) bool {
		left := &credentials[indexes[i]]
		right := &credentials[indexes[j]]
		if left.Version != right.Version {
			return left.Version > right.Version
		}
		return left.ID.String() < right.ID.String()
	})
	return indexes
}

// CredentialIsCurrent applies the shared status and expiry policy used by
// validation, webhook verification, and outbound delivery.
func CredentialIsCurrent(
	credential *models.ChannelCredential,
	now time.Time,
) bool {
	if credential == nil {
		return false
	}
	if credential.Status != models.ChannelCredentialStatusActive &&
		credential.Status != models.ChannelCredentialStatusExpiring {
		return false
	}
	return credential.ExpiresAt == nil || credential.ExpiresAt.After(now.UTC())
}

// CurrentCredentialCountOfKind counts only current credentials in one
// provider-required family. Callers that release delivery require this count
// to be exactly one so a missing or ambiguous provider secret fails closed.
func CurrentCredentialCountOfKind(
	credentials []models.ChannelCredential,
	kind models.ChannelCredentialKind,
	now time.Time,
) int {
	count := 0
	for index := range credentials {
		credential := &credentials[index]
		if credential.Kind == kind && CredentialIsCurrent(credential, now) {
			count++
		}
	}
	return count
}
