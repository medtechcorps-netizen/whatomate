package channel

import (
	"sort"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
)

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
