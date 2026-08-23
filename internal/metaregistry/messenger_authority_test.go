package metaregistry

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
)

func TestMessengerBusinessAuthorityCurrent(t *testing.T) {
	t.Parallel()

	checkedAt := time.Now().UTC().Truncate(time.Microsecond)
	oauthID := uuid.New()
	base := models.JSONB{
		"meta_authorization_token_kind": "SYSTEM_USER",
		"meta_granted_scopes": []string{
			"public_profile", "pages_show_list", "pages_manage_metadata", "pages_messaging",
		},
		"meta_ownership_checked_at":                       checkedAt.Format(time.RFC3339Nano),
		"meta_platform_app_id":                            "100000000000001",
		"meta_authorizing_user_id":                        "900000000000001",
		"meta_business_id":                                "200000000000001",
		MessengerBusinessAuthorityMetadataKey:             MessengerBusinessAuthoritySystemUserExactEdges,
		MessengerBusinessAuthorityCheckedAtMetadataKey:    checkedAt.Format(time.RFC3339Nano),
		MessengerBusinessAuthorityOAuthIDMetadataKey:      oauthID.String(),
		MessengerBusinessAuthorityOAuthVersionMetadataKey: 2,
		MessengerBusinessAuthorityAppIDMetadataKey:        "100000000000001",
		MessengerBusinessAuthorityUserIDMetadataKey:       "900000000000001",
		MessengerBusinessAuthorityBusinessIDMetadataKey:   "200000000000001",
		MessengerBusinessAuthorityPageIDMetadataKey:       "700000000000001",
	}

	assert.True(t, MessengerBusinessAuthorityMetadataValid(base, "700000000000001"))
	assert.True(t, MessengerBusinessAuthorityCurrent(base, "700000000000001", oauthID, 2))
	assert.True(t, MessengerBusinessAuthorityGenerationBound(base, "700000000000001", oauthID, 2))

	staleOwnership := cloneMessengerAuthorityJSONB(base)
	staleOwnership["meta_ownership_checked_at"] = checkedAt.Add(time.Second).Format(time.RFC3339Nano)
	assert.False(t, MessengerBusinessAuthorityCurrent(staleOwnership, "700000000000001", oauthID, 2))
	assert.True(t, MessengerBusinessAuthorityGenerationBound(
		staleOwnership,
		"700000000000001",
		oauthID,
		2,
	))

	for _, testCase := range []struct {
		name    string
		mutate  func(models.JSONB)
		oauth   uuid.UUID
		version int
	}{
		{name: "marker missing", mutate: func(metadata models.JSONB) {
			delete(metadata, MessengerBusinessAuthorityMetadataKey)
		}},
		{name: "unknown marker", mutate: func(metadata models.JSONB) {
			metadata[MessengerBusinessAuthorityMetadataKey] = "unknown_v2"
		}},
		{name: "user token cannot borrow BISU proof", mutate: func(metadata models.JSONB) {
			metadata["meta_authorization_token_kind"] = "USER"
		}},
		{name: "core scope missing", mutate: func(metadata models.JSONB) {
			metadata["meta_granted_scopes"] = []string{"public_profile", "pages_show_list", "pages_messaging"}
		}},
		{name: "proof timestamp differs from ownership", mutate: func(metadata models.JSONB) {
			metadata[MessengerBusinessAuthorityCheckedAtMetadataKey] = checkedAt.Add(-time.Second).Format(time.RFC3339Nano)
		}},
		{name: "identity mismatch", mutate: func(metadata models.JSONB) {
			metadata[MessengerBusinessAuthorityBusinessIDMetadataKey] = "200000000000099"
		}},
		{name: "old OAuth ID", oauth: uuid.New(), version: 2},
		{name: "old OAuth version", oauth: oauthID, version: 3},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			metadata := cloneMessengerAuthorityJSONB(base)
			if testCase.mutate != nil {
				testCase.mutate(metadata)
			}
			currentOAuth := testCase.oauth
			if currentOAuth == uuid.Nil {
				currentOAuth = oauthID
			}
			currentVersion := testCase.version
			if currentVersion == 0 {
				currentVersion = 2
			}
			assert.False(t, MessengerBusinessAuthorityCurrent(
				metadata, "700000000000001", currentOAuth, currentVersion,
			))
		})
	}

	legacy := cloneMessengerAuthorityJSONB(base)
	delete(legacy, MessengerBusinessAuthorityMetadataKey)
	legacy["meta_granted_scopes"] = append(
		legacy["meta_granted_scopes"].([]string),
		"business_management",
	)
	assert.True(t, MessengerBusinessAuthorityCurrent(
		legacy, "700000000000001", uuid.New(), 99,
	))
}

func cloneMessengerAuthorityJSONB(source models.JSONB) models.JSONB {
	clone := make(models.JSONB, len(source))
	for key, value := range source {
		if values, ok := value.([]string); ok {
			clone[key] = append([]string(nil), values...)
			continue
		}
		clone[key] = value
	}
	return clone
}
