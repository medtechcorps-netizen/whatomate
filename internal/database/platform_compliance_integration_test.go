package database_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const complianceWriteTrigger = "rereply_platform_compliance_write_guard"

func TestPlatformComplianceDatabaseWriteBarrier(t *testing.T) {
	db, runtimeRole := setupPlatformComplianceGuardTest(t)

	// Startup verification is part of the normal RLS contract, not a bootstrap
	// command side effect.
	runtimeDB := testutil.OpenTestDBAsRole(t, runtimeRole, guardRuntimePassword(runtimeRole))
	require.NoError(t, database.VerifyTenantRLS(runtimeDB, runtimeRole))

	purpose := createGuardTestOrganization(t, db, true)
	ordinary := createGuardTestOrganization(t, db, false)

	t.Run("direct insert and update-to-purpose are rejected", func(t *testing.T) {
		insert := db.Exec(`
			INSERT INTO public.users (id, organization_id, email, full_name, created_at, updated_at)
			VALUES (?, ?, ?, 'Blocked', now(), now())
		`, uuid.New(), purpose.ID, "blocked-"+uuid.NewString()+"@example.test")
		require.Error(t, insert.Error)
		assert.Contains(t, insert.Error.Error(), "rejects rows for platform compliance")

		userID := uuid.New()
		require.NoError(t, db.Exec(`
			INSERT INTO public.users (id, organization_id, email, full_name, created_at, updated_at)
			VALUES (?, ?, ?, 'Ordinary', now(), now())
		`, userID, ordinary.ID, "ordinary-"+uuid.NewString()+"@example.test").Error)
		move := db.Exec("UPDATE public.users SET organization_id = ? WHERE id = ?", purpose.ID, userID)
		require.Error(t, move.Error)
		assert.Contains(t, move.Error.Error(), "rejects rows for platform compliance")
	})

	t.Run("committed ordinary organization can never transition to purpose", func(t *testing.T) {
		candidate := createGuardTestOrganization(t, db, false)
		userID := uuid.New()
		require.NoError(t, db.Exec(`
			INSERT INTO public.users (id, organization_id, email, full_name, deleted_at, created_at, updated_at)
			VALUES (?, ?, ?, 'Historical', now(), now(), now())
		`, userID, candidate.ID, "history-"+uuid.NewString()+"@example.test").Error)
		transition := db.Exec(`
			UPDATE public.organizations
			SET settings = COALESCE(settings, '{}'::jsonb) || '{"platform_compliance_tenant":true}'::jsonb
			WHERE id = ?
		`, candidate.ID)
		require.Error(t, transition.Error)
		assert.Contains(t, transition.Error.Error(), "existing ordinary organizations cannot acquire")
		createErr := database.CreatePlatformComplianceOrganization(
			db, candidate.ID, "guard-test-existing-ordinary", true, false,
		)
		require.Error(t, createErr)
		assert.Contains(t, createErr.Error(), "already exists or was previously used")
	})

	t.Run("narrow Instagram fallback shape is admitted but ordinary shapes are not", func(t *testing.T) {
		now := time.Now().UTC()
		allowed := models.MetaInstagramDataDeletionEvent{
			Digest: strings.Repeat("a", 64), PlatformAppID: strings.Repeat("b", 64),
			AuthorizingUserID: strings.Repeat("c", 64), IssuedAt: now.Add(-time.Minute),
			VerifiedAt: now, State: "verified", OrganizationID: purpose.ID,
			TargetResolution: "no_current_target", IdentityHashed: true, LastAttemptAt: &now,
		}
		require.NoError(t, db.Create(&allowed).Error)
		moveAllowed := db.Model(&models.MetaInstagramDataDeletionEvent{}).
			Where("digest = ?", allowed.Digest).
			Update("organization_id", ordinary.ID)
		require.Error(t, moveAllowed.Error)
		assert.Contains(t, moveAllowed.Error.Error(), "rejects moving a platform compliance row")
		ordinaryShape := allowed
		ordinaryShape.Digest = strings.Repeat("d", 64)
		ordinaryShape.TargetResolution = "exact_target"
		require.Error(t, db.Create(&ordinaryShape).Error)

		ordinaryAudit := models.AuditLog{
			ID: uuid.New(), OrganizationID: purpose.ID, ResourceType: "contact",
			ResourceID: uuid.New(), UserID: uuid.New(), UserName: "clinic user",
			Action: models.AuditActionUpdated, Changes: models.JSONBArray{}, CreatedAt: now,
		}
		require.Error(t, db.Create(&ordinaryAudit).Error)

		binding := db.Exec(`
			INSERT INTO public.threads_platform_bindings
				(id, organization_id, integration_id, platform_app_key, platform_app_id,
				 oauth_subject_id, authority_asset_id, configuration_generation, claimed_at, created_at, updated_at)
			VALUES (?, ?, ?, 'managed', 'app', 'subject', 'asset', 1, now(), now(), now())
		`, uuid.New(), purpose.ID, uuid.New())
		require.Error(t, binding.Error)
	})

	t.Run("audit exemption rejects JSON escapes and runtime-forged evidence", func(t *testing.T) {
		var ownerCurrent, ownerSession string
		require.NoError(t, db.Raw(
			"SELECT current_user::text, session_user::text",
		).Row().Scan(&ownerCurrent, &ownerSession))
		const runID = "guard-test-audit-shape-001"
		validChanges := platformComplianceAuditChanges(
			runID, ownerCurrent, ownerSession,
			map[string]any{
				"field": "threads_managed_compliance_tenant", "old_value": nil, "new_value": true,
			},
		)
		valid := models.AuditLog{
			ID: purpose.ID, OrganizationID: purpose.ID, ResourceType: "platform_compliance",
			ResourceID: purpose.ID, UserID: uuid.Nil,
			UserName: "platform-compliance-bootstrap/" + runID,
			Action:   models.AuditActionUpdated, Changes: validChanges, CreatedAt: time.Now().UTC(),
		}
		valid.ID = uuid.New()
		directInsert := db.Create(&valid).Error
		require.Error(t, directInsert)
		assert.Contains(t, directInsert.Error(), "transaction-bound and append-only")
		var creationAudit models.AuditLog
		require.NoError(t, db.Where(
			"organization_id = ? AND resource_type = 'platform_compliance'", purpose.ID,
		).First(&creationAudit).Error)

		maliciousChanges := platformComplianceAuditChanges(
			runID, ownerCurrent, ownerSession,
			map[string]any{
				"field": "threads_managed_compliance_tenant", "old_value": nil, "new_value": true,
			},
		)
		maliciousChanges = append(maliciousChanges, map[string]any{
			"field":     "meta_instagram_data_deletion_compliance_tenant",
			"old_value": map[string]any{"secret": "escape"}, "new_value": map[string]any{"free": "text"},
		})
		malicious := valid
		malicious.ID = uuid.New()
		malicious.Changes = maliciousChanges
		require.Error(t, db.Create(&malicious).Error)
		require.Error(t, db.Model(&models.AuditLog{}).Where("id = ?", creationAudit.ID).
			Update("changes", maliciousChanges).Error)

		sqlDB, err := db.DB()
		require.NoError(t, err)
		connection, err := sqlDB.Conn(context.Background())
		require.NoError(t, err)
		defer func() {
			if closeErr := connection.Close(); closeErr != nil {
				t.Errorf("close runtime audit connection: %v", closeErr)
			}
		}()
		_, err = connection.ExecContext(context.Background(), "SET SESSION AUTHORIZATION "+runtimeRole)
		require.NoError(t, err)
		defer func() {
			if _, resetErr := connection.ExecContext(context.Background(), "RESET app.current_organization_id"); resetErr != nil {
				t.Errorf("reset runtime tenant context: %v", resetErr)
			}
			if _, resetErr := connection.ExecContext(context.Background(), "RESET SESSION AUTHORIZATION"); resetErr != nil {
				t.Errorf("reset session authorization: %v", resetErr)
			}
		}()
		_, err = connection.ExecContext(
			context.Background(),
			"SELECT pg_catalog.set_config('app.current_organization_id', $1, false)",
			purpose.ID.String(),
		)
		require.NoError(t, err)
		forgedChanges := platformComplianceAuditChanges(
			runID, ownerCurrent, runtimeRole,
			map[string]any{
				"field": "threads_managed_compliance_tenant", "old_value": nil, "new_value": true,
			},
		)
		forgedJSON, err := json.Marshal(forgedChanges)
		require.NoError(t, err)
		_, runtimeInsertErr := connection.ExecContext(context.Background(), `
			INSERT INTO public.audit_logs
				(id, organization_id, resource_type, resource_id, user_id, user_name, action, changes, created_at)
			VALUES ($1, $2, 'platform_compliance', $2, $3, $4, 'updated', CAST($5 AS jsonb), now())
		`, uuid.New(), purpose.ID, uuid.Nil, "platform-compliance-bootstrap/"+runID, string(forgedJSON))
		require.Error(t, runtimeInsertErr)
		assert.Contains(t, runtimeInsertErr.Error(), "transaction-bound and append-only")
	})

	t.Run("feature transitions emit one transaction-bound audit", func(t *testing.T) {
		runID := "guard-feature-audit-" + strings.ReplaceAll(uuid.NewString(), "-", "")
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			if err := database.SetPlatformComplianceOperatorIntent(
				tx, runID, "enable:threads",
			); err != nil {
				return err
			}
			return tx.Exec(`
				UPDATE public.organizations
				SET settings = settings || '{"threads_managed_compliance_tenant":true}'::jsonb,
					updated_at = now()
				WHERE id = ?
			`, purpose.ID).Error
		}))
		var auditCount int64
		require.NoError(t, db.Model(&models.AuditLog{}).
			Where("organization_id = ? AND user_name = ?", purpose.ID, "platform-compliance-bootstrap/"+runID).
			Count(&auditCount).Error)
		assert.Equal(t, int64(1), auditCount)

		duplicateRun := db.Transaction(func(tx *gorm.DB) error {
			if err := database.SetPlatformComplianceOperatorIntent(
				tx, runID, "remove:instagram",
			); err != nil {
				return err
			}
			return tx.Exec(`
				UPDATE public.organizations
				SET settings = settings - 'meta_instagram_data_deletion_compliance_tenant',
					updated_at = now()
				WHERE id = ?
			`, purpose.ID).Error
		})
		require.Error(t, duplicateRun)
		var instagramStillEnabled bool
		require.NoError(t, db.Raw(`
			SELECT settings -> 'meta_instagram_data_deletion_compliance_tenant' = 'true'::jsonb
			FROM public.organizations WHERE id = ?
		`, purpose.ID).Scan(&instagramStillEnabled).Error)
		assert.True(t, instagramStillEnabled)
	})

	t.Run("privacy exemptions admit only exact current fallback shapes", func(t *testing.T) {
		now := time.Now().UTC()
		requestID := uuid.New()
		request := models.PrivacyRequest{
			BaseModel: models.BaseModel{ID: requestID}, OrganizationID: purpose.ID,
			RequestNumber: "IGDEL" + strings.ToUpper(strings.ReplaceAll(requestID.String(), "-", "")),
			Type:          models.PrivacyRequestTypeDeletion, Status: models.PrivacyRequestStatusInProgress,
			SubjectType: "instagram_profile", SubjectKey: strings.Repeat("e", 64),
			ReceivedChannel: "instagram", RequesterProfile: models.JSONB{},
			RequestDetails: models.JSONB{
				"source": "meta_instagram_managed_login", "target_resolution": "no_current_target",
				"requires_manual_resolution": false,
			},
			VerificationMethod: "meta_instagram_signed_request", VerificationTokenHash: strings.Repeat("f", 64),
			ReceivedAt: now, DueAt: now.Add(30 * 24 * time.Hour), VerifiedAt: &now,
		}
		require.NoError(t, db.Create(&request).Error)
		event := models.PrivacyRequestEvent{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: purpose.ID,
			PrivacyRequestID: request.ID, EventType: "request_created",
			ToStatus: models.PrivacyRequestStatusInProgress,
			Message:  "Managed Instagram data deletion request verified and initiated",
			Details:  models.JSONB{"verification": "meta_instagram_signed_request"}, OccurredAt: now,
		}
		require.NoError(t, db.Create(&event).Error)

		require.Error(t, db.Model(&models.PrivacyRequest{}).Where("id = ?", request.ID).
			Update("decision_reason", "arbitrary free text").Error)
		require.Error(t, db.Model(&models.PrivacyRequestEvent{}).Where("id = ?", event.ID).
			Update("message", "arbitrary free text").Error)

		nearMiss := request
		nearMiss.ID = uuid.New()
		nearMiss.RequestNumber = "IGDEL" + strings.ToUpper(strings.ReplaceAll(nearMiss.ID.String(), "-", ""))
		nearMiss.RequestDetails = models.JSONB{
			"source": "meta_instagram_managed_login", "target_resolution": "no_current_target",
			"requires_manual_resolution": false, "unreviewed": "escape",
		}
		require.Error(t, db.Create(&nearMiss).Error)

		for name, mutation := range map[string]func() error{
			"privacy request evidence rewrite": func() error {
				return execAsRuntime(
					db, runtimeRole, purpose.ID,
					"UPDATE public.privacy_requests SET received_at = received_at + interval '1 second' WHERE id = ?",
					request.ID,
				)
			},
			"privacy event shape-preserving rewrite": func() error {
				return execAsRuntime(
					db, runtimeRole, purpose.ID,
					"UPDATE public.privacy_request_events SET occurred_at = occurred_at + interval '1 second' WHERE id = ?",
					event.ID,
				)
			},
			"privacy request soft delete": func() error {
				return deleteAsRuntime(
					db, runtimeRole, purpose.ID,
					&models.PrivacyRequest{}, "id = ?", request.ID,
				)
			},
			"privacy event soft delete": func() error {
				return deleteAsRuntime(
					db, runtimeRole, purpose.ID,
					&models.PrivacyRequestEvent{}, "id = ?", event.ID,
				)
			},
		} {
			t.Run(name, func(t *testing.T) {
				err := mutation()
				require.Error(t, err)
				assert.Contains(t, err.Error(), "rejects rows for platform compliance")
			})
		}
		var visibleRequest models.PrivacyRequest
		require.NoError(t, db.First(&visibleRequest, "id = ?", request.ID).Error)
		assert.False(t, visibleRequest.DeletedAt.Valid)
		var visibleEvent models.PrivacyRequestEvent
		require.NoError(t, db.First(&visibleEvent, "id = ?", event.ID).Error)
		assert.False(t, visibleEvent.DeletedAt.Valid)
	})

	t.Run("Instagram journal admits only linked immutable verified-to-completed transition", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Microsecond)
		journal := models.MetaInstagramDataDeletionEvent{
			Digest: hex64(), PlatformAppID: strings.Repeat("1", 64),
			AuthorizingUserID: strings.Repeat("2", 64), IssuedAt: now.Add(-time.Minute),
			VerifiedAt: now, State: "verified", OrganizationID: purpose.ID,
			TargetResolution: "no_current_target", IdentityHashed: true, LastAttemptAt: &now,
		}
		require.NoError(t, db.Create(&journal).Error)

		forgedRequestID := uuid.New()
		forgedNumber := "IGDEL" + strings.ToUpper(strings.ReplaceAll(forgedRequestID.String(), "-", ""))
		forged := execAsRuntime(
			db, runtimeRole, purpose.ID, `
				UPDATE public.meta_instagram_data_deletion_events
				SET state = 'completed', privacy_request_id = ?, request_number = ?,
					completed_at = ?, last_attempt_at = ?
				WHERE digest = ?
			`, forgedRequestID, forgedNumber, now, now, journal.Digest,
		)
		require.Error(t, forged)
		assert.Contains(t, forged.Error(), "rejects rows for platform compliance")

		requestID := uuid.New()
		requestNumber := "IGDEL" + strings.ToUpper(strings.ReplaceAll(requestID.String(), "-", ""))
		request := models.PrivacyRequest{
			BaseModel: models.BaseModel{ID: requestID}, OrganizationID: purpose.ID,
			RequestNumber: requestNumber,
			Type:          models.PrivacyRequestTypeDeletion, Status: models.PrivacyRequestStatusInProgress,
			SubjectType: "instagram_profile", SubjectKey: journal.AuthorizingUserID,
			ReceivedChannel: "instagram", RequesterProfile: models.JSONB{},
			RequestDetails: models.JSONB{
				"source": "meta_instagram_managed_login", "target_resolution": "no_current_target",
				"requires_manual_resolution": false,
			},
			VerificationMethod: "meta_instagram_signed_request", VerificationTokenHash: strings.Repeat("3", 64),
			ReceivedAt: now, DueAt: now.Add(30 * 24 * time.Hour), VerifiedAt: &now,
		}
		require.NoError(t, db.Create(&request).Error)
		completedAt := now.Add(time.Second)
		require.NoError(t, execAsRuntime(
			db, runtimeRole, purpose.ID, `
				UPDATE public.meta_instagram_data_deletion_events
				SET state = 'completed', privacy_request_id = ?, request_number = ?,
					completed_at = ?, last_attempt_at = ?
				WHERE digest = ?
			`, request.ID, request.RequestNumber, completedAt, completedAt, journal.Digest,
		))

		for name, statement := range map[string]string{
			"completed evidence rewrite":  "UPDATE public.meta_instagram_data_deletion_events SET last_attempt_at = last_attempt_at + interval '1 second' WHERE digest = ?",
			"completed evidence reversal": "UPDATE public.meta_instagram_data_deletion_events SET state = 'verified' WHERE digest = ?",
			"completed identity rewrite":  "UPDATE public.meta_instagram_data_deletion_events SET authorizing_user_id = repeat('4', 64) WHERE digest = ?",
		} {
			t.Run(name, func(t *testing.T) {
				err := execAsRuntime(db, runtimeRole, purpose.ID, statement, journal.Digest)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "rejects rows for platform compliance")
			})
		}

		var persisted models.MetaInstagramDataDeletionEvent
		require.NoError(t, db.First(&persisted, "digest = ?", journal.Digest).Error)
		assert.Equal(t, "completed", persisted.State)
		require.NotNil(t, persisted.PrivacyRequestID)
		assert.Equal(t, request.ID, *persisted.PrivacyRequestID)
		assert.Equal(t, request.RequestNumber, persisted.RequestNumber)
		assert.Equal(t, journal.AuthorizingUserID, persisted.AuthorizingUserID)
	})

	t.Run("runtime cannot delete retained compliance evidence", func(t *testing.T) {
		now := time.Now().UTC()
		var audit models.AuditLog
		require.NoError(t, db.Where(
			"organization_id = ? AND resource_type = 'platform_compliance'", purpose.ID,
		).First(&audit).Error)

		deletion := models.MetaInstagramDataDeletionEvent{
			Digest: hex64(), PlatformAppID: strings.Repeat("a", 64),
			AuthorizingUserID: strings.Repeat("b", 64), IssuedAt: now.Add(-time.Minute),
			VerifiedAt: now, State: "verified", OrganizationID: purpose.ID,
			TargetResolution: "no_current_target", IdentityHashed: true, LastAttemptAt: &now,
		}
		require.NoError(t, db.Create(&deletion).Error)

		requestID := uuid.New()
		request := models.PrivacyRequest{
			BaseModel: models.BaseModel{ID: requestID}, OrganizationID: purpose.ID,
			RequestNumber: "IGDEL" + strings.ToUpper(strings.ReplaceAll(requestID.String(), "-", "")),
			Type:          models.PrivacyRequestTypeDeletion, Status: models.PrivacyRequestStatusInProgress,
			SubjectType: "instagram_profile", SubjectKey: strings.Repeat("c", 64),
			ReceivedChannel: "instagram", RequesterProfile: models.JSONB{},
			RequestDetails: models.JSONB{
				"source": "meta_instagram_managed_login", "target_resolution": "no_current_target",
				"requires_manual_resolution": false,
			},
			VerificationMethod: "meta_instagram_signed_request", VerificationTokenHash: strings.Repeat("d", 64),
			ReceivedAt: now, DueAt: now.Add(30 * 24 * time.Hour), VerifiedAt: &now,
		}
		require.NoError(t, db.Create(&request).Error)
		event := models.PrivacyRequestEvent{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: purpose.ID,
			PrivacyRequestID: request.ID, EventType: "request_created",
			ToStatus: models.PrivacyRequestStatusInProgress,
			Message:  "Managed Instagram data deletion request verified and initiated",
			Details:  models.JSONB{"verification": "meta_instagram_signed_request"}, OccurredAt: now,
		}
		require.NoError(t, db.Create(&event).Error)

		for name, statement := range map[string]string{
			"audit":         "DELETE FROM public.audit_logs WHERE id = ?",
			"deletion":      "DELETE FROM public.meta_instagram_data_deletion_events WHERE digest = ?",
			"privacy":       "DELETE FROM public.privacy_requests WHERE id = ?",
			"privacy event": "DELETE FROM public.privacy_request_events WHERE id = ?",
		} {
			argument := any(request.ID)
			switch name {
			case "audit":
				argument = audit.ID
			case "deletion":
				argument = deletion.Digest
			case "privacy event":
				argument = event.ID
			}
			err := execAsRuntime(db, runtimeRole, purpose.ID, statement, argument)
			require.Error(t, err, name)
			if name == "audit" {
				assert.Contains(t, err.Error(), "transaction-bound and append-only", name)
			} else {
				assert.Contains(t, err.Error(), "rejects deleting retained platform compliance evidence", name)
			}
		}
	})

	t.Run("purpose organization row and control markers are runtime immutable", func(t *testing.T) {
		updates := []struct {
			name      string
			statement string
			args      []any
		}{
			{name: "identity", statement: "UPDATE public.organizations SET name = 'rewritten' WHERE id = ?", args: []any{purpose.ID}},
			{name: "slug", statement: "UPDATE public.organizations SET slug = ? WHERE id = ?", args: []any{"rewritten-" + uuid.NewString(), purpose.ID}},
			{name: "reparent", statement: "UPDATE public.organizations SET reseller_id = ? WHERE id = ?", args: []any{uuid.New(), purpose.ID}},
			{name: "archive", statement: "UPDATE public.organizations SET deleted_at = now() WHERE id = ?", args: []any{purpose.ID}},
			{name: "arbitrary settings", statement: "UPDATE public.organizations SET settings = settings || '{\"clinic_secret\":\"forbidden\"}'::jsonb WHERE id = ?", args: []any{purpose.ID}},
			{name: "feature add", statement: "UPDATE public.organizations SET settings = settings || '{\"threads_managed_compliance_tenant\":true}'::jsonb WHERE id = ?", args: []any{purpose.ID}},
			{name: "feature remove", statement: "UPDATE public.organizations SET settings = settings - 'meta_instagram_data_deletion_compliance_tenant' WHERE id = ?", args: []any{purpose.ID}},
			{name: "hard delete", statement: "DELETE FROM public.organizations WHERE id = ?", args: []any{purpose.ID}},
		}
		for _, update := range updates {
			err := execAsRuntime(db, runtimeRole, purpose.ID, update.statement, update.args...)
			require.Error(t, err, update.name)
		}

		for _, markerValue := range []string{"true", "false", `"malformed"`} {
			err := execAsRuntime(
				db, runtimeRole, ordinary.ID,
				"UPDATE public.organizations SET settings = settings || jsonb_build_object('threads_managed_compliance_tenant', CAST(? AS jsonb)) WHERE id = ?",
				markerValue, ordinary.ID,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "existing ordinary organizations cannot acquire or mutate platform compliance markers")
		}
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(
				"ALTER TABLE public.organizations DISABLE TRIGGER rereply_platform_compliance_classification_guard",
			).Error; err != nil {
				return err
			}
			if err := tx.Exec(`
				UPDATE public.organizations
				SET settings = settings || '{"threads_managed_compliance_tenant":true}'::jsonb
				WHERE id = ?
			`, ordinary.ID).Error; err != nil {
				return err
			}
			return tx.Exec(
				"ALTER TABLE public.organizations ENABLE TRIGGER rereply_platform_compliance_classification_guard",
			).Error
		}))
		removePreseed := execAsRuntime(
			db, runtimeRole, ordinary.ID,
			"UPDATE public.organizations SET settings = settings - 'threads_managed_compliance_tenant' WHERE id = ?",
			ordinary.ID,
		)
		require.Error(t, removePreseed)
		assert.Contains(t, removePreseed.Error(), "existing ordinary organizations cannot acquire or mutate platform compliance markers")

		var persisted models.Organization
		require.NoError(t, db.Unscoped().First(&persisted, "id = ?", purpose.ID).Error)
		assert.Equal(t, purpose.Name, persisted.Name)
		require.NotNil(t, persisted.ResellerID)
		assert.False(t, persisted.DeletedAt.Valid)
		assert.Equal(t, true, persisted.Settings[database.PlatformCompliancePurposeMarkerKey])
		assert.Equal(t, true, persisted.Settings[database.PlatformComplianceInstagramMarkerKey])
	})

	t.Run("hostile search path cannot shadow the public organization", func(t *testing.T) {
		schema := "compliance_hostile_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		require.NoError(t, db.Exec("CREATE SCHEMA \""+schema+"\"").Error)
		t.Cleanup(func() { _ = db.Exec("DROP SCHEMA IF EXISTS \"" + schema + "\" CASCADE").Error })
		require.NoError(t, db.Exec("CREATE TABLE \""+schema+"\".organizations (LIKE public.organizations INCLUDING ALL)").Error)
		require.NoError(t, db.Exec(
			"INSERT INTO \""+schema+"\".organizations SELECT * FROM public.organizations WHERE id = ?",
			purpose.ID,
		).Error)
		require.NoError(t, db.Exec(
			"UPDATE \""+schema+"\".organizations SET settings = '{}'::jsonb WHERE id = ?",
			purpose.ID,
		).Error)

		err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL search_path = \"" + schema + "\", public").Error; err != nil {
				return err
			}
			return tx.Exec(`
				INSERT INTO public.users (id, organization_id, email, full_name, created_at, updated_at)
				VALUES (?, ?, ?, 'Blocked', now(), now())
			`, uuid.New(), purpose.ID, "hostile-"+uuid.NewString()+"@example.test").Error
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rejects rows for platform compliance")
	})

	t.Run("purpose state cannot be cleared or malformed", func(t *testing.T) {
		immutable := createGuardTestOrganization(t, db, true)
		remove := db.Exec(`
			UPDATE public.organizations
			SET settings = COALESCE(settings, '{}'::jsonb) - 'platform_compliance_tenant'
			WHERE id = ?
		`, immutable.ID)
		require.Error(t, remove.Error)
		assert.Contains(t, remove.Error.Error(), "purpose classification is immutable")
		malform := db.Exec(`
			UPDATE public.organizations
			SET settings = COALESCE(settings, '{}'::jsonb) || '{"platform_compliance_tenant":"true"}'::jsonb
			WHERE id = ?
		`, immutable.ID)
		require.Error(t, malform.Error)
		assert.Contains(t, malform.Error.Error(), "purpose classification is immutable")
	})

	t.Run("purpose true is rejected on organization insert", func(t *testing.T) {
		organization := models.Organization{
			BaseModel: models.BaseModel{ID: uuid.New()}, Name: "Unsafe Insert",
			Slug:     "unsafe-purpose-insert-" + uuid.NewString(),
			Settings: models.JSONB{database.PlatformCompliancePurposeMarkerKey: true},
		}
		err := db.Create(&organization).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "atomic platform compliance creation payload")
	})

	t.Run("runtime cannot execute the atomic creator", func(t *testing.T) {
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL ROLE " + runtimeRole).Error; err != nil {
				return err
			}
			return database.CreatePlatformComplianceOrganization(
				tx, uuid.New(), "runtime-forged-atomic-create", true, false,
			)
		})
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "permission denied")
	})

	t.Run("canonical reader rejects malformed or preseeded reserved markers", func(t *testing.T) {
		markerFreeOrdinary := createGuardTestOrganization(t, db, false)
		compliance, err := database.IsPlatformComplianceOrganization(db, markerFreeOrdinary.ID)
		require.NoError(t, err)
		assert.False(t, compliance)
		compliance, err = database.IsPlatformComplianceOrganization(db, purpose.ID)
		require.NoError(t, err)
		assert.True(t, compliance)

		historical := createGuardTestOrganization(t, db, false)
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(`
				ALTER TABLE public.organizations
				DISABLE TRIGGER rereply_platform_compliance_classification_guard
			`).Error; err != nil {
				return err
			}
			if err := tx.Exec(`
				UPDATE public.organizations
				SET settings = '{"threads_managed_compliance_tenant":true}'::jsonb
				WHERE id = ?
			`, historical.ID).Error; err != nil {
				return err
			}
			return tx.Exec(`
				ALTER TABLE public.organizations
				ENABLE TRIGGER rereply_platform_compliance_classification_guard
			`).Error
		}))
		_, err = database.IsPlatformComplianceOrganization(db, historical.ID)
		require.ErrorIs(t, err, database.ErrPlatformComplianceMarkerInvalid)
	})
}

