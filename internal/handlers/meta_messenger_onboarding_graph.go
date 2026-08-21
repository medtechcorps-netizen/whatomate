package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
)

const (
	metaMessengerProductionGraphOrigin = "https://graph.facebook.com"
	metaMessengerGraphHTTPTimeout      = 30 * time.Second
	metaMessengerGraphMaxResponse      = int64(2 << 20)
	metaMessengerGraphPageLimit        = 100
	metaMessengerGraphMaxPages         = 20
	metaMessengerGraphMaxBusinesses    = 50
	metaMessengerGraphMaxPageAssets    = 500
	metaMessengerOwnershipOwned        = "owned"
	metaMessengerOwnershipClient       = "client_access"
	metaMessengerOwnershipUnverified   = "unverified"
	metaMessengerDisabledClient        = "client_access_only"
	metaMessengerDisabledUnverified    = "ownership_not_verified"
	metaMessengerDisabledAssignment    = "system_user_assignment_missing"
	metaMessengerDisabledTokenMissing  = "page_token_missing"
	metaMessengerDisabledTarget        = "permission_target_mismatch"
	metaMessengerDisabledTask          = "required_page_tasks_missing"
	metaMessengerTokenKindUser         = "USER"
	metaMessengerTokenKindSystemUser   = "SYSTEM_USER"
)

var metaMessengerRequiredScopes = []string{
	"public_profile",
	"pages_show_list",
	"pages_manage_metadata",
	"pages_messaging",
	// Discovery and the final ownership recheck use /me/businesses and
	// /{business-id}/owned_pages. Keep business_management tied to those
	// exact Business edges; it is not a generic Messenger requirement.
	"business_management",
}

var metaMessengerPageTargetScopes = []string{
	"pages_show_list",
	"pages_manage_metadata",
	"pages_messaging",
}

type metaMessengerTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

type metaMessengerGranularScope struct {
	Scope     string    `json:"scope"`
	TargetIDs *[]string `json:"target_ids"`
}

type metaMessengerTokenDebugResponse struct {
	Data struct {
		AppID               string                       `json:"app_id"`
		Type                string                       `json:"type"`
		Application         string                       `json:"application"`
		DataAccessExpiresAt int64                        `json:"data_access_expires_at"`
		ExpiresAt           int64                        `json:"expires_at"`
		IsValid             bool                         `json:"is_valid"`
		IssuedAt            int64                        `json:"issued_at"`
		ProfileID           string                       `json:"profile_id"`
		Scopes              []string                     `json:"scopes"`
		GranularScopes      []metaMessengerGranularScope `json:"granular_scopes"`
		UserID              string                       `json:"user_id"`
	} `json:"data"`
}

type metaMessengerTokenInspection struct {
	AppID                string
	Type                 string
	UserID               string
	Scopes               []string
	GranularScopeTargets map[string]map[string]struct{}
	ExpiresAt            *time.Time
	DataAccessExpiresAt  *time.Time
	CheckedAt            time.Time
}

type metaMessengerPlatformUser struct {
	UserID           string `json:"user_id"`
	Name             string `json:"name"`
	TokenKind        string `json:"token_kind"`
	ClientBusinessID string `json:"client_business_id,omitempty"`
}

type metaMessengerBusinessSummary struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	PermittedRoles []string `json:"permitted_roles,omitempty"`
}

type metaMessengerPageSummary struct {
	BusinessID     string   `json:"business_id,omitempty"`
	BusinessName   string   `json:"business_name,omitempty"`
	PageID         string   `json:"page_id"`
	PageName       string   `json:"page_name"`
	Ownership      string   `json:"ownership"`
	Selectable     bool     `json:"selectable"`
	DisabledReason string   `json:"disabled_reason,omitempty"`
	Tasks          []string `json:"tasks,omitempty"`
}

type metaMessengerStoredPage struct {
	metaMessengerPageSummary
	EncryptedPageToken  string    `json:"encrypted_page_token,omitempty"`
	OwnershipVerifiedAt time.Time `json:"ownership_verified_at,omitempty"`
}

type metaMessengerGraphPageAccess struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Tasks       []string `json:"tasks"`
	AccessToken string   `json:"access_token"`
}

type metaMessengerGraphBusiness struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	PermittedRoles []string `json:"permitted_roles"`
}

type metaMessengerGraphBusinessPage struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type metaMessengerGraphPaging struct {
	Cursors struct {
		After string `json:"after"`
	} `json:"cursors"`
	Next string `json:"next"`
}

type metaMessengerGraphList[T any] struct {
	Data   []T                      `json:"data"`
	Paging metaMessengerGraphPaging `json:"paging"`
}

type metaMessengerGraphErrorEnvelope struct {
	Error struct {
		Type         string `json:"type"`
		Code         int    `json:"code"`
		ErrorSubcode int    `json:"error_subcode"`
		FBTraceID    string `json:"fbtrace_id"`
	} `json:"error"`
}

type metaMessengerProviderError struct {
	StatusCode int
	Code       int
	Subcode    int
	Type       string
	TraceID    string
	RequestID  string
}

type metaMessengerRevalidationStage string

const (
	metaMessengerRevalidationStagePageAccounts         metaMessengerRevalidationStage = "page_accounts"
	metaMessengerRevalidationStageAssignedPages        metaMessengerRevalidationStage = "assigned_pages"
	metaMessengerRevalidationStageOwnedPages           metaMessengerRevalidationStage = "owned_pages"
	metaMessengerRevalidationStageFinalPredicates      metaMessengerRevalidationStage = "final_predicates"
	metaMessengerRevalidationStageCredentialProtection metaMessengerRevalidationStage = "credential_protection"
)

// metaMessengerRevalidationError identifies the exact fail-closed edge without
// copying a provider response or credential into its message. Unwrap preserves
// the existing errors.Is/errors.As behavior used by onboarding and lifecycle
// classification.
type metaMessengerRevalidationError struct {
	Stage metaMessengerRevalidationStage
	cause error
}

func (e *metaMessengerRevalidationError) Error() string {
	if e == nil || e.Stage == "" {
		return "Meta Messenger Page revalidation failed"
	}
	return "Meta Messenger Page revalidation failed at " + string(e.Stage)
}

