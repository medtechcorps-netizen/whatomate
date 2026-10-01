package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// newProcessorTestApp creates a minimal App suitable for chatbot processor tests.
// It connects to the test database and Redis, provides a mock WhatsApp client,
// and uses a no-op logger.
func newProcessorTestApp(t *testing.T) *App {
	t.Helper()
	db := testutil.SetupTestDB(t)
	log := testutil.NopLogger()

	// Mock WhatsApp API server that accepts all requests.
	waServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": "wamid.mock_" + uuid.New().String()[:8]}},
		})
	}))
	t.Cleanup(waServer.Close)

	app := &App{
		DB:         db,
		Log:        log,
		WhatsApp:   whatsapp.NewWithBaseURL(log, waServer.URL),
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
	if rdb := testutil.SetupTestRedis(t); rdb != nil {
		app.Redis = rdb
	}
	return app
}

// createProcessorTestOrg creates an organization and WhatsApp account for processor tests.
func createProcessorTestOrg(t *testing.T, app *App) (*models.Organization, *models.WhatsAppAccount) {
	t.Helper()
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	return org, account
}

func TestUpdateContactBSUIDFailureKeepsOuterTransactionUsable(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	suffix := uuid.New().String()[:8]
	functionName := "fail_bsuid_" + suffix
	triggerName := "fail_bsuid_trigger_" + suffix
	require.NoError(t, app.DB.Exec(fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			IF NEW.id = '%s'::uuid THEN
				RAISE EXCEPTION 'forced BSUID update failure';
			END IF;
			RETURN NEW;
		END;
		$$`, functionName, contact.ID)).Error)
	require.NoError(t, app.DB.Exec(fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE UPDATE OF bs_uid ON contacts
		FOR EACH ROW EXECUTE FUNCTION %s()`, triggerName, functionName)).Error)
	t.Cleanup(func() {
		app.DB.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON contacts", triggerName))
		app.DB.Exec(fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", functionName))
	})

	var messageID uuid.UUID
	err := app.DB.Transaction(func(tx *gorm.DB) error {
		scoped := &App{
			DB:  tx,
			Log: app.Log,
		}
		scoped.updateContactBSUID(contact, "US.test-business-scoped-id")

		messageID = uuid.New()
		return tx.Create(&models.Message{
			BaseModel:         models.BaseModel{ID: messageID},
			OrganizationID:    org.ID,
			WhatsAppAccount:   account.Name,
			ContactID:         contact.ID,
			WhatsAppMessageID: "wamid.bsuid-savepoint-" + suffix,
			Direction:         models.DirectionIncoming,
			MessageType:       models.MessageTypeText,
			Content:           "message survives optional metadata failure",
			Status:            models.MessageStatusReceived,
		}).Error
	})
	require.NoError(t, err)
	assert.Empty(t, contact.BSUID)

	var saved models.Message
	require.NoError(t, app.DB.First(&saved, "id = ?", messageID).Error)
	assert.Equal(t, contact.ID, saved.ContactID)
}

func TestUpdateContactBSUIDPersistsMappedColumn(t *testing.T) {
	app := newProcessorTestApp(t)
	org, _ := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	expected := "US." + uuid.New().String()

	app.updateContactBSUID(contact, expected)

	var saved models.Contact
	require.NoError(t, app.DB.First(&saved, "id = ?", contact.ID).Error)
	assert.Equal(t, expected, saved.BSUID)
}

// =============================================================================
// matchKeywordRules
// =============================================================================

func TestMatchKeywordRules_ExactMatch(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)

	rule := &models.KeywordRule{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "exact-hello",
		Keywords:        models.StringArray{"hello"},
		MatchType:       models.MatchTypeExact,
		ResponseType:    models.ResponseTypeText,
		ResponseContent: models.JSONB{"body": "Hello response"},
		Priority:        10,
		IsEnabled:       true,
	}
	require.NoError(t, app.DB.Create(rule).Error)

	resp, matched := app.matchKeywordRules(org.ID, account.Name, "hello")
	assert.True(t, matched)
	require.NotNil(t, resp)
	assert.Equal(t, "Hello response", resp.Body)

	// Different case should also match (case insensitive by default)
	resp2, matched2 := app.matchKeywordRules(org.ID, account.Name, "HELLO")
	assert.True(t, matched2)
	require.NotNil(t, resp2)
	assert.Equal(t, "Hello response", resp2.Body)

	// Partial should NOT match exact
	_, matched3 := app.matchKeywordRules(org.ID, account.Name, "hello world")
	assert.False(t, matched3)
}

func TestMatchKeywordRules_ExactMatch_CaseSensitive(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)

	rule := &models.KeywordRule{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "exact-case",
		Keywords:        models.StringArray{"Hello"},
		MatchType:       models.MatchTypeExact,
		CaseSensitive:   true,
		ResponseType:    models.ResponseTypeText,
		ResponseContent: models.JSONB{"body": "Case match"},
		Priority:        10,
		IsEnabled:       true,
	}
	require.NoError(t, app.DB.Create(rule).Error)

	_, matched := app.matchKeywordRules(org.ID, account.Name, "Hello")
	assert.True(t, matched)

	_, matched2 := app.matchKeywordRules(org.ID, account.Name, "hello")
	assert.False(t, matched2)
}

func TestMatchKeywordRules_ContainsMatch(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)

	rule := &models.KeywordRule{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "contains-help",
		Keywords:        models.StringArray{"help"},
		MatchType:       models.MatchTypeContains,
		ResponseType:    models.ResponseTypeText,
		ResponseContent: models.JSONB{"body": "Help response"},
		Priority:        10,
		IsEnabled:       true,
	}
	require.NoError(t, app.DB.Create(rule).Error)

	resp, matched := app.matchKeywordRules(org.ID, account.Name, "I need help please")
	assert.True(t, matched)
	require.NotNil(t, resp)
	assert.Equal(t, "Help response", resp.Body)

	_, matched2 := app.matchKeywordRules(org.ID, account.Name, "HELP ME")
	assert.True(t, matched2)

	_, matched3 := app.matchKeywordRules(org.ID, account.Name, "goodbye")
	assert.False(t, matched3)
}

func TestMatchKeywordRules_StartsWithMatch(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)

	rule := &models.KeywordRule{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "starts-with-hi",
		Keywords:        models.StringArray{"hi"},
		MatchType:       models.MatchTypeStartsWith,
		ResponseType:    models.ResponseTypeText,
		ResponseContent: models.JSONB{"body": "Hi response"},
		Priority:        10,
		IsEnabled:       true,
	}
	require.NoError(t, app.DB.Create(rule).Error)

	resp, matched := app.matchKeywordRules(org.ID, account.Name, "hi there")
	assert.True(t, matched)
	require.NotNil(t, resp)
	assert.Equal(t, "Hi response", resp.Body)

	_, matched2 := app.matchKeywordRules(org.ID, account.Name, "say hi")
	assert.False(t, matched2)
}

func TestMatchKeywordRules_RegexMatch(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)

	rule := &models.KeywordRule{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "regex-order",
		Keywords:        models.StringArray{`order\s*#?\d+`},
		MatchType:       models.MatchTypeRegex,
		ResponseType:    models.ResponseTypeText,
		ResponseContent: models.JSONB{"body": "Order lookup"},
		Priority:        10,
		IsEnabled:       true,
	}
	require.NoError(t, app.DB.Create(rule).Error)

	resp, matched := app.matchKeywordRules(org.ID, account.Name, "I have order #12345")
	assert.True(t, matched)
	require.NotNil(t, resp)
	assert.Equal(t, "Order lookup", resp.Body)

	_, matched2 := app.matchKeywordRules(org.ID, account.Name, "where is my package")
	assert.False(t, matched2)
}

func TestMatchKeywordRules_NoMatch(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)

	rule := &models.KeywordRule{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "nope",
		Keywords:        models.StringArray{"specific-keyword"},
		MatchType:       models.MatchTypeExact,
		ResponseType:    models.ResponseTypeText,
		ResponseContent: models.JSONB{"body": "reply"},
		Priority:        10,
		IsEnabled:       true,
	}
	require.NoError(t, app.DB.Create(rule).Error)

	resp, matched := app.matchKeywordRules(org.ID, account.Name, "random message")
	assert.False(t, matched)
	assert.Nil(t, resp)
}

