package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime/multipart"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The waiting handler owns a normal multi-connection pool. Do not pin its App
// to the holder's transaction: access checks may legitimately use a root pool.
// The unique application_name identifies its real blocking backend without a
// database-wide "some writer is waiting" timing oracle.
type laneFMetadataWait struct {
	ctx        context.Context
	cancel     context.CancelFunc
	app        *App
	pool       *sql.DB
	holder     *gorm.DB
	holderPID  int
	name       string
	finished   chan struct{}
	result     chan error
	started    bool
	observerDB *gorm.DB
}

func laneFMetadataEnv(t *testing.T) laneFNameEnv {
	t.Helper()
	// These regressions are required when selected, not optional observations.
	require.NotEmpty(t, os.Getenv("TEST_DATABASE_URL"))
	require.NotEmpty(t, os.Getenv("TEST_REDIS_URL"))
	require.NotNil(t, testutil.SetupTestRedis(t), "synthetic Redis fixture must be available")
	return laneFNewNameEnv(t, true)
}

func laneFNewMetadataWait(t *testing.T, env laneFNameEnv, id uuid.UUID, deleted bool) (*laneFMetadataWait, models.Contact) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	f := &laneFMetadataWait{
		ctx: ctx, cancel: cancel, name: "lanef_metadata_" + uuid.NewString(),
		finished: make(chan struct{}), result: make(chan error, 1),
		observerDB: env.app.DB.WithContext(ctx),
	}
	t.Cleanup(func() { f.close(t) })
	parsed, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, parsed.Scheme)
	query := parsed.Query()
	query.Set("application_name", f.name)
	query.Set("statement_cache_capacity", "0")
	query.Set("default_query_exec_mode", "describe_exec")
	parsed.RawQuery = query.Encode()
	pool, err := database.NewPostgresWithContext(ctx, &config.DatabaseConfig{
		URL: parsed.String(), MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: 300,
	}, false)
	require.NoError(t, err)
	f.pool, err = pool.DB()
	require.NoError(t, err)
	f.app = &App{Config: env.app.Config, DB: pool.WithContext(ctx), Redis: env.app.Redis, Log: env.app.Log}
	f.holder = env.app.DB.WithContext(ctx).Begin()
	require.NoError(t, f.holder.Error)
	require.NoError(t, f.holder.Raw("SELECT pg_backend_pid()").Scan(&f.holderPID).Error)
	require.Positive(t, f.holderPID)
	rows := f.holder
	if deleted {
		rows = rows.Unscoped()
	}
	var locked models.Contact
	require.NoError(t, rows.Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&locked, "organization_id = ? AND id = ?", env.account.OrganizationID, id).Error)
	return f, locked
}

func (f *laneFMetadataWait) start(handler func() error) {
	f.started = true
	go func() {
		defer close(f.finished)
		// Assertions and response decoding stay on the test goroutine. A Fatal
		// here would otherwise abandon the result channel and hide the cause.
		f.result <- handler()
	}()
}

func (f *laneFMetadataWait) awaitBlocked(t *testing.T) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var pids []int
		require.NoError(t, f.observerDB.Raw(`
			SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND application_name = ?
			  AND wait_event_type = 'Lock' AND ? = ANY(pg_blocking_pids(pid))
		`, f.name, f.holderPID).Scan(&pids).Error)
		require.LessOrEqual(t, len(pids), 1, "only one waiting handler is launched")
		select {
		case <-f.finished:
			t.Fatalf("handler returned before the required contact-row wait: %v", <-f.result)
		default:
		}
		if len(pids) == 1 {
			require.Positive(t, pids[0])
			require.NotEqual(t, f.holderPID, pids[0])
			return
		}
		select {
		case <-f.ctx.Done():
			t.Fatal("fixture context ended before the exact waiter blocked")
		case <-deadline.C:
			t.Fatal("tagged handler backend never blocked on the holder PID")
		case <-tick.C:
		}
	}
}

