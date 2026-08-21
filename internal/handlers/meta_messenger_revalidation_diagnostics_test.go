package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	metaMessengerRevalidationTestAuthorityToken = "synthetic-system-user-token-never-log"
	metaMessengerRevalidationTestPageToken      = "synthetic-page-token-never-log"
	metaMessengerRevalidationTestAssignedToken  = "synthetic-assigned-token-never-log"
	metaMessengerRevalidationTestProviderBody   = "provider-response-body-secret-never-log"
	metaMessengerRevalidationTestPageName       = "Private Patient Page Name Never Log"
	metaMessengerRevalidationTestBusinessName   = "Private Business Name Never Log"
	metaMessengerRevalidationTestProviderType   = "EAABwzLixnjYBOTypeToken_1234567890"
	metaMessengerRevalidationTestTraceID        = "ArhamZakaria_PrivateTrace_1234567890"
	metaMessengerRevalidationTestRequestID      = "EAAJZARequestToken_1234567890abcdef"
)

func TestMetaMessengerRevalidationLogsExactSafeStageWithoutCredentialsOrProviderBody(t *testing.T) {
	organizationID := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	for _, testCase := range []struct {
		name             string
		wantStage        metaMessengerRevalidationStage
		providerStage    metaMessengerRevalidationStage
		omitPageToken    bool
		omitRequiredTask bool
		userFlow         bool
		selectionInvalid bool
	}{
		{
			name:          "Page accounts provider rejection",
			wantStage:     metaMessengerRevalidationStagePageAccounts,
			providerStage: metaMessengerRevalidationStagePageAccounts,
			userFlow:      true,
		},
		{
			name:          "assigned pages provider rejection",
			wantStage:     metaMessengerRevalidationStageAssignedPages,
			providerStage: metaMessengerRevalidationStageAssignedPages,
		},
		{
			name:          "owned pages provider rejection",
			wantStage:     metaMessengerRevalidationStageOwnedPages,
			providerStage: metaMessengerRevalidationStageOwnedPages,
		},
		{
			name:          "direct Page credential provider rejection",
			wantStage:     metaMessengerRevalidationStageDirectPageCredentialEdge,
			providerStage: metaMessengerRevalidationStageDirectPageCredentialEdge,
		},
		{
			name:             "direct Page credential missing",
			wantStage:        metaMessengerRevalidationStageDirectPageCredentialEdge,
			omitPageToken:    true,
			selectionInvalid: true,
		},
		{
			name:             "final predicates rejected",
			wantStage:        metaMessengerRevalidationStageFinalPredicates,
			omitRequiredTask: true,
			selectionInvalid: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			providerError := func(writer http.ResponseWriter) {
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("X-FB-Request-ID", metaMessengerRevalidationTestRequestID)
				writer.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(
					writer,
					`{"error":{"message":%q,"type":%q,"code":190,"error_subcode":463,"fbtrace_id":%q}}`,
					metaMessengerRevalidationTestProviderBody+" "+
						metaMessengerRevalidationTestAuthorityToken+" "+metaLifecycleTestUserID,
					metaMessengerRevalidationTestProviderType,
					metaMessengerRevalidationTestTraceID,
				)
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				require.Equal(t, "Bearer "+metaMessengerRevalidationTestAuthorityToken, request.Header.Get("Authorization"))
				writer.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/v25.0/me/accounts":
					if testCase.providerStage == metaMessengerRevalidationStagePageAccounts {
						providerError(writer)
						return
					}
					http.Error(writer, "unexpected Page accounts request", http.StatusNotFound)
				case "/v25.0/" + metaLifecycleTestUserID + "/assigned_pages":
					if testCase.providerStage == metaMessengerRevalidationStageAssignedPages {
						providerError(writer)
						return
					}
					tasks := `["MESSAGING","MODERATE"]`
					if testCase.omitRequiredTask {
						tasks = `["MESSAGING"]`
					}
					_, _ = fmt.Fprintf(
						writer,
						`{"data":[{"id":%q,"name":%q,"tasks":%s,"access_token":%q}]}`,
						metaLifecycleTestPageID,
						metaMessengerRevalidationTestPageName,
						tasks,
						metaMessengerRevalidationTestAssignedToken,
					)
				case "/v25.0/" + metaLifecycleTestBusinessID + "/owned_pages":
					if testCase.providerStage == metaMessengerRevalidationStageOwnedPages {
						providerError(writer)
						return
					}
					_, _ = fmt.Fprintf(
						writer,
						`{"data":[{"id":%q,"name":%q}]}`,
						metaLifecycleTestPageID,
						metaMessengerRevalidationTestPageName,
					)
				case "/v25.0/" + metaLifecycleTestPageID:
					if testCase.providerStage == metaMessengerRevalidationStageDirectPageCredentialEdge {
						providerError(writer)
						return
					}
					pageToken := metaMessengerRevalidationTestPageToken
					if testCase.omitPageToken {
						pageToken = ""
					}
					_, _ = fmt.Fprintf(
						writer,
						`{"id":%q,"name":%q,"access_token":%q}`,
						metaLifecycleTestPageID,
						metaMessengerRevalidationTestPageName,
						pageToken,
					)
				default:
					http.Error(writer, "unexpected endpoint", http.StatusNotFound)
				}
			}))
			defer server.Close()

			var logs bytes.Buffer
			app := newMetaLifecycleGraphApp(t, server)
			app.Log = metaMessengerDebugTestLogger(&logs)
			tokenKind := metaMessengerTokenKindSystemUser
			if testCase.userFlow {
				tokenKind = metaMessengerTokenKindUser
			}
			fresh, err := app.revalidateMetaMessengerOwnedPage(
				t.Context(),
				organizationID,
				metaMessengerRevalidationTestAuthorityToken,
				metaMessengerTokenInspection{
					AppID:     metaLifecycleTestAppID,
					Type:      tokenKind,
					UserID:    metaLifecycleTestUserID,
					Scopes:    append([]string(nil), metaMessengerRequiredScopes...),
					CheckedAt: time.Now().UTC(),
				},
				metaMessengerStoredPage{metaMessengerPageSummary: metaMessengerPageSummary{
					BusinessID:   metaLifecycleTestBusinessID,
					BusinessName: metaMessengerRevalidationTestBusinessName,
					PageID:       metaLifecycleTestPageID,
					PageName:     metaMessengerRevalidationTestPageName,
					Ownership:    metaMessengerOwnershipOwned,
					Selectable:   true,
				}},
			)
			require.Error(t, err)
			assert.Empty(t, fresh.EncryptedPageToken)
			var staged *metaMessengerRevalidationError
			require.True(t, errors.As(err, &staged))
			assert.Equal(t, testCase.wantStage, staged.Stage)

			output := logs.String()
			assert.Contains(t, output, "stage="+string(testCase.wantStage))
			assert.Contains(t, output, "organization_id="+organizationID.String())
			assert.Contains(t, output, "page_id="+metaLifecycleTestPageID)
			assert.Contains(t, output, "business_id="+metaLifecycleTestBusinessID)
			if testCase.providerStage != "" {
				var provider *metaMessengerProviderError
				require.True(t, errors.As(err, &provider))
				assert.Contains(t, output, "meta_http_status=400")
				assert.Contains(t, output, "meta_code=190")
				assert.Contains(t, output, "meta_subcode=463")
			} else {
				assert.NotContains(t, output, "meta_http_status=")
				if testCase.selectionInvalid {
					assert.ErrorIs(t, err, errMetaMessengerSelectionInvalid)
				} else {
					assert.NotErrorIs(t, err, errMetaMessengerSelectionInvalid)
				}
			}
			assert.NotContains(t, output, "meta_type=")
			assert.NotContains(t, output, "meta_trace_id=")
			assert.NotContains(t, output, "meta_request_id=")

			combined := err.Error() + output
			for _, forbidden := range []string{
				metaMessengerRevalidationTestAuthorityToken,
				metaMessengerRevalidationTestPageToken,
				metaMessengerRevalidationTestAssignedToken,
				metaMessengerRevalidationTestProviderBody,
				metaMessengerRevalidationTestPageName,
				metaMessengerRevalidationTestBusinessName,
				metaMessengerRevalidationTestProviderType,
				metaMessengerRevalidationTestTraceID,
				metaMessengerRevalidationTestRequestID,
				metaLifecycleTestUserID,
			} {
				assert.NotContains(t, combined, forbidden)
			}
		})
	}
}