func (e *metaMessengerRevalidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// metaMessengerRevalidationFailure emits only an allowlisted diagnostic
// envelope. Provider-origin strings are omitted because a character filter
// cannot prove that a type, trace, or request ID is not token-shaped or PII. In
// particular, never attach the error, token, response body, Page name, tasks,
// or authorizing Meta user to this log record.
func (a *App) metaMessengerRevalidationFailure(
	organizationID uuid.UUID,
	selected metaMessengerStoredPage,
	stage metaMessengerRevalidationStage,
	cause error,
) error {
	if cause == nil {
		cause = errMetaMessengerSelectionInvalid
	}
	failure := &metaMessengerRevalidationError{Stage: stage, cause: cause}
	if a == nil || a.Log.Writer == nil {
		return failure
	}

	pageID := strings.TrimSpace(selected.PageID)
	if !validCanonicalMetaID(pageID) {
		pageID = ""
	}
	businessID := strings.TrimSpace(selected.BusinessID)
	if !validCanonicalMetaID(businessID) {
		businessID = ""
	}
	fields := []any{
		"stage", string(stage),
		"organization_id", organizationID,
		"page_id", pageID,
		"business_id", businessID,
	}
	var provider *metaMessengerProviderError
	if errors.As(cause, &provider) && provider != nil {
		fields = append(fields,
			"meta_http_status", provider.StatusCode,
			"meta_code", provider.Code,
			"meta_subcode", provider.Subcode,
		)
	}
	a.Log.Warn("Messenger Page ownership revalidation failed", fields...)
	return failure
}

func (e *metaMessengerProviderError) Error() string {
	if e == nil {
		return "Meta provider request failed"
	}
	parts := []string{fmt.Sprintf("HTTP %d", e.StatusCode)}
	if e.Code != 0 {
		parts = append(parts, fmt.Sprintf("code %d", e.Code))
	}
	if e.Subcode != 0 {
		parts = append(parts, fmt.Sprintf("subcode %d", e.Subcode))
	}
	if e.Type != "" {
		parts = append(parts, "type "+e.Type)
	}
	if e.TraceID != "" {
		parts = append(parts, "trace "+e.TraceID)
	}
	if e.RequestID != "" {
		parts = append(parts, "request "+e.RequestID)
	}
	return "Meta provider request failed (" + strings.Join(parts, ", ") + ")"
}

func (a *App) exchangeMetaMessengerAuthorizationCode(
	ctx context.Context,
	code string,
) (metaMessengerTokenResponse, error) {
	var response metaMessengerTokenResponse
	settings, err := a.metaMessengerOnboardingSettings()
	if err != nil {
		return response, err
	}
	err = a.doMetaMessengerGraphJSON(ctx, http.MethodPost, "oauth/access_token", url.Values{
		"client_id":     {settings.AppID},
		"client_secret": {settings.AppSecret},
		"code":          {strings.TrimSpace(code)},
	}, "", &response)
	if err != nil {
		return response, err
	}
	if strings.TrimSpace(response.AccessToken) == "" {
		return response, errors.New("meta authorization code exchange returned no access token")
	}
	return response, nil
}

func (a *App) inspectMetaMessengerToken(
	ctx context.Context,
	accessToken string,
	requireUserScopes bool,
) (metaMessengerTokenInspection, error) {
	settings, err := a.metaMessengerOnboardingSettings()
	if err != nil {
		return metaMessengerTokenInspection{}, err
	}
	var response metaMessengerTokenDebugResponse
	appAccessToken := settings.AppID + "|" + settings.AppSecret
	// Meta exposes debug_token as a read-only GET edge and requires the token
	// being inspected in input_token. Keep the app access token in the
	// Authorization header; doMetaMessengerGraphJSON also discards transport
	// errors that could echo the required query string.
	err = a.doMetaMessengerGraphJSON(ctx, http.MethodGet, "debug_token", url.Values{
		"input_token": {strings.TrimSpace(accessToken)},
	}, appAccessToken, &response)
	if err != nil {
		return metaMessengerTokenInspection{}, err
	}
	data := response.Data
	now := time.Now().UTC()
	if !data.IsValid || strings.TrimSpace(data.AppID) != settings.AppID {
		return metaMessengerTokenInspection{}, errors.New("meta token is invalid or belongs to a different app")
	}
	tokenKind := strings.ToUpper(strings.TrimSpace(data.Type))
	if requireUserScopes && tokenKind != metaMessengerTokenKindSystemUser &&
		(tokenKind != metaMessengerTokenKindUser || !settings.AllowDevelopmentUserToken) {
		return metaMessengerTokenInspection{}, errors.New("meta authorization must return a Business Integration System User token")
	}
	toExpiry := func(unixSeconds int64) (*time.Time, error) {
		if unixSeconds <= 0 {
			return nil, nil
		}
		value := time.Unix(unixSeconds, 0).UTC()
		if !value.After(now) {
			return nil, errors.New("meta token or data access has expired")
		}
		return &value, nil
	}
	expiresAt, err := toExpiry(data.ExpiresAt)
	if err != nil {
		return metaMessengerTokenInspection{}, err
	}
	dataAccessExpiresAt, err := toExpiry(data.DataAccessExpiresAt)
	if err != nil {
		return metaMessengerTokenInspection{}, err
	}

	granted := make(map[string]struct{}, len(data.Scopes)+len(data.GranularScopes))
	for _, scope := range data.Scopes {
		scope = strings.TrimSpace(scope)
		if scope != "" {
			granted[scope] = struct{}{}
		}
	}
	granularTargets := make(map[string]map[string]struct{}, len(data.GranularScopes))
	for _, granular := range data.GranularScopes {
		scope := strings.TrimSpace(granular.Scope)
		if scope == "" {
			continue
		}
		granted[scope] = struct{}{}
		// Meta's Debug Token contract makes target_ids optional: when the
		// permission applies to all targets, target_ids is not shown. Preserve
		// the distinction between an omitted/null field and a present array.
		// https://developers.facebook.com/docs/graph-api/reference/debug_token/
		if granular.TargetIDs == nil {
			continue
		}
		targets := make(map[string]struct{}, len(*granular.TargetIDs))
		for _, targetID := range *granular.TargetIDs {
			targetID = strings.TrimSpace(targetID)
			if targetID != "" {
				targets[targetID] = struct{}{}
			}
		}
		// A present but empty/invalid array is not the documented applies-to-all
		// representation, so retain an explicit empty restriction fail-closed.
		granularTargets[scope] = targets
	}
	if requireUserScopes {
		if missing := missingMetaMessengerScopes(granted); len(missing) > 0 {
			return metaMessengerTokenInspection{}, fmt.Errorf(
				"meta authorization is missing required permissions: %s",
				strings.Join(missing, ", "),
			)
		}
	}
	verifiedScopes := make([]string, 0, len(granted))
	for scope := range granted {
		verifiedScopes = append(verifiedScopes, scope)
	}
	sort.Strings(verifiedScopes)
	userID := strings.TrimSpace(data.UserID)
	if userID == "" {
		userID = strings.TrimSpace(data.ProfileID)
	}
	if requireUserScopes && !validCanonicalMetaID(userID) {
		return metaMessengerTokenInspection{}, errors.New("meta user token identity is missing or invalid")
	}
	return metaMessengerTokenInspection{
		AppID:                strings.TrimSpace(data.AppID),
		Type:                 tokenKind,
		UserID:               userID,
		Scopes:               verifiedScopes,
		GranularScopeTargets: granularTargets,
		ExpiresAt:            expiresAt,
		DataAccessExpiresAt:  dataAccessExpiresAt,
		CheckedAt:            now,
	}, nil
}

func missingMetaMessengerScopes(granted map[string]struct{}) []string {
	missing := make([]string, 0, len(metaMessengerRequiredScopes))
	for _, required := range metaMessengerRequiredScopes {
		if _, ok := granted[required]; !ok {
			missing = append(missing, required)
		}
	}
	return missing
}

func (inspection metaMessengerTokenInspection) targetAllowed(scope, targetID string) bool {
	targets, restricted := inspection.GranularScopeTargets[scope]
	if !restricted {
		return true
	}
	_, allowed := targets[strings.TrimSpace(targetID)]
	return allowed
}

func (a *App) discoverMetaMessengerInventory(
	ctx context.Context,
	userToken string,
	inspection metaMessengerTokenInspection,
) (metaMessengerPlatformUser, []metaMessengerBusinessSummary, []metaMessengerStoredPage, error) {
	platform, err := a.fetchMetaMessengerPlatformIdentity(ctx, userToken, inspection)
	if err != nil {
		return metaMessengerPlatformUser{}, nil, nil, err
	}
	if strings.EqualFold(strings.TrimSpace(inspection.Type), metaMessengerTokenKindSystemUser) {
		businesses, pages, discoveryErr := a.discoverMetaMessengerSystemUserInventory(
			ctx,
			userToken,
			inspection,
			platform,
		)
		return platform, businesses, pages, discoveryErr
	}

	pageAccess, err := fetchMetaMessengerGraphList[metaMessengerGraphPageAccess](
		a,
		ctx,
		"me/accounts",
		url.Values{"fields": {"id,name,tasks,access_token"}},
		userToken,
		metaMessengerGraphMaxPageAssets,
	)
	if err != nil {
		return platform, nil, nil, err
	}
	graphBusinesses, err := fetchMetaMessengerGraphList[metaMessengerGraphBusiness](
		a,
		ctx,
		"me/businesses",
		url.Values{"fields": {"id,name,permitted_roles"}},
		userToken,
		metaMessengerGraphMaxBusinesses,
	)
	if err != nil {
		return platform, nil, nil, err
	}

	accessByPage := make(map[string]metaMessengerGraphPageAccess, len(pageAccess))
	for _, page := range pageAccess {
		page.ID = strings.TrimSpace(page.ID)
		page.Name = strings.TrimSpace(page.Name)
		page.AccessToken = strings.TrimSpace(page.AccessToken)
		if !validCanonicalMetaID(page.ID) {
			continue
		}
		page.Tasks = normalizedMetaMessengerValues(page.Tasks)
		accessByPage[page.ID] = page
	}

	businesses := make([]metaMessengerBusinessSummary, 0, len(graphBusinesses))
	storedPages := make([]metaMessengerStoredPage, 0, len(accessByPage))
	ownedCandidate := make(map[string]bool, len(accessByPage))
	for _, business := range graphBusinesses {
		business.ID = strings.TrimSpace(business.ID)
		business.Name = strings.TrimSpace(business.Name)
		if !validCanonicalMetaID(business.ID) {
			continue
		}
		business.PermittedRoles = normalizedMetaMessengerValues(business.PermittedRoles)
		businesses = append(businesses, metaMessengerBusinessSummary(business))
		ownedPages, fetchErr := fetchMetaMessengerGraphList[metaMessengerGraphBusinessPage](
			a,
			ctx,
			url.PathEscape(business.ID)+"/owned_pages",
			url.Values{"fields": {"id,name"}},
			userToken,
			metaMessengerGraphMaxPageAssets,
		)
		if fetchErr != nil {
			return platform, nil, nil, fetchErr
		}
		clientPages, fetchErr := fetchMetaMessengerGraphList[metaMessengerGraphBusinessPage](
			a,
			ctx,
			url.PathEscape(business.ID)+"/client_pages",
			url.Values{"fields": {"id,name"}},
			userToken,
			metaMessengerGraphMaxPageAssets,
		)
		if fetchErr != nil {
			return platform, nil, nil, fetchErr
		}
		ownedInBusiness := make(map[string]struct{}, len(ownedPages))
		for _, page := range ownedPages {
			page.ID = strings.TrimSpace(page.ID)
			access, accessible := accessByPage[page.ID]
			if !accessible {
				continue
			}
			ownedInBusiness[page.ID] = struct{}{}
			ownedCandidate[page.ID] = true
			candidate := metaMessengerPageSummary{
				BusinessID:   business.ID,
				BusinessName: business.Name,
				PageID:       page.ID,
				PageName:     firstNonemptyMetaMessengerValue(strings.TrimSpace(page.Name), access.Name),
				Ownership:    metaMessengerOwnershipOwned,
				Selectable:   true,
				Tasks:        access.Tasks,
			}
			if access.AccessToken == "" {
				candidate.Selectable = false
				candidate.DisabledReason = metaMessengerDisabledTokenMissing
			} else if !metaMessengerHasRequiredPageTasks(access.Tasks) {
				candidate.Selectable = false
				candidate.DisabledReason = metaMessengerDisabledTask
			} else if !metaMessengerCandidateTargetsAllowed(inspection, business.ID, page.ID) {
				candidate.Selectable = false
				candidate.DisabledReason = metaMessengerDisabledTarget
			}
			encryptedToken := ""
			if candidate.Selectable {
				encryptedToken, fetchErr = appcrypto.Encrypt(access.AccessToken, a.integrationEncryptionKey())
				if fetchErr != nil || !appcrypto.IsEncrypted(encryptedToken) {
					return platform, nil, nil, errors.New("meta Page token could not be protected")
				}
			}
			storedPages = append(storedPages, metaMessengerStoredPage{
				metaMessengerPageSummary: candidate,
				EncryptedPageToken:       encryptedToken,
				OwnershipVerifiedAt:      inspection.CheckedAt,
			})
		}
		for _, page := range clientPages {
			page.ID = strings.TrimSpace(page.ID)
			if _, alsoOwned := ownedInBusiness[page.ID]; alsoOwned {
				continue
			}
			access, accessible := accessByPage[page.ID]
			if !accessible {
				continue
			}
			storedPages = append(storedPages, metaMessengerStoredPage{
				metaMessengerPageSummary: metaMessengerPageSummary{
					BusinessID:     business.ID,
					BusinessName:   business.Name,
					PageID:         page.ID,
					PageName:       firstNonemptyMetaMessengerValue(strings.TrimSpace(page.Name), access.Name),
					Ownership:      metaMessengerOwnershipClient,
					Selectable:     false,
					DisabledReason: metaMessengerDisabledClient,
					Tasks:          access.Tasks,
				},
			})
		}
	}
	for pageID, access := range accessByPage {
		if ownedCandidate[pageID] {
			continue
		}
		alreadyRepresented := false
		for _, page := range storedPages {
			if page.PageID == pageID {
				alreadyRepresented = true
				break
			}
		}
		if alreadyRepresented {
			continue
		}
		storedPages = append(storedPages, metaMessengerStoredPage{
			metaMessengerPageSummary: metaMessengerPageSummary{
				PageID:         pageID,
				PageName:       access.Name,
				Ownership:      metaMessengerOwnershipUnverified,
				Selectable:     false,
				DisabledReason: metaMessengerDisabledUnverified,
				Tasks:          access.Tasks,
			},
		})
	}

	sortMetaMessengerInventory(businesses, storedPages)
	return platform, businesses, storedPages, nil
}

func (a *App) fetchMetaMessengerPlatformIdentity(
	ctx context.Context,
	accessToken string,
	inspection metaMessengerTokenInspection,
) (metaMessengerPlatformUser, error) {
	tokenKind := strings.ToUpper(strings.TrimSpace(inspection.Type))
	fields := ""
	switch tokenKind {
	case metaMessengerTokenKindUser:
		fields = "id,name"
	case metaMessengerTokenKindSystemUser:
		fields = "id,client_business_id"
	default:
		return metaMessengerPlatformUser{}, errors.New("meta authorization token kind is unsupported")
	}
	var profile struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		ClientBusinessID string `json:"client_business_id"`
	}
	if err := a.doMetaMessengerGraphJSON(ctx, http.MethodGet, "me", url.Values{
		"fields": {fields},
	}, accessToken, &profile); err != nil {
		return metaMessengerPlatformUser{}, err
	}
	platform := metaMessengerPlatformUser{
		UserID:           strings.TrimSpace(profile.ID),
		Name:             strings.TrimSpace(profile.Name),
		TokenKind:        tokenKind,
		ClientBusinessID: strings.TrimSpace(profile.ClientBusinessID),
	}
	if platform.UserID == "" || (inspection.UserID != "" && platform.UserID != inspection.UserID) {
		return platform, errors.New("meta authorization identity did not match the inspected token")
	}
	switch platform.TokenKind {
	case metaMessengerTokenKindUser:
		platform.ClientBusinessID = ""
	case metaMessengerTokenKindSystemUser:
		if !validCanonicalMetaID(platform.ClientBusinessID) {
			return platform, errors.New("business Integration System User client_business_id is missing or invalid")
		}
		if platform.Name == "" {
			platform.Name = "Business Integration System User"
		}
	default:
		return platform, errors.New("meta authorization token kind is unsupported")
	}
	return platform, nil
}