func TestPlatformComplianceAtomicBirthAndMonotonicIdentity(t *testing.T) {
	db, _ := setupPlatformComplianceGuardTest(t)

	t.Run("child rows fail both before and after atomic birth", func(t *testing.T) {
		organizationID := uuid.New()
		before := db.Exec(`
			INSERT INTO public.users (id, organization_id, email, full_name, created_at, updated_at)
			VALUES (?, ?, ?, 'Before', now(), now())
		`, uuid.New(), organizationID, "before-"+uuid.NewString()+"@example.test")
		require.Error(t, before.Error)
		require.NoError(t, createAtomicGuardTestOrganization(t, db, organizationID))
		after := db.Exec(`
			INSERT INTO public.users (id, organization_id, email, full_name, created_at, updated_at)
			VALUES (?, ?, ?, 'After', now(), now())
		`, uuid.New(), organizationID, "after-"+uuid.NewString()+"@example.test")
		require.Error(t, after.Error)
		assert.Contains(t, after.Error.Error(), "rejects rows for platform compliance")
	})

	t.Run("every birth claims UUID and hard delete cannot make it reusable", func(t *testing.T) {
		organization := createGuardTestOrganization(t, db, false)
		var claims int64
		require.NoError(t, db.Table("public.organization_identity_registry").
			Where("organization_id = ?", organization.ID).Count(&claims).Error)
		assert.Equal(t, int64(1), claims)
		require.NoError(t, db.Unscoped().Delete(&organization).Error)
		require.NoError(t, db.Table("public.organization_identity_registry").
			Where("organization_id = ?", organization.ID).Count(&claims).Error)
		assert.Equal(t, int64(1), claims)
		err := createAtomicGuardTestOrganization(t, db, organization.ID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already exists or was previously used")
	})

	t.Run("identity registry rejects direct mutation and truncate", func(t *testing.T) {
		organization := createGuardTestOrganization(t, db, false)
		directInsert := db.Exec(`
			INSERT INTO public.organization_identity_registry
				(organization_id, claimed_at, database_current_user, database_session_user)
			VALUES (?, now(), current_user::text, session_user::text)
		`, uuid.New())
		require.Error(t, directInsert.Error)
		assert.Contains(t, directInsert.Error.Error(), "only be claimed by the organization insert guard")
		for name, statement := range map[string]string{
			"update":   "UPDATE public.organization_identity_registry SET claimed_at = now() WHERE organization_id = ?",
			"delete":   "DELETE FROM public.organization_identity_registry WHERE organization_id = ?",
			"truncate": "TRUNCATE TABLE public.organization_identity_registry",
		} {
			var result *gorm.DB
			if name == "truncate" {
				result = db.Exec(statement)
			} else {
				result = db.Exec(statement, organization.ID)
			}
			require.Error(t, result.Error, name)
			assert.Contains(t, result.Error.Error(), "organization identity registry is", name)
		}
		var count int64
		require.NoError(t, db.Table("public.organization_identity_registry").
			Where("organization_id = ?", organization.ID).Count(&count).Error)
		assert.Equal(t, int64(1), count)
	})

	t.Run("forged owner intent cannot skip canonical or footprint checks", func(t *testing.T) {
		organizationID := uuid.New()
		name := "Platform Compliance " + organizationID.String()
		slug := "platform-compliance-" + organizationID.String()
		intent := map[string]any{
			"token": uuid.NewString(), "phase": "insert",
			"organization_id": organizationID.String(), "operator_run_id": "forged-owner-intent-001",
			"instagram": true, "threads": false, "name": name, "slug": slug,
			"reseller_id": uuid.NewString(),
		}
		intentJSON, err := json.Marshal(intent)
		require.NoError(t, err)
		err = db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(
				"SELECT pg_catalog.set_config('app.platform_compliance_creation_intent', ?, true)",
				string(intentJSON),
			).Error; err != nil {
				return err
			}
			return tx.Exec(`
				INSERT INTO public.organizations
					(id, created_at, updated_at, reseller_id, name, slug, settings)
				VALUES (?, now(), now(), ?, ?, ?,
					'{"platform_compliance_tenant":true,"meta_instagram_data_deletion_compliance_tenant":true}'::jsonb)
			`, organizationID, intent["reseller_id"], name, slug).Error
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "active platform-direct reseller")

		var exists bool
		require.NoError(t, db.Raw(
			"SELECT EXISTS (SELECT 1 FROM public.organizations WHERE id = ?)", organizationID,
		).Scan(&exists).Error)
		assert.False(t, exists)
	})

	t.Run("organization primary key and truncate are rejected", func(t *testing.T) {
		organization := createGuardTestOrganization(t, db, false)
		updateID := db.Exec("UPDATE public.organizations SET id = ? WHERE id = ?", uuid.New(), organization.ID)
		require.Error(t, updateID.Error)
		assert.Contains(t, updateID.Error.Error(), "UUID identity is immutable")
		truncate := db.Exec("TRUNCATE TABLE public.organizations CASCADE")
		require.Error(t, truncate.Error)
		assert.Contains(t, truncate.Error.Error(), "identity registry cannot be truncated")
	})

	t.Run("ordinary task serializes hard delete before registry blocks rebirth", func(t *testing.T) {
		organization := createGuardTestOrganization(t, db, false)
		holder := db.Begin()
		require.NoError(t, holder.Error)
		t.Cleanup(func() { _ = holder.Rollback().Error })
		require.NoError(t, holder.Exec(
			"SELECT id FROM public.organizations WHERE id = ? FOR KEY SHARE", organization.ID,
		).Error)

		pidReady := make(chan int, 1)
		deleteResult := make(chan error, 1)
		go func() {
			tx := db.Begin()
			if tx.Error != nil {
				deleteResult <- tx.Error
				return
			}
			defer tx.Rollback()
			var pid int
			if err := tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				deleteResult <- err
				return
			}
			pidReady <- pid
			if err := tx.Exec("DELETE FROM public.organizations WHERE id = ?", organization.ID).Error; err != nil {
				deleteResult <- err
				return
			}
			deleteResult <- tx.Commit().Error
		}()
		pid := <-pidReady
		testutil.RequirePostgresBackendWaitingForLock(t, db, pid)
		require.NoError(t, holder.Commit().Error)
		require.NoError(t, <-deleteResult)
		err := createAtomicGuardTestOrganization(t, db, organization.ID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already exists or was previously used")
	})

	t.Run("uncommitted ordinary insert delete claim blocks concurrent purpose birth", func(t *testing.T) {
		ensurePlatformDirectReseller(t, db)
		organizationID := uuid.New()
		ordinary := db.Begin()
		require.NoError(t, ordinary.Error)
		t.Cleanup(func() { _ = ordinary.Rollback().Error })
		organization := models.Organization{
			BaseModel: models.BaseModel{ID: organizationID}, Name: "Transient Ordinary",
			Slug: "transient-ordinary-" + uuid.NewString(), Settings: models.JSONB{},
		}
		require.NoError(t, ordinary.Create(&organization).Error)
		require.NoError(t, ordinary.Unscoped().Delete(&organization).Error)

		pidReady := make(chan int, 1)
		creatorResult := make(chan error, 1)
		go func() {
			creator := db.Begin()
			if creator.Error != nil {
				creatorResult <- creator.Error
				return
			}
			defer creator.Rollback()
			var pid int
			if err := creator.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				creatorResult <- err
				return
			}
			pidReady <- pid
			err := database.CreatePlatformComplianceOrganization(
				creator, organizationID, "atomic-transient-ordinary-race", true, false,
			)
			if err == nil {
				err = creator.Commit().Error
			}
			creatorResult <- err
		}()
		pid := <-pidReady
		testutil.RequirePostgresBackendWaitingForLock(t, db, pid)
		require.NoError(t, ordinary.Commit().Error)
		err := <-creatorResult
		require.Error(t, err)
		assert.Contains(t, err.Error(), "organization_identity_registry_pkey")
		var organizationCount, claimCount int64
		require.NoError(t, db.Unscoped().Model(&models.Organization{}).
			Where("id = ?", organizationID).Count(&organizationCount).Error)
		require.NoError(t, db.Table("public.organization_identity_registry").
			Where("organization_id = ?", organizationID).Count(&claimCount).Error)
		assert.Zero(t, organizationCount)
		assert.Equal(t, int64(1), claimCount)
	})

	t.Run("purpose birth claim blocks concurrent ordinary insert", func(t *testing.T) {
		organizationID := uuid.New()
		creator := db.Begin()
		require.NoError(t, creator.Error)
		t.Cleanup(func() { _ = creator.Rollback().Error })
		require.NoError(t, createAtomicGuardTestOrganization(t, creator, organizationID))

		pidReady := make(chan int, 1)
		ordinaryResult := make(chan error, 1)
		go func() {
			ordinary := db.Begin()
			if ordinary.Error != nil {
				ordinaryResult <- ordinary.Error
				return
			}
			defer ordinary.Rollback()
			var pid int
			if err := ordinary.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				ordinaryResult <- err
				return
			}
			pidReady <- pid
			err := ordinary.Create(&models.Organization{
				BaseModel: models.BaseModel{ID: organizationID}, Name: "Concurrent Ordinary",
				Slug: "concurrent-ordinary-" + uuid.NewString(), Settings: models.JSONB{},
			}).Error
			ordinaryResult <- err
		}()
		pid := <-pidReady
		testutil.RequirePostgresBackendWaitingForLock(t, db, pid)
		require.NoError(t, creator.Commit().Error)
		err := <-ordinaryResult
		require.Error(t, err)
		assert.Contains(t, err.Error(), "organization_identity_registry_pkey")
	})

	t.Run("concurrent strict creation has one winner", func(t *testing.T) {
		organizationID := uuid.New()
		results := make(chan error, 2)
		for index := 0; index < 2; index++ {
			index := index
			go func() {
				results <- database.CreatePlatformComplianceOrganization(
					db, organizationID,
					fmt.Sprintf("atomic-concurrent-create-%d", index), true, false,
				)
			}()
		}
		first, second := <-results, <-results
		assert.NotEqual(t, first == nil, second == nil, "exactly one strict creator must commit")
	})

	t.Run("creation holds the active platform reseller stable through commit", func(t *testing.T) {
		creator := db.Begin()
		require.NoError(t, creator.Error)
		t.Cleanup(func() { _ = creator.Rollback().Error })
		require.NoError(t, createAtomicGuardTestOrganization(t, creator, uuid.New()))

		pidReady := make(chan int, 1)
		updateResult := make(chan error, 1)
		go func() {
			updater := db.Begin()
			if updater.Error != nil {
				updateResult <- updater.Error
				return
			}
			defer updater.Rollback()
			var pid int
			if err := updater.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				updateResult <- err
				return
			}
			pidReady <- pid
			if err := updater.Exec(`
				UPDATE public.resellers SET status = 'suspended'
				WHERE slug = 'platform-direct'
			`).Error; err != nil {
				updateResult <- err
				return
			}
			updateResult <- updater.Rollback().Error
		}()
		pid := <-pidReady
		testutil.RequirePostgresBackendWaitingForLock(t, db, pid)
		require.NoError(t, creator.Commit().Error)
		require.NoError(t, <-updateResult)
	})
}

