package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/access"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// This handle shares only the real SQL pool, not GORM's callback registry.
// Never close its pool: the owning fixture retains that responsibility.
func observationCallbackDB(t *testing.T, healthy *gorm.DB) *gorm.DB {
	t.Helper()
	pool, err := healthy.DB()
	require.NoError(t, err)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: healthy.Logger})
	require.NoError(t, err)
	return db
}

func observationAssertUnavailable(t *testing.T, f observationFixture) {
	t.Helper()
	r, _ := f.list(t, 1)
	require.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
	assert.NotContains(t, string(testutil.GetResponseBody(r)), "actionable_total")
	assert.NotContains(t, string(testutil.GetResponseBody(r)), "authorized held copy")
}

func observationHeldConnectionAndLockBudget(t *testing.T) {
	f := newObservationFixture(t)
	pool, err := f.app.DB.DB()
	require.NoError(t, err)
	maximum := pool.Stats().MaxOpenConnections
	allReadersJoined := true
	defer func() {
		if allReadersJoined {
			pool.SetMaxOpenConns(maximum)
		} else {
			t.Error("shared pool limit was not restored because an owned reader did not join")
		}
	}()
	pool.SetMaxOpenConns(1)
	require.NoError(t, f.app.DB.Transaction(func(tx *gorm.DB) error {
		result, err := f.app.scopedApp(tx, f.account.OrganizationID).observeStagedReviewSnapshot(f.account.OrganizationID, f.resolver.ID, nil, uuid.Nil, Pagination{Page: 1, Limit: 100}, false)
		if err != nil {
			return err
		}
		require.True(t, result.Known)
		return nil
	}))
	pool.SetMaxOpenConns(3) // writer, observed reader, independent lock observer
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			writer := f.app.DB.Begin()
			require.NoError(t, writer.Error)
			defer func() { _ = writer.Rollback().Error }()
			var writerPID int
			require.NoError(t, writer.Raw("SELECT pg_backend_pid()").Scan(&writerPID).Error)
			writer = writer.Session(&gorm.Session{NewDB: true})
			require.NoError(t, lockStagedReviewObservation(writer, f.account.OrganizationID))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			type outcome struct {
				value *stagedReviewObservation
				err   error
			}
			done := make(chan outcome, 1)
			pids := make(chan int, 1)
			joined := false
			defer func() {
				cancel()
				_ = writer.Rollback().Error
				if !joined {
					select {
					case <-done:
						joined = true
					case <-time.After(6 * time.Second):
						t.Error("owned observation did not stop before fixture cleanup")
					}
				}
				if !joined {
					allReadersJoined = false
				}
			}()
			go func() {
				var value *stagedReviewObservation
				err := f.app.DB.WithContext(ctx).Connection(func(conn *gorm.DB) error {
					var pid int
					if err := conn.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
						return err
					}
					pids <- pid
					// Connection returns an initialized statement; discard the PID Scan's Dest.
					scoped := f.app.scopedApp(conn.Session(&gorm.Session{NewDB: true}), f.account.OrganizationID)
					var err error
					value, err = scoped.observeStagedReviewSnapshot(f.account.OrganizationID, f.resolver.ID, nil, uuid.Nil, Pagination{Page: 1, Limit: 100}, false)
					return err
				})
				done <- outcome{value, err}
			}()
			var readerPID int
			select {
			case readerPID = <-pids:
			case <-time.After(2 * time.Second):
				t.Fatal("reader never acquired its held connection")
			}
			var probeErr error
			require.Eventually(t, func() bool {
				var blocked bool
				probeErr = f.app.DB.Raw(`SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity
					WHERE pid=? AND wait_event_type='Lock' AND ?=ANY(pg_catalog.pg_blocking_pids(pid))
					AND query LIKE '%pg_advisory_xact_lock%')`, readerPID, writerPID).Scan(&blocked).Error
				return probeErr == nil && blocked
			}, time.Second, 10*time.Millisecond, "must observe this reader blocked behind this policy writer before cancellation")
			require.NoError(t, probeErr)
			if mode == "cancel" {
				cancel()
			}
			var result outcome
			select {
			case result = <-done:
				joined = true
			case <-time.After(5 * time.Second):
				t.Fatal("blocked observer failed to terminate within its caller budget")
			}
			require.Error(t, result.err)
			assert.Nil(t, result.value)
			if mode == "cancel" {
				assert.ErrorIs(t, ctx.Err(), context.Canceled)
			} else {
				assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
			}
			require.NoError(t, writer.Rollback().Error)
			var healthy int
			require.NoError(t, f.app.DB.Raw("SELECT 1").Scan(&healthy).Error)
			require.Equal(t, 1, healthy)
			recovered, err := f.app.observeStagedReviewSnapshot(f.account.OrganizationID, f.resolver.ID, nil, uuid.Nil, Pagination{Page: 1, Limit: 100}, false)
			require.NoError(t, err)
			require.True(t, recovered.Known)
		})
		if !allReadersJoined {
			return
		} // Do not start another reader after failed ownership cleanup.
	}
}