func (a *App) discoverMetaMessengerSystemUserInventory(
	ctx context.Context,
	accessToken string,
	inspection metaMessengerTokenInspection,
	platform metaMessengerPlatformUser,
) ([]metaMessengerBusinessSummary, []metaMessengerStoredPage, error) {
	businessID := platform.ClientBusinessID
	assignedPages, err := fetchMetaMessengerGraphList[metaMessengerGraphPageAccess](
		a,
		ctx,
		url.PathEscape(platform.UserID)+"/assigned_pages",
		url.Values{"fields": {"id,name,tasks"}},
		accessToken,
		metaMessengerGraphMaxPageAssets,
	)
	if err != nil {
		return nil, nil, err
	}
	ownedPages, err := fetchMetaMessengerGraphList[metaMessengerGraphBusinessPage](
		a,
		ctx,
		url.PathEscape(businessID)+"/owned_pages",
		url.Values{"fields": {"id,name"}},
		accessToken,
		metaMessengerGraphMaxPageAssets,
	)
	if err != nil {
		return nil, nil, err
	}
	clientPages, err := fetchMetaMessengerGraphList[metaMessengerGraphBusinessPage](
		a,
		ctx,
		url.PathEscape(businessID)+"/client_pages",
		url.Values{"fields": {"id,name"}},
		accessToken,
		metaMessengerGraphMaxPageAssets,
	)
	if err != nil {
		return nil, nil, err
	}
	// assigned_pages is the authority edge for this BISU: its tasks are the
	// roles actually granted to the system user. Business Page
	// permitted_tasks describe assignable capabilities and must never satisfy
	// the Messenger authorization check.
	businessName := "Business Portfolio " + businessID
	pages := make([]metaMessengerStoredPage, 0, len(ownedPages)+len(clientPages))
	assignedByPage := make(map[string]metaMessengerGraphPageAccess, len(assignedPages))
	for _, page := range assignedPages {
		page.ID = strings.TrimSpace(page.ID)
		if !validCanonicalMetaID(page.ID) {
			continue
		}
		page.Name = strings.TrimSpace(page.Name)
		page.Tasks = normalizedMetaMessengerValues(page.Tasks)
		assignedByPage[page.ID] = page
	}
	ownedIDs := make(map[string]struct{}, len(ownedPages))
	for _, page := range ownedPages {
		page.ID = strings.TrimSpace(page.ID)
		if !validCanonicalMetaID(page.ID) {
			continue
		}
		ownedIDs[page.ID] = struct{}{}
		access, assigned := assignedByPage[page.ID]
		candidate := metaMessengerPageSummary{
			BusinessID:   businessID,
			BusinessName: businessName,
			PageID:       page.ID,
			PageName:     firstNonemptyMetaMessengerValue(strings.TrimSpace(page.Name), access.Name),
			Ownership:    metaMessengerOwnershipOwned,
			Selectable:   true,
			Tasks:        access.Tasks,
		}
		if !assigned {
			candidate.Selectable = false
			candidate.DisabledReason = metaMessengerDisabledAssignment
		} else if !metaMessengerHasRequiredPageTasks(access.Tasks) {
			candidate.Selectable = false
			candidate.DisabledReason = metaMessengerDisabledTask
		} else if !metaMessengerCandidateTargetsAllowed(inspection, businessID, page.ID) {
			candidate.Selectable = false
			candidate.DisabledReason = metaMessengerDisabledTarget
		}
		encryptedToken := ""
		if candidate.Selectable {
			encryptedToken, err = appcrypto.Encrypt(accessToken, a.integrationEncryptionKey())
			if err != nil || !appcrypto.IsEncrypted(encryptedToken) {
				return nil, nil, errors.New("meta Page token could not be protected")
			}
		}
		pages = append(pages, metaMessengerStoredPage{
			metaMessengerPageSummary: candidate,
			EncryptedPageToken:       encryptedToken,
			OwnershipVerifiedAt:      inspection.CheckedAt,
		})
	}
	for _, page := range clientPages {
		page.ID = strings.TrimSpace(page.ID)
		if !validCanonicalMetaID(page.ID) {
			continue
		}
		if _, owned := ownedIDs[page.ID]; owned {
			continue
		}
		access := assignedByPage[page.ID]
		pages = append(pages, metaMessengerStoredPage{
			metaMessengerPageSummary: metaMessengerPageSummary{
				BusinessID:     businessID,
				BusinessName:   businessName,
				PageID:         page.ID,
				PageName:       firstNonemptyMetaMessengerValue(strings.TrimSpace(page.Name), access.Name),
				Ownership:      metaMessengerOwnershipClient,
				Selectable:     false,
				DisabledReason: metaMessengerDisabledClient,
				Tasks:          access.Tasks,
			},
		})
	}
	businesses := []metaMessengerBusinessSummary{{ID: businessID, Name: businessName}}
	sortMetaMessengerInventory(businesses, pages)
	return businesses, pages, nil
}