func TestPlatformComplianceAtomicCreationIdempotenceRejectsDrift(t *testing.T) {
	db, _ := setupPlatformComplianceGuardTest(t)
	organizationID := uuid.New()
	runID := "atomic-idempotence-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ensurePlatformDirectReseller(t, db)
	require.NoError(t, database.CreatePlatformComplianceOrganization(
		db, organizationID, runID, true, false,
	))
	require.NoError(t, database.VerifyPlatformComplianceAtomicCreation(
		db, organizationID, runID, true, false, false,
	))

	t.Run("canonical identity drift", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec(`
			ALTER TABLE public.organizations
			DISABLE TRIGGER rereply_platform_compliance_classification_guard
		`).Error)
		require.NoError(t, tx.Exec(
			"UPDATE public.organizations SET name = 'drifted' WHERE id = ?", organizationID,
		).Error)
		require.NoError(t, tx.Exec(`
			ALTER TABLE public.organizations
			ENABLE TRIGGER rereply_platform_compliance_classification_guard
		`).Error)
		err := database.VerifyPlatformComplianceAtomicCreation(
			tx, organizationID, runID, true, false, false,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a canonical active platform-direct")
	})

	t.Run("prohibited footprint drift", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec(`
			ALTER TABLE public.users
			DISABLE TRIGGER rereply_platform_compliance_write_guard
		`).Error)
		require.NoError(t, tx.Exec(`
			INSERT INTO public.users (id, organization_id, email, full_name, created_at, updated_at)
			VALUES (?, ?, ?, 'Historical', now(), now())
		`, uuid.New(), organizationID, "historical-"+uuid.NewString()+"@example.test").Error)
		require.NoError(t, tx.Exec(`
			ALTER TABLE public.users
			ENABLE TRIGGER rereply_platform_compliance_write_guard
		`).Error)
		err := database.VerifyPlatformComplianceAtomicCreation(
			tx, organizationID, runID, true, false, false,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "has rows in users")
	})

	t.Run("creation audit drift", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec(`
			ALTER TABLE public.audit_logs
			DISABLE TRIGGER rereply_platform_compliance_write_guard
		`).Error)
		require.NoError(t, tx.Exec(`
			UPDATE public.audit_logs
			SET action = 'deleted'
			WHERE organization_id = ? AND user_name = ?
		`, organizationID, "platform-compliance-bootstrap/"+runID).Error)
		require.NoError(t, tx.Exec(`
			ALTER TABLE public.audit_logs
			ENABLE TRIGGER rereply_platform_compliance_write_guard
		`).Error)
		err := database.VerifyPlatformComplianceAtomicCreation(
			tx, organizationID, runID, true, false, false,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "has rows in audit_logs")
	})
}