func TestMatchKeywordRules_Priority(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)

	// Lower priority rule
	lowRule := &models.KeywordRule{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "low-priority",
		Keywords:        models.StringArray{"test"},
		MatchType:       models.MatchTypeContains,
		ResponseType:    models.ResponseTypeText,
		ResponseContent: models.JSONB{"body": "Low priority"},
		Priority:        5,
		IsEnabled:       true,
	}
	require.NoError(t, app.DB.Create(lowRule).Error)

	// Higher priority rule
	highRule := &models.KeywordRule{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "high-priority",
		Keywords:        models.StringArray{"test"},
		MatchType:       models.MatchTypeContains,
		ResponseType:    models.ResponseTypeText,
		ResponseContent: models.JSONB{"body": "High priority"},
		Priority:        20,
		IsEnabled:       true,
	}
	require.NoError(t, app.DB.Create(highRule).Error)

	// The higher priority rule should be returned (rules are ORDER BY priority DESC)
	resp, matched := app.matchKeywordRules(org.ID, account.Name, "this is a test")
	assert.True(t, matched)
	require.NotNil(t, resp)
	assert.Equal(t, "High priority", resp.Body)
}

func TestMatchKeywordRules_DisabledRuleIgnored(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)

	rule := &models.KeywordRule{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "disabled",
		Keywords:        models.StringArray{"disabled"},
		MatchType:       models.MatchTypeExact,
		ResponseType:    models.ResponseTypeText,
		ResponseContent: models.JSONB{"body": "Should not match"},
		Priority:        10,
		IsEnabled:       true, // Create as enabled first
	}
	require.NoError(t, app.DB.Create(rule).Error)
	// Explicitly disable: GORM skips zero-value bools with default:true on INSERT.
	require.NoError(t, app.DB.Model(rule).Update("is_enabled", false).Error)

	_, matched := app.matchKeywordRules(org.ID, account.Name, "disabled")
	assert.False(t, matched)
}

func TestMatchKeywordRules_TransferType(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)

	rule := &models.KeywordRule{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "agent",
		Keywords:        models.StringArray{"agent"},
		MatchType:       models.MatchTypeExact,
		ResponseType:    models.ResponseTypeTransfer,
		ResponseContent: models.JSONB{"body": "Connecting you to an agent..."},
		Priority:        10,
		IsEnabled:       true,
	}
	require.NoError(t, app.DB.Create(rule).Error)

	resp, matched := app.matchKeywordRules(org.ID, account.Name, "agent")
	assert.True(t, matched)
	require.NotNil(t, resp)
	assert.Equal(t, models.ResponseTypeTransfer, resp.ResponseType)
	assert.Equal(t, "Connecting you to an agent...", resp.Body)
}

func TestMatchKeywordRules_WithButtons(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)

	rule := &models.KeywordRule{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "menu",
		Keywords:        models.StringArray{"menu"},
		MatchType:       models.MatchTypeExact,
		ResponseType:    models.ResponseTypeText,
		ResponseContent: models.JSONB{
			"body": "Choose an option:",
			"buttons": []any{
				map[string]any{"id": "opt1", "title": "Option 1"},
				map[string]any{"id": "opt2", "title": "Option 2"},
			},
		},
		Priority:  10,
		IsEnabled: true,
	}
	require.NoError(t, app.DB.Create(rule).Error)

	resp, matched := app.matchKeywordRules(org.ID, account.Name, "menu")
	assert.True(t, matched)
	require.NotNil(t, resp)
	assert.Equal(t, "Choose an option:", resp.Body)
	assert.Len(t, resp.Buttons, 2)
}

// =============================================================================
// getOrCreateSession
// =============================================================================

func TestGetOrCreateSession_NewSession(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	session, isNew, err := app.getOrCreateSession(org.ID, contact.ID, account.Name, contact.PhoneNumber, 30)
	require.NoError(t, err)
	assert.True(t, isNew)
	require.NotNil(t, session)
	assert.Equal(t, models.SessionStatusActive, session.Status)
	assert.Equal(t, org.ID, session.OrganizationID)
	assert.Equal(t, contact.ID, session.ContactID)
	assert.Equal(t, account.Name, session.WhatsAppAccount)

	// Verify it was persisted
	var dbSession models.ChatbotSession
	require.NoError(t, app.DB.First(&dbSession, session.ID).Error)
	assert.Equal(t, models.SessionStatusActive, dbSession.Status)
}

func TestGetOrCreateSession_ExistingSession(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	// Create an active session
	existing := models.ChatbotSession{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.SessionStatusActive,
		SessionData:     models.JSONB{"key": "value"},
		StartedAt:       time.Now(),
		LastActivityAt:  time.Now(),
	}
	require.NoError(t, app.DB.Create(&existing).Error)

	session, isNew, err := app.getOrCreateSession(org.ID, contact.ID, account.Name, contact.PhoneNumber, 30)
	require.NoError(t, err)
	assert.False(t, isNew)
	require.NotNil(t, session)
	assert.Equal(t, existing.ID, session.ID)
}

func TestGetOrCreateSession_ExpiredSession(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	// Create an expired session (last activity 60 minutes ago, timeout is 30 minutes)
	expired := models.ChatbotSession{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.SessionStatusActive,
		SessionData:     models.JSONB{},
		StartedAt:       time.Now().Add(-60 * time.Minute),
		LastActivityAt:  time.Now().Add(-60 * time.Minute),
	}
	require.NoError(t, app.DB.Create(&expired).Error)

	session, isNew, err := app.getOrCreateSession(org.ID, contact.ID, account.Name, contact.PhoneNumber, 30)
	require.NoError(t, err)
	assert.True(t, isNew)
	require.NotNil(t, session)
	assert.NotEqual(t, expired.ID, session.ID, "should create a new session, not return expired one")

	var storedExpired models.ChatbotSession
	require.NoError(t, app.DB.First(&storedExpired, expired.ID).Error)
	assert.Equal(t, models.SessionStatusTimeout, storedExpired.Status)
	require.NotNil(t, storedExpired.CompletedAt)
}

func TestGetOrCreateSession_ReconcilesDuplicateActiveRows(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	now := time.Now()

	newest := models.ChatbotSession{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.SessionStatusActive,
		SessionData:     models.JSONB{"session": "newest"},
		StartedAt:       now.Add(-10 * time.Minute),
		LastActivityAt:  now.Add(-time.Minute),
	}
	duplicateLive := models.ChatbotSession{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.SessionStatusActive,
		SessionData:     models.JSONB{"session": "duplicate-live"},
		StartedAt:       now.Add(-20 * time.Minute),
		LastActivityAt:  now.Add(-5 * time.Minute),
	}
	stale := models.ChatbotSession{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.SessionStatusActive,
		SessionData:     models.JSONB{"session": "stale"},
		StartedAt:       now.Add(-2 * time.Hour),
		LastActivityAt:  now.Add(-2 * time.Hour),
	}
	require.NoError(t, app.DB.Create(&newest).Error)
	require.NoError(t, app.DB.Create(&duplicateLive).Error)
	require.NoError(t, app.DB.Create(&stale).Error)

	session, isNew, err := app.getOrCreateSession(
		org.ID,
		contact.ID,
		account.Name,
		contact.PhoneNumber,
		30,
	)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.False(t, isNew)
	assert.Equal(t, newest.ID, session.ID)

	var storedDuplicate, storedStale models.ChatbotSession
	require.NoError(t, app.DB.First(&storedDuplicate, duplicateLive.ID).Error)
	require.NoError(t, app.DB.First(&storedStale, stale.ID).Error)
	assert.Equal(t, models.SessionStatusCancelled, storedDuplicate.Status)
	assert.Equal(t, models.SessionStatusTimeout, storedStale.Status)
	require.NotNil(t, storedDuplicate.CompletedAt)
	require.NotNil(t, storedStale.CompletedAt)

	var activeCount int64
	require.NoError(t, app.DB.Model(&models.ChatbotSession{}).
		Where(
			"organization_id = ? AND contact_id = ? AND whats_app_account = ? AND status = ?",
			org.ID,
			contact.ID,
			account.Name,
			models.SessionStatusActive,
		).
		Count(&activeCount).Error)
	assert.Equal(t, int64(1), activeCount)
}

func TestGetOrCreateSession_ConcurrentAliasCreatorsReuseCanonicalSession(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	canonical := testutil.CreateTestContact(t, app.DB, org.ID)
	alias := testutil.CreateTestContact(t, app.DB, org.ID)
	require.NoError(t, app.DB.Unscoped().Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", alias.ID, org.ID).
		Updates(map[string]any{
			"merged_into_id": canonical.ID,
			"deleted_at":     time.Now(),
		}).Error)

	type sessionResult struct {
		session *models.ChatbotSession
		isNew   bool
		err     error
	}
	const creators = 8
	start := make(chan struct{})
	results := make(chan sessionResult, creators)
	for range creators {
		go func() {
			<-start
			session, isNew, err := app.getOrCreateSession(
				org.ID,
				alias.ID,
				account.Name,
				alias.PhoneNumber,
				30,
			)
			results <- sessionResult{session: session, isNew: isNew, err: err}
		}()
	}
	close(start)

	var sessionID uuid.UUID
	newCount := 0
	for range creators {
		result := <-results
		require.NoError(t, result.err)
		require.NotNil(t, result.session)
		if sessionID == uuid.Nil {
			sessionID = result.session.ID
		}
		assert.Equal(t, sessionID, result.session.ID)
		assert.Equal(t, canonical.ID, result.session.ContactID)
		if result.isNew {
			newCount++
		}
	}
	assert.Equal(t, 1, newCount)

	var activeCount int64
	require.NoError(t, app.DB.Model(&models.ChatbotSession{}).
		Where(
			"organization_id = ? AND contact_id = ? AND whats_app_account = ? AND status = ?",
			org.ID,
			canonical.ID,
			account.Name,
			models.SessionStatusActive,
		).
		Count(&activeCount).Error)
	assert.Equal(t, int64(1), activeCount)
}

