package handlers

import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

func marketingPreferenceContacts(t *testing.T, app *App, organizationID uuid.UUID) []models.Contact {
	t.Helper()
	var contacts []models.Contact
	require.NoError(t, app.DB.Unscoped().Where("organization_id = ?", organizationID).Order("id").Find(&contacts).Error)
	return contacts
}

func marketingPreferenceContact(t *testing.T, app *App, organizationID uuid.UUID, phone, bsuid string) *models.Contact {
	t.Helper()
	contact := testutil.CreateTestContact(t, app.DB, organizationID)
	require.NoError(t, app.DB.Model(contact).Updates(map[string]any{
		"phone_number": phone, "bs_uid": bsuid, "marketing_opt_out": true,
	}).Error)
	require.NoError(t, app.DB.First(contact, "id = ?", contact.ID).Error)
	return contact
}

func mergeMarketingPreferenceAlias(t *testing.T, app *App, alias, canonical *models.Contact) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, app.DB.Model(alias).Updates(map[string]any{
		"merged_into_id": canonical.ID, "merged_at": now, "deleted_at": now,
	}).Error)
}

func TestMarketingPreference_ConflictingSelectorsNeverMutateEitherOwner(t *testing.T) {
	for _, preference := range []string{"STOP", "RESUME"} {
		t.Run(preference, func(t *testing.T) {
			app := newProcessorTestApp(t)
			org, account := createProcessorTestOrg(t, app)
			phoneOwner := marketingPreferenceContact(t, app, org.ID, "60120000001", "owned-a")
			bsuidOwner := marketingPreferenceContact(t, app, org.ID, "60120000002", "owned-b")
			// STOP must also be a real attempted change, not a no-op assertion.
			if preference == "STOP" {
				require.NoError(t, app.DB.Model(&models.Contact{}).Where("organization_id = ?", org.ID).
					Update("marketing_opt_out", false).Error)
			}
			before := marketingPreferenceContacts(t, app, org.ID)
			err := app.processMarketingPreference(account.PhoneID, phoneOwner.PhoneNumber, bsuidOwner.BSUID, preference)
			require.ErrorIs(t, err, errMarketingPreferenceIdentityConflict)
			assert.Equal(t, before, marketingPreferenceContacts(t, app, org.ID), "no consent, identity, create or restore writes")
		})
	}
}

func TestMarketingPreference_RejectsAmbiguousOrDeletedOwners(t *testing.T) {
	for _, fixture := range []string{"duplicate_bsuid", "plus_phone_owner", "formatted_phone_owner", "deleted_phone_owner", "unowned_phone_known_bsuid"} {
		t.Run(fixture, func(t *testing.T) {
			app := newProcessorTestApp(t)
			org, account := createProcessorTestOrg(t, app)
			first := marketingPreferenceContact(t, app, org.ID, "60123456789", "sender-bsuid")
			second := marketingPreferenceContact(t, app, org.ID, "60129876543", "other-bsuid")
			phone, bsuid := first.PhoneNumber, first.BSUID
			switch fixture {
			case "duplicate_bsuid":
				require.NoError(t, app.DB.Model(second).Update("bs_uid", bsuid).Error)
				phone = ""
			case "plus_phone_owner":
				require.NoError(t, app.DB.Model(second).Update("phone_number", "+"+phone).Error)
			case "formatted_phone_owner":
				require.NoError(t, app.DB.Model(second).Update("phone_number", "+60 12-345 6789").Error)
			case "deleted_phone_owner":
				require.NoError(t, app.DB.Delete(first).Error)
				bsuid = second.BSUID
			case "unowned_phone_known_bsuid":
				phone = "60129999999"
			}
			before := marketingPreferenceContacts(t, app, org.ID)
			require.ErrorIs(t, app.processMarketingPreference(account.PhoneID, phone, bsuid, "RESUME"), errMarketingPreferenceIdentityConflict)
			assert.Equal(t, before, marketingPreferenceContacts(t, app, org.ID))
		})
	}
}

func TestMarketingPreference_UnownedBSUIDCannotReplaceOrBorrowAnotherIdentity(t *testing.T) {
	for _, fixture := range []string{"canonical_bsuid", "merged_family_bsuid", "stored_identity", "conflicting_identity", "old_phone_alias"} {
		t.Run(fixture, func(t *testing.T) {
			app := newProcessorTestApp(t)
			org, account := createProcessorTestOrg(t, app)
			canonical := marketingPreferenceContact(t, app, org.ID, "60123456789", "")
			phone := canonical.PhoneNumber
			switch fixture {
			case "canonical_bsuid":
				require.NoError(t, app.DB.Model(canonical).Update("bs_uid", "established-user").Error)
			case "merged_family_bsuid", "old_phone_alias":
				alias := marketingPreferenceContact(t, app, org.ID, "60129876543", "")
				if fixture == "merged_family_bsuid" {
					require.NoError(t, app.DB.Model(alias).Update("bs_uid", "established-user").Error)
				} else {
					phone = alias.PhoneNumber
				}
				mergeMarketingPreferenceAlias(t, app, alias, canonical)
			case "stored_identity":
				require.NoError(t, app.DB.Model(canonical).Update("metadata", models.JSONB{"coexistence_user_id": "established-user"}).Error)
			case "conflicting_identity":
				require.NoError(t, app.DB.Model(canonical).Update("metadata", models.JSONB{"coexistence_phone_conflict": true}).Error)
			}
			before := marketingPreferenceContacts(t, app, org.ID)
			require.ErrorIs(t, app.processMarketingPreference(account.PhoneID, phone, "unowned-new-user", "RESUME"), errMarketingPreferenceIdentityConflict)
			assert.Equal(t, before, marketingPreferenceContacts(t, app, org.ID))
		})
	}
}