func TestStagedObservationCompleteClosingUnionIncludesOlderTransferOnlyMember(t *testing.T) {
	f := newObservationFixture(t)
	newPhone := "60" + testutil.NewTestGraphObjectID()[:10]
	newContact := syncPhoneOnlyCoexistenceContact(t, f.app, f.account, newPhone)
	work, _ := admitCoexistenceAdmissionMessage(t, f.app, f.account, coexistenceAdmissionMessage(t, "wamid.union-"+uuid.NewString(), newPhone, f.principal, "newest copy"), "Held")
	require.Nil(t, work)
	latest := latestWhatsAppIdentityReviewHoldForBSUID(t, f.app, f.account.OrganizationID, f.principal)
	require.Greater(t, latest.PrincipalGeneration, f.hold.PrincipalGeneration)
	role := testutil.CreateTestRoleWithKeys(t, f.app.DB, f.account.OrganizationID, "assigned-union-"+uuid.NewString(), []string{"contacts:write", "contacts.identity_review:write"})
	f.resolver = testutil.CreateTestUser(t, f.app.DB, f.account.OrganizationID, testutil.WithRoleID(&role.ID))
	require.NoError(t, f.app.DB.Model(newContact).Update("assigned_user_id", f.resolver.ID).Error)
	check := func(want bool) {
		t.Helper()
		result, err := f.app.observeStagedReviewSnapshot(f.account.OrganizationID, f.resolver.ID, []uuid.UUID{latest.ID}, uuid.Nil, Pagination{Page: 1, Limit: 100}, false)
		require.NoError(t, err)
		require.True(t, result.Known)
		assert.EqualValues(t, 1, result.Total)
		var previewErr error
		require.NoError(t, f.app.DB.Transaction(func(tx *gorm.DB) error {
			_, previewErr = f.app.PreviewWhatsAppIdentityReviewDecision(tx, WhatsAppIdentityReviewPreviewInput{OrganizationID: f.account.OrganizationID, HoldID: latest.ID, ResolverUserID: f.resolver.ID})
			return nil
		}))
		if want {
			assert.EqualValues(t, 1, result.ActionableTotal)
			require.NoError(t, previewErr)
		} else {
			assert.Zero(t, result.ActionableTotal)
			assert.Equal(t, "reviewer_scope", result.ReasonByHold[latest.ID])
			assert.ErrorIs(t, previewErr, ErrWhatsAppIdentityReviewUnauthorized)
		}
	}
	check(false) // visible latest member is assigned, but the older union member is not
	transfer := models.AgentTransfer{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: f.account.OrganizationID, ContactID: f.contact.ID, WhatsAppAccount: f.account.Name, PhoneNumber: f.phone, Status: models.TransferStatusActive, AgentID: &f.resolver.ID}
	require.NoError(t, f.app.DB.Create(&transfer).Error)
	check(true)
	require.NoError(t, f.app.DB.Model(&transfer).Update("status", models.TransferStatusResumed).Error)
	check(false)
}