func TestGetOrCreateSession_WaitsForMergeThenUsesCanonicalContact(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	canonical := testutil.CreateTestContact(t, app.DB, org.ID)
	source := testutil.CreateTestContact(t, app.DB, org.ID)

	mergeTx := app.DB.Begin()
	require.NoError(t, mergeTx.Error)
	t.Cleanup(func() { _ = mergeTx.Rollback().Error })

	var lockedSource models.Contact
	require.NoError(t, mergeTx.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", source.ID, org.ID).
		First(&lockedSource).Error)
	require.NoError(t, mergeTx.Unscoped().Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", source.ID, org.ID).
		Updates(map[string]any{
			"merged_into_id": canonical.ID,
			"deleted_at":     time.Now(),
		}).Error)

	type sessionResult struct {
		session *models.ChatbotSession
		isNew   bool
		err     error
	}
	resultCh := make(chan sessionResult, 1)
	go func() {
		session, isNew, err := app.getOrCreateSession(
			org.ID,
			source.ID,
			account.Name,
			source.PhoneNumber,
			30,
		)
		resultCh <- sessionResult{session: session, isNew: isNew, err: err}
	}()

	select {
	case result := <-resultCh:
		t.Fatalf("session creator bypassed the merge row lock: %+v", result)
	case <-time.After(75 * time.Millisecond):
	}

	require.NoError(t, mergeTx.Commit().Error)
	result := <-resultCh
	require.NoError(t, result.err)
	require.True(t, result.isNew)
	require.NotNil(t, result.session)
	assert.Equal(t, canonical.ID, result.session.ContactID)
}

// =============================================================================
// isWithinBusinessHours
// =============================================================================

func TestIsWithinBusinessHours_WithinHours(t *testing.T) {
	app := newProcessorTestApp(t)
	now := time.Now()
	dayOfWeek := float64(now.Weekday())

	hours := models.JSONBArray{
		map[string]any{
			"day":        dayOfWeek,
			"enabled":    true,
			"start_time": "00:00",
			"end_time":   "23:59",
		},
	}

	result := app.isWithinBusinessHours(hours)
	assert.True(t, result)
}

func TestIsWithinBusinessHours_OutsideHours(t *testing.T) {
	app := newProcessorTestApp(t)
	now := time.Now()
	dayOfWeek := float64(now.Weekday())

	// Set hours to a time window that has definitely passed
	// Use a very narrow window in the past
	hours := models.JSONBArray{
		map[string]any{
			"day":        dayOfWeek,
			"enabled":    true,
			"start_time": "00:00",
			"end_time":   "00:01",
		},
	}

	// This will only be true if running at midnight; for all practical purposes it tests false
	currentTime := now.Format("15:04")
	if currentTime > "00:01" {
		result := app.isWithinBusinessHours(hours)
		assert.False(t, result)
	}
}

func TestIsWithinBusinessHours_DayDisabled(t *testing.T) {
	app := newProcessorTestApp(t)
	now := time.Now()
	dayOfWeek := float64(now.Weekday())

	hours := models.JSONBArray{
		map[string]any{
			"day":        dayOfWeek,
			"enabled":    false,
			"start_time": "00:00",
			"end_time":   "23:59",
		},
	}

	result := app.isWithinBusinessHours(hours)
	assert.False(t, result)
}

func TestIsWithinBusinessHours_NoMatchingDay(t *testing.T) {
	app := newProcessorTestApp(t)
	now := time.Now()
	// Use a different day of the week
	otherDay := float64((int(now.Weekday()) + 1) % 7)

	hours := models.JSONBArray{
		map[string]any{
			"day":        otherDay,
			"enabled":    true,
			"start_time": "00:00",
			"end_time":   "23:59",
		},
	}

	result := app.isWithinBusinessHours(hours)
	assert.False(t, result)
}

func TestIsWithinBusinessHours_EmptyHours(t *testing.T) {
	app := newProcessorTestApp(t)

	result := app.isWithinBusinessHours(models.JSONBArray{})
	assert.False(t, result)
}

// =============================================================================
// shouldSkipStep
// =============================================================================

func TestExitFlow_UpdatesSession(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	flow := &models.ChatbotFlow{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "Exit Test Flow",
		IsEnabled:       true,
	}
	require.NoError(t, app.DB.Create(flow).Error)

	session := &models.ChatbotSession{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.SessionStatusActive,
		CurrentFlowID:   &flow.ID,
		CurrentStep:     "step2",
		StepRetries:     2,
		SessionData:     models.JSONB{},
		StartedAt:       time.Now(),
		LastActivityAt:  time.Now(),
	}
	require.NoError(t, app.DB.Create(session).Error)

	app.exitFlow(session)

	var dbSession models.ChatbotSession
	require.NoError(t, app.DB.First(&dbSession, session.ID).Error)
	assert.Equal(t, models.SessionStatusCompleted, dbSession.Status)
	assert.Equal(t, "", dbSession.CurrentStep)
	assert.Equal(t, 0, dbSession.StepRetries)
	assert.NotNil(t, dbSession.CompletedAt)
}

// =============================================================================
// saveIncomingMessage
// =============================================================================

func TestSaveIncomingMessage_TextMessage(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	waMsgID := "wamid." + uuid.New().String()[:16]
	app.saveIncomingMessage(account, contact, waMsgID, "text", "Hello from test", nil, "")

	// Verify message was saved
	var msg models.Message
	require.NoError(t, app.DB.Where("whats_app_message_id = ?", waMsgID).First(&msg).Error)
	assert.Equal(t, models.DirectionIncoming, msg.Direction)
	assert.Equal(t, models.MessageTypeText, msg.MessageType)
	assert.Equal(t, "Hello from test", msg.Content)
	assert.Equal(t, contact.ID, msg.ContactID)
	assert.Equal(t, account.Name, msg.WhatsAppAccount)
	assert.Equal(t, models.MessageStatusReceived, msg.Status)

	// Verify contact was updated
	var dbContact models.Contact
	require.NoError(t, app.DB.First(&dbContact, contact.ID).Error)
	assert.NotNil(t, dbContact.LastMessageAt)
	assert.Equal(t, "Hello from test", dbContact.LastMessagePreview)
	assert.False(t, dbContact.IsRead)
}

func TestSaveIncomingMessage_WithMedia(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	waMsgID := "wamid." + uuid.New().String()[:16]
	media := &MediaInfo{
		MediaURL:      "/uploads/test-image.jpg",
		MediaMimeType: "image/jpeg",
		MediaFilename: "photo.jpg",
	}
	app.saveIncomingMessage(account, contact, waMsgID, "image", "Look at this", media, "")

	var msg models.Message
	require.NoError(t, app.DB.Where("whats_app_message_id = ?", waMsgID).First(&msg).Error)
	assert.Equal(t, "/uploads/test-image.jpg", msg.MediaURL)
	assert.Equal(t, "image/jpeg", msg.MediaMimeType)
	assert.Equal(t, "photo.jpg", msg.MediaFilename)

	// Non-text messages show type in preview
	var dbContact models.Contact
	require.NoError(t, app.DB.First(&dbContact, contact.ID).Error)
	assert.Equal(t, "[image]", dbContact.LastMessagePreview)
}

func TestSaveIncomingMessage_PersistsFlowResponse(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	waMsgID := "wamid." + uuid.New().String()[:16]
	flowData := map[string]any{
		"service_id": "physio-initial",
		"slot":       "2026-08-03T09:00:00+08:00",
	}
	app.saveIncomingMessageWithFlow(
		account,
		contact,
		waMsgID,
		"nfm_reply",
		"Appointment request",
		nil,
		"",
		flowData,
	)

	var msg models.Message
	require.NoError(t, app.DB.Where("whats_app_message_id = ?", waMsgID).First(&msg).Error)
	assert.Equal(t, "physio-initial", msg.FlowResponse["service_id"])
	assert.Equal(t, "2026-08-03T09:00:00+08:00", msg.FlowResponse["slot"])
}