func TestMetaMessengerCredentialProtectionDiagnosticRedactsLocalCause(t *testing.T) {
	organizationID := uuid.MustParse("99999999-8888-4777-8666-555555555555")
	var logs bytes.Buffer
	app := &App{Log: metaMessengerDebugTestLogger(&logs)}
	err := app.metaMessengerRevalidationFailure(
		organizationID,
		metaMessengerStoredPage{metaMessengerPageSummary: metaMessengerPageSummary{
			BusinessID: metaLifecycleTestBusinessID,
			PageID:     metaLifecycleTestPageID,
		}},
		metaMessengerRevalidationStageCredentialProtection,
		errors.New("credential protection failed for "+metaMessengerRevalidationTestPageToken),
	)
	require.Error(t, err)
	var staged *metaMessengerRevalidationError
	require.True(t, errors.As(err, &staged))
	assert.Equal(t, metaMessengerRevalidationStageCredentialProtection, staged.Stage)
	output := logs.String()
	assert.Contains(t, output, "stage=credential_protection")
	assert.Contains(t, output, "organization_id="+organizationID.String())
	assert.Contains(t, output, "page_id="+metaLifecycleTestPageID)
	assert.Contains(t, output, "business_id="+metaLifecycleTestBusinessID)
	assert.NotContains(t, err.Error()+output, metaMessengerRevalidationTestPageToken)
	assert.NotContains(t, output, "meta_http_status=")
	assert.NotContains(t, output, "meta_type=")
	assert.NotContains(t, output, "meta_trace_id=")
	assert.NotContains(t, output, "meta_request_id=")
}