func TestStagedObservationCurrentAuthorityMatchesCanonicalMembershipAndSelectedRole(t *testing.T) {
	for _, name := range []string{"direct", "direct_ignores_reseller_suspension", "reseller_owner", "reseller_admin", "reseller_member_inactive", "reseller_inactive", "reseller_wrong_role", "reseller_missing_backing", "selected_role_missing", "super_selected_live", "super_selected_missing", "super_global_fallback", "inactive_user"} {
		t.Run(name, func(t *testing.T) {
			f := newObservationFixture(t)
			f.app.Redis = nil // compare current DB facts, never a cached role
			var membership models.UserOrganization
			require.NoError(t, f.app.DB.Where("user_id=? AND organization_id=?", f.resolver.ID, f.account.OrganizationID).First(&membership).Error)
			reseller := testutil.CreateTestReseller(t, f.app.DB)
			require.NoError(t, f.app.DB.Model(&models.Organization{}).Where("id=?", f.account.OrganizationID).Update("reseller_id", reseller.ID).Error)
			member := models.ResellerMember{BaseModel: models.BaseModel{ID: uuid.New()}, ResellerID: reseller.ID, UserID: f.resolver.ID, Role: models.ResellerRoleAdmin, IsActive: true}
			require.NoError(t, f.app.DB.Create(&member).Error)
			if strings.HasPrefix(name, "reseller_") {
				require.NoError(t, f.app.DB.Model(&membership).Updates(map[string]any{"source": models.MembershipSourceReseller, "reseller_member_id": member.ID}).Error)
			}
			if strings.HasPrefix(name, "super_") {
				require.NoError(t, f.app.DB.Model(f.resolver).Update("is_super_admin", true).Error)
			}
			switch name {
			case "direct_ignores_reseller_suspension", "reseller_inactive":
				require.NoError(t, f.app.DB.Model(reseller).Update("status", "suspended").Error)
			case "reseller_owner":
				require.NoError(t, f.app.DB.Model(&member).Update("role", models.ResellerRoleOwner).Error)
			case "reseller_member_inactive":
				require.NoError(t, f.app.DB.Model(&member).Update("is_active", false).Error)
			case "reseller_wrong_role":
				require.NoError(t, f.app.DB.Model(&member).Update("role", "viewer").Error)
			case "reseller_missing_backing":
				require.NoError(t, f.app.DB.Model(&membership).Update("reseller_member_id", nil).Error)
			case "selected_role_missing", "super_selected_missing":
				require.NotNil(t, membership.RoleID)
				require.NoError(t, f.app.DB.Delete(&models.CustomRole{}, "id=?", *membership.RoleID).Error)
			case "super_global_fallback":
				require.NoError(t, f.app.DB.Delete(&membership).Error)
			case "inactive_user":
				require.NoError(t, f.app.DB.Model(f.resolver).Update("is_active", false).Error)
			}
			_, active, err := access.OrganizationMembershipChecked(f.app.DB, f.resolver.ID, f.account.OrganizationID)
			require.NoError(t, err)
			var current models.User
			require.NoError(t, f.app.DB.First(&current, "id=?", f.resolver.ID).Error)
			want := current.IsActive && (current.IsSuperAdmin || active) && f.app.HasPermission(f.resolver.ID, models.ResourceContacts, models.ActionWrite, f.account.OrganizationID) && f.app.HasPermission(f.resolver.ID, models.ResourceContactsIdentityReview, models.ActionWrite, f.account.OrganizationID)
			expected := true
			switch name {
			case "reseller_member_inactive", "reseller_inactive", "reseller_wrong_role", "reseller_missing_backing", "selected_role_missing", "super_selected_missing", "inactive_user":
				expected = false
			}
			require.Equal(t, expected, want, "fixture must establish the named canonical admission state")
			r, page := f.list(t, 1)
			if want {
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
				require.True(t, page.Known)
				assert.EqualValues(t, 1, page.Actionable)
			} else {
				assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(r))
				assert.NotContains(t, string(testutil.GetResponseBody(r)), "actionable_total")
			}
		})
	}
}

func TestStagedObservationPermissionPreflightFaultIs503BeforeLockOrContent(t *testing.T) {
	f := newObservationFixture(t)
	private := observationCallbackDB(t, f.app.DB)
	f.app = f.app.scopedApp(private, f.account.OrganizationID)
	var preflights, locks, finals atomic.Int32
	require.NoError(t, private.Callback().Row().Before("gorm:row").Register("test:observation-preflight-fault", func(tx *gorm.DB) {
		sql := tx.Statement.SQL.String()
		if strings.Contains(sql, "receipt_scope AS NOT MATERIALIZED") {
			finals.Add(1)
		} else if strings.HasSuffix(sql, " SELECT allowed FROM authority") {
			preflights.Add(1)
			_ = tx.AddError(errors.New("private authority dependency unavailable"))
		}
	}))
	require.NoError(t, private.Callback().Raw().Before("gorm:raw").Register("test:observation-preflight-lock", func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "pg_advisory_xact_lock") {
			locks.Add(1)
		}
	}))
	var healthy int
	require.NoError(t, private.Raw("SELECT 1").Scan(&healthy).Error)
	require.Equal(t, 1, healthy)
	observationAssertUnavailable(t, f)
	detail := testutil.NewGETRequest(t)
	testutil.SetAuthContext(detail, f.account.OrganizationID, f.resolver.ID)
	testutil.SetPathParam(detail, "id", f.event.ID.String())
	testutil.SetQueryParam(detail, "eligibility", "current-v1")
	require.NoError(t, f.app.GetStagedContactIdentityReview(detail))
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(detail))
	assert.NotContains(t, string(testutil.GetResponseBody(detail)), "authorized held copy")
	assert.EqualValues(t, 2, preflights.Load())
	assert.Zero(t, locks.Load())
	assert.Zero(t, finals.Load())
}