func TestSaveIncomingMessage_WithReplyContext(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	// Create original message to reply to
	originalWAMID := "wamid.original_" + uuid.New().String()[:8]
	originalMsg := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    org.ID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: originalWAMID,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeText,
		Content:           "Original message",
		Status:            models.MessageStatusReceived,
	}
	require.NoError(t, app.DB.Create(&originalMsg).Error)

	// Attribute the existing random-ID outgoing row through the real legacy
	// mirror. A display-name/contact match alone is not account authority.
	require.NoError(t, persistLegacyWhatsAppMessageMirror(app.DB, account, originalMsg.ID))
	var mirrored models.Message
	require.NoError(t, app.DB.Where("organization_id = ? AND id = ?", org.ID, originalMsg.ID).First(&mirrored).Error)
	require.Equal(t, originalMsg.ID, mirrored.ID)
	require.Equal(t, originalWAMID, mirrored.WhatsAppMessageID)
	require.Equal(t, org.ID, mirrored.OrganizationID)
	require.Equal(t, contact.ID, mirrored.ContactID)
	require.Equal(t, originalMsg.Direction, mirrored.Direction)
	require.Equal(t, originalMsg.Status, mirrored.Status)
	require.Equal(t, originalMsg.Content, mirrored.Content)
	require.NotNil(t, mirrored.InboxConversationID)
	var conversation models.InboxConversation
	require.NoError(t, app.DB.Where("organization_id = ? AND id = ?", org.ID, *mirrored.InboxConversationID).First(&conversation).Error)
	require.Equal(t, models.ChannelWhatsApp, conversation.Channel)
	require.Equal(t, contact.ID, conversation.ContactID)
	require.Equal(t, "legacy-contact:"+contact.ID.String(), conversation.ExternalConversationID)
	require.NotNil(t, conversation.ContactIdentityID)
	var channel models.ChannelAccount
	require.NoError(t, app.DB.Where("organization_id = ? AND id = ?", org.ID, conversation.ChannelAccountID).First(&channel).Error)
	require.Equal(t, models.ChannelWhatsApp, channel.Channel)
	require.Equal(t, channelapi.LegacyMetaProvider, channel.Provider)
	require.Equal(t, "legacy-account:"+account.ID.String(), channel.ExternalAccountID)
	boundAccountID, err := channelapi.LegacyMetaWhatsAppAccountID(&channel)
	require.NoError(t, err)
	require.Equal(t, account.ID, boundAccountID)
	var identity models.ContactIdentity
	require.NoError(t, app.DB.Where("organization_id = ? AND id = ?", org.ID, *conversation.ContactIdentityID).First(&identity).Error)
	require.Equal(t, channel.ID, identity.ChannelAccountID)
	require.Equal(t, models.ChannelWhatsApp, identity.Channel)
	require.Equal(t, contact.ID, identity.ContactID)
	require.Equal(t, "legacy-contact:"+contact.ID.String(), identity.ExternalID)

	// Save reply message
	replyWAMID := "wamid.reply_" + uuid.New().String()[:8]
	app.saveIncomingMessage(account, contact, replyWAMID, "text", "Reply to your message", nil, originalWAMID)

	var replyMsg models.Message
	require.NoError(t, app.DB.Where("whats_app_message_id = ?", replyWAMID).First(&replyMsg).Error)
	assert.True(t, replyMsg.IsReply)
	require.NotNil(t, replyMsg.ReplyToMessageID)
	assert.Equal(t, originalMsg.ID, *replyMsg.ReplyToMessageID)
}

func TestSaveIncomingMessage_LongContent(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	// Create a message with content longer than 100 characters
	longContent := ""
	for i := 0; i < 120; i++ {
		longContent += "x"
	}
	waMsgID := "wamid." + uuid.New().String()[:16]
	app.saveIncomingMessage(account, contact, waMsgID, "text", longContent, nil, "")

	var dbContact models.Contact
	require.NoError(t, app.DB.First(&dbContact, contact.ID).Error)
	// Preview should be truncated to 97 chars + "..."
	assert.Len(t, dbContact.LastMessagePreview, 100)
	assert.True(t, len(dbContact.LastMessagePreview) <= 100)
}

// =============================================================================
// processTemplate with session data (replaces former replaceVariables)
// =============================================================================

func TestProcessTemplateSessionData_Basic(t *testing.T) {
	result := processTemplate("Hello {{name}}, your order is {{order_id}}", models.JSONB{
		"name":     "John",
		"order_id": "12345",
	})
	assert.Equal(t, "Hello John, your order is 12345", result)
}

func TestProcessTemplateSessionData_NilData(t *testing.T) {
	result := processTemplate("Hello {{name}}", nil)
	// processTemplate replaces unresolved variables with empty string
	assert.Equal(t, "Hello ", result)
}

func TestProcessTemplateSessionData_MissingVariable(t *testing.T) {
	result := processTemplate("Hello {{name}}", models.JSONB{})
	// processTemplate replaces unresolved variables with empty string
	assert.Equal(t, "Hello ", result)
}

// =============================================================================
// logSessionMessage
// =============================================================================

func TestLogSessionMessage(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	session := &models.ChatbotSession{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.SessionStatusActive,
		SessionData:     models.JSONB{},
		StartedAt:       time.Now(),
		LastActivityAt:  time.Now(),
	}
	require.NoError(t, app.DB.Create(session).Error)

	app.logSessionMessage(session.ID, models.DirectionIncoming, "test message", "greeting")

	var msgs []models.ChatbotSessionMessage
	require.NoError(t, app.DB.Where("session_id = ?", session.ID).Find(&msgs).Error)
	require.Len(t, msgs, 1)
	assert.Equal(t, "test message", msgs[0].Message)
	assert.Equal(t, "greeting", msgs[0].StepName)
	assert.Equal(t, models.DirectionIncoming, msgs[0].Direction)
}

// =============================================================================
// matchFlowTrigger
// =============================================================================

func TestMatchFlowTrigger_Match(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)

	flow := &models.ChatbotFlow{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "Order Flow",
		TriggerKeywords: models.StringArray{"order", "buy"},
		IsEnabled:       true,
	}
	require.NoError(t, app.DB.Create(flow).Error)

	result := app.matchFlowTrigger(org.ID, "I want to order")
	require.NotNil(t, result)
	assert.Equal(t, flow.ID, result.ID)

	// No match
	noMatch := app.matchFlowTrigger(org.ID, "hello there")
	assert.Nil(t, noMatch)
}

func TestIncomingReactionRejectsInvalidScopeWithoutDependencies(t *testing.T) {
	validAccount := &models.WhatsAppAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: uuid.New(),
	}
	for _, tc := range []struct {
		name    string
		app     *App
		account *models.WhatsAppAccount
	}{
		{name: "nil_receiver", account: validAccount},
		{name: "nil_account", app: &App{}},
		{name: "missing_organization", app: &App{}, account: &models.WhatsAppAccount{BaseModel: models.BaseModel{ID: uuid.New()}}},
		{name: "missing_account", app: &App{}, account: &models.WhatsAppAccount{OrganizationID: uuid.New()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotPanics(t, func() {
				tc.app.handleIncomingReaction(tc.account, "60123456789", "wamid.invalid-scope", "👍", "Patient")
			}, "invalid scope must return before using a logger, database, or provider")
		})
	}
}