func TestPlatformComplianceGlobalDatabaseSeedsSkipPurposeOrganization(t *testing.T) {
	db, _ := setupPlatformComplianceGuardTest(t)
	purpose := createGuardTestOrganization(t, db, true)
	require.NoError(t, database.CreateDefaultAdmin(db, &config.DefaultAdminConfig{
		Email:    "purpose-seed-admin-" + uuid.NewString() + "@example.test",
		Password: "synthetic-test-password-123", FullName: "Synthetic Admin",
	}))
	require.NoError(t, database.SeedSystemRolesForAllOrgs(db))
	require.NoError(t, database.SeedDefaultWidgets(db))
	require.NoError(t, database.ApplyManagerSettingsPolicyMigration(db))

	var admin models.User
	require.NoError(t, db.Where("is_super_admin = true").First(&admin).Error)
	assert.NotEqual(t, purpose.ID, admin.OrganizationID)
	var purposeRoles, purposeWidgets, purposeUsers int64
	require.NoError(t, db.Model(&models.CustomRole{}).
		Where("organization_id = ?", purpose.ID).Count(&purposeRoles).Error)
	require.NoError(t, db.Model(&models.Widget{}).
		Where("organization_id = ?", purpose.ID).Count(&purposeWidgets).Error)
	require.NoError(t, db.Model(&models.User{}).
		Where("organization_id = ?", purpose.ID).Count(&purposeUsers).Error)
	assert.Zero(t, purposeRoles)
	assert.Zero(t, purposeWidgets)
	assert.Zero(t, purposeUsers)
	var settings models.JSONB
	require.NoError(t, db.Raw(
		"SELECT settings FROM public.organizations WHERE id = ?", purpose.ID,
	).Scan(&settings).Error)
	assert.NotContains(t, settings, database.ManagerSettingsPolicyVersionKey)
	footprint, err := database.FindPlatformComplianceDisallowedFootprint(db, purpose.ID)
	require.NoError(t, err)
	assert.Empty(t, footprint)
}

