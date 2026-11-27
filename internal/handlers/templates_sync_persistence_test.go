package handlers_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var errTemplateSyncPersistence = errors.New("injected template sync persistence failure")

// The ordinary test app shares its GORM callback registry with other tests.
// Open a private registry over the borrowed pool; never close that shared pool.
func templateSyncPrivateDB(t *testing.T, app *handlers.App, pool gorm.ConnPool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{
		Logger:               app.DB.Logger,
		DisableAutomaticPing: true,
	})
	require.NoError(t, err)
	return db
}

func templateSyncIsTemplate(tx *gorm.DB) bool {
	return tx.Statement.Schema != nil && tx.Statement.Schema.Table == "templates"
}

func TestApp_SyncTemplates_LaterPageFailureDoesNotPersist(t *testing.T) {
	const cursor = "synthetic-template-continue"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		switch calls.Add(1) {
		case 1:
			assert.False(t, r.URL.Query().Has("after"))
			_, _ = w.Write([]byte(`{"data":[
				{"id":"1","name":"page_one_update","language":"en","status":"APPROVED","category":"UTILITY"},
				{"id":"2","name":"page_one_insert","language":"en","status":"APPROVED","category":"UTILITY"}
			],"paging":{"next":"/ignored","cursors":{"after":"` + cursor + `"}}}`))
		case 2:
			assert.Equal(t, cursor, r.URL.Query().Get("after"))
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"` + cursor + `"}}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	app := newTemplateTestApp(t, server)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createTemplateTestUser(t, app, org.ID, "templates:sync")
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	existing := createTestTemplateInDB(t, app, org.ID, account.Name, "page_one_update", "PENDING")

	req := testutil.NewJSONRequest(t, map[string]any{"whatsapp_account": account.Name})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.SyncTemplates(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadGateway, "Failed to fetch templates from Meta")
	assert.NotContains(t, string(testutil.GetResponseBody(req)), cursor)
	require.EqualValues(t, 2, calls.Load())

	var rows []models.Template
	require.NoError(t, app.DB.Unscoped().Where("organization_id = ? AND whats_app_account = ?", org.ID, account.Name).Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.Equal(t, existing.ID, rows[0].ID)
	assert.Equal(t, existing.MetaTemplateID, rows[0].MetaTemplateID)
	assert.Equal(t, existing.Status, rows[0].Status)
	assert.Equal(t, existing.BodyContent, rows[0].BodyContent)
}