func (f *laneFMetadataWait) commitAndJoin(t *testing.T) {
	t.Helper()
	require.NoError(t, f.holder.Commit().Error)
	select {
	case <-f.finished:
		require.NoError(t, <-f.result)
	case <-f.ctx.Done():
		t.Fatal("handler did not finish within the fixture context")
	}
}

func (f *laneFMetadataWait) close(t *testing.T) {
	t.Helper()
	// Attempt all cleanup steps even if a prior one fails. No backend is killed
	// and no shared pool is closed; cancellation belongs only to this fixture.
	if f.holder != nil {
		if err := f.holder.Rollback().Error; err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback held contact: %v", err)
		}
	}
	f.cancel()
	if f.started {
		select {
		case <-f.finished:
		case <-time.After(10 * time.Second):
			t.Error("waiting handler did not join during cleanup")
		}
	}
	if f.app != nil {
		joined := make(chan struct{})
		go func() { f.app.wg.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("owned App background work did not join")
		}
	}
	if f.pool != nil {
		closed := make(chan error, 1)
		go func() { closed <- f.pool.Close() }()
		select {
		case err := <-closed:
			assert.NoError(t, err, "close owned waiter pool")
		case <-time.After(5 * time.Second):
			t.Error("owned waiter pool did not close")
		}
	}
}

func laneFCurrentNestedMetadata() map[string]any {
	return map[string]any{"revision": "current", "nested": map[string]any{"keep": "current", "enabled": true}}
}

// Like the established held-row Lane F fixtures, this represents an in-flight
// writer using the production name decision and row-local manual-unread SQL.
// It is not a replay of the entire signed state-sync/admission route. Unscoped
// applies only to the deliberately soft-deleted restore fixture.
func laneFWriteHeldMetadata(t *testing.T, f *laneFMetadataWait, locked models.Contact, manual bool, name string) {
	t.Helper()
	locked.Metadata = contactutil.CloneContactMetadata(locked.Metadata)
	locked.Metadata["opaque_extension"] = laneFCurrentNestedMetadata()
	updates := map[string]any{}
	if manual {
		applyContactEditName(&locked, &name, nil, updates, false)
	} else {
		updates["profile_name"] = applyCoexistenceSavedContactName(locked.ProfileName, locked.Metadata, name, laneFAt(20))
		updates["metadata"] = locked.Metadata
	}
	rows := f.holder
	if locked.DeletedAt.Valid {
		rows = rows.Unscoped()
	}
	require.NoError(t, rows.Model(&models.Contact{}).
		Where("organization_id = ? AND id = ?", locked.OrganizationID, locked.ID).Updates(updates).Error)
	writer := &App{DB: rows}
	require.NoError(t, writer.setContactManualUnread(locked.OrganizationID, locked.ID))
	var current models.Contact
	require.NoError(t, rows.First(&current, "organization_id = ? AND id = ?", locked.OrganizationID, locked.ID).Error)
	require.Equal(t, laneFCurrentNestedMetadata(), current.Metadata["opaque_extension"])
	require.Equal(t, true, current.Metadata[contactManualUnreadKey])
	require.Equal(t, name, current.ProfileName)
}

func laneFMetadataImportRequest(t *testing.T, env laneFNameEnv, phone, name string) *fastglue.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("table", "contacts"))
	require.NoError(t, writer.WriteField("update_on_duplicate", "true"))
	part, err := writer.CreateFormFile("file", "contacts.csv")
	require.NoError(t, err)
	_, err = fmt.Fprintf(part, "phone_number,profile_name,tags\n%s,%s,owned_import\n", phone, name)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType(writer.FormDataContentType())
	req.RequestCtx.Request.SetBody(body.Bytes())
	testutil.SetAuthContext(req, env.account.OrganizationID, env.admin.ID)
	return req
}