func TestPlatformComplianceScanAuthorityRejectsPolicyGaps(t *testing.T) {
	db, runtimeRole := setupPlatformComplianceGuardTest(t)
	require.NoError(t, database.VerifyPlatformComplianceScanAuthority(db))
	table := "agent_transfers"

	for _, hierarchy := range []struct {
		name   string
		create []string
	}{
		{
			name: "partition parent",
			create: []string{
				"CREATE TABLE public.agent_transfers (organization_id uuid NOT NULL, shard integer NOT NULL) PARTITION BY HASH (shard)",
			},
		},
		{
			name: "inheritance parent",
			create: []string{
				"CREATE TABLE public.agent_transfers (organization_id uuid NOT NULL)",
				"CREATE TABLE public.compliance_inheritance_child () INHERITS (public.agent_transfers)",
			},
		},
		{
			name: "registered inheritance child",
			create: []string{
				"CREATE TABLE public.compliance_inheritance_parent (organization_id uuid NOT NULL)",
				"CREATE TABLE public.agent_transfers () INHERITS (public.compliance_inheritance_parent)",
			},
		},
	} {
		t.Run(hierarchy.name, func(t *testing.T) {
			tx := db.Begin()
			require.NoError(t, tx.Error)
			defer tx.Rollback()
			require.NoError(t, tx.Exec("ALTER TABLE public.agent_transfers RENAME TO compliance_original_agent_transfers").Error)
			for _, statement := range hierarchy.create {
				require.NoError(t, tx.Exec(statement).Error)
			}
			err := database.VerifyPlatformComplianceScanAuthority(tx)
			require.ErrorIs(t, err, database.ErrPlatformComplianceScanAuthority)
			assert.Contains(t, err.Error(), "partition/inheritance hierarchies are unsupported")
		})
	}

	t.Run("missing and mistargeted policy", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec("DROP POLICY rereply_migration_access ON public."+table).Error)
		require.ErrorIs(t, database.VerifyPlatformComplianceScanAuthority(tx), database.ErrPlatformComplianceScanAuthority)
		require.NoError(t, tx.Exec(fmt.Sprintf(
			"CREATE POLICY compliance_mistargeted ON public.%s TO %s USING (true) WITH CHECK (true)",
			table, runtimeRole,
		)).Error)
		require.ErrorIs(t, database.VerifyPlatformComplianceScanAuthority(tx), database.ErrPlatformComplianceScanAuthority)

		createErr := database.CreatePlatformComplianceOrganization(
			tx, uuid.New(), "guard-policy-gap-create", true, false,
		)
		require.Error(t, createErr)
		assert.Contains(t, createErr.Error(), "scan policy is incomplete")
	})

	t.Run("applicable permissive false policy", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec("DROP POLICY rereply_migration_access ON public."+table).Error)
		require.NoError(t, tx.Exec("CREATE POLICY compliance_false ON public."+table+" TO PUBLIC USING (false) WITH CHECK (false)").Error)
		require.ErrorIs(t, database.VerifyPlatformComplianceScanAuthority(tx), database.ErrPlatformComplianceScanAuthority)
	})

	t.Run("public permissive true policy is never migration authority", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec("DROP POLICY rereply_migration_access ON public."+table).Error)
		require.NoError(t, tx.Exec(
			"CREATE POLICY compliance_public_true ON public."+table+
				" TO PUBLIC USING (true) WITH CHECK (true)",
		).Error)
		require.ErrorIs(t, database.VerifyPlatformComplianceScanAuthority(tx), database.ErrPlatformComplianceScanAuthority)
	})

	t.Run("migration policy role set must contain only the exact owner role", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		var migrationRole string
		require.NoError(t, tx.Raw("SELECT current_user::text").Scan(&migrationRole).Error)
		require.NoError(t, tx.Exec("DROP POLICY rereply_migration_access ON public."+table).Error)
		require.NoError(t, tx.Exec(fmt.Sprintf(
			"CREATE POLICY rereply_migration_access ON public.%s TO %s, %s USING (true) WITH CHECK (true)",
			table, migrationRole, runtimeRole,
		)).Error)
		require.ErrorIs(t, database.VerifyPlatformComplianceScanAuthority(tx), database.ErrPlatformComplianceScanAuthority)
	})

	t.Run("applicable restrictive false policy", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec("CREATE POLICY compliance_restrictive_false ON public."+table+" AS RESTRICTIVE TO PUBLIC USING (false)").Error)
		require.ErrorIs(t, database.VerifyPlatformComplianceScanAuthority(tx), database.ErrPlatformComplianceScanAuthority)
	})

	t.Run("applicable restrictive false policy through indirect membership", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		var migrationRole string
		require.NoError(t, tx.Raw("SELECT current_user::text").Scan(&migrationRole).Error)
		outerRole := "rereply_scan_outer_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		innerRole := "rereply_scan_inner_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		require.NoError(t, tx.Exec("CREATE ROLE "+outerRole+" NOLOGIN NOSUPERUSER NOBYPASSRLS").Error)
		require.NoError(t, tx.Exec("CREATE ROLE "+innerRole+" NOLOGIN NOSUPERUSER NOBYPASSRLS").Error)
		require.NoError(t, tx.Exec("GRANT "+outerRole+" TO "+innerRole).Error)
		require.NoError(t, tx.Exec("GRANT "+innerRole+" TO "+migrationRole).Error)
		require.NoError(t, tx.Exec(
			"CREATE POLICY compliance_inherited_restrictive_false ON public."+table+
				" AS RESTRICTIVE TO "+outerRole+" USING (false)",
		).Error)
		require.ErrorIs(t, database.VerifyPlatformComplianceScanAuthority(tx), database.ErrPlatformComplianceScanAuthority)

		createErr := database.CreatePlatformComplianceOrganization(
			tx, uuid.New(), "guard-inherited-policy-create", true, false,
		)
		require.Error(t, createErr)
		assert.Contains(t, createErr.Error(), "scan policy is incomplete")
	})

	t.Run("pre-tenant exemption rejects any RLS visibility drift", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec("ALTER TABLE public.users ENABLE ROW LEVEL SECURITY").Error)
		require.NoError(t, tx.Exec(
			"CREATE POLICY compliance_hidden_users ON public.users AS RESTRICTIVE TO PUBLIC USING (false)",
		).Error)
		err := database.VerifyPlatformComplianceScanAuthority(tx)
		require.ErrorIs(t, err, database.ErrPlatformComplianceScanAuthority)
		assert.Contains(t, err.Error(), "pre-tenant exemptions must remain outside RLS")

		createErr := database.CreatePlatformComplianceOrganization(
			tx, uuid.New(), "guard-exemption-rls-create", true, false,
		)
		require.Error(t, createErr)
		assert.Contains(t, createErr.Error(), "exemption unexpectedly uses RLS")
	})

	t.Run("current role without effective select privilege", func(t *testing.T) {
		role := "rereply_scanless_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		require.NoError(t, db.Exec("CREATE ROLE "+role+" NOLOGIN NOSUPERUSER NOBYPASSRLS").Error)
		defer db.Exec("DROP ROLE IF EXISTS " + role)
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec("SET LOCAL ROLE "+role).Error)
		require.ErrorIs(t, database.VerifyPlatformComplianceScanAuthority(tx), database.ErrPlatformComplianceScanAuthority)
	})
}