func TestStagedObservationRechecksAuthorityAfterSuccessfulPreflight(t *testing.T) {
	for _, detail := range []bool{false, true} {
		t.Run(fmt.Sprintf("detail_%t", detail), func(t *testing.T) {
			f := newObservationFixture(t)
			healthy := f.app.DB
			baseline, err := f.app.observeStagedReviewSnapshot(f.account.OrganizationID, f.resolver.ID, nil, uuid.Nil, Pagination{Page: 1, Limit: 100}, false)
			require.NoError(t, err)
			require.True(t, baseline.Known)
			private := observationCallbackDB(t, healthy)
			f.app = f.app.scopedApp(private, f.account.OrganizationID)
			var preflights, mutations atomic.Int32
			var mutationErr error
			require.NoError(t, private.Callback().Row().Before("gorm:row").Register("test:observation-authority-drift", func(tx *gorm.DB) {
				sql := tx.Statement.SQL.String()
				if strings.HasSuffix(sql, " SELECT allowed FROM authority") {
					preflights.Add(1)
				}
				if strings.Contains(sql, "receipt_scope AS NOT MATERIALIZED") && mutations.CompareAndSwap(0, 1) {
					// The preflight returned and the policy lock is held, but this final
					// statement has not acquired its READ COMMITTED snapshot yet.
					mutationErr = healthy.Model(&models.User{}).Where("id=?", f.resolver.ID).Update("is_active", false).Error
					if mutationErr != nil {
						_ = tx.AddError(mutationErr)
					}
				}
			}))
			r := testutil.NewGETRequest(t)
			testutil.SetAuthContext(r, f.account.OrganizationID, f.resolver.ID)
			if detail {
				testutil.SetPathParam(r, "id", f.event.ID.String())
				testutil.SetQueryParam(r, "eligibility", "current-v1")
				require.NoError(t, f.app.GetStagedContactIdentityReview(r))
			} else {
				require.NoError(t, f.app.ListStagedContactIdentityReviews(r))
			}
			require.NoError(t, mutationErr)
			assert.EqualValues(t, 1, preflights.Load())
			assert.EqualValues(t, 1, mutations.Load())
			assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(r))
			assert.NotContains(t, string(testutil.GetResponseBody(r)), "authorized held copy")
			assert.NotContains(t, string(testutil.GetResponseBody(r)), "actionable_total")
		})
	}
}

// Uses actual restricted PostgreSQL credentials and the actual Tenant wrapper;
// serial because installing RLS policies changes the disposable test schema.
func TestStagedObservationRuntimeRoleTenantOneConnectionAndNoScopeLeak(t *testing.T) {
	// Other serial fixtures share this schema. ApplyTenantRLS checks every
	// active organization, so start clean before creating reseller-bound rows.
	testutil.TruncateTables(testutil.SetupTestDB(t))
	f := newObservationFixture(t)
	admin := f.app.DB
	reseller := testutil.CreateTestReseller(t, admin)
	require.NoError(t, admin.Model(&models.Organization{}).Where("id=?", f.account.OrganizationID).Update("reseller_id", reseller.ID).Error)
	other := testutil.CreateTestOrganizationForReseller(t, admin, reseller.ID)
	role := "rereply_observe_" + uuid.NewString()[:8]
	password := "synthetic" + uuid.NewString()[:8]
	require.NoError(t, admin.Exec(fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS", role, password)).Error)
	t.Cleanup(func() {
		_ = database.RemoveTenantRLS(admin)
		_ = admin.Exec("DROP OWNED BY " + role).Error
		_ = admin.Exec("DROP ROLE IF EXISTS " + role).Error
		testutil.TruncateTables(admin)
	})
	require.NoError(t, database.ApplyTenantRLS(admin, role))
	runtime := openRuntimeRoleTestDB(t, role, password)
	require.NoError(t, database.VerifyTenantRLS(runtime, role))
	pool, err := runtime.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	bounded, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	app := &App{DB: runtime.WithContext(bounded), Log: testutil.NopLogger(), Config: &config.Config{Database: config.DatabaseConfig{RLSEnabled: true, RuntimeRole: role}}}
	r := testutil.NewGETRequest(t)
	testutil.SetAuthContext(r, f.account.OrganizationID, f.resolver.ID)
	require.NoError(t, app.Tenant((*App).ListStagedContactIdentityReviews)(r))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
	var page stagedReviewObservationPage
	testutil.ParseEnvelopeResponse(t, r, &page)
	require.True(t, page.Known)
	assert.EqualValues(t, 1, page.Actionable)
	detail := testutil.NewGETRequest(t)
	testutil.SetAuthContext(detail, f.account.OrganizationID, f.resolver.ID)
	testutil.SetPathParam(detail, "id", f.event.ID.String())
	require.NoError(t, app.Tenant((*App).GetStagedContactIdentityReview)(detail))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(detail))
	var unscoped int64
	require.NoError(t, runtime.Model(&models.InboundEvent{}).Where("id=?", f.event.ID).Count(&unscoped).Error)
	assert.Zero(t, unscoped)
	require.NoError(t, database.WithTenant(runtime, other.ID, func(tx *gorm.DB) error {
		var foreign int64
		if err := tx.Model(&models.InboundEvent{}).Where("id=?", f.event.ID).Count(&foreign).Error; err != nil {
			return err
		}
		assert.Zero(t, foreign)
		return nil
	}))
	// A real foreign transaction cannot be replaced by a client-supplied header.
	require.NoError(t, database.WithTenant(runtime, other.ID, func(tx *gorm.DB) error {
		scoped := app.scopedApp(tx, other.ID)
		foreign := testutil.NewGETRequest(t)
		testutil.SetAuthContext(foreign, other.ID, f.resolver.ID)
		foreign.RequestCtx.Request.Header.Set("X-Organization-ID", f.account.OrganizationID.String())
		if err := scoped.ListStagedContactIdentityReviews(foreign); err != nil {
			return err
		}
		assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(foreign))
		assert.NotContains(t, string(testutil.GetResponseBody(foreign)), "actionable_total")
		return nil
	}))
}