func TestIncomingReactionHistoricalReservationDoesNotAuthorizeChangedProjection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		column string
		value  any
	}{
		{name: "conversation_binding", column: "external_conversation_id", value: "unrelated-conversation"},
		{name: "conversation_channel", column: "channel", value: models.ChannelInstagram},
		{name: "missing_contact_identity", column: "contact_identity_id", value: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, account, contact := whatsappIdentityFixture(t)
			wamid := "wamid.reaction-reserved-" + uuid.NewString()
			message := models.Message{
				BaseModel:      models.BaseModel{ID: uuid.NewSHA1(account.ID, []byte("coexistence-message:"+wamid))},
				OrganizationID: account.OrganizationID, ContactID: contact.ID,
				WhatsAppAccount: account.Name, WhatsAppMessageID: wamid,
				Direction: models.DirectionOutgoing, MessageType: models.MessageTypeText,
				Content: "reserved owner", Status: models.MessageStatusSent, Metadata: models.JSONB{},
			}
			require.NoError(t, app.DB.Create(&message).Error)
			mirror, err := channelapi.MirrorLegacyWhatsAppMessage(app.DB, channelapi.LegacyMetaAccountRef{
				ID: account.ID, OrganizationID: account.OrganizationID, Name: account.Name, Status: account.Status,
			}, message.ID)
			require.NoError(t, err)
			require.True(t, mirror.Linked)

			// First prove that the real mirror is usable and its trigger-owned
			// reservation is present, rather than hand-authoring either proof.
			app.handleIncomingReaction(account, contact.PhoneNumber, wamid, "👍", "Patient")
			var before models.Message
			require.NoError(t, app.DB.First(&before, "id = ?", message.ID).Error)
			require.NotNil(t, before.InboxConversationID)
			require.Equal(t, true, before.Metadata[database.WhatsAppWAMIDOwnerMetadataKey])
			reactions, ok := before.Metadata["reactions"].([]any)
			require.True(t, ok)
			require.Len(t, reactions, 1)
			reaction, ok := reactions[0].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "👍", reaction["emoji"])
			require.NoError(t, app.DB.Model(&models.InboxConversation{}).
				Where("id = ? AND organization_id = ?", mirror.ConversationID, account.OrganizationID).
				Update(tc.column, tc.value).Error)
			var projectionBefore models.InboxConversation
			require.NoError(t, app.DB.First(&projectionBefore, "id = ?", mirror.ConversationID).Error)

			app.handleIncomingReaction(account, contact.PhoneNumber, wamid, "🔥", "Patient")

			var after models.Message
			require.NoError(t, app.DB.First(&after, "id = ?", message.ID).Error)
			assert.Equal(t, before, after, "historical reservation must not authorize a live mutation through a changed projection")
			var projectionAfter models.InboxConversation
			require.NoError(t, app.DB.First(&projectionAfter, "id = ?", mirror.ConversationID).Error)
			assert.Equal(t, projectionBefore, projectionAfter, "rejection must not silently repair or relink the projection")
			var claimed bool
			require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
				if err := database.LockWhatsAppWAMIDScopes(scoped.DB, account.OrganizationID, wamid); err != nil {
					return err
				}
				if err := database.LockOrganizationPolicyScope(scoped.DB, account.OrganizationID); err != nil {
					return err
				}
				claimed, err = database.WhatsAppIdentityReviewWAMIDClaimed(scoped.DB, account.OrganizationID, wamid)
				return err
			}))
			assert.True(t, claimed, "rejecting mutation must not release the historical WAMID reservation")
			var owners int64
			require.NoError(t, app.DB.Unscoped().Model(&models.Message{}).
				Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, wamid).
				Count(&owners).Error)
			assert.EqualValues(t, 1, owners)
		})
	}
}

func TestIncomingReactionUsesStableOwnerAcrossAccountRename(t *testing.T) {
	app, account, contact := whatsappIdentityFixture(t)
	wamid := "wamid.reaction-rename-" + uuid.NewString()
	message := models.Message{
		BaseModel:      models.BaseModel{ID: uuid.NewSHA1(account.ID, []byte("coexistence-message:"+wamid))},
		OrganizationID: account.OrganizationID, ContactID: contact.ID,
		WhatsAppAccount: account.Name, WhatsAppMessageID: wamid,
		Direction: models.DirectionOutgoing, MessageType: models.MessageTypeText,
		Content: "stable owner", Status: models.MessageStatusSent, Metadata: models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&message).Error)
	originalAccountName := account.Name
	renameWhatsAppIdentityAccount(t, app, account)

	app.handleIncomingReaction(account, contact.PhoneNumber, wamid, "👍", "Patient")

	var stored models.Message
	require.NoError(t, app.DB.First(&stored, "id = ?", message.ID).Error)
	assert.NotEqual(t, originalAccountName, stored.WhatsAppAccount)
	assert.Equal(t, account.Name, stored.WhatsAppAccount,
		"the stable account proof may repair only the mutable display projection")
	reactions, ok := stored.Metadata["reactions"].([]any)
	require.True(t, ok)
	require.Len(t, reactions, 1)
}

func TestIncomingReactionRejectsUnprovenAndSoftDeletedOwners(t *testing.T) {
	for _, tc := range []struct {
		name          string
		deterministic bool
		softDelete    bool
	}{
		{name: "unproven", deterministic: false},
		{name: "soft_deleted", deterministic: true, softDelete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, account, contact := whatsappIdentityFixture(t)
			wamid := "wamid.reaction-reject-" + uuid.NewString()
			id := uuid.New()
			if tc.deterministic {
				id = uuid.NewSHA1(account.ID, []byte("coexistence-message:"+wamid))
			}
			message := models.Message{
				BaseModel: models.BaseModel{ID: id}, OrganizationID: account.OrganizationID,
				ContactID: contact.ID, WhatsAppAccount: account.Name, WhatsAppMessageID: wamid,
				Direction: models.DirectionOutgoing, MessageType: models.MessageTypeText,
				Content: "immutable owner", Status: models.MessageStatusSent, Metadata: models.JSONB{},
			}
			require.NoError(t, app.DB.Create(&message).Error)
			if tc.softDelete {
				require.NoError(t, app.DB.Delete(&message).Error)
			}

			app.handleIncomingReaction(account, contact.PhoneNumber, wamid, "🔥", "Patient")

			var stored models.Message
			require.NoError(t, app.DB.Unscoped().First(&stored, "id = ?", message.ID).Error)
			assert.NotContains(t, stored.Metadata, "reactions")
			assert.Equal(t, tc.softDelete, stored.DeletedAt.Valid)
		})
	}
}

func TestIncomingReactionSerializesMergedContactAliases(t *testing.T) {
	app, account, canonical := whatsappIdentityFixture(t)
	now := time.Now().UTC()
	alias := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	alias.PhoneNumber = "60" + testutil.NewTestGraphObjectID()
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", alias.ID).Updates(map[string]any{
		"phone_number": alias.PhoneNumber, "merged_into_id": canonical.ID,
		"merged_at": now, "deleted_at": now,
	}).Error)

	wamid := "wamid.reaction-concurrent-" + uuid.NewString()
	message := models.Message{
		BaseModel:      models.BaseModel{ID: uuid.NewSHA1(account.ID, []byte("coexistence-message:"+wamid))},
		OrganizationID: account.OrganizationID, ContactID: canonical.ID,
		WhatsAppAccount: account.Name, WhatsAppMessageID: wamid,
		Direction: models.DirectionOutgoing, MessageType: models.MessageTypeText,
		Content: "serialize reactions", Status: models.MessageStatusSent, Metadata: models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&message).Error)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, input := range []struct{ phone, emoji string }{
		{canonical.PhoneNumber, "👍"}, {alias.PhoneNumber, "❤️"},
	} {
		input := input
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			app.handleIncomingReaction(account, input.phone, wamid, input.emoji, "Patient")
		}()
	}
	close(start)
	wg.Wait()

	var stored models.Message
	require.NoError(t, app.DB.First(&stored, "id = ?", message.ID).Error)
	reactions, ok := stored.Metadata["reactions"].([]any)
	require.True(t, ok)
	require.Len(t, reactions, 2, "row and WAMID locks must prevent a lost read/modify/write")
	phones := map[string]bool{}
	for _, raw := range reactions {
		reaction, ok := raw.(map[string]any)
		require.True(t, ok)
		phones[getStringFromMap(reaction, "from_phone")] = true
	}
	assert.True(t, phones[normalizeCoexistencePhone(canonical.PhoneNumber)])
	assert.True(t, phones[normalizeCoexistencePhone(alias.PhoneNumber)])
}

// =============================================================================
// evaluateExpression (package-level, not on App)
// =============================================================================

// =============================================================================
// BSUID-only inbound senders on regular (non-Coexistence) accounts
// =============================================================================

func TestContactHasDialablePhone(t *testing.T) {
	t.Parallel()
	for _, phone := range []string{"60123456789", "+60123456789", "120363000000000000@g.us"} {
		assert.True(t, contactHasDialablePhone(&models.Contact{PhoneNumber: phone}), phone)
	}
	for _, phone := range []string{"", "+", " ", "bsuid:abc", "user:abc", "event:abc", "id:abc"} {
		assert.False(t, contactHasDialablePhone(&models.Contact{PhoneNumber: phone}), phone)
	}
	assert.False(t, contactHasDialablePhone(nil))
}

// Pure unit test: the refusal happens before any database access, so a nil DB
// proves no write was attempted.
func TestUpdateContactBSUIDNeverChangesContactWithoutDialablePhone(t *testing.T) {
	t.Parallel()
	app := &App{Log: testutil.NopLogger()}
	for _, phone := range []string{"", "+", "bsuid:0123", "user:0123", "event:0123", "id:0123"} {
		owned := &models.Contact{BaseModel: models.BaseModel{ID: uuid.New()}, PhoneNumber: phone, BSUID: "US.first-customer"}
		app.updateContactBSUID(owned, "US.second-customer")
		assert.Equal(t, "US.first-customer", owned.BSUID, "phone %q", phone)

		unowned := &models.Contact{BaseModel: models.BaseModel{ID: uuid.New()}, PhoneNumber: phone}
		app.updateContactBSUID(unowned, "US.second-customer")
		assert.Empty(t, unowned.BSUID, "phone %q", phone)
	}
}