func TestRemoveTenantRLSPreservesPlatformComplianceBarrier(t *testing.T) {
	db, runtimeRole := setupPlatformComplianceGuardTest(t)
	require.NoError(t, database.RemoveTenantRLS(db))
	var triggerCount int64
	require.NoError(t, db.Raw(`
		SELECT COUNT(*) FROM pg_catalog.pg_trigger
		WHERE tgname = ? AND NOT tgisinternal
	`, complianceWriteTrigger).Scan(&triggerCount).Error)
	assert.NotZero(t, triggerCount)

	createErr := database.CreatePlatformComplianceOrganization(
		db, uuid.New(), "guard-after-rls-removal", true, false,
	)
	require.Error(t, createErr)
	assert.Contains(t, createErr.Error(), "scan policy is incomplete")

	require.NoError(t, database.ApplyTenantRLS(db, runtimeRole))
	purpose := createGuardTestOrganization(t, db, true)
	require.ErrorIs(t, database.RemoveTenantRLS(db), database.ErrPlatformComplianceRollbackBlocked)
	hardDelete := db.Unscoped().Delete(&models.Organization{}, "id = ?", purpose.ID)
	require.Error(t, hardDelete.Error)
	assert.Contains(t, hardDelete.Error.Error(), "lifecycle is immutable")
	require.ErrorIs(t, database.RemoveTenantRLS(db), database.ErrPlatformComplianceRollbackBlocked)
}