// Bulk boundary fixtures obey every existing immutable hold/member constraint.
// They are stored synthetic claims, not webhook/admission execution evidence.
func observationPersistHold(t *testing.T, f observationFixture, principal, phone string, generation uint64, contacts []uuid.UUID, receipt bool) models.WhatsAppIdentityReviewHold {
	t.Helper()
	reasons := map[uuid.UUID]models.WhatsAppIdentityReviewSelectorReason{}
	for _, id := range contacts {
		reasons[id] = models.WhatsAppIdentityReviewSelectorPhone
	}
	items := identityReviewCandidateMapSlice(reasons)
	claim, err := normalizeWhatsAppIdentityReviewClaim(WhatsAppIdentityReviewClaim{OrganizationID: f.account.OrganizationID, WhatsAppAccountID: f.account.ID, OnboardingCycle: f.hold.OnboardingCycle,
		DirectPrimaryBSUID: principal, Phone: phone, VerifiedEventDigest: fmt.Sprintf("%064x", generation), SelectorBodyDigest: fmt.Sprintf("%064x", generation), VerifiedEventProvenance: WhatsAppIdentityReviewVerifiedMetaEvent})
	require.NoError(t, err)
	hold := f.hold
	hold.ID = uuid.New()
	hold.DirectPrimaryBSUID = principal
	hold.ParentBSUID = ""
	hold.Phone = phone
	hold.PrincipalGeneration = generation
	hold.VerifiedEventDigest = claim.VerifiedEventDigest
	hold.SelectorBodyDigest = claim.SelectorBodyDigest
	hold.MemberCount = uint32(len(items))
	hold.MemberDigest = identityReviewMemberDigest(items)
	hold.SemanticClaimDigest = identityReviewSemanticDigest(claim, hold.MemberDigest)
	hold.Members = nil
	require.NoError(t, f.app.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&hold).Error; err != nil {
			return err
		}
		members := make([]models.WhatsAppIdentityReviewMember, 0, len(items))
		for _, item := range items {
			members = append(members, models.WhatsAppIdentityReviewMember{OrganizationID: f.account.OrganizationID, HoldID: hold.ID, ContactID: item.ContactID, SelectorReasons: item.SelectorReasons})
		}
		if len(members) > 0 {
			if err := tx.CreateInBatches(members, 100).Error; err != nil {
				return err
			}
		}
		if receipt {
			event := f.event
			event.BaseModel = models.BaseModel{ID: uuid.New()}
			event.ReviewHoldID = &hold.ID
			event.ProviderEventID = "wamid.boundary-" + uuid.NewString()
			event.DedupeKey = "identity-review:" + event.ProviderEventID
			if err := tx.Create(&event).Error; err != nil {
				return err
			}
		}
		return nil
	}))
	return hold
}

// Exact raw phone strings remain distinct under idx_contacts_org_phone while
// the production selector normalizes all of them to the same digit sequence.
func observationPhoneVariant(phone string, index int) string {
	var out strings.Builder
	for offset, r := range phone {
		if offset < 9 && index&(1<<offset) != 0 {
			out.WriteByte('-')
		}
		out.WriteRune(r)
	}
	return out.String()
}