func sortMetaMessengerInventory(
	businesses []metaMessengerBusinessSummary,
	storedPages []metaMessengerStoredPage,
) {
	sort.Slice(businesses, func(i, j int) bool {
		if businesses[i].Name == businesses[j].Name {
			return businesses[i].ID < businesses[j].ID
		}
		return businesses[i].Name < businesses[j].Name
	})
	sort.Slice(storedPages, func(i, j int) bool {
		if storedPages[i].Selectable != storedPages[j].Selectable {
			return storedPages[i].Selectable
		}
		if storedPages[i].BusinessName != storedPages[j].BusinessName {
			return storedPages[i].BusinessName < storedPages[j].BusinessName
		}
		if storedPages[i].PageName != storedPages[j].PageName {
			return storedPages[i].PageName < storedPages[j].PageName
		}
		if storedPages[i].BusinessID != storedPages[j].BusinessID {
			return storedPages[i].BusinessID < storedPages[j].BusinessID
		}
		return storedPages[i].PageID < storedPages[j].PageID
	})
}

func metaMessengerHasMessagingTask(tasks []string) bool {
	for _, task := range tasks {
		switch strings.ToUpper(strings.TrimSpace(task)) {
		case "MESSAGING", "PROFILE_PLUS_MESSAGING":
			return true
		}
	}
	return false
}