func TestPlatformComplianceRemoveTenantRLSSerializesAtomicCreation(t *testing.T) {
	t.Run("atomic creation commits before rollback eligibility check", func(t *testing.T) {
		db, _ := setupPlatformComplianceGuardTest(t)
		creator := db.Begin()
		require.NoError(t, creator.Error)
		t.Cleanup(func() { _ = creator.Rollback().Error })
		require.NoError(t, createAtomicGuardTestOrganization(t, creator, uuid.New()))

		pidReady := make(chan int, 1)
		result := make(chan error, 1)
		go func() {
			result <- db.Connection(func(connection *gorm.DB) error {
				var pid int
				if err := connection.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
					return err
				}
				pidReady <- pid
				return database.RemoveTenantRLS(connection)
			})
		}()
		pid := <-pidReady
		testutil.RequirePostgresBackendWaitingForLock(t, db, pid)
		require.NoError(t, creator.Commit().Error)
		err := <-result
		require.ErrorIs(t, err, database.ErrPlatformComplianceRollbackBlocked)
	})

	t.Run("rollback commits before waiting atomic creation", func(t *testing.T) {
		db, _ := setupPlatformComplianceGuardTest(t)
		blocker := db.Begin()
		require.NoError(t, blocker.Error)
		t.Cleanup(func() { _ = blocker.Rollback().Error })
		require.NoError(t, blocker.Exec("LOCK TABLE public.agent_transfers IN ACCESS SHARE MODE").Error)

		removePIDReady := make(chan int, 1)
		removeResult := make(chan error, 1)
		go func() {
			removeResult <- db.Connection(func(connection *gorm.DB) error {
				var pid int
				if err := connection.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
					return err
				}
				removePIDReady <- pid
				return database.RemoveTenantRLS(connection)
			})
		}()
		removePID := <-removePIDReady
		testutil.RequirePostgresBackendWaitingForLock(t, db, removePID)

		pidReady := make(chan int, 1)
		result := make(chan error, 1)
		go func() {
			tx := db.Begin()
			if tx.Error != nil {
				result <- tx.Error
				return
			}
			defer tx.Rollback()
			var pid int
			if err := tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				result <- err
				return
			}
			pidReady <- pid
			result <- createAtomicGuardTestOrganization(t, tx, uuid.New())
		}()
		pid := <-pidReady
		testutil.RequirePostgresBackendWaitingForLock(t, db, pid)
		require.NoError(t, blocker.Commit().Error)
		require.NoError(t, <-removeResult)
		err := <-result
		require.Error(t, err)
		assert.Contains(t, err.Error(), "scan policy is incomplete")
	})
}

func TestRemoveTenantRLSRejectsNestedAndRepeatableReadSnapshots(t *testing.T) {
	db, _ := setupPlatformComplianceGuardTest(t)
	createGuardTestOrganization(t, db, true)

	repeatable := db.Begin(&sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	require.NoError(t, repeatable.Error)
	t.Cleanup(func() { _ = repeatable.Rollback().Error })
	var snapshotCount int64
	require.NoError(t, repeatable.Raw(
		"SELECT count(*) FROM public.organizations",
	).Scan(&snapshotCount).Error)

	require.ErrorIs(t, database.RemoveTenantRLS(repeatable), database.ErrTenantRLSRemovalTransaction)
	require.NoError(t, repeatable.Rollback().Error)

	readCommitted := db.Begin(&sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, readCommitted.Error)
	require.ErrorIs(t, database.RemoveTenantRLS(readCommitted), database.ErrTenantRLSRemovalTransaction)
	require.NoError(t, readCommitted.Rollback().Error)

	// Neither rejected nested call may tear down the additive barrier or tenant
	// policies, and the fresh root rollback still observes the committed marker.
	require.ErrorIs(t, database.RemoveTenantRLS(db), database.ErrPlatformComplianceRollbackBlocked)
	var enabled bool
	require.NoError(t, db.Raw(`
		SELECT count(*) = 1 AND pg_catalog.bool_and(trigger.tgenabled = 'O')
		FROM pg_catalog.pg_trigger AS trigger
		JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		WHERE namespace.nspname = 'public' AND relation.relname = 'organizations'
		  AND trigger.tgname = 'rereply_platform_compliance_classification_guard'
		  AND NOT trigger.tgisinternal
	`).Scan(&enabled).Error)
	assert.True(t, enabled)
}

func TestPlatformComplianceStartupVerificationRejectsContractDrift(t *testing.T) {
	db, runtimeRole := setupPlatformComplianceGuardTest(t)
	verifyAsRuntime := func(tx *gorm.DB) error {
		return database.VerifyPlatformComplianceGuardContract(tx, runtimeRole)
	}

	t.Run("trigger timing and event", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec("DROP TRIGGER rereply_platform_compliance_write_guard ON public.users").Error)
		require.NoError(t, tx.Exec(`
			CREATE TRIGGER rereply_platform_compliance_write_guard
			AFTER INSERT ON public.users
			FOR EACH ROW EXECUTE FUNCTION public.rereply_platform_compliance_write_guard()
		`).Error)
		err := verifyAsRuntime(tx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "platform compliance write guard is incomplete")
	})

	t.Run("extra late row trigger is rejected", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec(`
			CREATE FUNCTION public.zz_platform_compliance_rewrite()
			RETURNS trigger LANGUAGE plpgsql
			AS $function$ BEGIN RETURN NEW; END $function$
		`).Error)
		require.NoError(t, tx.Exec(`
			CREATE TRIGGER zz_platform_compliance_rewrite
			BEFORE INSERT OR UPDATE ON public.users
			FOR EACH ROW EXECUTE FUNCTION public.zz_platform_compliance_rewrite()
		`).Error)
		err := verifyAsRuntime(tx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "trigger/rule set is not exact")
	})

	t.Run("DML rule is rejected", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec(`
			CREATE RULE zz_platform_compliance_swallow AS
			ON INSERT TO public.users DO INSTEAD NOTHING
		`).Error)
		err := verifyAsRuntime(tx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "trigger/rule set is not exact")
	})

	t.Run("identity registry trigger set is exact", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec(`
			ALTER TABLE public.organization_identity_registry
			DISABLE TRIGGER rereply_platform_compliance_identity_registry_write_guard
		`).Error)
		err := verifyAsRuntime(tx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "identity registry contract is invalid")
	})

	t.Run("identity registry check definitions and count are exact", func(t *testing.T) {
		t.Run("definition", func(t *testing.T) {
			tx := db.Begin()
			require.NoError(t, tx.Error)
			defer tx.Rollback()
			require.NoError(t, tx.Exec(`
				ALTER TABLE public.organization_identity_registry
					DROP CONSTRAINT organization_identity_registry_current_user_format,
					ADD CONSTRAINT organization_identity_registry_current_user_format CHECK (true)
			`).Error)
			err := verifyAsRuntime(tx)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "identity registry contract is invalid")
		})
		t.Run("extra", func(t *testing.T) {
			tx := db.Begin()
			require.NoError(t, tx.Error)
			defer tx.Rollback()
			require.NoError(t, tx.Exec(`
				ALTER TABLE public.organization_identity_registry
					ADD CONSTRAINT organization_identity_registry_extra CHECK (true)
			`).Error)
			err := verifyAsRuntime(tx)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "identity registry contract is invalid")
		})
	})

	t.Run("nonowner trigger ACLs are rejected and reconciled", func(t *testing.T) {
		for _, test := range []struct {
			name  string
			setup func(*gorm.DB, string) error
		}{
			{
				name: "runtime",
				setup: func(tx *gorm.DB, _ string) error {
					return tx.Exec("GRANT TRIGGER ON TABLE public.users TO " + runtimeRole).Error
				},
			},
			{
				name: "public",
				setup: func(tx *gorm.DB, _ string) error {
					return tx.Exec("GRANT TRIGGER ON TABLE public.users TO PUBLIC").Error
				},
			},
			{
				name: "inherited role",
				setup: func(tx *gorm.DB, role string) error {
					if err := tx.Exec("GRANT TRIGGER ON TABLE public.users TO " + role).Error; err != nil {
						return err
					}
					return tx.Exec("GRANT " + role + " TO " + runtimeRole).Error
				},
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				tx := db.Begin()
				require.NoError(t, tx.Error)
				defer tx.Rollback()
				role := "rereply_guard_trigger_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
				if test.name == "inherited role" {
					require.NoError(t, tx.Exec("CREATE ROLE "+role+" NOLOGIN NOSUPERUSER NOBYPASSRLS").Error)
				}
				require.NoError(t, test.setup(tx, role))
				err := verifyAsRuntime(tx)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "owner/trigger ACL contract is invalid")
				require.NoError(t, database.ApplyTenantRLS(tx, runtimeRole))
				require.NoError(t, verifyAsRuntime(tx))
			})
		}
	})

	t.Run("audit indexes are exact", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec(
			"DROP INDEX public.rereply_platform_compliance_audit_run_unique",
		).Error)
		require.NoError(t, tx.Exec(`
			CREATE UNIQUE INDEX rereply_platform_compliance_audit_run_unique
			ON public.audit_logs (organization_id, user_name)
			WHERE resource_type = 'other'
		`).Error)
		err := verifyAsRuntime(tx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "audit index")
	})

	t.Run("known product trigger function is pinned", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec(`
			CREATE OR REPLACE FUNCTION public.rereply_reject_customer_activity_mutation()
			RETURNS trigger LANGUAGE plpgsql
			AS $function$ BEGIN RETURN NEW; END $function$
		`).Error)
		err := verifyAsRuntime(tx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not match the platform compliance contract")
	})

	t.Run("guard function body", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec(`
			CREATE OR REPLACE FUNCTION public.rereply_platform_compliance_write_guard()
			RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
			SET search_path = pg_catalog, public
			AS $function$ BEGIN RETURN NEW; END $function$
		`).Error)
		err := verifyAsRuntime(tx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not match contract")
	})

	t.Run("durable fingerprint", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec(`
			CREATE OR REPLACE FUNCTION public.rereply_platform_compliance_guard_contract()
			RETURNS text LANGUAGE sql IMMUTABLE
			SET search_path = pg_catalog, public
			AS $function$ SELECT 'v1:tampered'::text $function$
		`).Error)
		err := verifyAsRuntime(tx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not match contract")
	})

	t.Run("scan helper execute ACL is exact and stale grants are reconciled", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		thirdRole := "rereply_guard_acl_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		require.NoError(t, tx.Exec("CREATE ROLE "+thirdRole+" NOLOGIN NOSUPERUSER NOBYPASSRLS").Error)
		require.NoError(t, tx.Exec(
			"GRANT EXECUTE ON FUNCTION public.rereply_assert_platform_compliance_scan_authority() TO "+thirdRole,
		).Error)
		err := verifyAsRuntime(tx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not match contract")
		require.NoError(t, tx.Exec("RESET ROLE").Error)
		require.NoError(t, database.ApplyTenantRLS(tx, runtimeRole))
		require.NoError(t, verifyAsRuntime(tx))
	})

	t.Run("scan helper rejects indirect membership in its owner role", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		var migrationRole string
		require.NoError(t, tx.Raw("SELECT current_user::text").Scan(&migrationRole).Error)
		outerRole := "rereply_guard_owner_outer_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		innerRole := "rereply_guard_owner_inner_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		require.NoError(t, tx.Exec("CREATE ROLE "+outerRole+" NOLOGIN NOSUPERUSER NOBYPASSRLS").Error)
		require.NoError(t, tx.Exec("CREATE ROLE "+innerRole+" NOLOGIN NOSUPERUSER NOBYPASSRLS").Error)
		require.NoError(t, tx.Exec("GRANT "+migrationRole+" TO "+outerRole).Error)
		require.NoError(t, tx.Exec("GRANT "+outerRole+" TO "+innerRole).Error)
		err := verifyAsRuntime(tx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not match contract")
	})

	t.Run("runtime cannot own a guarded relation", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		require.NoError(t, tx.Exec("ALTER TABLE public.users OWNER TO "+runtimeRole).Error)
		err := verifyAsRuntime(tx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "owner/trigger ACL contract is invalid")
	})

	t.Run("runtime cannot inherit effective ownership of a guarded relation", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		ownerRole := "rereply_guard_relation_owner_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		require.NoError(t, tx.Exec(
			"CREATE ROLE "+ownerRole+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
		).Error)
		require.NoError(t, tx.Exec("ALTER TABLE public.users OWNER TO "+ownerRole).Error)
		require.NoError(t, tx.Exec("GRANT "+ownerRole+" TO "+runtimeRole).Error)
		err := verifyAsRuntime(tx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "owner/trigger ACL contract is invalid")
	})

	t.Run("guard installation rejects ordinary organization marker preseed", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		for _, trigger := range []string{
			"rereply_platform_compliance_classification_guard",
			"rereply_platform_compliance_creation_audit",
		} {
			require.NoError(t, tx.Exec(
				"ALTER TABLE public.organizations DISABLE TRIGGER "+trigger,
			).Error)
		}
		organization := models.Organization{
			BaseModel: models.BaseModel{ID: uuid.New()}, Name: "Preseeded Ordinary",
			Slug:     "preseeded-ordinary-" + uuid.NewString(),
			Settings: models.JSONB{database.PlatformComplianceThreadsMarkerKey: true},
		}
		require.NoError(t, tx.Create(&organization).Error)
		for _, trigger := range []string{
			"rereply_platform_compliance_classification_guard",
			"rereply_platform_compliance_creation_audit",
		} {
			require.NoError(t, tx.Exec(
				"ALTER TABLE public.organizations ENABLE TRIGGER "+trigger,
			).Error)
		}
		err := database.ApplyTenantRLS(tx, runtimeRole)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unreviewed platform compliance control markers")
	})
}