func TestApp_SyncTemplates_PersistenceFailureRollsBack(t *testing.T) {
	for _, failure := range []string{"lookup", "update", "create", "update_zero_rows", "create_zero_rows"} {
		t.Run(failure, func(t *testing.T) {
			server := newMockTemplateServer(t)
			defer server.Close()
			app := newTemplateTestApp(t, server)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := createTemplateTestUser(t, app, org.ID, "templates:sync")
			account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
			var existing *models.Template
			if failure == "update" || failure == "update_zero_rows" {
				existing = createTestTemplateInDB(t, app, org.ID, account.Name, "synced_template_two", "PENDING")
			}

			originalDB := app.DB
			pool, err := originalDB.DB()
			require.NoError(t, err)
			app.DB = templateSyncPrivateDB(t, app, pool)
			armed := true
			queries, creates, updates := 0, 0, 0
			require.NoError(t, app.DB.Callback().Query().Before("gorm:query").Register("test:sync_lookup", func(tx *gorm.DB) {
				if armed && templateSyncIsTemplate(tx) {
					queries++
					if failure == "lookup" {
						tx.AddError(errTemplateSyncPersistence)
					}
				}
			}))
			require.NoError(t, app.DB.Callback().Create().Before("gorm:create").Register("test:sync_create", func(tx *gorm.DB) {
				if armed && templateSyncIsTemplate(tx) {
					creates++
					if failure == "create" && creates == 2 {
						tx.AddError(errTemplateSyncPersistence)
					}
				}
			}))
			require.NoError(t, app.DB.Callback().Update().Before("gorm:update").Register("test:sync_update", func(tx *gorm.DB) {
				if armed && templateSyncIsTemplate(tx) {
					updates++
					if failure == "update" {
						tx.AddError(errTemplateSyncPersistence)
					}
				}
			}))
			if failure == "update_zero_rows" {
				original := app.DB.Callback().Update().Get("gorm:update")
				require.NotNil(t, original)
				require.NoError(t, app.DB.Callback().Update().Replace("gorm:update", func(tx *gorm.DB) {
					if armed && templateSyncIsTemplate(tx) {
						tx.RowsAffected = 0
						return
					}
					original(tx)
				}))
			}
			if failure == "create_zero_rows" {
				original := app.DB.Callback().Create().Get("gorm:create")
				require.NotNil(t, original)
				require.NoError(t, app.DB.Callback().Create().Replace("gorm:create", func(tx *gorm.DB) {
					if armed && templateSyncIsTemplate(tx) && creates == 2 {
						tx.RowsAffected = 0
						return
					}
					original(tx)
				}))
			}

			req := testutil.NewJSONRequest(t, map[string]any{"whatsapp_account": account.Name})
			testutil.SetAuthContext(req, org.ID, user.ID)
			require.NoError(t, app.SyncTemplates(req))
			armed = false
			testutil.AssertErrorResponse(t, req, fasthttp.StatusInternalServerError, "Failed to save synced templates")
			assert.NotContains(t, string(testutil.GetResponseBody(req)), errTemplateSyncPersistence.Error())
			assert.NotContains(t, string(testutil.GetResponseBody(req)), "Synced ")

			var rows []models.Template
			require.NoError(t, originalDB.Unscoped().Where("organization_id = ? AND whats_app_account = ?", org.ID, account.Name).Find(&rows).Error)
			switch failure {
			case "lookup":
				assert.Equal(t, 1, queries)
				assert.Zero(t, creates, "a failed lookup must not fall through to create")
				assert.Zero(t, updates)
				require.Empty(t, rows)
			case "update", "update_zero_rows":
				assert.Equal(t, 2, queries)
				assert.Equal(t, 1, creates, "the first successful write must be rolled back")
				assert.Equal(t, 1, updates)
				require.Len(t, rows, 1)
				assert.Equal(t, existing.ID, rows[0].ID)
				assert.Equal(t, existing.BodyContent, rows[0].BodyContent)
				assert.Empty(t, rows[0].MetaTemplateID)
			case "create", "create_zero_rows":
				assert.Equal(t, 2, queries)
				assert.Equal(t, 2, creates)
				assert.Zero(t, updates)
				require.Empty(t, rows, "the first successful create must be rolled back")
			}
		})
	}
}

// Force the outer handler transaction to fail after its callback returned nil.
// Roll back the real fixture transaction before returning the synthetic error.
// This tests return-value handling; it does not simulate an unknown DB commit.
type templateSyncFailCommitPool struct {
	*sql.DB
}

func (pool *templateSyncFailCommitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := pool.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &templateSyncFailCommitTx{Tx: tx}, nil
}

type templateSyncFailCommitTx struct {
	*sql.Tx
}

func (tx *templateSyncFailCommitTx) Commit() error {
	if err := tx.Tx.Rollback(); err != nil {
		return err
	}
	return errTemplateSyncPersistence
}

