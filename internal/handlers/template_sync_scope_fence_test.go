package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

func TestApp_SyncTemplates_RejectsScopeChangesDuringProviderFetch(t *testing.T) {
	const (
		accountRebound         = "account_rebound"
		roleRevoked            = "selected_role_revoked"
		orgArchived            = "selected_organization_archived"
		membershipLookupFailed = "selected_membership_lookup_failed"
	)
	for _, tc := range []struct {
		name       string
		wantStatus int
		wantError  string
	}{
		{
			name:       accountRebound,
			wantStatus: fasthttp.StatusConflict,
			wantError:  "WhatsApp account changed while syncing templates. Try again.",
		},
		{
			name:       roleRevoked,
			wantStatus: fasthttp.StatusForbidden,
			wantError:  "Insufficient permissions",
		},
		{
			name:       orgArchived,
			wantStatus: fasthttp.StatusForbidden,
			wantError:  "Selected organization is not available",
		},
		{
			name:       membershipLookupFailed,
			wantStatus: fasthttp.StatusInternalServerError,
			wantError:  "Failed to save synced templates",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp(t)
			home := testutil.CreateTestOrganization(t, app.DB)
			org := testutil.CreateTestOrganization(t, app.DB)
			// The still-authorized home role must not rescue the selected role.
			user := createTemplateTestUser(t, app, home.ID, "templates:sync")
			role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "selected-template-sync", []string{"templates:sync"})
			require.NoError(t, app.DB.Create(&models.UserOrganization{
				UserID: user.ID, OrganizationID: org.ID, RoleID: &role.ID,
			}).Error)
			account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
			otherAccount := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
			existing := createTestTemplateInDB(t, app, org.ID, account.Name, "scope_existing", "PENDING")
			otherRow := createTestTemplateInDB(t, app, org.ID, otherAccount.Name, "scope_existing", "PENDING")
			newBusinessID := testutil.NewTestGraphObjectID()
			wantPath := "/" + account.APIVersion + "/" + account.BusinessID + "/message_templates"

			var calls, failedMembershipReads atomic.Int32
			errMembershipLookup := errors.New("synthetic template membership lookup error")
			if tc.name == membershipLookupFailed {
				pool, err := app.DB.DB()
				require.NoError(t, err)
				// Use a private callback registry over the same test pool.
				app.DB = templateSyncPrivateDB(t, app, pool)
				require.NoError(t, app.DB.Callback().Query().Before("gorm:query").Register("test:template_sync_membership_error", func(tx *gorm.DB) {
					if calls.Load() > 0 && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "user_organizations" {
						failedMembershipReads.Add(1)
						tx.AddError(errMembershipLookup)
					}
				}))
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) != 1 || r.Method != http.MethodGet || r.URL.Path != wantPath {
					t.Error("unexpected request to the synthetic template provider")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				err := app.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					var result *gorm.DB
					switch tc.name {
					case accountRebound:
						if err := channel.LockLegacyMetaOrganizationAndWhatsAppAccount(tx, org.ID, account.ID); err != nil {
							return err
						}
						result = tx.Model(&models.WhatsAppAccount{}).
							Where("organization_id = ? AND id = ?", org.ID, account.ID).
							Update("business_id", newBusinessID)
					case roleRevoked:
						// Leave any cached grant in place to exercise the fresh role check.
						result = tx.Where("custom_role_id = ?", role.ID).Delete(&models.RolePermission{})
					case orgArchived:
						// Archive by soft-deleting the organization row while keeping
						// the account, member, and role that previously authorized sync.
						result = tx.Where("id = ?", org.ID).Delete(&models.Organization{})
					case membershipLookupFailed:
						// Leave the selected role intact. Only its lookup will fail.
						return nil
					default:
						return errors.New("unexpected synthetic scope change")
					}
					if result.Error != nil {
						return result.Error
					}
					if result.RowsAffected != 1 {
						return errors.New("expected one synthetic scope change")
					}
					return nil
				})
				if err != nil {
					t.Errorf("change synthetic scope: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []whatsapp.MetaTemplate{
					{ID: "10001", Name: "scope_existing", Language: "en", Category: "UTILITY", Status: "APPROVED",
						Components: []whatsapp.TemplateComponent{{Type: "BODY", Text: "Stale provider response"}}},
					{ID: "10002", Name: "scope_new", Language: "en", Category: "UTILITY", Status: "APPROVED"},
				}})
			}))
			defer server.Close()
			app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, server.URL)

			req := testutil.NewJSONRequest(t, map[string]any{"whatsapp_account": account.Name})
			testutil.SetAuthContext(req, home.ID, user.ID)
			testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
			require.NoError(t, app.Tenant((*handlers.App).SyncTemplates)(req))
			testutil.AssertErrorResponse(t, req, tc.wantStatus, tc.wantError)
			require.EqualValues(t, 1, calls.Load())

			var current models.WhatsAppAccount
			require.NoError(t, app.DB.Where("organization_id = ? AND id = ?", org.ID, account.ID).First(&current).Error)
			assert.Equal(t, account.Name, current.Name)
			assert.Equal(t, account.PhoneID, current.PhoneID)
			switch tc.name {
			case accountRebound:
				assert.Equal(t, newBusinessID, current.BusinessID)
			case roleRevoked:
				var grants int64
				require.NoError(t, app.DB.Model(&models.RolePermission{}).Where("custom_role_id = ?", role.ID).Count(&grants).Error)
				assert.Zero(t, grants)
			case orgArchived:
				var archived models.Organization
				require.NoError(t, app.DB.Unscoped().First(&archived, "id = ?", org.ID).Error)
				assert.True(t, archived.DeletedAt.Valid)
				var membership models.UserOrganization
				require.NoError(t, app.DB.Where("organization_id = ? AND user_id = ?", org.ID, user.ID).First(&membership).Error)
				require.NotNil(t, membership.RoleID)
				assert.Equal(t, role.ID, *membership.RoleID)
				var grants int64
				require.NoError(t, app.DB.Model(&models.RolePermission{}).Where("custom_role_id = ?", role.ID).Count(&grants).Error)
				assert.EqualValues(t, 1, grants)
			case membershipLookupFailed:
				assert.EqualValues(t, 1, failedMembershipReads.Load())
				assert.NotContains(t, string(testutil.GetResponseBody(req)), errMembershipLookup.Error())
			}
			for _, before := range []*models.Template{existing, otherRow} {
				var after models.Template
				require.NoError(t, app.DB.Unscoped().First(&after, "id = ?", before.ID).Error)
				assert.Equal(t, before.WhatsAppAccount, after.WhatsAppAccount)
				assert.Equal(t, before.MetaTemplateID, after.MetaTemplateID)
				assert.Equal(t, before.Status, after.Status)
				assert.Equal(t, before.BodyContent, after.BodyContent)
				assert.Equal(t, before.DeletedAt, after.DeletedAt)
			}
			var count int64
			require.NoError(t, app.DB.Unscoped().Model(&models.Template{}).
				Where("organization_id = ?", org.ID).Count(&count).Error)
			assert.EqualValues(t, 2, count, "the fetched batch must not add templates")
		})
	}
}