func TestStagedObservationUnitAndGenerationCapBoundaries(t *testing.T) {
	for _, kind := range []string{"units", "generations"} {
		t.Run(kind, func(t *testing.T) {
			f := newObservationFixture(t)
			for index := 1; index < 256; index++ {
				principal, generation := f.principal, uint64(index+1)
				if kind == "units" {
					principal = "US.units-" + uuid.NewString()
					generation = 1
				}
				observationPersistHold(t, f, principal, f.phone, generation, []uuid.UUID{f.contact.ID}, true)
			}
			r, boundary := f.list(t, 1)
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
			require.True(t, boundary.Known)
			assert.EqualValues(t, 256, boundary.Total)
			assert.EqualValues(t, 256, boundary.Actionable)
			if kind == "units" {
				assert.EqualValues(t, 256, boundary.Chains)
			} else {
				assert.EqualValues(t, 1, boundary.Chains)
			}
			principal, generation := f.principal, uint64(257)
			if kind == "units" {
				principal = "US.units-overflow-" + uuid.NewString()
				generation = 1
			}
			observationPersistHold(t, f, principal, f.phone, generation, []uuid.UUID{f.contact.ID}, true)
			observationAssertUnavailable(t, f)
		})
	}
}

func TestStagedObservationSelectorCapDoesNotPretendAnIncompleteSetIsReadonly(t *testing.T) {
	f := newObservationFixture(t)
	for index := 1; index < 256; index++ {
		testutil.CreateTestContactWith(t, f.app.DB, f.account.OrganizationID, testutil.WithPhoneNumber(observationPhoneVariant(f.phone, index)))
	}
	r, boundary := f.list(t, 1)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
	require.True(t, boundary.Known)
	assert.EqualValues(t, 1, boundary.Total)
	assert.Zero(t, boundary.Actionable)
	require.Len(t, boundary.Reviews, 1)
	assert.Equal(t, "membership_changed", boundary.Reviews[0].EligibilityReason)
	testutil.CreateTestContactWith(t, f.app.DB, f.account.OrganizationID, testutil.WithPhoneNumber(observationPhoneVariant(f.phone, 256)))
	observationAssertUnavailable(t, f)
}

// The adapter's terminal projection is replaced only in this SQL-source probe,
// leaving all recursive/association CTEs intact. This isolates the8192 database
// association boundary from the independent16384 wire-record budget. It is not
// an assertion that a fully populated8192-association HTTP response must fit.
func TestStagedObservationGlobalAssociationSQLBoundaryBeforeWireBudget(t *testing.T) {
	f := newObservationFixture(t)
	contacts := []uuid.UUID{f.contact.ID}
	for index := 1; index < 256; index++ {
		c := testutil.CreateTestContactWith(t, f.app.DB, f.account.OrganizationID, testutil.WithPhoneNumber(observationPhoneVariant(f.phone, index)))
		contacts = append(contacts, c.ID)
	}
	holdIDs := make([]uuid.UUID, 0, 33)
	for index := 0; index < 32; index++ {
		h := observationPersistHold(t, f, "US.association-"+uuid.NewString(), f.phone, 1, contacts, true)
		holdIDs = append(holdIDs, h.ID)
	}
	check := func(want int64, overflow bool) {
		t.Helper()
		encoded, err := json.Marshal(holdIDs)
		require.NoError(t, err)
		cut := strings.LastIndex(stagedReviewSnapshotSQL, "SELECT kind,unit,")
		require.Greater(t, cut, 0)
		query := stagedReviewSnapshotSQL[:cut] + `SELECT (SELECT count(*) FROM members) AS members,
			(SELECT sum(member_count) FROM chains) AS declared,
			(SELECT count(*) FROM selectors) AS selectors,
			(SELECT overflow FROM chain_guard) AS chain_overflow,
			(SELECT incomplete FROM control) AS incomplete`
		var probe struct {
			Members       int64
			Declared      int64
			Selectors     int64
			ChainOverflow bool
			Incomplete    bool
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, f.app.DB.WithContext(ctx).Raw(query, f.account.OrganizationID, f.resolver.ID, string(encoded), uuid.Nil, 1, 0, false).Scan(&probe).Error)
		assert.Equal(t, want, probe.Declared)
		assert.Equal(t, overflow, probe.ChainOverflow)
		assert.Equal(t, overflow, probe.Incomplete)
		if !overflow {
			assert.EqualValues(t, 8192, probe.Members)
			assert.EqualValues(t, 8192, probe.Selectors)
		} else {
			assert.Zero(t, probe.Members, "overflow stops member retention")
		}
	}
	check(8192, false)
	extraPhone := "60" + testutil.NewTestGraphObjectID()[:10]
	extra := testutil.CreateTestContactWith(t, f.app.DB, f.account.OrganizationID, testutil.WithPhoneNumber(extraPhone))
	hold := observationPersistHold(t, f, "US.association-overflow-"+uuid.NewString(), extraPhone, 1, []uuid.UUID{extra.ID}, true)
	holdIDs = append(holdIDs, hold.ID)
	check(8193, true)
	result, err := f.app.observeStagedReviewSnapshot(f.account.OrganizationID, f.resolver.ID, holdIDs, uuid.Nil, Pagination{Page: 1, Limit: 1}, false)
	require.NoError(t, err)
	require.False(t, result.Known)
	assert.Zero(t, result.Total)
	assert.Zero(t, result.ActionableTotal)
}