func TestInboundSenderIdentityUsesWebhookContact(t *testing.T) {
	t.Parallel()
	webhookContact := &CoexistenceWebhookContact{WaID: "+60111111111", UserID: "US.sender"}
	webhookContact.Profile.Name = "Sender"
	webhookContact.Profile.Username = " sender_username "

	bsuidOnly := IncomingTextMessage{ID: "wamid.a", FromUserID: "US.sender", FromParentUserID: "US.parent"}
	bsuidOnly = bsuidOnly.withWebhookSenderContact(webhookContact)
	identity := inboundSenderIdentity(bsuidOnly, " Sender ")
	assert.Equal(t, "60111111111", identity.Phone, "wa_id stands in for an omitted from")
	assert.Equal(t, "US.sender", identity.UserID)
	assert.Equal(t, "US.parent", identity.ParentUserID)
	assert.Equal(t, "sender_username", identity.Username)
	assert.Equal(t, "Sender", identity.ProfileName)
	assert.True(t, bsuidOnly.hasSenderIdentity())

	withFrom := IncomingTextMessage{ID: "wamid.b", From: "60122222222", FromUserID: "US.sender"}
	withFrom = withFrom.withWebhookSenderContact(webhookContact)
	assert.Empty(t, withFrom.senderWaID, "wa_id never overrides an explicit from")
	assert.Equal(t, "60122222222", inboundSenderIdentity(withFrom, "").Phone)

	// The sidecar is never serialized into the durable continuation payload.
	encoded, err := json.Marshal(bsuidOnly)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "sender_username")
	assert.NotContains(t, string(encoded), "60111111111")
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, "", decoded["from"])
	assert.Equal(t, "US.sender", decoded["from_user_id"])

	assert.False(t, IncomingTextMessage{ID: "wamid.c"}.hasSenderIdentity())
	assert.Equal(t, IncomingTextMessage{ID: "wamid.d"}, IncomingTextMessage{ID: "wamid.d"}.withWebhookSenderContact(nil))
}

// Pure unit test: a message without any sender identity is rejected before
// any database access instead of reaching GetOrCreateContact with "".
func TestGetOrCreateNewInboundSenderContactRejectsAnonymousSender(t *testing.T) {
	t.Parallel()
	app := &App{Log: testutil.NopLogger()}
	account := &models.WhatsAppAccount{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: uuid.New()}
	contact, err := app.getOrCreateNewInboundSenderContact(account, IncomingTextMessage{ID: "wamid.anonymous"}, "Name")
	require.ErrorIs(t, err, errInboundSenderIdentityMissing)
	assert.Nil(t, contact)
}

func bsuidOnlyInboundMessage(t *testing.T, wamid, from, userID, body string) IncomingTextMessage {
	t.Helper()
	var message IncomingTextMessage
	require.NoError(t, json.Unmarshal([]byte(`{
		"from": "`+from+`",
		"from_user_id": "`+userID+`",
		"id": "`+wamid+`",
		"timestamp": "1722222222",
		"type": "text",
		"text": {"body": "`+body+`"}
	}`), &message))
	return message
}

func persistNonSMBInbound(t *testing.T, app *App, account *models.WhatsAppAccount, message IncomingTextMessage, profileName string) *persistedIncomingMessage {
	t.Helper()
	work, duplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		profileName,
		strings.Repeat("a", 64),
	)
	require.NoError(t, err)
	require.False(t, duplicate)
	require.NotNil(t, work)
	return work
}

func TestNonSMBBSUIDOnlySendersGetDistinctPlaceholderContacts(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	require.False(t, account.IsSMB)
	uid := uuid.NewString()[:8]
	firstUserID := "US.first-" + uid
	secondUserID := "US.second-" + uid

	firstContactSidecar := &CoexistenceWebhookContact{UserID: firstUserID}
	firstContactSidecar.Profile.Name = "First Username User"
	firstContactSidecar.Profile.Username = "first_" + uid
	first := bsuidOnlyInboundMessage(t, "wamid.bsuid-first-"+uid, "", firstUserID, "Hai").
		withWebhookSenderContact(firstContactSidecar)
	firstWork := persistNonSMBInbound(t, app, account, first, "First Username User")

	second := bsuidOnlyInboundMessage(t, "wamid.bsuid-second-"+uid, "", secondUserID, "Hello")
	secondWork := persistNonSMBInbound(t, app, account, second, "Second Username User")

	require.NotEqual(t, firstWork.Contact.ID, secondWork.Contact.ID)

	var firstContact, secondContact models.Contact
	require.NoError(t, app.DB.First(&firstContact, "id = ?", firstWork.Contact.ID).Error)
	require.NoError(t, app.DB.First(&secondContact, "id = ?", secondWork.Contact.ID).Error)
	assert.Equal(t, firstUserID, firstContact.BSUID, "the first sender's BSUID is never overwritten")
	assert.Equal(t, secondUserID, secondContact.BSUID)
	assert.Equal(t, coexistenceIdentityPlaceholder(coexistenceContactIdentity{UserID: firstUserID}), firstContact.PhoneNumber)
	assert.Equal(t, coexistenceIdentityPlaceholder(coexistenceContactIdentity{UserID: secondUserID}), secondContact.PhoneNumber)
	assert.True(t, strings.HasPrefix(firstContact.PhoneNumber, "bsuid:"))
	assert.Equal(t, "First Username User", firstContact.ProfileName)
	assert.Equal(t, "Second Username User", secondContact.ProfileName)
	assert.Equal(t, true, firstContact.Metadata["coexistence_phone_unavailable"])
	assert.Equal(t, true, firstContact.Metadata["coexistence_phone_placeholder"])
	assert.Equal(t, "first_"+uid, firstContact.Metadata["coexistence_username"])
	assert.Equal(t, account.Name, firstContact.WhatsAppAccount)

	var emptyPhoneCount int64
	require.NoError(t, app.DB.Unscoped().Model(&models.Contact{}).
		Where("organization_id = ? AND phone_number IN ?", org.ID, []string{"", "+"}).
		Count(&emptyPhoneCount).Error)
	assert.Zero(t, emptyPhoneCount)

	// A second message from the first sender stays on the first contact.
	again := bsuidOnlyInboundMessage(t, "wamid.bsuid-first-again-"+uid, "", firstUserID, "Again")
	againWork := persistNonSMBInbound(t, app, account, again, "First Username User")
	assert.Equal(t, firstContact.ID, againWork.Contact.ID)

	// Outbound to a placeholder contact carries only the BSUID.
	payload := map[string]any{}
	whatsapp.Recipient{Phone: secondContact.PhoneNumber, BSUID: secondContact.BSUID}.SetOnPayload(payload)
	_, hasTo := payload["to"]
	assert.False(t, hasTo)
	assert.Equal(t, secondUserID, payload["recipient"])
}

func TestNonSMBBSUIDPhoneRevealUpgradesPlaceholder(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	uid := uuid.NewString()[:8]
	userID := "US.reveal-" + uid
	phone := "6019" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]

	firstWork := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.reveal-first-"+uid, "", userID, "Hai"), "Reveal User")
	require.True(t, strings.HasPrefix(firstWork.Contact.PhoneNumber, "bsuid:"))

	revealWork := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.reveal-phone-"+uid, phone, userID, "Now with phone"), "Reveal User")
	assert.Equal(t, firstWork.Contact.ID, revealWork.Contact.ID, "the placeholder is upgraded, not duplicated")

	var upgraded models.Contact
	require.NoError(t, app.DB.First(&upgraded, "id = ?", firstWork.Contact.ID).Error)
	assert.Equal(t, phone, upgraded.PhoneNumber)
	assert.Equal(t, userID, upgraded.BSUID)
	assert.Equal(t, false, upgraded.Metadata["coexistence_phone_unavailable"])
	assert.Equal(t, false, upgraded.Metadata["coexistence_phone_placeholder"])

	var sharing int64
	require.NoError(t, app.DB.Model(&models.Contact{}).
		Where("organization_id = ? AND bs_uid = ?", org.ID, userID).
		Count(&sharing).Error)
	assert.EqualValues(t, 1, sharing, "no second contact shares the BSUID")

	payload := map[string]any{}
	whatsapp.Recipient{Phone: upgraded.PhoneNumber, BSUID: upgraded.BSUID}.SetOnPayload(payload)
	assert.Equal(t, phone, payload["to"])
}