func metaMessengerHasModerationTask(tasks []string) bool {
	for _, task := range tasks {
		if strings.EqualFold(strings.TrimSpace(task), "MODERATE") {
			return true
		}
	}
	return false
}

// metaMessengerHasRequiredPageTasks is the Page-authority fence used during
// discovery and repeated on the fresh Graph payload immediately before the
// Page can be subscribed or persisted. Messenger requires the messaging task;
// ReReply also requires moderation authority for the Page metadata/webhook
// lifecycle it manages. PROFILE_PLUS_MESSAGING is retained because Meta already
// returns it in the supported Graph task payloads; no undocumented aliases are
// accepted.
func metaMessengerHasRequiredPageTasks(tasks []string) bool {
	return metaMessengerHasMessagingTask(tasks) && metaMessengerHasModerationTask(tasks)
}

func metaMessengerCandidateTargetsAllowed(
	inspection metaMessengerTokenInspection,
	businessID, pageID string,
) bool {
	// A SYSTEM_USER is bounded by the freshly inspected client_business_id
	// and the exact intersection of its assigned_pages (including the actual
	// task and Page token) with that Business's owned_pages. Meta may omit
	// target_ids from debug_token for this token kind, so those optional IDs
	// must not override the authoritative BISU edges.
	if strings.EqualFold(strings.TrimSpace(inspection.Type), metaMessengerTokenKindSystemUser) {
		return true
	}
	if !inspection.targetAllowed("business_management", businessID) {
		return false
	}
	for _, scope := range metaMessengerPageTargetScopes {
		if !inspection.targetAllowed(scope, pageID) {
			return false
		}
	}
	return true
}