func setupPlatformComplianceGuardTest(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	require.NoError(t, database.RemoveTenantRLS(db))
	runtimeRole := "rereply_guard_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	require.NoError(t, db.Exec(
		"CREATE ROLE "+runtimeRole+" LOGIN PASSWORD '"+guardRuntimePassword(runtimeRole)+"' "+
			"NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
	).Error)
	t.Cleanup(func() {
		testutil.TruncateTables(db)
		_ = database.RemoveTenantRLS(db)
		_ = db.Exec("DROP OWNED BY " + runtimeRole).Error
		_ = db.Exec("DROP ROLE IF EXISTS " + runtimeRole).Error
	})
	require.NoError(t, database.ApplyTenantRLS(db, runtimeRole))
	return db, runtimeRole
}

func guardRuntimePassword(runtimeRole string) string {
	return "synthetic_" + runtimeRole
}

func createGuardTestOrganization(t *testing.T, db *gorm.DB, purpose bool) models.Organization {
	t.Helper()
	if purpose {
		organizationID := uuid.New()
		require.NoError(t, createAtomicGuardTestOrganization(t, db, organizationID))
		var organization models.Organization
		require.NoError(t, db.Unscoped().First(&organization, "id = ?", organizationID).Error)
		return organization
	}
	organization := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()}, Name: "Guard Test",
		Slug: "guard-test-" + uuid.NewString(), Settings: models.JSONB{},
	}
	require.NoError(t, db.Create(&organization).Error)
	return organization
}

func createAtomicGuardTestOrganization(t *testing.T, db *gorm.DB, organizationID uuid.UUID) error {
	t.Helper()
	ensurePlatformDirectReseller(t, db)
	return database.CreatePlatformComplianceOrganization(
		db, organizationID,
		"guard-create-"+strings.ReplaceAll(uuid.NewString(), "-", ""),
		true, false,
	)
}

func ensurePlatformDirectReseller(t *testing.T, db *gorm.DB) models.Reseller {
	t.Helper()
	var reseller models.Reseller
	err := db.Where("slug = ?", database.PlatformResellerSlug).First(&reseller).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		reseller = models.Reseller{
			BaseModel: models.BaseModel{ID: uuid.New()}, Name: "Platform Direct",
			Slug: database.PlatformResellerSlug, Status: models.ResellerStatusActive,
			Plan: models.ResellerPlanEnterprise, MaxOrganizations: 1000,
		}
		if err := db.Create(&reseller).Error; err != nil {
			require.NoError(t, err)
		}
	} else if err != nil {
		require.NoError(t, err)
	}
	return reseller
}

func platformComplianceAuditChanges(
	runID, currentUser, sessionUser string,
	marker map[string]any,
) models.JSONBArray {
	return models.JSONBArray{
		marker,
		map[string]any{"field": "operator_run_id", "old_value": nil, "new_value": runID},
		map[string]any{"field": "database_current_user", "old_value": nil, "new_value": currentUser},
		map[string]any{"field": "database_session_user", "old_value": nil, "new_value": sessionUser},
		map[string]any{"field": "control_plane_operation", "old_value": nil, "new_value": "enable:threads"},
	}
}

func execAsRuntime(
	db *gorm.DB,
	runtimeRole string,
	organizationID uuid.UUID,
	statement string,
	args ...any,
) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL ROLE " + runtimeRole).Error; err != nil {
			return err
		}
		if err := database.SetTenantContext(tx, organizationID); err != nil {
			return err
		}
		return tx.Exec(statement, args...).Error
	})
}

func deleteAsRuntime(
	db *gorm.DB,
	runtimeRole string,
	organizationID uuid.UUID,
	value any,
	query string,
	args ...any,
) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL ROLE " + runtimeRole).Error; err != nil {
			return err
		}
		if err := database.SetTenantContext(tx, organizationID); err != nil {
			return err
		}
		conditions := append([]any{query}, args...)
		return tx.Delete(value, conditions...).Error
	})
}

func hex64() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "") +
		strings.ReplaceAll(uuid.NewString(), "-", "")
}