func TestLaneFImportRestoreWaitingOnMetadataUpdate(t *testing.T) {
	for _, operation := range []string{"import", "restore"} {
		t.Run(operation, func(t *testing.T) {
			env := laneFMetadataEnv(t)
			phone := laneFPhone()
			laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved initial", at: laneFAt(1)})
			contact := laneFContactByPhone(t, env, phone)
			contact.Metadata["opaque_extension"] = map[string]any{"revision": "before"}
			contact.Metadata["unrelated"] = "retained"
			require.NoError(t, env.app.DB.Model(&contact).Update("metadata", contact.Metadata).Error)
			if operation == "restore" {
				require.NoError(t, env.app.DB.Delete(&contact).Error)
			}
			requestedName, expectedSource := "Saved current", contactutil.ContactNameSourceWABusiness
			var req *fastglue.Request
			if operation == "import" {
				req = laneFMetadataImportRequest(t, env, phone, requestedName)
			} else {
				requestedName, expectedSource = "Restored manual", contactutil.ContactNameSourceManual
				// A name-only restore preserves unrelated metadata. An explicit
				// editable-metadata replacement has a different public contract.
				req = testutil.NewJSONRequest(t, map[string]any{"phone_number": phone, "profile_name": requestedName})
				testutil.SetAuthContext(req, env.account.OrganizationID, env.admin.ID)
			}
			f, locked := laneFNewMetadataWait(t, env, contact.ID, operation == "restore")
			laneFWriteHeldMetadata(t, f, locked, false, "Saved current")
			if operation == "import" {
				f.start(func() error { return f.app.ImportData(req) })
			} else {
				f.start(func() error { return f.app.CreateContact(req) })
			}
			f.awaitBlocked(t)
			f.commitAndJoin(t)
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
			if operation == "import" {
				var response map[string]any
				testutil.ParseEnvelopeResponse(t, req, &response)
				assert.EqualValues(t, 1, response["updated"])
				for _, key := range []string{"created", "skipped", "errors"} {
					assert.EqualValues(t, 0, response[key], key)
				}
			} else {
				var response ContactResponse
				testutil.ParseEnvelopeResponse(t, req, &response)
				assert.Equal(t, contact.ID, response.ID)
				assert.Equal(t, requestedName, response.ProfileName)
			}
			bounded := env
			bounded.app = f.app
			stored := laneFContact(t, bounded, contact.ID)
			assert.False(t, stored.DeletedAt.Valid)
			if operation == "import" {
				assert.Equal(t, models.JSONBArray{"owned_import"}, stored.Tags, "the waiting import made its own positive change")
			}
			laneFAssertName(t, stored, requestedName, expectedSource)
			assert.Equal(t, laneFCurrentNestedMetadata(), stored.Metadata["opaque_extension"])
			assert.Equal(t, true, stored.Metadata[contactManualUnreadKey])
			assert.Equal(t, "retained", stored.Metadata["unrelated"])
			assert.Equal(t, "Saved current", stored.Metadata[contactutil.ContactWABusinessSavedNameKey])
			assert.Equal(t, laneFAt(20).Format(time.RFC3339), stored.Metadata[contactutil.ContactWABusinessSavedNameAtKey])
			laneFInbound(t, bounded, phone, "Later profile")
			laneFAssertName(t, laneFContact(t, bounded, contact.ID), requestedName, expectedSource)
			if operation == "restore" {
				laneFSync(t, bounded, laneFSyncItem{action: "add", phone: phone, fullName: "Later saved", at: laneFAt(30)})
				laneFAssertName(t, laneFContact(t, bounded, contact.ID), requestedName, expectedSource)
			}
		})
	}
}