func TestMetaMessengerRevalidationLogOmitsCharacterSafeTokenShapedProviderStrings(t *testing.T) {
	organizationID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	for _, providerValue := range []string{
		metaMessengerRevalidationTestProviderType,
		metaMessengerRevalidationTestTraceID,
		metaMessengerRevalidationTestRequestID,
	} {
		require.LessOrEqual(t, len(providerValue), 128)
		require.Regexp(t, `^[A-Za-z0-9_.:-]+$`, providerValue)
	}
	var logs bytes.Buffer
	app := &App{Log: metaMessengerDebugTestLogger(&logs)}
	err := app.metaMessengerRevalidationFailure(
		organizationID,
		metaMessengerStoredPage{metaMessengerPageSummary: metaMessengerPageSummary{
			BusinessID: metaLifecycleTestBusinessID,
			PageID:     metaLifecycleTestPageID,
		}},
		metaMessengerRevalidationStageAssignedPages,
		&metaMessengerProviderError{
			StatusCode: http.StatusBadRequest,
			Code:       190,
			Subcode:    463,
			Type:       metaMessengerRevalidationTestProviderType,
			TraceID:    metaMessengerRevalidationTestTraceID,
			RequestID:  metaMessengerRevalidationTestRequestID,
		},
	)
	require.Error(t, err)
	output := logs.String()
	combined := err.Error() + output
	assert.Contains(t, output, "meta_http_status=400")
	assert.Contains(t, output, "meta_code=190")
	assert.Contains(t, output, "meta_subcode=463")
	assert.NotContains(t, output, "meta_type=")
	assert.NotContains(t, output, "meta_trace_id=")
	assert.NotContains(t, output, "meta_request_id=")
	assert.NotContains(t, combined, metaMessengerRevalidationTestProviderType)
	assert.NotContains(t, combined, metaMessengerRevalidationTestTraceID)
	assert.NotContains(t, combined, metaMessengerRevalidationTestRequestID)
}