func TestMarketingPreference_KnownMergedAliasPreservesEstablishedCanonicalBSUID(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	canonical := marketingPreferenceContact(t, app, org.ID, "60123456789", "canonical-user")
	alias := marketingPreferenceContact(t, app, org.ID, "60129876543", "historic-user")
	mergeMarketingPreferenceAlias(t, app, alias, canonical)
	beforeAlias := marketingPreferenceContacts(t, app, org.ID)
	for _, selector := range []struct{ phone, bsuid string }{
		{"", "historic-user"}, {"+60129876543", "historic-user"}, {"60123456789", "historic-user"},
	} {
		require.NoError(t, app.processMarketingPreference(account.PhoneID, selector.phone, selector.bsuid, "RESUME"))
		var stored models.Contact
		require.NoError(t, app.DB.First(&stored, "id = ?", canonical.ID).Error)
		assert.False(t, stored.MarketingOptOut)
		assert.Equal(t, "canonical-user", stored.BSUID)
	}
	for _, before := range beforeAlias {
		if before.ID == alias.ID {
			var after models.Contact
			require.NoError(t, app.DB.Unscoped().First(&after, "id = ?", alias.ID).Error)
			assert.Equal(t, before, after)
		}
	}
}

func TestMarketingPreference_KnownAliasInMixedFamilyChangesConsentWithoutEnrichment(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	canonical := marketingPreferenceContact(t, app, org.ID, "60123456789", "")
	known := marketingPreferenceContact(t, app, org.ID, "60129876543", "known-user")
	other := marketingPreferenceContact(t, app, org.ID, "60129876544", "other-user")
	mergeMarketingPreferenceAlias(t, app, known, canonical)
	mergeMarketingPreferenceAlias(t, app, other, canonical)
	require.NoError(t, app.processMarketingPreference(account.PhoneID, "", "known-user", "RESUME"))
	var stored models.Contact
	require.NoError(t, app.DB.First(&stored, "id = ?", canonical.ID).Error)
	assert.False(t, stored.MarketingOptOut)
	assert.Empty(t, stored.BSUID)
}

func TestMarketingPreference_CurrentPhoneEnrichesEmptyBSUIDIdempotently(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := marketingPreferenceContact(t, app, org.ID, "60123456789", "")
	for attempt := 0; attempt < 2; attempt++ {
		require.NoError(t, app.processMarketingPreference(" "+account.PhoneID+" ", " +60123456789 ", " new-user ", "RESUME"))
	}
	var stored models.Contact
	require.NoError(t, app.DB.First(&stored, "id = ?", contact.ID).Error)
	assert.False(t, stored.MarketingOptOut)
	assert.Equal(t, "new-user", stored.BSUID)
	assert.Equal(t, account.Name, stored.WhatsAppAccount)
	assert.Len(t, marketingPreferenceContacts(t, app, org.ID), 1)
}

func TestMarketingPreference_NewUnownedPhoneCreatesOnlyOneContact(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	for attempt := 0; attempt < 2; attempt++ {
		require.NoError(t, app.processMarketingPreference(account.PhoneID, "+60123456789", "new-user", "STOP"))
	}
	contacts := marketingPreferenceContacts(t, app, org.ID)
	require.Len(t, contacts, 1)
	assert.Equal(t, "60123456789", contacts[0].PhoneNumber)
	assert.Equal(t, "new-user", contacts[0].BSUID)
	assert.True(t, contacts[0].MarketingOptOut)
}

func TestMarketingPreference_UnknownBSUIDAndForeignScopeNeverCreateOrMutate(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	otherOrg, otherAccount := createProcessorTestOrg(t, app)
	contact := marketingPreferenceContact(t, app, org.ID, "60123456789", "known-user")
	other := marketingPreferenceContact(t, app, otherOrg.ID, contact.PhoneNumber, "other-user")
	before, otherBefore := marketingPreferenceContacts(t, app, org.ID), marketingPreferenceContacts(t, app, otherOrg.ID)
	require.Error(t, app.processMarketingPreference(account.PhoneID, "", "unknown-user", "RESUME"))
	require.Error(t, app.processMarketingPreference(account.PhoneID, "", "", "RESUME"))
	scoped := app.scopedApp(app.DB, org.ID)
	require.Error(t, scoped.processMarketingPreference(otherAccount.PhoneID, other.PhoneNumber, other.BSUID, "RESUME"))
	assert.Equal(t, before, marketingPreferenceContacts(t, app, org.ID))
	assert.Equal(t, otherBefore, marketingPreferenceContacts(t, app, otherOrg.ID))
}