func TestStagedObservationForwardDepthAndCycleCannotPublishPartialCounts(t *testing.T) {
	for _, kind := range []string{"depth", "cycle"} {
		t.Run(kind, func(t *testing.T) {
			f := newObservationFixture(t)
			ids := []uuid.UUID{f.contact.ID}
			for index := 0; index < 15; index++ {
				next := testutil.CreateTestContact(t, f.app.DB, f.account.OrganizationID)
				require.NoError(t, f.app.DB.Model(&models.Contact{}).Where("id=?", ids[len(ids)-1]).Update("merged_into_id", next.ID).Error)
				ids = append(ids, next.ID)
			}
			r, boundary := f.list(t, 1)
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
			require.True(t, boundary.Known)
			assert.EqualValues(t, 1, boundary.Total)
			assert.Zero(t, boundary.Actionable)
			require.Len(t, boundary.Reviews, 1)
			assert.Equal(t, "membership_changed", boundary.Reviews[0].EligibilityReason)
			if kind == "depth" {
				next := testutil.CreateTestContact(t, f.app.DB, f.account.OrganizationID)
				require.NoError(t, f.app.DB.Model(&models.Contact{}).Where("id=?", ids[len(ids)-1]).Update("merged_into_id", next.ID).Error)
			} else {
				require.NoError(t, f.app.DB.Model(&models.Contact{}).Where("id=?", ids[len(ids)-1]).Update("merged_into_id", ids[0]).Error)
			}
			observationAssertUnavailable(t, f)
		})
	}
}

// A real PG result stream feeds the unchanged reader, with a private callback
// replacing only its SQL. No claims about application SQL planning follow from
// these decoder fixtures; each budget is tested at its exact boundary and+1.
func TestStagedObservationWireRecordAndByteBudgetsRefuseBeforeRetention(t *testing.T) {
	for _, kind := range []string{"records", "bytes"} {
		for _, overflow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_overflow_%t", kind, overflow), func(t *testing.T) {
				f := newObservationFixture(t)
				private := observationCallbackDB(t, f.app.DB)
				control := `{"allowed":true,"read_all":true,"total":0,"incomplete":false}`
				count := 16383
				bodySize := 14
				lastSize := 14
				if kind == "records" {
					if overflow {
						count++
					}
				} else {
					count = 128
					bodySize = 65536
					lastSize = 8*1024*1024 - len(control) - (count-1)*bodySize
					if overflow {
						lastSize++
					}
					require.GreaterOrEqual(t, lastSize, 14)
					require.LessOrEqual(t, lastSize, 65536)
				}
				var calls atomic.Int32
				require.NoError(t, private.Callback().Row().Before("gorm:row").Register("test:observation-reader-budget", func(tx *gorm.DB) {
					if !strings.Contains(tx.Statement.SQL.String(), "receipt_scope AS NOT MATERIALIZED") {
						return
					}
					calls.Add(1)
					// Row callbacks run after GORM binds Raw SQL; use PostgreSQL's
					// numbered parameters when replacing the already-built statement.
					tx.Statement.SQL.Reset()
					tx.Statement.SQL.WriteString(`SELECT kind,unit,body,false AS oversized FROM (
				SELECT 0 AS ordinal,'control'::text AS kind,'00000000-0000-0000-0000-000000000000'::uuid AS unit,$1::text AS body
				UNION ALL SELECT n,'hold','00000000-0000-0000-0000-000000000000'::uuid,
				'{"padding":"'||repeat('x',(CASE WHEN n=$2::int THEN $3::int ELSE $4::int END)-14)||'"}' FROM generate_series(1,$5::int) AS n
			) wire ORDER BY ordinal`)
					tx.Statement.Vars = []any{control, count, lastSize, bodySize, count}
				}))
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				facts, err := readStagedReviewSnapshot(private.WithContext(ctx), f.account.OrganizationID, f.resolver.ID, "[]", uuid.Nil, Pagination{Page: 1, Limit: 1}, false)
				require.NoError(t, err)
				assert.EqualValues(t, 1, calls.Load())
				assert.Equal(t, overflow, facts.Incomplete)
				retained := len(facts.Holds[uuid.Nil])
				if overflow {
					assert.Equal(t, count-1, retained)
				} else {
					assert.Equal(t, count, retained)
				}
			})
		}
	}
}

