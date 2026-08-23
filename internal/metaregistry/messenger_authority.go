package metaregistry

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
)

var messengerCoreScopes = []string{
	"public_profile",
	"pages_show_list",
	"pages_manage_metadata",
	"pages_messaging",
}

// MessengerBusinessAuthorityMetadataValid validates the self-contained stored
// proof shape. Runtime callers that already hold the current OAuth generation
// must use MessengerBusinessAuthorityCurrent instead.
func MessengerBusinessAuthorityMetadataValid(metadata models.JSONB, externalAccountID string) bool {
	oauthID, err := uuid.Parse(messengerMetadataText(metadata, MessengerBusinessAuthorityOAuthIDMetadataKey))
	if err != nil {
		oauthID = uuid.Nil
	}
	return MessengerBusinessAuthorityCurrent(
		metadata,
		externalAccountID,
		oauthID,
		messengerMetadataInt(metadata, MessengerBusinessAuthorityOAuthVersionMetadataKey),
	)
}

// MessengerBusinessAuthorityCurrent accepts the legacy literal-scope contract
// or a versioned BISU exact-edge proof bound to the current OAuth generation.
// It never treats the fallback marker as a granted Meta permission.
func MessengerBusinessAuthorityCurrent(
	metadata models.JSONB,
	externalAccountID string,
	oauthCredentialID uuid.UUID,
	oauthCredentialVersion int,
) bool {
	granted := messengerGrantedScopes(metadata)
	for _, required := range messengerCoreScopes {
		if !granted[required] {
			return false
		}
	}
	tokenKind := strings.ToUpper(messengerMetadataText(metadata, "meta_authorization_token_kind"))
	if granted["business_management"] {
		return tokenKind == "SYSTEM_USER" || tokenKind == "USER"
	}
	if !MessengerBusinessAuthorityGenerationBound(
		metadata,
		externalAccountID,
		oauthCredentialID,
		oauthCredentialVersion,
	) {
		return false
	}
	checkedAt, err := time.Parse(
		time.RFC3339Nano,
		messengerMetadataText(metadata, MessengerBusinessAuthorityCheckedAtMetadataKey),
	)
	if err != nil || checkedAt.IsZero() {
		return false
	}
	ownershipCheckedAt, err := time.Parse(
		time.RFC3339Nano,
		messengerMetadataText(metadata, "meta_ownership_checked_at"),
	)
	return err == nil && checkedAt.Equal(ownershipCheckedAt)
}

// MessengerBusinessAuthorityGenerationBound validates the immutable identity
// and OAuth-generation portion of an exact BISU proof without requiring its
// timestamp to equal the latest ownership check. The scheduled Graph
// revalidator uses this narrow predicate to recover an account after a stale
// result advanced meta_ownership_checked_at; all normal runtime readers must
// continue using MessengerBusinessAuthorityCurrent.
func MessengerBusinessAuthorityGenerationBound(
	metadata models.JSONB,
	externalAccountID string,
	oauthCredentialID uuid.UUID,
	oauthCredentialVersion int,
) bool {
	granted := messengerGrantedScopes(metadata)
	for _, required := range messengerCoreScopes {
		if !granted[required] {
			return false
		}
	}
	if strings.ToUpper(messengerMetadataText(metadata, "meta_authorization_token_kind")) != "SYSTEM_USER" ||
		messengerMetadataText(metadata, MessengerBusinessAuthorityMetadataKey) !=
			MessengerBusinessAuthoritySystemUserExactEdges ||
		oauthCredentialID == uuid.Nil || oauthCredentialVersion < 1 ||
		messengerMetadataText(metadata, MessengerBusinessAuthorityOAuthIDMetadataKey) != oauthCredentialID.String() ||
		messengerMetadataInt(metadata, MessengerBusinessAuthorityOAuthVersionMetadataKey) != oauthCredentialVersion {
		return false
	}
	checkedAt, err := time.Parse(
		time.RFC3339Nano,
		messengerMetadataText(metadata, MessengerBusinessAuthorityCheckedAtMetadataKey),
	)
	if err != nil || checkedAt.IsZero() {
		return false
	}
	appID := messengerMetadataText(metadata, "meta_platform_app_id")
	userID := messengerMetadataText(metadata, "meta_authorizing_user_id")
	businessID := messengerMetadataText(metadata, "meta_business_id")
	pageID := strings.TrimSpace(externalAccountID)
	return appID != "" && userID != "" && businessID != "" && pageID != "" &&
		messengerMetadataText(metadata, MessengerBusinessAuthorityAppIDMetadataKey) == appID &&
		messengerMetadataText(metadata, MessengerBusinessAuthorityUserIDMetadataKey) == userID &&
		messengerMetadataText(metadata, MessengerBusinessAuthorityBusinessIDMetadataKey) == businessID &&
		messengerMetadataText(metadata, MessengerBusinessAuthorityPageIDMetadataKey) == pageID
}

// MessengerUsesExactSystemUserBusinessAuthority identifies bindings whose
// authority depends on the exact-edge fallback rather than the literal scope.
func MessengerUsesExactSystemUserBusinessAuthority(metadata models.JSONB) bool {
	return !messengerGrantedScopes(metadata)["business_management"] &&
		strings.EqualFold(messengerMetadataText(metadata, "meta_authorization_token_kind"), "SYSTEM_USER") &&
		messengerMetadataText(metadata, MessengerBusinessAuthorityMetadataKey) ==
			MessengerBusinessAuthoritySystemUserExactEdges
}

func messengerGrantedScopes(metadata models.JSONB) map[string]bool {
	granted := make(map[string]bool)
	switch values := metadata["meta_granted_scopes"].(type) {
	case []string:
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value != "" {
				granted[value] = true
			}
		}
	case []any:
		for _, raw := range values {
			value, _ := raw.(string)
			value = strings.TrimSpace(value)
			if value != "" {
				granted[value] = true
			}
		}
	}
	return granted
}

func messengerMetadataText(metadata models.JSONB, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func messengerMetadataInt(metadata models.JSONB, key string) int {
	switch value := metadata[key].(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}