// revalidateMetaMessengerOwnedPage repeats the two authoritative edges at the
// point of selection. The earlier inventory is only a UI snapshot: it cannot
// authorize persistence after the user's Page task or Business ownership has
// changed. For a USER, /me/accounts supplies the Page token directly. For a
// SYSTEM_USER, assigned_pages proves the exact task assignment while the
// freshly inspected BISU token remains the operational Page credential.
func (a *App) revalidateMetaMessengerOwnedPage(
	ctx context.Context,
	organizationID uuid.UUID,
	userToken string,
	inspection metaMessengerTokenInspection,
	selected metaMessengerStoredPage,
) (metaMessengerStoredPage, error) {
	if selected.Ownership != metaMessengerOwnershipOwned || !selected.Selectable ||
		!validCanonicalMetaID(selected.BusinessID) || !validCanonicalMetaID(selected.PageID) {
		return metaMessengerStoredPage{}, a.metaMessengerRevalidationFailure(
			organizationID, selected, metaMessengerRevalidationStageFinalPredicates,
			errMetaMessengerSelectionInvalid,
		)
	}
	var accessible metaMessengerGraphPageAccess
	ownedName := ""
	switch strings.ToUpper(strings.TrimSpace(inspection.Type)) {
	case metaMessengerTokenKindUser:
		pageAccess, err := fetchMetaMessengerGraphList[metaMessengerGraphPageAccess](
			a,
			ctx,
			"me/accounts",
			url.Values{"fields": {"id,name,tasks,access_token"}},
			userToken,
			metaMessengerGraphMaxPageAssets,
		)
		if err != nil {
			return metaMessengerStoredPage{}, a.metaMessengerRevalidationFailure(
				organizationID, selected, metaMessengerRevalidationStagePageAccounts, err,
			)
		}
		foundAccess := false
		for index := range pageAccess {
			if strings.TrimSpace(pageAccess[index].ID) == selected.PageID {
				accessible = pageAccess[index]
				foundAccess = true
				break
			}
		}
		if !foundAccess {
			return metaMessengerStoredPage{}, a.metaMessengerRevalidationFailure(
				organizationID, selected, metaMessengerRevalidationStagePageAccounts,
				errMetaMessengerSelectionInvalid,
			)
		}
		ownedPages, err := fetchMetaMessengerGraphList[metaMessengerGraphBusinessPage](
			a,
			ctx,
			url.PathEscape(selected.BusinessID)+"/owned_pages",
			url.Values{"fields": {"id,name"}},
			userToken,
			metaMessengerGraphMaxPageAssets,
		)
		if err != nil {
			return metaMessengerStoredPage{}, a.metaMessengerRevalidationFailure(
				organizationID, selected, metaMessengerRevalidationStageOwnedPages, err,
			)
		}
		owned := false
		for _, page := range ownedPages {
			if strings.TrimSpace(page.ID) == selected.PageID {
				owned = true
				ownedName = strings.TrimSpace(page.Name)
				break
			}
		}
		if !owned {
			return metaMessengerStoredPage{}, a.metaMessengerRevalidationFailure(
				organizationID, selected, metaMessengerRevalidationStageOwnedPages,
				errMetaMessengerSelectionInvalid,
			)
		}
	case metaMessengerTokenKindSystemUser:
		pageAccess, err := fetchMetaMessengerGraphList[metaMessengerGraphPageAccess](
			a,
			ctx,
			url.PathEscape(inspection.UserID)+"/assigned_pages",
			url.Values{"fields": {"id,name,tasks"}},
			userToken,
			metaMessengerGraphMaxPageAssets,
		)
		if err != nil {
			return metaMessengerStoredPage{}, a.metaMessengerRevalidationFailure(
				organizationID, selected, metaMessengerRevalidationStageAssignedPages, err,
			)
		}
		foundAccess := false
		for index := range pageAccess {
			if strings.TrimSpace(pageAccess[index].ID) == selected.PageID {
				accessible = pageAccess[index]
				foundAccess = true
				break
			}
		}
		if !foundAccess {
			return metaMessengerStoredPage{}, a.metaMessengerRevalidationFailure(
				organizationID, selected, metaMessengerRevalidationStageAssignedPages,
				errMetaMessengerSelectionInvalid,
			)
		}
		ownedPages, err := fetchMetaMessengerGraphList[metaMessengerGraphBusinessPage](
			a,
			ctx,
			url.PathEscape(selected.BusinessID)+"/owned_pages",
			url.Values{"fields": {"id,name"}},
			userToken,
			metaMessengerGraphMaxPageAssets,
		)
		if err != nil {
			return metaMessengerStoredPage{}, a.metaMessengerRevalidationFailure(
				organizationID, selected, metaMessengerRevalidationStageOwnedPages, err,
			)
		}
		foundOwned := false
		for _, page := range ownedPages {
			if strings.TrimSpace(page.ID) == selected.PageID {
				ownedName = strings.TrimSpace(page.Name)
				foundOwned = true
				break
			}
		}
		if !foundOwned {
			return metaMessengerStoredPage{}, a.metaMessengerRevalidationFailure(
				organizationID, selected, metaMessengerRevalidationStageOwnedPages,
				errMetaMessengerSelectionInvalid,
			)
		}
		// The freshly inspected Business Integration System User token is the
		// operational credential for its assigned Page. assigned_pages proves
		// the exact Page task assignment; owned_pages proves the Business owns
		// it. Meta may return an access_token-looking field on assigned_pages,
		// but that value is not a usable Graph credential for this token kind.
		accessible.AccessToken = strings.TrimSpace(userToken)
	default:
		return metaMessengerStoredPage{}, a.metaMessengerRevalidationFailure(
			organizationID, selected, metaMessengerRevalidationStageFinalPredicates,
			errMetaMessengerSelectionInvalid,
		)
	}
	accessible.AccessToken = strings.TrimSpace(accessible.AccessToken)
	accessible.Tasks = normalizedMetaMessengerValues(accessible.Tasks)
	if accessible.AccessToken == "" || !metaMessengerHasRequiredPageTasks(accessible.Tasks) ||
		!metaMessengerCandidateTargetsAllowed(
			inspection,
			selected.BusinessID,
			selected.PageID,
		) {
		return metaMessengerStoredPage{}, a.metaMessengerRevalidationFailure(
			organizationID, selected, metaMessengerRevalidationStageFinalPredicates,
			errMetaMessengerSelectionInvalid,
		)
	}
	encryptedPageToken, err := appcrypto.Encrypt(
		accessible.AccessToken,
		a.integrationEncryptionKey(),
	)
	if err != nil || !appcrypto.IsEncrypted(encryptedPageToken) {
		return metaMessengerStoredPage{}, a.metaMessengerRevalidationFailure(
			organizationID, selected, metaMessengerRevalidationStageCredentialProtection,
			errors.New("meta Page token could not be protected"),
		)
	}
	selected.PageName = firstNonemptyMetaMessengerValue(
		ownedName,
		strings.TrimSpace(accessible.Name),
		selected.PageName,
	)
	selected.Tasks = accessible.Tasks
	selected.EncryptedPageToken = encryptedPageToken
	selected.OwnershipVerifiedAt = time.Now().UTC()
	return selected, nil
}