func TestNonSMBBSUIDOnlyUsesWaIDOnlyWhenItDoesNotBelongToAnotherBSUID(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	uid := uuid.NewString()[:8]

	// wa_id for a phone nobody owns yet: the contact gets the real phone.
	freePhone := "6017" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	freeUserID := "US.wa-free-" + uid
	freeSidecar := &CoexistenceWebhookContact{WaID: freePhone, UserID: freeUserID}
	freeWork := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.wa-free-"+uid, "", freeUserID, "Hai").withWebhookSenderContact(freeSidecar),
		"WaID User")
	assert.Equal(t, freePhone, freeWork.Contact.PhoneNumber)
	assert.Equal(t, freeUserID, freeWork.Contact.BSUID)

	// wa_id for a phone whose contact belongs to another BSUID: never attach
	// this sender to it; use the per-sender placeholder instead.
	owner := testutil.CreateTestContact(t, app.DB, org.ID)
	require.NoError(t, app.DB.Model(owner).Update("bs_uid", "US.owner-"+uid).Error)
	otherUserID := "US.wa-other-" + uid
	otherSidecar := &CoexistenceWebhookContact{WaID: owner.PhoneNumber, UserID: otherUserID}
	otherWork := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.wa-other-"+uid, "", otherUserID, "Hai").withWebhookSenderContact(otherSidecar),
		"Other User")
	assert.NotEqual(t, owner.ID, otherWork.Contact.ID)
	assert.True(t, strings.HasPrefix(otherWork.Contact.PhoneNumber, "bsuid:"))

	var reloadedOwner models.Contact
	require.NoError(t, app.DB.First(&reloadedOwner, "id = ?", owner.ID).Error)
	assert.Equal(t, "US.owner-"+uid, reloadedOwner.BSUID)
	assert.Equal(t, owner.ProfileName, reloadedOwner.ProfileName)
}

func TestUpdateContactBSUIDDoesNotOverwriteBSUIDOfContactWithoutDialablePhone(t *testing.T) {
	app := newProcessorTestApp(t)
	org, _ := createProcessorTestOrg(t, app)
	uid := uuid.NewString()[:8]
	for _, phone := range []string{"", "bsuid:" + uid} {
		contact := &models.Contact{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: org.ID,
			PhoneNumber:    phone,
			BSUID:          "US.original-" + uid,
		}
		require.NoError(t, app.DB.Create(contact).Error)

		app.updateContactBSUID(contact, "US.intruder-"+uid)

		var saved models.Contact
		require.NoError(t, app.DB.First(&saved, "id = ?", contact.ID).Error)
		assert.Equal(t, "US.original-"+uid, saved.BSUID, "phone %q", phone)
		assert.Equal(t, "US.original-"+uid, contact.BSUID, "phone %q", phone)
	}
}

func TestInboundSenderMatchesUserIDs(t *testing.T) {
	t.Parallel()
	identity := coexistenceContactIdentity{UserID: "US.child", ParentUserID: "US.parent"}
	assert.True(t, inboundSenderMatchesUserIDs([]string{"US.child"}, identity))
	assert.True(t, inboundSenderMatchesUserIDs([]string{"US.child", "US.parent"}, identity))
	assert.False(t, inboundSenderMatchesUserIDs(nil, identity), "no proven inbound message")
	assert.False(t, inboundSenderMatchesUserIDs([]string{"US.child", ""}, identity), "a message without a recorded sender")
	assert.False(t, inboundSenderMatchesUserIDs([]string{"US.child", "US.other"}, identity), "a second sender")
	assert.False(t, inboundSenderMatchesUserIDs([]string{""}, coexistenceContactIdentity{}))
}

func TestLegacyEmptyPhoneAndSenderUserIDHelpers(t *testing.T) {
	t.Parallel()
	for _, phone := range []string{"", "+", " + "} {
		assert.True(t, isLegacyEmptyPhoneContact(&models.Contact{PhoneNumber: phone}), "phone %q", phone)
	}
	for _, phone := range []string{"bsuid:abc", "60123456789"} {
		assert.False(t, isLegacyEmptyPhoneContact(&models.Contact{PhoneNumber: phone}), "phone %q", phone)
	}
	assert.False(t, isLegacyEmptyPhoneContact(nil))

	identity := coexistenceContactIdentity{UserID: "US.child", ParentUserID: "US.parent"}
	assert.True(t, contactHoldsSenderUserID(&models.Contact{BSUID: "US.child"}, identity))
	assert.True(t, contactHoldsSenderUserID(&models.Contact{BSUID: "US.parent"}, identity))
	assert.False(t, contactHoldsSenderUserID(&models.Contact{BSUID: "US.other"}, identity))
	assert.False(t, contactHoldsSenderUserID(&models.Contact{}, coexistenceContactIdentity{}))
	assert.False(t, contactHoldsSenderUserID(nil, identity))
}

// Pure unit test: a "+"-only "from" is no phone. It must neither reach
// GetOrCreateContact (ErrEmptyPhoneNumber would make /api/webhook answer 503
// on every retry) nor hide the webhook contact's wa_id.
func TestPlusOnlyFromIsTreatedAsAbsentPhone(t *testing.T) {
	t.Parallel()
	app := &App{Log: testutil.NopLogger()}
	account := &models.WhatsAppAccount{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: uuid.New()}
	contact, err := app.getOrCreateNewInboundSenderContact(account, IncomingTextMessage{ID: "wamid.plus", From: " + "}, "Name")
	require.ErrorIs(t, err, errInboundSenderIdentityMissing)
	assert.Nil(t, contact)
	assert.False(t, IncomingTextMessage{ID: "wamid.plus", From: "+"}.hasSenderIdentity())

	withWaID := IncomingTextMessage{ID: "wamid.plus-wa", From: "+", FromUserID: "US.sender"}.
		withWebhookSenderContact(&CoexistenceWebhookContact{WaID: "60133333333", UserID: "US.sender"})
	assert.Equal(t, "60133333333", inboundSenderIdentity(withWaID, "").Phone)
}

func persistNonSMBInboundError(t *testing.T, app *App, account *models.WhatsAppAccount, message IncomingTextMessage, profileName string) error {
	t.Helper()
	work, _, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		profileName,
		strings.Repeat("a", 64),
	)
	require.Error(t, err)
	assert.Nil(t, work)
	return err
}

func testRevealedPhone() string {
	return "6016" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
}

func createLegacyEmptyPhoneContact(t *testing.T, app *App, orgID uuid.UUID, bsuid string) *models.Contact {
	t.Helper()
	contact := &models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		PhoneNumber:    "",
		BSUID:          bsuid,
		ProfileName:    "Legacy empty-phone contact",
	}
	require.NoError(t, app.DB.Create(contact).Error)
	return contact
}

func countContactsWithPhone(t *testing.T, app *App, orgID uuid.UUID, phone string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, app.DB.Unscoped().Model(&models.Contact{}).
		Where("organization_id = ? AND phone_number IN ?", orgID, []string{phone, "+" + phone}).
		Count(&count).Error)
	return count
}

// Route (b) with a legacy empty-phone contact that, under the old bug, holds
// two BSUID-only customers' messages: a phone reveal by the current bs_uid
// holder must not give that mixed row a real phone (it would drop out of the
// repair audit). The webhook keeps answering 503, as before the fix, until
// the row is repaired with the runbook.
func TestNonSMBPhoneRevealDoesNotUpgradeSharedLegacyEmptyPhoneContact(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	uid := uuid.NewString()[:8]
	firstUserID, secondUserID := "US.legacy-first-"+uid, "US.legacy-second-"+uid
	legacy := createLegacyEmptyPhoneContact(t, app, org.ID, firstUserID)

	firstWork := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.legacy-first-"+uid, "", firstUserID, "Hai"), "First")
	require.Equal(t, legacy.ID, firstWork.Contact.ID)
	// Reproduce the pre-fix overwrite by a second BSUID-only sender.
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", legacy.ID).Update("bs_uid", secondUserID).Error)
	secondWork := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.legacy-second-"+uid, "", secondUserID, "Hello"), "Second")
	require.Equal(t, legacy.ID, secondWork.Contact.ID)

	phone := testRevealedPhone()
	err := persistNonSMBInboundError(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.legacy-reveal-"+uid, phone, secondUserID, "Now with phone"), "Second")
	assert.ErrorIs(t, err, errEmptyPhoneContactNeedsRepair)

	var reloaded models.Contact
	require.NoError(t, app.DB.First(&reloaded, "id = ?", legacy.ID).Error)
	assert.Equal(t, "", reloaded.PhoneNumber, "the shared row stays visible to the runbook audit")
	assert.Equal(t, secondUserID, reloaded.BSUID)
	assert.Nil(t, reloaded.Metadata[emptyPhoneUpgradedFromKey])
	assert.Zero(t, countContactsWithPhone(t, app, org.ID, phone))

	// The same applies when the phone arrives only as contacts[].wa_id.
	sidecar := &CoexistenceWebhookContact{WaID: phone, UserID: secondUserID}
	waWork := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.legacy-waid-"+uid, "", secondUserID, "wa_id").withWebhookSenderContact(sidecar),
		"Second")
	assert.Equal(t, legacy.ID, waWork.Contact.ID, "wa_id is dropped; the message stays with the current bs_uid holder")
	require.NoError(t, app.DB.First(&reloaded, "id = ?", legacy.ID).Error)
	assert.Equal(t, "", reloaded.PhoneNumber)
	assert.Zero(t, countContactsWithPhone(t, app, org.ID, phone))
}