func TestApp_SyncTemplates_CommitFailureDoesNotReportSuccess(t *testing.T) {
	server := newMockTemplateServer(t)
	defer server.Close()
	app := newTemplateTestApp(t, server)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createTemplateTestUser(t, app, org.ID, "templates:sync")
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	originalDB := app.DB
	pool, err := originalDB.DB()
	require.NoError(t, err)
	app.DB = templateSyncPrivateDB(t, app, &templateSyncFailCommitPool{DB: pool})

	req := testutil.NewJSONRequest(t, map[string]any{"whatsapp_account": account.Name})
	testutil.SetAuthContext(req, org.ID, user.ID)
	require.NoError(t, app.SyncTemplates(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusInternalServerError, "Failed to save synced templates")
	assert.NotContains(t, string(testutil.GetResponseBody(req)), "Synced ")
	var count int64
	require.NoError(t, originalDB.Model(&models.Template{}).Where("organization_id = ?", org.ID).Count(&count).Error)
	assert.Zero(t, count)
}

func templateSyncCreateVariant(t *testing.T, app *handlers.App, orgID uuid.UUID, account, language string) *models.Template {
	t.Helper()
	row := &models.Template{
		OrganizationID:  orgID,
		WhatsAppAccount: account,
		Name:            "shared_name",
		DisplayName:     "shared_name",
		Language:        language,
		Category:        "MARKETING",
		Status:          "PENDING",
		BodyContent:     "Original body",
	}
	require.NoError(t, app.DB.Create(row).Error)
	return row
}

func TestApp_SyncTemplates_PersistenceSuccessPreservesScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte("{\"data\":[{\"id\":\"new-en\",\"name\":\"shared_name\",\"language\":\"en\",\"category\":\"MARKETING\",\"status\":\"APPROVED\",\"components\":[{\"type\":\"BODY\",\"text\":\"Updated English\"}]},{\"id\":\"new-ms\",\"name\":\"shared_name\",\"language\":\"ms\",\"category\":\"UTILITY\",\"status\":\"APPROVED\",\"components\":[{\"type\":\"BODY\",\"text\":\"Updated Malay\"}]},{\"id\":\"new-created\",\"name\":\"created_name\",\"language\":\"en\",\"category\":\"UTILITY\",\"status\":\"APPROVED\",\"components\":[{\"type\":\"BODY\",\"text\":\"Created body\"}]}]}"))
	}))
	defer server.Close()
	app := newTemplateTestApp(t, server)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createTemplateTestUser(t, app, org.ID, "templates:sync")
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	otherAccount := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	otherOrg := testutil.CreateTestOrganization(t, app.DB)
	english := createTestTemplateInDB(t, app, org.ID, account.Name, "shared_name", "PENDING")
	require.NoError(t, app.DB.Model(english).Update("quality_rating", "HIGH").Error)
	require.NoError(t, app.DB.Delete(english).Error)
	malay := templateSyncCreateVariant(t, app, org.ID, account.Name, "ms")
	otherLanguage := templateSyncCreateVariant(t, app, org.ID, account.Name, "fr")
	otherAccountRow := createTestTemplateInDB(t, app, org.ID, otherAccount.Name, "shared_name", "PENDING")
	// Use the other organization's own account to respect the existing
	// account/name/language unique index.
	otherOrgAccount := testutil.CreateTestWhatsAppAccount(t, app.DB, otherOrg.ID)
	otherOrgRow := createTestTemplateInDB(t, app, otherOrg.ID, otherOrgAccount.Name, "shared_name", "PENDING")

	req := testutil.NewJSONRequest(t, map[string]any{"whatsapp_account": account.Name})
	testutil.SetAuthContext(req, org.ID, user.ID)
	require.NoError(t, app.SyncTemplates(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	var response struct {
		Data struct {
			Count   int
			Message string
		}
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
	assert.Equal(t, 3, response.Data.Count)
	assert.Equal(t, "Synced 3 templates", response.Data.Message)

	var restored, updated, created models.Template
	require.NoError(t, app.DB.First(&restored, "id = ?", english.ID).Error)
	assert.False(t, restored.DeletedAt.Valid)
	assert.Equal(t, "Updated English", restored.BodyContent)
	assert.Equal(t, "APPROVED", restored.Status)
	assert.Equal(t, "HIGH", restored.QualityRating, "missing provider rating must preserve the known rating")
	require.NoError(t, app.DB.First(&updated, "id = ?", malay.ID).Error)
	assert.Equal(t, "ms", updated.Language)
	assert.Equal(t, "Updated Malay", updated.BodyContent)
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_account = ? AND name = ?", org.ID, account.Name, "created_name").First(&created).Error)
	assert.Equal(t, "Created body", created.BodyContent)
	for _, untouched := range []*models.Template{otherLanguage, otherAccountRow, otherOrgRow} {
		var actual models.Template
		require.NoError(t, app.DB.First(&actual, "id = ?", untouched.ID).Error)
		assert.Equal(t, untouched.BodyContent, actual.BodyContent)
		assert.Equal(t, "PENDING", actual.Status)
		assert.Empty(t, actual.MetaTemplateID)
	}
	var count int64
	require.NoError(t, app.DB.Model(&models.Template{}).Where("organization_id = ? AND whats_app_account = ?", org.ID, account.Name).Count(&count).Error)
	assert.Equal(t, int64(4), count, "three synchronized templates plus untouched French row")
}

func TestApp_SyncTemplates_EmptyProviderBatchReturnsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"data\":[]}"))
	}))
	defer server.Close()
	app := newTemplateTestApp(t, server)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createTemplateTestUser(t, app, org.ID, "templates:sync")
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	existing := createTestTemplateInDB(t, app, org.ID, account.Name, "existing", "APPROVED")
	req := testutil.NewJSONRequest(t, map[string]any{"whatsapp_account": account.Name})
	testutil.SetAuthContext(req, org.ID, user.ID)
	require.NoError(t, app.SyncTemplates(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	var response struct {
		Data struct {
			Count   int
			Message string
		}
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
	assert.Zero(t, response.Data.Count)
	assert.Equal(t, "Synced 0 templates", response.Data.Message)
	var rows []models.Template
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_account = ?", org.ID, account.Name).Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.Equal(t, existing.ID, rows[0].ID)
	assert.Equal(t, existing.BodyContent, rows[0].BodyContent)
}

// The existing unique index spans account, name and language across organizations.
// A foreign organization's conflicting row must never be adopted or updated; the
// whole provider batch must roll back when its later insert hits that constraint.
func TestApp_SyncTemplates_CrossOrganizationConflictRollsBack(t *testing.T) {
	server := newMockTemplateServer(t)
	defer server.Close()
	app := newTemplateTestApp(t, server)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createTemplateTestUser(t, app, org.ID, "templates:sync")
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	otherOrg := testutil.CreateTestOrganization(t, app.DB)

	// Deliberately adversarial ownership: no same-key row exists in the syncing
	// organization. Omitting the organization predicate would update this row.
	foreign := createTestTemplateInDB(t, app, otherOrg.ID, account.Name, "synced_template_two", "PENDING")

	originalDB := app.DB
	pool, err := originalDB.DB()
	require.NoError(t, err)
	app.DB = templateSyncPrivateDB(t, app, pool)
	armed := false
	var createOrder []string
	t.Cleanup(func() {
		armed = false
		app.DB = originalDB
	})
	requestTemplate := func(tx *gorm.DB) (*models.Template, bool) {
		if !armed || !templateSyncIsTemplate(tx) {
			return nil, false
		}
		row, ok := tx.Statement.Dest.(*models.Template)
		return row, ok && row != nil && row.OrganizationID == org.ID &&
			row.WhatsAppAccount == account.Name && row.Language == "en"
	}
	require.NoError(t, app.DB.Callback().Create().Before("gorm:create").Register("test:sync_conflict_attempt", func(tx *gorm.DB) {
		if row, ok := requestTemplate(tx); ok {
			createOrder = append(createOrder, row.Name+":attempt")
		}
	}))
	require.NoError(t, app.DB.Callback().Create().After("gorm:create").Register("test:sync_conflict_result", func(tx *gorm.DB) {
		if row, ok := requestTemplate(tx); ok {
			if tx.Error == nil && tx.RowsAffected == 1 {
				createOrder = append(createOrder, row.Name+":created")
			} else if tx.Error != nil {
				createOrder = append(createOrder, row.Name+":refused")
			}
		}
	}))

	req := testutil.NewJSONRequest(t, map[string]any{"whatsapp_account": account.Name})
	testutil.SetAuthContext(req, org.ID, user.ID)
	armed = true
	require.NoError(t, app.SyncTemplates(req))
	armed = false
	app.DB = originalDB
	assert.Equal(t, []string{
		"synced_template_one:attempt",
		"synced_template_one:created",
		"synced_template_two:attempt",
		"synced_template_two:refused",
	}, createOrder, "one successful insert must precede the later conflicting insert")
	testutil.AssertErrorResponse(t, req, fasthttp.StatusInternalServerError, "Failed to save synced templates")
	assert.NotContains(t, string(testutil.GetResponseBody(req)), "Synced ")

	var actual models.Template
	require.NoError(t, app.DB.Unscoped().First(&actual, "id = ?", foreign.ID).Error)
	assert.Equal(t, foreign.OrganizationID, actual.OrganizationID)
	assert.Equal(t, foreign.WhatsAppAccount, actual.WhatsAppAccount)
	assert.Equal(t, foreign.Name, actual.Name)
	assert.Equal(t, foreign.Language, actual.Language)
	assert.Equal(t, foreign.BodyContent, actual.BodyContent)
	assert.Equal(t, foreign.Status, actual.Status)
	assert.Empty(t, actual.MetaTemplateID)
	assert.False(t, actual.DeletedAt.Valid)

	var count int64
	require.NoError(t, app.DB.Unscoped().Model(&models.Template{}).Where("organization_id = ? AND whats_app_account = ?", org.ID, account.Name).Count(&count).Error)
	assert.Zero(t, count, "the first successful provider insert must roll back with the later conflict")
}