func fetchMetaMessengerGraphList[T any](
	a *App,
	ctx context.Context,
	endpoint string,
	baseValues url.Values,
	accessToken string,
	maximum int,
) ([]T, error) {
	if maximum <= 0 {
		return nil, errors.New("meta inventory limit is invalid")
	}
	values := cloneURLValues(baseValues)
	values.Set("limit", fmt.Sprintf("%d", metaMessengerGraphPageLimit))
	result := make([]T, 0)
	for pageNumber := 0; pageNumber < metaMessengerGraphMaxPages; pageNumber++ {
		var page metaMessengerGraphList[T]
		if err := a.doMetaMessengerGraphJSON(
			ctx,
			http.MethodGet,
			endpoint,
			values,
			accessToken,
			&page,
		); err != nil {
			return nil, err
		}
		if len(result)+len(page.Data) > maximum {
			return nil, errors.New("meta inventory exceeded the safe discovery limit")
		}
		result = append(result, page.Data...)
		if strings.TrimSpace(page.Paging.Next) == "" {
			return result, nil
		}
		after := strings.TrimSpace(page.Paging.Cursors.After)
		if after == "" || len(after) > 2048 {
			return nil, errors.New("meta inventory pagination was invalid")
		}
		values.Set("after", after)
	}
	return nil, errors.New("meta inventory exceeded the safe pagination limit")
}

func cloneURLValues(values url.Values) url.Values {
	cloned := make(url.Values, len(values))
	for key, items := range values {
		cloned[key] = append([]string(nil), items...)
	}
	return cloned
}

func normalizedMetaMessengerValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func firstNonemptyMetaMessengerValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "Messenger Page"
}

func validCanonicalMetaID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func (a *App) bindMetaMessengerPageToken(
	ctx context.Context,
	pageID, pageToken string,
	authorityInspection metaMessengerTokenInspection,
) (metaMessengerTokenInspection, string, error) {
	var binding struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	endpoint := "me"
	var inspection metaMessengerTokenInspection
	if strings.EqualFold(strings.TrimSpace(authorityInspection.Type), metaMessengerTokenKindSystemUser) {
		// A freshly inspected BISU token is already bound to the app, system
		// user, and Business. The immediately preceding assigned_pages and
		// owned_pages checks bind it to the exact Page and messaging tasks.
		// Verify that it can read the explicit Page-ID edge; BISU credentials do
		// not use the normal Page-token debug_token or /me surfaces.
		endpoint = url.PathEscape(strings.TrimSpace(pageID))
		inspection = metaMessengerTokenInspection{
			AppID:               authorityInspection.AppID,
			Type:                "PAGE",
			UserID:              strings.TrimSpace(pageID),
			ExpiresAt:           authorityInspection.ExpiresAt,
			DataAccessExpiresAt: authorityInspection.DataAccessExpiresAt,
			CheckedAt:           time.Now().UTC(),
		}
	} else {
		var err error
		inspection, err = a.inspectMetaMessengerToken(ctx, pageToken, false)
		if err != nil {
			return inspection, "", err
		}
	}
	if err := a.doMetaMessengerGraphJSON(ctx, http.MethodGet, endpoint, url.Values{
		"fields": {"id,name"},
	}, pageToken, &binding); err != nil {
		return inspection, "", err
	}
	if strings.TrimSpace(binding.ID) != strings.TrimSpace(pageID) {
		return inspection, "", errors.New("meta Page token is bound to a different Page")
	}
	return inspection, strings.TrimSpace(binding.Name), nil
}

func (a *App) subscribeMetaMessengerPage(
	ctx context.Context,
	pageID, pageToken string,
) error {
	var subscribed struct {
		Success bool `json:"success"`
	}
	endpoint := url.PathEscape(strings.TrimSpace(pageID)) + "/subscribed_apps"
	if err := a.doMetaMessengerGraphJSON(ctx, http.MethodPost, endpoint, url.Values{
		"subscribed_fields": {"messages"},
	}, pageToken, &subscribed); err != nil {
		return err
	}
	if !subscribed.Success {
		return errors.New("meta did not confirm the messages webhook subscription")
	}
	var subscriptions struct {
		Data []struct {
			ID               string   `json:"id"`
			SubscribedFields []string `json:"subscribed_fields"`
		} `json:"data"`
	}
	if err := a.doMetaMessengerGraphJSON(ctx, http.MethodGet, endpoint, url.Values{
		"fields": {"id,name,subscribed_fields"},
	}, pageToken, &subscriptions); err != nil {
		return err
	}
	settings, err := a.metaMessengerOnboardingSettings()
	if err != nil {
		return err
	}
	for _, subscription := range subscriptions.Data {
		if strings.TrimSpace(subscription.ID) != settings.AppID {
			continue
		}
		for _, field := range subscription.SubscribedFields {
			if strings.EqualFold(strings.TrimSpace(field), "messages") {
				return nil
			}
		}
	}
	return errors.New("the configured Meta app messages webhook subscription could not be verified")
}