func TestMarketingPreference_RechecksAccountAfterPolicyFence(t *testing.T) {
	app := newProcessorTestApp(t)
	org, account := createProcessorTestOrg(t, app)
	contact := marketingPreferenceContact(t, app, org.ID, "60123456789", "known-user")
	before := marketingPreferenceContacts(t, app, org.ID)
	ownerTx := app.DB.Begin()
	require.NoError(t, ownerTx.Error)
	t.Cleanup(func() { _ = ownerTx.Rollback().Error })
	require.NoError(t, database.LockOrganizationPolicyScope(ownerTx, org.ID))

	writerPID := make(chan int, 1)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- app.DB.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var pid int
			if err := session.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				writerPID <- 0
				return err
			}
			writerPID <- pid
			writer := &App{DB: session, Log: app.Log, Config: app.Config}
			return writer.processMarketingPreference(account.PhoneID, contact.PhoneNumber, contact.BSUID, "RESUME")
		})
	}()
	select {
	case pid := <-writerPID:
		require.Positive(t, pid)
		testutil.RequirePostgresBackendWaitingForLock(t, app.DB, pid)
	case <-time.After(5 * time.Second):
		t.Fatal("marketing preference writer did not obtain a connection")
	}
	// The writer has read the old account and is waiting for the organization
	// fence. Rebinding that account before releasing it must invalidate the
	// previously read authority, even though the contact still matches.
	require.NoError(t, ownerTx.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, org.ID).
		Update("phone_id", account.PhoneID+"-rebound").Error)
	require.NoError(t, ownerTx.Commit().Error)
	select {
	case err := <-writerDone:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("marketing preference writer did not finish after account rebind")
	}
	assert.Equal(t, before, marketingPreferenceContacts(t, app, org.ID))
}

func TestMarketingPreference_UnownedBSUIDCannotBorrowReviewedPrincipalContact(t *testing.T) {
	app, account, _ := whatsappIdentityFixture(t)
	phone := "60" + testutil.NewTestGraphObjectID()[:10]
	contact := syncPhoneOnlyCoexistenceContact(t, app, account, phone)
	principal := "US.reviewed-" + uuid.NewString()
	message := inboundContinuationTextMessage(t, "wamid.marketing-owner-"+uuid.NewString(), phone, "reviewed principal")
	message.FromUserID = principal
	_, duplicate, err := app.persistAuthenticatedIncomingMessageBeforeAck(
		account.PhoneID, message, "Reviewed principal", strings.Repeat("a", sha256.Size*2),
	)
	require.NoError(t, err)
	require.False(t, duplicate)
	resolver := createWhatsAppIdentityReviewResolver(t, app.DB, account.OrganizationID)
	status, body, preview := previewContactIdentityReviewOverHTTP(
		t, app, account.OrganizationID, resolver.ID, contact.ID,
	)
	require.Equal(t, fasthttp.StatusOK, status, body)
	status, body, decision := decideContactIdentityReviewOverHTTP(
		t, app, account.OrganizationID, resolver.ID, contact.ID, contact.ID, preview,
	)
	require.Equal(t, fasthttp.StatusOK, status, body)
	require.Equal(t, models.WhatsAppIdentityReviewDispositionFutureRouting, decision.Snapshot.Disposition)
	require.NoError(t, app.DB.Model(contact).Update("marketing_opt_out", true).Error)
	require.NoError(t, app.DB.First(contact, "id = ?", contact.ID).Error)
	require.Empty(t, contact.BSUID, "the existing reviewed route must be the ownership evidence")
	before := marketingPreferenceContacts(t, app, account.OrganizationID)
	var holdsBefore []models.WhatsAppIdentityReviewHold
	require.NoError(t, app.DB.Where("organization_id = ?", account.OrganizationID).Order("id").Find(&holdsBefore).Error)

	require.ErrorIs(t, app.processMarketingPreference(
		account.PhoneID, phone, "different-unowned-user", "RESUME",
	), errMarketingPreferenceIdentityConflict)
	assert.Equal(t, before, marketingPreferenceContacts(t, app, account.OrganizationID))
	var holdsAfter []models.WhatsAppIdentityReviewHold
	require.NoError(t, app.DB.Where("organization_id = ?", account.OrganizationID).Order("id").Find(&holdsAfter).Error)
	assert.Equal(t, holdsBefore, holdsAfter, "marketing consent cannot rewrite or extend a reviewed route")
}