// Route (b) with a legacy empty-phone contact whose every inbound message is
// proven to come from this sender: the phone reveal upgrades it and records
// the original empty phone so the runbook audit still lists it.
func TestNonSMBPhoneRevealUpgradesSingleSenderLegacyEmptyPhoneContact(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	uid := uuid.NewString()[:8]
	userID := "US.legacy-single-" + uid
	legacy := createLegacyEmptyPhoneContact(t, app, org.ID, userID)

	firstWork := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.legacy-single-"+uid, "", userID, "Hai"), "Single")
	require.Equal(t, legacy.ID, firstWork.Contact.ID)

	phone := testRevealedPhone()
	revealWork := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.legacy-single-reveal-"+uid, phone, userID, "Now with phone"), "Single")
	assert.Equal(t, legacy.ID, revealWork.Contact.ID)

	var upgraded models.Contact
	require.NoError(t, app.DB.First(&upgraded, "id = ?", legacy.ID).Error)
	assert.Equal(t, phone, upgraded.PhoneNumber)
	assert.Equal(t, userID, upgraded.BSUID)
	assert.Equal(t, "", upgraded.Metadata[emptyPhoneUpgradedFromKey])
	assert.NotEmpty(t, upgraded.Metadata[emptyPhoneUpgradedAtKey])
	assert.EqualValues(t, 1, countContactsWithPhone(t, app, org.ID, phone))
}

// Route (b) reconciliation: the BSUID is on a placeholder and the revealed
// phone belongs to a contact without a BSUID. The phone contact takes the
// BSUID and the placeholder releases it.
func TestNonSMBPhoneRevealReconcilesPlaceholderWithPhoneOwner(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	uid := uuid.NewString()[:8]
	userID := "US.reconcile-" + uid

	placeholderWork := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.reconcile-first-"+uid, "", userID, "Hai"), "Reconcile User")
	require.True(t, strings.HasPrefix(placeholderWork.Contact.PhoneNumber, "bsuid:"))
	phoneOwner := testutil.CreateTestContact(t, app.DB, org.ID)

	revealWork := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.reconcile-phone-"+uid, strings.TrimPrefix(phoneOwner.PhoneNumber, "+"), userID, "Phone"),
		"Reconcile User")
	assert.Equal(t, phoneOwner.ID, revealWork.Contact.ID)

	var owner, placeholder models.Contact
	require.NoError(t, app.DB.First(&owner, "id = ?", phoneOwner.ID).Error)
	require.NoError(t, app.DB.First(&placeholder, "id = ?", placeholderWork.Contact.ID).Error)
	assert.Equal(t, userID, owner.BSUID)
	assert.Empty(t, placeholder.BSUID)
	assert.Equal(t, phoneOwner.ID.String(), placeholder.Metadata["coexistence_reconciled_contact_id"])
	assert.Equal(t, placeholderWork.Contact.PhoneNumber, placeholder.PhoneNumber)

	var holders int64
	require.NoError(t, app.DB.Model(&models.Contact{}).
		Where("organization_id = ? AND bs_uid = ?", org.ID, userID).Count(&holders).Error)
	assert.EqualValues(t, 1, holders)
}

// Route (b) phone conflict: the revealed phone belongs to a contact with a
// different BSUID. Nothing is attached or rewritten; the admission fails
// identity validation and rolls back (503, Meta retries) for manual review.
func TestNonSMBPhoneRevealConflictRollsBack(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	uid := uuid.NewString()[:8]
	userID, otherUserID := "US.conflict-"+uid, "US.conflict-owner-"+uid

	placeholderWork := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.conflict-first-"+uid, "", userID, "Hai"), "Conflict User")
	phoneOwner := testutil.CreateTestContact(t, app.DB, org.ID)
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", phoneOwner.ID).Update("bs_uid", otherUserID).Error)

	persistNonSMBInboundError(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.conflict-phone-"+uid, strings.TrimPrefix(phoneOwner.PhoneNumber, "+"), userID, "Phone"),
		"Conflict User")

	var owner, placeholder models.Contact
	require.NoError(t, app.DB.First(&owner, "id = ?", phoneOwner.ID).Error)
	require.NoError(t, app.DB.First(&placeholder, "id = ?", placeholderWork.Contact.ID).Error)
	assert.Equal(t, otherUserID, owner.BSUID)
	assert.Equal(t, userID, placeholder.BSUID)
	assert.Equal(t, placeholderWork.Contact.PhoneNumber, placeholder.PhoneNumber)
	assert.Nil(t, placeholder.Metadata["coexistence_phone_conflict"], "the conflict note rolled back with the admission")
}

// Route (a): a contacts[].wa_id must never rewrite the phone of the
// established contact that already holds the sender's BSUID.
func TestNonSMBWaIDDoesNotRewritePhoneOfBSUIDOwner(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	uid := uuid.NewString()[:8]
	userID := "US.wa-owner-" + uid
	owner := testutil.CreateTestContact(t, app.DB, org.ID)
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", owner.ID).Update("bs_uid", userID).Error)

	otherPhone := testRevealedPhone()
	sidecar := &CoexistenceWebhookContact{WaID: otherPhone, UserID: userID}
	work := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.wa-owner-"+uid, "", userID, "Hai").withWebhookSenderContact(sidecar),
		"Owner")
	assert.Equal(t, owner.ID, work.Contact.ID)

	var reloaded models.Contact
	require.NoError(t, app.DB.First(&reloaded, "id = ?", owner.ID).Error)
	assert.Equal(t, owner.PhoneNumber, reloaded.PhoneNumber)
	assert.Equal(t, userID, reloaded.BSUID)
	assert.Zero(t, countContactsWithPhone(t, app, org.ID, otherPhone))
}

// Route (c): the sender's phone is a merge alias of a "WhatsApp number
// hidden" placeholder whose bs_uid is empty. The placeholder learns the
// BSUID instead of failing identity validation (503) on every retry.
func TestNonSMBPhoneMergedIntoPlaceholderFillsEmptyBSUID(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	uid := uuid.NewString()[:8]
	userID := "US.merged-" + uid
	placeholder := &models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		PhoneNumber:    coexistenceIdentityPlaceholder(coexistenceContactIdentity{UserID: "US.earlier-" + uid}),
		ProfileName:    "Hidden number",
	}
	require.NoError(t, app.DB.Create(placeholder).Error)
	alias := testutil.CreateTestContact(t, app.DB, org.ID)
	aliasPhone := testRevealedPhone()
	now := time.Now().UTC()
	require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", alias.ID).Updates(map[string]any{
		"phone_number": aliasPhone, "merged_into_id": placeholder.ID,
		"merged_at": now, "deleted_at": now,
	}).Error)

	work := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.merged-"+uid, aliasPhone, userID, "Hai"), "Merged")
	assert.Equal(t, placeholder.ID, work.Contact.ID)

	var reloaded models.Contact
	require.NoError(t, app.DB.First(&reloaded, "id = ?", placeholder.ID).Error)
	assert.Equal(t, userID, reloaded.BSUID)
	assert.Equal(t, placeholder.PhoneNumber, reloaded.PhoneNumber)
}

// updateContactBSUID never gives a second live contact a BSUID that another
// contact already holds (the call webhook path matches by phone first).
func TestUpdateContactBSUIDSkipsBSUIDHeldByAnotherContact(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	uid := uuid.NewString()[:8]
	userID := "US.held-" + uid
	placeholderWork := persistNonSMBInbound(t, app, account,
		bsuidOnlyInboundMessage(t, "wamid.held-"+uid, "", userID, "Hai"), "Held")

	phoneContact := testutil.CreateTestContact(t, app.DB, org.ID)
	app.updateContactBSUID(phoneContact, userID)
	assert.Empty(t, phoneContact.BSUID)

	var reloaded models.Contact
	require.NoError(t, app.DB.First(&reloaded, "id = ?", phoneContact.ID).Error)
	assert.Empty(t, reloaded.BSUID)
	require.NoError(t, app.DB.First(&reloaded, "id = ?", placeholderWork.Contact.ID).Error)
	assert.Equal(t, userID, reloaded.BSUID)
}