func TestStagedObservationSavedMediaRevisionIsValidatedWithoutObjectAccess(t *testing.T) {
	for _, kind := range []string{"valid", "wrong_digest", "changed_caption"} {
		t.Run(kind, func(t *testing.T) {
			f := newObservationFixture(t)
			copy := f.event
			copy.BaseModel = models.BaseModel{ID: uuid.New()}
			copy.ProviderEventID = "wamid.observation-media-" + uuid.NewString()
			copy.DedupeKey = "identity-review:" + copy.ProviderEventID
			copy.Payload = models.JSONB{"schema_version": 1, "message_type": "image", "content": "saved caption", "media_id": "synthetic-observation-media", "media_generation": uint64(1), "media_mime_type": "image/png", "media_filename": "fixture.png", "media_sha256": strings.Repeat("a", 64), "media_status": "pending"}
			copy.Payload["media_revision"] = coexistenceStagedMediaRevisionDigest(copy.Payload, 1)
			_, _, _, _, supported := coexistenceStagedMediaRevision(&copy)
			require.True(t, supported, "valid stored-shape control must be usable before deliberate revision drift")
			switch kind {
			case "wrong_digest":
				require.NotEqual(t, strings.Repeat("f", 64), copy.Payload["media_revision"])
				copy.Payload["media_revision"] = strings.Repeat("f", 64)
			case "changed_caption":
				copy.Payload["content"] = "different caption"
			}
			require.NoError(t, f.app.DB.Create(&copy).Error)
			// Use the one permitted pending->ready transition, never bypass the
			// immutable receipt trigger or alter the captured revision afterwards.
			copy.Payload["media_status"] = "ready"
			copy.Payload["media_url"] = "private/observation-only.png"
			copy.Payload["media_hydrated_id"] = "synthetic-observation-media"
			require.NoError(t, f.app.DB.Model(&copy).Update("payload", copy.Payload).Error)
			detail := testutil.NewGETRequest(t)
			testutil.SetAuthContext(detail, f.account.OrganizationID, f.resolver.ID)
			testutil.SetPathParam(detail, "id", copy.ID.String())
			require.NoError(t, f.app.GetStagedContactIdentityReview(detail))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(detail), string(testutil.GetResponseBody(detail)))
			var body map[string]any
			testutil.ParseEnvelopeResponse(t, detail, &body)
			assert.Equal(t, kind == "valid", body["media_available"])
			if kind == "valid" {
				assert.Equal(t, copy.Payload["media_revision"], body["revision"])
			} else {
				assert.Empty(t, body["revision"])
			}
			assert.NotContains(t, string(testutil.GetResponseBody(detail)), "private/observation-only.png")
		})
	}
}

// These shapes cannot exist as stored receipts under the installed constraint.
// Keep database refusal distinct from the defensive in-memory decoder checks;
// never weaken or disable the schema guard to manufacture a readable row.
func TestStagedObservationForbiddenMediaShapesRemainConstraintRefusals(t *testing.T) {
	for _, kind := range []string{"invalid_sha", "revoked", "zero_generation"} {
		t.Run(kind, func(t *testing.T) {
			f := newObservationFixture(t)
			copy := f.event
			copy.BaseModel = models.BaseModel{ID: uuid.New()}
			copy.ProviderEventID = "wamid.observation-forbidden-media-" + uuid.NewString()
			copy.DedupeKey = "identity-review:" + copy.ProviderEventID
			copy.Payload = models.JSONB{"schema_version": 1, "message_type": "image", "content": "saved caption", "media_id": "synthetic-observation-media", "media_generation": uint64(1), "media_mime_type": "image/png", "media_filename": "fixture.png", "media_sha256": strings.Repeat("a", 64), "media_status": "pending"}
			copy.Payload["media_revision"] = coexistenceStagedMediaRevisionDigest(copy.Payload, 1)
			_, _, _, _, supported := coexistenceStagedMediaRevision(&copy)
			require.True(t, supported)
			switch kind {
			case "invalid_sha":
				copy.Payload["media_sha256"] = "synthetic-sha"
			case "revoked":
				copy.Payload["media_revoked"] = true
			case "zero_generation":
				copy.Payload["media_generation"] = uint64(0)
			}
			if kind != "invalid_sha" {
				_, revision, _, _, supported := coexistenceStagedMediaRevision(&copy)
				assert.False(t, supported, "defensive decoder refuses impossible stored shape before any SQL")
				assert.Empty(t, revision)
			}
			err := f.app.DB.Create(&copy).Error
			require.Error(t, err)
			require.Contains(t, err.Error(), "chk_inbound_events_identity_review_shape")
			var count int64
			require.NoError(t, f.app.DB.Model(&models.InboundEvent{}).Where("id=?", copy.ID).Count(&count).Error)
			assert.Zero(t, count)
			var healthy int
			require.NoError(t, f.app.DB.Raw("SELECT 1").Scan(&healthy).Error)
			assert.Equal(t, 1, healthy)
		})
	}
}