func TestLaneFMergeWaitingOnCurrentNameOwner(t *testing.T) {
	for _, owner := range []string{"manual", "state_sync"} {
		t.Run(owner, func(t *testing.T) {
			env := laneFMetadataEnv(t)
			phone := laneFPhone()
			laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved initial", at: laneFAt(1)})
			target := laneFContactByPhone(t, env, phone)
			target.Metadata["opaque_extension"] = map[string]any{"revision": "before"}
			require.NoError(t, env.app.DB.Model(&target).Update("metadata", target.Metadata).Error)
			source := models.Contact{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: env.account.OrganizationID,
				PhoneNumber: laneFPhone(), ProfileName: "Source manual",
				Metadata: models.JSONB{
					contactutil.ContactNameSourceKey:          contactutil.ContactNameSourceManual,
					contactutil.ContactWABusinessSavedNameKey: "Source saved",
					"opaque_extension":                        map[string]any{"revision": "source must not win"},
					"source_extension":                        map[string]any{"retained": true},
				},
			}
			require.NoError(t, env.app.DB.Create(&source).Error)
			key := "lanef-metadata-merge-" + uuid.NewString()
			req := testutil.NewJSONRequest(t, MergeContactRequest{
				SourceContactID: source.ID, Confirm: true, IdempotencyKey: key, Reason: "synthetic metadata intersection",
			})
			testutil.SetAuthContext(req, env.account.OrganizationID, env.admin.ID)
			testutil.SetPathParam(req, "id", target.ID.String())
			f, locked := laneFNewMetadataWait(t, env, target.ID, false)
			name, expectedSource, saved, savedAt := "Target current saved", contactutil.ContactNameSourceWABusiness, "Target current saved", laneFAt(20)
			if owner == "manual" {
				name, expectedSource, saved, savedAt = "Target current manual", contactutil.ContactNameSourceManual, "Saved initial", laneFAt(1)
			}
			laneFWriteHeldMetadata(t, f, locked, owner == "manual", name)
			f.start(func() error { return f.app.MergeContact(req) })
			f.awaitBlocked(t)
			f.commitAndJoin(t)
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
			var response map[string]any
			testutil.ParseEnvelopeResponse(t, req, &response)
			assert.Equal(t, true, response["merged"])
			assert.Equal(t, target.ID.String(), response["target_contact_id"])
			assert.Equal(t, source.ID.String(), response["source_contact_id"])
			bounded := env
			bounded.app = f.app
			stored := laneFContact(t, bounded, target.ID)
			laneFAssertName(t, stored, name, expectedSource)
			assert.Equal(t, laneFCurrentNestedMetadata(), stored.Metadata["opaque_extension"])
			assert.Equal(t, true, stored.Metadata[contactManualUnreadKey])
			assert.Equal(t, map[string]any{"retained": true}, stored.Metadata["source_extension"])
			assert.Equal(t, saved, stored.Metadata[contactutil.ContactWABusinessSavedNameKey])
			assert.Equal(t, savedAt.Format(time.RFC3339), stored.Metadata[contactutil.ContactWABusinessSavedNameAtKey])
			var alias models.Contact
			require.NoError(t, f.app.DB.Unscoped().First(&alias, "organization_id = ? AND id = ?", env.account.OrganizationID, source.ID).Error)
			require.NotNil(t, alias.MergedIntoID)
			assert.Equal(t, target.ID, *alias.MergedIntoID)
			assert.True(t, alias.DeletedAt.Valid)
			var activities int64
			require.NoError(t, f.app.DB.Model(&models.CustomerActivityEvent{}).
				Where("organization_id = ? AND contact_id = ? AND source_object_id = ? AND event_type = ? AND idempotency_key = ?",
					env.account.OrganizationID, target.ID, source.ID, models.CustomerActivityContactMerged, "contact-merge:"+key).
				Count(&activities).Error)
			assert.EqualValues(t, 1, activities)
			laneFInbound(t, bounded, phone, "Later profile")
			laneFAssertName(t, laneFContact(t, bounded, target.ID), name, expectedSource)
			if owner == "manual" {
				laneFSync(t, bounded, laneFSyncItem{action: "add", phone: phone, fullName: "Later saved", at: laneFAt(30)})
				laneFAssertName(t, laneFContact(t, bounded, target.ID), name, expectedSource)
			}
		})
	}
}