func (a *App) unsubscribeMetaMessengerPage(
	ctx context.Context,
	pageID, pageToken string,
) error {
	if !validCanonicalMetaID(pageID) || strings.TrimSpace(pageToken) == "" {
		return errors.New("meta Page unsubscribe binding is invalid")
	}
	endpoint := url.PathEscape(strings.TrimSpace(pageID)) + "/subscribed_apps"
	var unsubscribed struct {
		Success bool `json:"success"`
	}
	deleteCtx, cancelDelete := context.WithTimeout(ctx, 15*time.Second)
	deleteErr := a.doMetaMessengerGraphJSON(
		deleteCtx,
		http.MethodDelete,
		endpoint,
		url.Values{},
		pageToken,
		&unsubscribed,
	)
	cancelDelete()
	if deleteErr == nil && !unsubscribed.Success {
		deleteErr = errors.New("meta did not confirm the messages webhook removal")
	}

	// DELETE can time out after Meta has committed it. A separate GET resolves
	// that ambiguity and is also the idempotency check for duplicate cleanup.
	getCtx, cancelGet := context.WithTimeout(ctx, 15*time.Second)
	present, getErr := a.metaMessengerPageHasConfiguredAppSubscription(
		getCtx,
		pageID,
		pageToken,
	)
	cancelGet()
	if getErr == nil && !present {
		return nil
	}
	if getErr != nil {
		return errors.Join(deleteErr, getErr)
	}
	if deleteErr != nil {
		return errors.Join(deleteErr, errors.New("the configured Meta app is still subscribed"))
	}
	return errors.New("the configured Meta app is still subscribed")
}

func (a *App) metaMessengerPageHasConfiguredAppSubscription(
	ctx context.Context, pageID, pageToken string,
) (bool, error) {
	if !validCanonicalMetaID(pageID) || strings.TrimSpace(pageToken) == "" {
		return false, errors.New("meta Page subscription binding is invalid")
	}
	settings, err := a.metaMessengerOnboardingSettings()
	if err != nil {
		return false, err
	}
	endpoint := url.PathEscape(strings.TrimSpace(pageID)) + "/subscribed_apps"
	var subscriptions struct {
		Data []struct {
			ID               string   `json:"id"`
			SubscribedFields []string `json:"subscribed_fields"`
		} `json:"data"`
	}
	if err := a.doMetaMessengerGraphJSON(ctx, http.MethodGet, endpoint, url.Values{
		"fields": {"id,subscribed_fields"},
	}, pageToken, &subscriptions); err != nil {
		return false, err
	}
	for _, subscription := range subscriptions.Data {
		if strings.TrimSpace(subscription.ID) != settings.AppID {
			continue
		}
		for _, field := range subscription.SubscribedFields {
			if strings.EqualFold(strings.TrimSpace(field), "messages") {
				return true, nil
			}
		}
	}
	return false, nil
}

func (a *App) doMetaMessengerGraphJSON(
	ctx context.Context,
	method, endpoint string,
	values url.Values,
	accessToken string,
	destination any,
) error {
	values = cloneURLValues(values)
	if accessToken = strings.TrimSpace(accessToken); accessToken != "" {
		settings, settingsErr := a.metaMessengerOnboardingSettings()
		if settingsErr != nil {
			return settingsErr
		}
		mac := hmac.New(sha256.New, []byte(settings.AppSecret))
		_, _ = mac.Write([]byte(accessToken))
		values.Set("appsecret_proof", hex.EncodeToString(mac.Sum(nil)))
	}
	target, err := a.metaMessengerGraphEndpoint(endpoint)
	if err != nil {
		return err
	}
	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewBufferString(values.Encode())
	} else {
		target.RawQuery = values.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return errors.New("meta request could not be prepared")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Messenger-Onboarding/1.0")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	client := oauthHTTPClientWithoutRedirects(a.HTTPClient)
	client.Timeout = metaMessengerGraphHTTPTimeout
	response, err := client.Do(request)
	if err != nil {
		return errors.New("meta provider request failed")
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(response.Body, metaMessengerGraphMaxResponse+1))
	if err != nil || int64(len(payload)) > metaMessengerGraphMaxResponse {
		return errors.New("meta provider response was invalid")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var envelope metaMessengerGraphErrorEnvelope
		_ = json.Unmarshal(payload, &envelope)
		traceID := safeThreadsOAuthDiagnostic(envelope.Error.FBTraceID)
		if traceID == "" {
			traceID = safeThreadsOAuthDiagnostic(response.Header.Get("X-FB-Trace-ID"))
		}
		return &metaMessengerProviderError{
			StatusCode: response.StatusCode,
			Code:       envelope.Error.Code,
			Subcode:    envelope.Error.ErrorSubcode,
			Type:       safeThreadsOAuthDiagnostic(envelope.Error.Type),
			TraceID:    traceID,
			RequestID:  safeThreadsOAuthDiagnostic(response.Header.Get("X-FB-Request-ID")),
		}
	}
	if destination == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(destination); err != nil {
		return errors.New("meta provider response was invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("meta provider response was invalid")
	}
	return nil
}

func (a *App) metaMessengerGraphEndpoint(endpoint string) (*url.URL, error) {
	settings, err := a.metaMessengerOnboardingSettings()
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(strings.TrimSpace(settings.GraphBaseURL))
	if err != nil || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("meta Graph endpoint is invalid")
	}
	production := a.Config != nil && strings.EqualFold(strings.TrimSpace(a.Config.App.Environment), "production")
	if production && (settings.GraphBaseURL != metaMessengerProductionGraphOrigin ||
		base.Scheme != "https" || base.Host != "graph.facebook.com" ||
		base.Path != "" || base.RawPath != "") {
		return nil, errors.New("meta Graph endpoint is invalid")
	}
	if !strings.EqualFold(base.Scheme, "https") {
		host := strings.Trim(base.Hostname(), "[]")
		ip := net.ParseIP(host)
		developmentLoopback := a.Config != nil && !production &&
			strings.EqualFold(base.Scheme, "http") && ip != nil && ip.IsLoopback()
		if !developmentLoopback {
			return nil, errors.New("meta Graph endpoint is invalid")
		}
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" +
		strings.Trim(settings.GraphAPIVersion, "/") + "/" + strings.TrimLeft(endpoint, "/")
	return base, nil
}
