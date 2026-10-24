package handlers

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/utils"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// laneFNameEnv is one organization with a Coexistence (or classic) account and
// an admin who edits contacts.
type laneFNameEnv struct {
	app     *App
	account models.WhatsAppAccount
	admin   *models.User
}

func laneFNewNameEnv(t *testing.T, isSMB bool) laneFNameEnv {
	t.Helper()
	app := webhookTestApp(t)
	t.Cleanup(app.wg.Wait)
	account := orphanStatusTestAccount(t, app, isSMB)
	role := testutil.CreateAdminRole(t, app.DB, account.OrganizationID)
	admin := testutil.CreateTestUser(t, app.DB, account.OrganizationID, testutil.WithRoleID(&role.ID))
	return laneFNameEnv{app: app, account: account, admin: admin}
}

func laneFPhone() string {
	return "601" + testutil.NewTestGraphObjectID()[:9]
}

func laneFAt(seconds int64) time.Time {
	return time.Unix(1_760_000_000+seconds, 0).UTC()
}

type laneFSyncItem struct {
	action, phone, userID, username, fullName, firstName string
	at                                                   time.Time
	// timestamp is sent as-is when at is zero (a missing or invalid Meta time).
	timestamp string
}

func laneFSync(t *testing.T, env laneFNameEnv, item laneFSyncItem) {
	t.Helper()
	require.NoError(t, laneFSyncErr(env, item))
}

func laneFSyncErr(env laneFNameEnv, item laneFSyncItem) error {
	var sync CoexistenceStateSyncItem
	sync.Type = "contact"
	sync.Action = item.action
	sync.Contact.PhoneNumber = item.phone
	sync.Contact.UserID = item.userID
	sync.Contact.Username = item.username
	sync.Contact.FullName = item.fullName
	sync.Contact.FirstName = item.firstName
	sync.Metadata.Timestamp = item.timestamp
	if !item.at.IsZero() {
		sync.Metadata.Timestamp = strconv.FormatInt(item.at.Unix(), 10)
	}
	return env.app.persistContactStateSyncBeforeAck(env.account.PhoneID, []CoexistenceStateSyncItem{sync})
}

// laneFInbound resolves a Cloud API inbound sender the way a new inbound
// message does (getOrCreateNewInboundSenderContact → getOrCreateInboundContact).
func laneFInbound(t *testing.T, env laneFNameEnv, phone, profileName string) *models.Contact {
	t.Helper()
	contact, _, err := env.app.getOrCreateInboundContact(&env.account, phone, profileName, "")
	require.NoError(t, err)
	return contact
}

// laneFCoexistenceIdentity resolves a Coexistence history/echo/first-sender
// identity inside one tenant transaction, as those webhooks do.
func laneFCoexistenceIdentity(t *testing.T, env laneFNameEnv, identity coexistenceContactIdentity) *models.Contact {
	t.Helper()
	var contact *models.Contact
	require.NoError(t, env.app.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		contact, _, err = env.app.scopedApp(tx, env.account.OrganizationID).
			getOrCreateCoexistenceContact(&env.account, identity)
		return err
	}))
	return contact
}

func laneFUpdateContact(t *testing.T, env laneFNameEnv, contactID uuid.UUID, body map[string]any) ContactResponse {
	t.Helper()
	req := testutil.NewJSONRequest(t, body)
	testutil.SetAuthContext(req, env.account.OrganizationID, env.admin.ID)
	testutil.SetPathParam(req, "id", contactID.String())
	require.NoError(t, env.app.UpdateContact(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	var response ContactResponse
	testutil.ParseEnvelopeResponse(t, req, &response)
	return response
}

func laneFContact(t *testing.T, env laneFNameEnv, id uuid.UUID) models.Contact {
	t.Helper()
	var contact models.Contact
	require.NoError(t, env.app.DB.First(&contact, "organization_id = ? AND id = ?", env.account.OrganizationID, id).Error)
	return contact
}

func laneFContactByPhone(t *testing.T, env laneFNameEnv, phone string) models.Contact {
	t.Helper()
	var contact models.Contact
	require.NoError(t, env.app.DB.First(&contact, "organization_id = ? AND phone_number = ?", env.account.OrganizationID, phone).Error)
	return contact
}

func laneFAssertName(t *testing.T, contact models.Contact, displayName, source string) {
	t.Helper()
	assert.Equal(t, displayName, contact.ProfileName, "display name")
	assert.Equal(t, source, contactutil.ContactNameSource(contact.Metadata), "name source")
}

// The WhatsApp Business saved name owns the display name against every
// profile-level source the audit found overwriting it: Cloud API inbound,
// Coexistence history/echo/first-sender identities and campaign recipients.
// Each still refreshes nothing but metadata.wa_profile_name.
func TestLaneFSavedNameOwnsDisplayAgainstProfileSources(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()

	created := laneFInbound(t, env, phone, "Ali WA")
	laneFAssertName(t, laneFContact(t, env, created.ID), "Ali WA", contactutil.ContactNameSourceWAProfile)

	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Ali (Clinic)", firstName: "Ali", at: laneFAt(10)})
	stored := laneFContact(t, env, created.ID)
	laneFAssertName(t, stored, "Ali (Clinic)", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, "Ali (Clinic)", stored.Metadata[contactutil.ContactWABusinessSavedNameKey])
	assert.Equal(t, laneFAt(10).Format(time.RFC3339), stored.Metadata[contactutil.ContactWABusinessSavedNameAtKey])
	assert.Equal(t, true, stored.Metadata["coexistence_app_contact"])
	assert.Equal(t, "Ali WA", stored.Metadata[contactutil.ContactWAProfileNameKey])

	resolved := laneFInbound(t, env, phone, "Ali Changed")
	assert.Equal(t, "Ali (Clinic)", resolved.ProfileName, "the returned contact matches the row")
	stored = laneFContact(t, env, created.ID)
	laneFAssertName(t, stored, "Ali (Clinic)", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, "Ali Changed", stored.Metadata[contactutil.ContactWAProfileNameKey])

	laneFCoexistenceIdentity(t, env, coexistenceContactIdentity{Phone: phone, ProfileName: "Ali Echo", Username: "ali_handle"})
	stored = laneFContact(t, env, created.ID)
	laneFAssertName(t, stored, "Ali (Clinic)", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, "Ali Echo", stored.Metadata[contactutil.ContactWAProfileNameKey])

	_, _, err := contactutil.GetOrCreateContact(env.app.DB, env.account.OrganizationID, phone, "Campaign Recipient")
	require.NoError(t, err)
	laneFAssertName(t, laneFContact(t, env, created.ID), "Ali (Clinic)", contactutil.ContactNameSourceWABusiness)

	response := env.app.buildContactResponse(&stored, env.account.OrganizationID)
	assert.Equal(t, "Ali (Clinic)", response.ProfileName)
	assert.Equal(t, contactutil.ContactNameSourceWABusiness, response.NameSource)
	assert.Equal(t, "Ali Echo", response.WAProfileName)
	assert.Equal(t, "Ali (Clinic)", response.WABusinessSavedName)
	assert.False(t, response.WABusinessSavedNameRemoved)
	visible, ok := response.Metadata.(models.JSONB)
	require.True(t, ok)
	for _, key := range contactNameMetadataKeys() {
		assert.NotContains(t, visible, key, "name provenance is not exposed as editable metadata")
	}
	assert.Equal(t, true, visible["coexistence_app_contact"])
}

// first_name is the saved name when full_name is absent.
func TestLaneFStateSyncFirstNameFallback(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, firstName: "Siti", at: laneFAt(1)})
	laneFAssertName(t, laneFContactByPhone(t, env, phone), "Siti", contactutil.ContactNameSourceWABusiness)
}

// A removal keeps the last saved name as the display name and marks it
// removed; a later add or edit replaces it and clears the mark, including a
// remove and re-add within the same second.
func TestLaneFStateSyncRemoveThenAdd(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()

	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved A", at: laneFAt(10)})
	laneFSync(t, env, laneFSyncItem{action: "remove", phone: phone, at: laneFAt(20)})
	stored := laneFContactByPhone(t, env, phone)
	laneFAssertName(t, stored, "Saved A", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, "Saved A", stored.Metadata[contactutil.ContactWABusinessSavedNameKey])
	assert.Equal(t, laneFAt(20).Format(time.RFC3339), stored.Metadata[contactutil.ContactWABusinessSavedNameRemovedAtKey])
	assert.Equal(t, false, stored.Metadata["coexistence_app_contact"])
	assert.True(t, env.app.buildContactResponse(&stored, env.account.OrganizationID).WABusinessSavedNameRemoved)

	laneFInbound(t, env, phone, "Profile Name")
	laneFAssertName(t, laneFContactByPhone(t, env, phone), "Saved A", contactutil.ContactNameSourceWABusiness)

	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved B", at: laneFAt(30)})
	stored = laneFContactByPhone(t, env, phone)
	laneFAssertName(t, stored, "Saved B", contactutil.ContactNameSourceWABusiness)
	assert.NotContains(t, stored.Metadata, contactutil.ContactWABusinessSavedNameRemovedAtKey)
	assert.False(t, env.app.buildContactResponse(&stored, env.account.OrganizationID).WABusinessSavedNameRemoved)

	laneFSync(t, env, laneFSyncItem{action: "remove", phone: phone, at: laneFAt(40)})
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved C", at: laneFAt(40)})
	stored = laneFContactByPhone(t, env, phone)
	laneFAssertName(t, stored, "Saved C", contactutil.ContactNameSourceWABusiness)
	assert.NotContains(t, stored.Metadata, contactutil.ContactWABusinessSavedNameRemovedAtKey)
}

// Meta may redeliver or reorder address-book events. An event older than the
// last applied add/edit or removal changes no name.
func TestLaneFStateSyncIgnoresOlderAddressBookEvents(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()

	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Newest", at: laneFAt(50)})
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Stale", at: laneFAt(30)})
	stored := laneFContactByPhone(t, env, phone)
	laneFAssertName(t, stored, "Newest", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, laneFAt(50).Format(time.RFC3339), stored.Metadata[contactutil.ContactWABusinessSavedNameAtKey])

	laneFSync(t, env, laneFSyncItem{action: "remove", phone: phone, at: laneFAt(60)})
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Older Than Removal", at: laneFAt(55)})
	stored = laneFContactByPhone(t, env, phone)
	laneFAssertName(t, stored, "Newest", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, laneFAt(60).Format(time.RFC3339), stored.Metadata[contactutil.ContactWABusinessSavedNameRemovedAtKey])

	laneFSync(t, env, laneFSyncItem{action: "remove", phone: phone, at: laneFAt(45)})
	assert.Equal(t, laneFAt(60).Format(time.RFC3339),
		laneFContactByPhone(t, env, phone).Metadata[contactutil.ContactWABusinessSavedNameRemovedAtKey],
		"an older removal does not move the removal mark back")
}

// A manual CRM rename owns the display name against the saved name and the
// profile name. Re-saving the form with the same name keeps the source, and
// clearing the name falls back to the saved name, then the profile name.
func TestLaneFManualRenameOwnsDisplayName(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	contact := laneFInbound(t, env, phone, "Profile A")

	response := laneFUpdateContact(t, env, contact.ID, map[string]any{"profile_name": "Dr Ali"})
	assert.Equal(t, "Dr Ali", response.ProfileName)
	assert.Equal(t, contactutil.ContactNameSourceManual, response.NameSource)
	assert.Equal(t, "Profile A", response.WAProfileName)

	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved X", at: laneFAt(10)})
	stored := laneFContact(t, env, contact.ID)
	laneFAssertName(t, stored, "Dr Ali", contactutil.ContactNameSourceManual)
	assert.Equal(t, "Saved X", stored.Metadata[contactutil.ContactWABusinessSavedNameKey])

	laneFInbound(t, env, phone, "Profile B")
	laneFCoexistenceIdentity(t, env, coexistenceContactIdentity{Phone: phone, ProfileName: "Profile C"})
	stored = laneFContact(t, env, contact.ID)
	laneFAssertName(t, stored, "Dr Ali", contactutil.ContactNameSourceManual)
	assert.Equal(t, "Profile C", stored.Metadata[contactutil.ContactWAProfileNameKey])

	// The detail form always re-sends profile_name; an edit of other fields
	// must not change who owns the name.
	response = laneFUpdateContact(t, env, contact.ID, map[string]any{"profile_name": "Dr Ali", "tags": []string{"vip"}})
	assert.Equal(t, contactutil.ContactNameSourceManual, response.NameSource)

	// Server-owned provenance cannot be set or erased through metadata.
	response = laneFUpdateContact(t, env, contact.ID, map[string]any{
		"profile_name": "Dr Ali",
		"metadata": map[string]any{
			"crm_note":                                  "kept",
			contactutil.ContactNameSourceKey:            contactutil.ContactNameSourceWAProfile,
			contactutil.ContactWABusinessSavedNameKey:   "Spoofed",
			contactutil.ContactWABusinessSavedNameAtKey: "2099-01-01T00:00:00Z",
		},
	})
	stored = laneFContact(t, env, contact.ID)
	laneFAssertName(t, stored, "Dr Ali", contactutil.ContactNameSourceManual)
	assert.Equal(t, "Saved X", stored.Metadata[contactutil.ContactWABusinessSavedNameKey])
	assert.Equal(t, laneFAt(10).Format(time.RFC3339), stored.Metadata[contactutil.ContactWABusinessSavedNameAtKey])
	assert.Equal(t, "Profile C", stored.Metadata[contactutil.ContactWAProfileNameKey])
	assert.Equal(t, "kept", stored.Metadata["crm_note"])
	assert.Equal(t, "Saved X", response.WABusinessSavedName)

	response = laneFUpdateContact(t, env, contact.ID, map[string]any{"profile_name": ""})
	assert.Equal(t, "Saved X", response.ProfileName)
	assert.Equal(t, contactutil.ContactNameSourceWABusiness, response.NameSource)

	// Saving the form again with the fallback name keeps it a saved name.
	response = laneFUpdateContact(t, env, contact.ID, map[string]any{"profile_name": "Saved X"})
	assert.Equal(t, contactutil.ContactNameSourceWABusiness, response.NameSource)
}

func TestLaneFClearingManualNameWithoutSavedNameFallsBackToProfile(t *testing.T) {
	env := laneFNewNameEnv(t, false)
	phone := laneFPhone()
	contact := laneFInbound(t, env, phone, "Profile A")
	laneFUpdateContact(t, env, contact.ID, map[string]any{"profile_name": "Dr Ali"})
	laneFInbound(t, env, phone, "Profile B")
	laneFAssertName(t, laneFContact(t, env, contact.ID), "Dr Ali", contactutil.ContactNameSourceManual)

	response := laneFUpdateContact(t, env, contact.ID, map[string]any{"profile_name": ""})
	assert.Equal(t, "Profile B", response.ProfileName)
	assert.Equal(t, contactutil.ContactNameSourceWAProfile, response.NameSource)

	laneFInbound(t, env, phone, "Profile C")
	laneFAssertName(t, laneFContact(t, env, contact.ID), "Profile C", contactutil.ContactNameSourceWAProfile)
}

// Release canary rule: with no saved or manual name, the WhatsApp profile name
// still names the contact, for new contacts and for contacts named before
// provenance was recorded.
func TestLaneFProfileNameWinsWithoutOwnedName(t *testing.T) {
	env := laneFNewNameEnv(t, false)
	phone := laneFPhone()
	created := laneFInbound(t, env, phone, "Brand New")
	assert.Equal(t, "Brand New", created.ProfileName)
	laneFAssertName(t, laneFContact(t, env, created.ID), "Brand New", contactutil.ContactNameSourceWAProfile)

	legacy := testutil.CreateTestContactWith(t, env.app.DB, env.account.OrganizationID, testutil.WithPhoneNumber(laneFPhone()))
	require.NoError(t, env.app.DB.Model(legacy).Updates(map[string]any{"profile_name": "Legacy", "metadata": models.JSONB{}}).Error)
	laneFInbound(t, env, legacy.PhoneNumber, "Updated Profile")
	laneFAssertName(t, laneFContact(t, env, legacy.ID), "Updated Profile", contactutil.ContactNameSourceWAProfile)
}

// End to end through /api/webhook on a pure Cloud API number: an inbound
// message records the sender's profile name but no longer replaces a manual
// rename; a new sender is still named by its profile.
func TestLaneFCloudAPIInboundWebhookKeepsManualName(t *testing.T) {
	env := laneFNewNameEnv(t, false)
	phone := laneFPhone()
	existing := models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  env.account.OrganizationID,
		PhoneNumber:     phone,
		ProfileName:     "Dr Ali",
		WhatsAppAccount: env.account.Name,
		Metadata:        models.JSONB{contactutil.ContactNameSourceKey: contactutil.ContactNameSourceManual},
	}
	require.NoError(t, env.app.DB.Create(&existing).Error)
	newPhone := laneFPhone()

	now := time.Now()
	body := retryPOSTBody(t, env.account, []map[string]any{
		retryPOSTInboundChange(env.account, models.Contact{PhoneNumber: phone, ProfileName: "Ali WA"},
			"wamid.lanef-manual-"+uuid.NewString(), now),
		retryPOSTInboundChange(env.account, models.Contact{PhoneNumber: newPhone, ProfileName: "Fresh WA"},
			"wamid.lanef-new-"+uuid.NewString(), now),
	})
	require.Equal(t, http.StatusOK, signedWebhookStatusCode(t, env.app, body))
	env.app.wg.Wait()

	stored := laneFContact(t, env, existing.ID)
	laneFAssertName(t, stored, "Dr Ali", contactutil.ContactNameSourceManual)
	assert.Equal(t, "Ali WA", stored.Metadata[contactutil.ContactWAProfileNameKey])
	laneFAssertName(t, laneFContactByPhone(t, env, newPhone), "Fresh WA", contactutil.ContactNameSourceWAProfile)
}

// A BSUID-only address-book entry names the BSUID's own contact, never a
// phone-keyed contact without that BSUID. Its username is the profile-level
// name; the saved name owns the display.
func TestLaneFStateSyncBSUIDOnlyContact(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phoneKeyed := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: env.account.OrganizationID,
		PhoneNumber:    laneFPhone(),
		ProfileName:    "Phone Person",
		Metadata:       models.JSONB{},
	}
	require.NoError(t, env.app.DB.Create(&phoneKeyed).Error)
	userID := "US.lanef" + testutil.NewTestGraphObjectID()[:12]

	laneFSync(t, env, laneFSyncItem{action: "add", userID: userID, username: "ali_handle", fullName: "Ali Saved", at: laneFAt(1)})
	var byUser models.Contact
	require.NoError(t, env.app.DB.First(&byUser, "organization_id = ? AND bs_uid = ?", env.account.OrganizationID, userID).Error)
	assert.NotEqual(t, phoneKeyed.ID, byUser.ID)
	assert.True(t, isCoexistencePlaceholderPhone(byUser.PhoneNumber))
	laneFAssertName(t, byUser, "Ali Saved", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, "ali_handle", byUser.Metadata[contactutil.ContactWAProfileNameKey])

	laneFCoexistenceIdentity(t, env, coexistenceContactIdentity{UserID: userID, ProfileName: "Ali Profile"})
	byUser = laneFContact(t, env, byUser.ID)
	laneFAssertName(t, byUser, "Ali Saved", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, "Ali Profile", byUser.Metadata[contactutil.ContactWAProfileNameKey])
	assert.Equal(t, userID, byUser.BSUID)

	unchanged := laneFContact(t, env, phoneKeyed.ID)
	assert.Equal(t, "Phone Person", unchanged.ProfileName)
	assert.Empty(t, contactutil.ContactNameSource(unchanged.Metadata))
	assert.Empty(t, unchanged.BSUID)

	// A later phone-bearing entry for the same BSUID keeps the name on the
	// BSUID's contact.
	laneFSync(t, env, laneFSyncItem{action: "add", userID: userID, fullName: "Ali Saved 2", at: laneFAt(2)})
	laneFAssertName(t, laneFContact(t, env, byUser.ID), "Ali Saved 2", contactutil.ContactNameSourceWABusiness)
}

func TestLaneFCreateContactNameIsManual(t *testing.T) {
	env := laneFNewNameEnv(t, false)
	req := testutil.NewJSONRequest(t, map[string]any{
		"phone_number": laneFPhone(),
		"profile_name": "Typed Name",
		"metadata": map[string]any{
			"crm_note":                       "kept",
			contactutil.ContactNameSourceKey: contactutil.ContactNameSourceWABusiness,
		},
	})
	testutil.SetAuthContext(req, env.account.OrganizationID, env.admin.ID)
	require.NoError(t, env.app.CreateContact(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	var response ContactResponse
	testutil.ParseEnvelopeResponse(t, req, &response)
	assert.Equal(t, "Typed Name", response.ProfileName)
	assert.Equal(t, contactutil.ContactNameSourceManual, response.NameSource)

	stored := laneFContact(t, env, response.ID)
	laneFAssertName(t, stored, "Typed Name", contactutil.ContactNameSourceManual)
	assert.Equal(t, "kept", stored.Metadata["crm_note"])

	laneFInbound(t, env, stored.PhoneNumber, "Profile")
	laneFAssertName(t, laneFContact(t, env, stored.ID), "Typed Name", contactutil.ContactNameSourceManual)
}

func laneFImportContacts(t *testing.T, env laneFNameEnv, csv string) map[string]any {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("table", "contacts"))
	require.NoError(t, writer.WriteField("update_on_duplicate", "true"))
	part, err := writer.CreateFormFile("file", "contacts.csv")
	require.NoError(t, err)
	_, err = part.Write([]byte(csv))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType(writer.FormDataContentType())
	req.RequestCtx.Request.SetBody(body.Bytes())
	testutil.SetAuthContext(req, env.account.OrganizationID, env.admin.ID)
	require.NoError(t, env.app.ImportData(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	var result map[string]any
	testutil.ParseEnvelopeResponse(t, req, &result)
	return result
}

// An imported name is a manual name, on create and when it changes an existing
// contact's name; re-importing an unchanged name keeps the current source and
// the update keeps the rest of the metadata.
func TestLaneFImportedNameIsManual(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	newPhone := laneFPhone()
	savedPhone := laneFPhone()
	renamedPhone := laneFPhone()
	laneFSync(t, env, laneFSyncItem{action: "add", phone: savedPhone, fullName: "Saved Same", at: laneFAt(1)})
	laneFSync(t, env, laneFSyncItem{action: "add", phone: renamedPhone, fullName: "Saved Old", at: laneFAt(1)})

	result := laneFImportContacts(t, env, fmt.Sprintf(
		"phone_number,profile_name\n%s,Imported New\n%s,Saved Same\n%s,Imported Rename\n",
		newPhone, savedPhone, renamedPhone,
	))
	assert.EqualValues(t, 1, result["created"])
	assert.EqualValues(t, 2, result["updated"])

	laneFAssertName(t, laneFContactByPhone(t, env, newPhone), "Imported New", contactutil.ContactNameSourceManual)
	laneFAssertName(t, laneFContactByPhone(t, env, savedPhone), "Saved Same", contactutil.ContactNameSourceWABusiness)
	renamed := laneFContactByPhone(t, env, renamedPhone)
	laneFAssertName(t, renamed, "Imported Rename", contactutil.ContactNameSourceManual)
	assert.Equal(t, "Saved Old", renamed.Metadata[contactutil.ContactWABusinessSavedNameKey])
	assert.Equal(t, true, renamed.Metadata["coexistence_app_contact"])
}

func TestLaneFContactNameFieldsAreMasked(t *testing.T) {
	var response ContactResponse
	applyContactNameFields(&response, models.JSONB{
		contactutil.ContactNameSourceKey:                   contactutil.ContactNameSourceWABusiness,
		contactutil.ContactWAProfileNameKey:                "+60123456789",
		contactutil.ContactWABusinessSavedNameKey:          "60198765432",
		contactutil.ContactWABusinessSavedNameRemovedAtKey: "2026-10-16T00:00:00Z",
	}, true)
	assert.Equal(t, contactutil.ContactNameSourceWABusiness, response.NameSource)
	assert.NotContains(t, response.WAProfileName, "23456")
	assert.NotContains(t, response.WABusinessSavedName, "98765")
	assert.True(t, response.WABusinessSavedNameRemoved)
}

// laneFHoldContactRow locks a contact row in a transaction the test commits
// later, standing in for a webhook or edit that is mid-flight.
func laneFHoldContactRow(t *testing.T, env laneFNameEnv, id uuid.UUID) (*gorm.DB, models.Contact) {
	t.Helper()
	tx := env.app.DB.Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { _ = tx.Rollback().Error })
	var locked models.Contact
	require.NoError(t, tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&locked, "organization_id = ? AND id = ?", env.account.OrganizationID, id).Error)
	return tx, locked
}

func laneFAwaitLockWaiter(t *testing.T, env laneFNameEnv) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting int64
		if err := env.app.DB.Raw(
			"SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'",
		).Scan(&waiting).Error; err != nil {
			return false
		}
		return waiting > 0
	}, 10*time.Second, 10*time.Millisecond, "the concurrent writer never waited for the contact row")
}

// A manual rename that waits for an in-flight saved-name write decides on the
// committed row: it becomes manual and keeps the saved name the webhook wrote.
func TestLaneFManualRenameWaitingOnStateSync(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	contact := laneFInbound(t, env, laneFPhone(), "Profile")

	tx, locked := laneFHoldContactRow(t, env, contact.ID)
	metadata := contactutil.CloneContactMetadata(locked.Metadata)
	displayName := applyCoexistenceSavedContactName(locked.ProfileName, metadata, "Saved X", laneFAt(5))
	require.NoError(t, tx.Model(&models.Contact{}).Where("id = ?", contact.ID).
		Updates(map[string]any{"profile_name": displayName, "metadata": metadata}).Error)

	done := make(chan ContactResponse, 1)
	go func() { done <- laneFUpdateContact(t, env, contact.ID, map[string]any{"profile_name": "Dr Ali"}) }()
	laneFAwaitLockWaiter(t, env)
	require.NoError(t, tx.Commit().Error)
	response := <-done

	assert.Equal(t, "Dr Ali", response.ProfileName)
	stored := laneFContact(t, env, contact.ID)
	laneFAssertName(t, stored, "Dr Ali", contactutil.ContactNameSourceManual)
	assert.Equal(t, "Saved X", stored.Metadata[contactutil.ContactWABusinessSavedNameKey])
}

// A saved-name sync that waits for an in-flight manual rename keeps the
// manual name and records the saved name.
func TestLaneFStateSyncWaitingOnManualRename(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	contact := laneFInbound(t, env, phone, "Profile")

	tx, locked := laneFHoldContactRow(t, env, contact.ID)
	updates := map[string]any{}
	name := "Dr Ali"
	applyContactEditName(&locked, &name, nil, updates, false)
	require.NoError(t, tx.Model(&models.Contact{}).Where("id = ?", contact.ID).Updates(updates).Error)

	done := make(chan error, 1)
	go func() {
		done <- laneFSyncErr(env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved X", at: laneFAt(5)})
	}()
	laneFAwaitLockWaiter(t, env)
	require.NoError(t, tx.Commit().Error)
	require.NoError(t, <-done)

	stored := laneFContact(t, env, contact.ID)
	laneFAssertName(t, stored, "Dr Ali", contactutil.ContactNameSourceManual)
	assert.Equal(t, "Saved X", stored.Metadata[contactutil.ContactWABusinessSavedNameKey])
	assert.Equal(t, true, stored.Metadata["coexistence_app_contact"])
}

// An inbound message that waits for an in-flight saved-name sync keeps the
// saved name and records its profile name.
func TestLaneFInboundWaitingOnStateSync(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	contact := laneFInbound(t, env, phone, "Profile A")

	tx, locked := laneFHoldContactRow(t, env, contact.ID)
	metadata := contactutil.CloneContactMetadata(locked.Metadata)
	displayName := applyCoexistenceSavedContactName(locked.ProfileName, metadata, "Saved X", laneFAt(5))
	require.NoError(t, tx.Model(&models.Contact{}).Where("id = ?", contact.ID).
		Updates(map[string]any{"profile_name": displayName, "metadata": metadata}).Error)

	done := make(chan error, 1)
	go func() {
		_, _, err := env.app.getOrCreateInboundContact(&env.account, phone, "Profile B", "")
		done <- err
	}()
	laneFAwaitLockWaiter(t, env)
	require.NoError(t, tx.Commit().Error)
	require.NoError(t, <-done)

	stored := laneFContact(t, env, contact.ID)
	laneFAssertName(t, stored, "Saved X", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, "Profile B", stored.Metadata[contactutil.ContactWAProfileNameKey])
}

// A saved-name sync that waits for an in-flight inbound profile write applies
// the saved name and keeps the profile name.
func TestLaneFStateSyncWaitingOnInbound(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	contact := laneFInbound(t, env, phone, "Profile A")

	tx, locked := laneFHoldContactRow(t, env, contact.ID)
	updates := contactutil.ObserveContactProfileName(&locked, "Profile B")
	require.NotNil(t, updates)
	require.NoError(t, tx.Model(&models.Contact{}).Where("id = ?", contact.ID).Updates(updates).Error)

	done := make(chan error, 1)
	go func() {
		done <- laneFSyncErr(env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved X", at: laneFAt(5)})
	}()
	laneFAwaitLockWaiter(t, env)
	require.NoError(t, tx.Commit().Error)
	require.NoError(t, <-done)

	stored := laneFContact(t, env, contact.ID)
	laneFAssertName(t, stored, "Saved X", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, "Profile B", stored.Metadata[contactutil.ContactWAProfileNameKey])
}

// Whatever order a manual rename, a saved-name sync, an inbound profile name
// and a campaign recipient name run in, they converge: the manual name shows,
// and the saved and profile names are both recorded.
func TestLaneFConcurrentNameWritersConverge(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	for round := 0; round < 6; round++ {
		phone := laneFPhone()
		contact := laneFInbound(t, env, phone, "Profile A")

		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		run := func(fn func() error) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- fn()
			}()
		}
		run(func() error {
			return laneFSyncErr(env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved X", at: laneFAt(int64(round))})
		})
		run(func() error {
			_, _, err := env.app.getOrCreateInboundContact(&env.account, phone, "Profile B", "")
			return err
		})
		run(func() error {
			return env.app.DB.Transaction(func(tx *gorm.DB) error {
				_, _, err := contactutil.GetOrCreateContact(tx, env.account.OrganizationID, phone, "Campaign Name")
				return err
			})
		})
		run(func() error {
			name := "Dr Ali"
			return canonicalContactWriteTransaction(env.app.DB, func(tx *gorm.DB) error {
				locked, err := contactutil.ResolveCanonicalContactForUpdate(tx, env.account.OrganizationID, contact.ID)
				if err != nil {
					return err
				}
				updates := map[string]any{}
				applyContactEditName(locked, &name, nil, updates, false)
				return tx.Model(&models.Contact{}).Where("id = ?", locked.ID).Updates(updates).Error
			})
		})
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}

		stored := laneFContact(t, env, contact.ID)
		laneFAssertName(t, stored, "Dr Ali", contactutil.ContactNameSourceManual)
		assert.Equal(t, "Saved X", stored.Metadata[contactutil.ContactWABusinessSavedNameKey], "round %d", round)
		assert.Equal(t, "Profile B", stored.Metadata[contactutil.ContactWAProfileNameKey], "round %d", round)
		assert.Equal(t, true, stored.Metadata["coexistence_app_contact"], "round %d", round)
	}
}

// Re-creating a soft-deleted contact with a name restores it with that name as
// a manual name. A name-only restore merges the source into the stored
// metadata instead of writing back the earlier unlocked read.
func TestLaneFRestoredContactNameIsManual(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved X", at: laneFAt(1)})
	stored := laneFContactByPhone(t, env, phone)
	require.NoError(t, env.app.DB.Delete(&stored).Error)

	req := testutil.NewJSONRequest(t, map[string]any{"phone_number": phone, "profile_name": " Restored Name "})
	testutil.SetAuthContext(req, env.account.OrganizationID, env.admin.ID)
	require.NoError(t, env.app.CreateContact(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	var response ContactResponse
	testutil.ParseEnvelopeResponse(t, req, &response)
	assert.Equal(t, stored.ID, response.ID)
	assert.Equal(t, "Restored Name", response.ProfileName)
	assert.Equal(t, contactutil.ContactNameSourceManual, response.NameSource)

	restored := laneFContact(t, env, stored.ID)
	laneFAssertName(t, restored, "Restored Name", contactutil.ContactNameSourceManual)
	assert.Equal(t, "Saved X", restored.Metadata[contactutil.ContactWABusinessSavedNameKey])
	assert.Equal(t, true, restored.Metadata["coexistence_app_contact"])

	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved Y", at: laneFAt(2)})
	laneFAssertName(t, laneFContact(t, env, stored.ID), "Restored Name", contactutil.ContactNameSourceManual)
}

// A contact edit makes one metadata merge: the stored manual unread mark and
// display-name provenance survive whatever metadata the client sends, while
// the client's own keys are written.
func TestLaneFUpdateContactKeepsManualUnreadAndNameKeys(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	contact := laneFInbound(t, env, phone, "Profile A")
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved X", at: laneFAt(10)})
	laneFSetManualUnread(t, env, contact.ID)

	for _, body := range []map[string]any{
		{"profile_name": "Saved X", "metadata": laneFSpoofedMetadata("first")},
		{"metadata": laneFSpoofedMetadata("second")},
	} {
		response := laneFUpdateContact(t, env, contact.ID, body)
		assert.True(t, response.ManualUnread, "the manual unread mark survives the edit")
		assert.Equal(t, contactutil.ContactNameSourceWABusiness, response.NameSource)

		stored := laneFContact(t, env, contact.ID)
		laneFAssertName(t, stored, "Saved X", contactutil.ContactNameSourceWABusiness)
		assert.Equal(t, true, stored.Metadata[contactManualUnreadKey])
		assert.Equal(t, "Saved X", stored.Metadata[contactutil.ContactWABusinessSavedNameKey])
		assert.Equal(t, laneFAt(10).Format(time.RFC3339), stored.Metadata[contactutil.ContactWABusinessSavedNameAtKey])
		assert.Equal(t, "Profile A", stored.Metadata[contactutil.ContactWAProfileNameKey])
		assert.Equal(t, body["metadata"].(map[string]any)["crm_note"], stored.Metadata["crm_note"])
	}

	// A far-future time sent by a client cannot block later address-book events.
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved Y", at: laneFAt(20)})
	laneFAssertName(t, laneFContact(t, env, contact.ID), "Saved Y", contactutil.ContactNameSourceWABusiness)
}

func laneFSetManualUnread(t *testing.T, env laneFNameEnv, id uuid.UUID) {
	t.Helper()
	require.NoError(t, env.app.DB.Exec(
		"UPDATE contacts SET metadata = metadata || jsonb_build_object(?::text, true) WHERE id = ?",
		contactManualUnreadKey, id,
	).Error)
}

func laneFSpoofedMetadata(note string) map[string]any {
	return map[string]any{
		"crm_note":                                  note,
		contactManualUnreadKey:                      false,
		contactutil.ContactNameSourceKey:            contactutil.ContactNameSourceWAProfile,
		contactutil.ContactWABusinessSavedNameKey:   "Spoofed",
		contactutil.ContactWABusinessSavedNameAtKey: "2099-01-01T00:00:00Z",
		contactutil.ContactWAProfileNameKey:         "Spoofed Profile",
	}
}

// With phone masking on, the edit form shows a phone-shaped name masked. A
// save that echoes that masked string is not a rename; a real new name is.
func TestLaneFMaskedNameEchoIsNotARename(t *testing.T) {
	env := laneFNewNameEnv(t, false)
	require.NoError(t, env.app.DB.Model(&models.Organization{}).
		Where("id = ?", env.account.OrganizationID).
		Update("settings", models.JSONB{"mask_phone_numbers": true}).Error)
	require.True(t, env.app.ShouldMaskPhoneNumbers(env.account.OrganizationID))
	contact := laneFInbound(t, env, laneFPhone(), "+60123456789")
	masked := utils.MaskIfPhoneNumber("+60123456789")
	require.NotEqual(t, "+60123456789", masked)

	response := laneFUpdateContact(t, env, contact.ID, map[string]any{"profile_name": masked, "tags": []string{"vip"}})
	assert.Equal(t, masked, response.ProfileName, "the response is masked as before")
	laneFAssertName(t, laneFContact(t, env, contact.ID), "+60123456789", contactutil.ContactNameSourceWAProfile)

	laneFUpdateContact(t, env, contact.ID, map[string]any{"profile_name": masked})
	laneFAssertName(t, laneFContact(t, env, contact.ID), "+60123456789", contactutil.ContactNameSourceWAProfile)

	laneFUpdateContact(t, env, contact.ID, map[string]any{"profile_name": "Dr Ali"})
	laneFAssertName(t, laneFContact(t, env, contact.ID), "Dr Ali", contactutil.ContactNameSourceManual)
}

// A contact first created by an address-book add is created with the saved
// name, so its "Contact created" activity names it.
func TestLaneFStateSyncCreatedContactActivityUsesSavedName(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	for _, item := range []laneFSyncItem{
		{action: "add", phone: laneFPhone(), fullName: "Saved Phone Contact", at: laneFAt(1)},
		{action: "add", userID: "US.lanef" + testutil.NewTestGraphObjectID()[:12], username: "handle_only",
			fullName: "Saved BSUID Contact", at: laneFAt(1)},
	} {
		laneFSync(t, env, item)
		var contact models.Contact
		if item.phone != "" {
			contact = laneFContactByPhone(t, env, item.phone)
		} else {
			require.NoError(t, env.app.DB.First(&contact,
				"organization_id = ? AND bs_uid = ?", env.account.OrganizationID, item.userID).Error)
		}
		laneFAssertName(t, contact, item.fullName, contactutil.ContactNameSourceWABusiness)

		var activity models.CustomerActivityEvent
		require.NoError(t, env.app.DB.Where(
			"organization_id = ? AND contact_id = ? AND title = ?",
			env.account.OrganizationID, contact.ID, "Contact created",
		).First(&activity).Error)
		assert.Equal(t, item.fullName, activity.Summary)
	}
}

// Restoring a soft-deleted contact with metadata keeps its server-owned keys
// (decided in the UPDATE, not from the unlocked read) and makes a new name
// manual.
func TestLaneFRestoreWithMetadataKeepsServerOwnedKeys(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved X", at: laneFAt(1)})
	stored := laneFContactByPhone(t, env, phone)
	laneFSetManualUnread(t, env, stored.ID)
	require.NoError(t, env.app.DB.Delete(&stored).Error)

	restore := func(body map[string]any) ContactResponse {
		req := testutil.NewJSONRequest(t, body)
		testutil.SetAuthContext(req, env.account.OrganizationID, env.admin.ID)
		require.NoError(t, env.app.CreateContact(req))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
		var response ContactResponse
		testutil.ParseEnvelopeResponse(t, req, &response)
		return response
	}

	response := restore(map[string]any{"phone_number": phone, "metadata": laneFSpoofedMetadata("restored")})
	assert.Equal(t, stored.ID, response.ID)
	restored := laneFContact(t, env, stored.ID)
	laneFAssertName(t, restored, "Saved X", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, true, restored.Metadata[contactManualUnreadKey])
	assert.Equal(t, "Saved X", restored.Metadata[contactutil.ContactWABusinessSavedNameKey])
	assert.Equal(t, laneFAt(1).Format(time.RFC3339), restored.Metadata[contactutil.ContactWABusinessSavedNameAtKey])
	assert.NotContains(t, restored.Metadata, contactutil.ContactWAProfileNameKey, "a client value is not written")
	assert.Equal(t, "restored", restored.Metadata["crm_note"])

	require.NoError(t, env.app.DB.Delete(&restored).Error)
	restore(map[string]any{"phone_number": phone, "profile_name": " Dr Ali ", "metadata": map[string]any{"crm_note": "again"}})
	restored = laneFContact(t, env, stored.ID)
	laneFAssertName(t, restored, "Dr Ali", contactutil.ContactNameSourceManual)
	assert.Equal(t, "Saved X", restored.Metadata[contactutil.ContactWABusinessSavedNameKey])
	assert.Equal(t, true, restored.Metadata[contactManualUnreadKey])
	assert.Equal(t, "again", restored.Metadata["crm_note"])
}

// A contact merge keeps the display-name provenance of whichever contact's
// name it keeps.
func TestLaneFMergeKeepsProvenanceOfKeptName(t *testing.T) {
	env := laneFNewNameEnv(t, false)
	merge := func(target, source models.Contact) models.Contact {
		req := testutil.NewJSONRequest(t, MergeContactRequest{
			SourceContactID: source.ID,
			Confirm:         true,
			IdempotencyKey:  "lanef-merge-" + uuid.NewString(),
			Reason:          "duplicate profile",
		})
		testutil.SetAuthContext(req, env.account.OrganizationID, env.admin.ID)
		testutil.SetPathParam(req, "id", target.ID.String())
		require.NoError(t, env.app.MergeContact(req))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
		return laneFContact(t, env, target.ID)
	}
	newContact := func(name string, metadata models.JSONB) models.Contact {
		contact := models.Contact{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: env.account.OrganizationID,
			PhoneNumber:    laneFPhone(),
			ProfileName:    name,
			Metadata:       metadata,
		}
		require.NoError(t, env.app.DB.Create(&contact).Error)
		return contact
	}
	manual := models.JSONB{
		contactutil.ContactNameSourceKey:          contactutil.ContactNameSourceManual,
		contactutil.ContactWABusinessSavedNameKey: "Source Saved",
	}

	merged := merge(newContact("", models.JSONB{"crm": "target"}), newContact("Dr Ali", manual))
	laneFAssertName(t, merged, "Dr Ali", contactutil.ContactNameSourceManual)
	assert.Equal(t, "Source Saved", merged.Metadata[contactutil.ContactWABusinessSavedNameKey])
	assert.Equal(t, "target", merged.Metadata["crm"])

	merged = merge(
		newContact("Profile T", models.JSONB{
			contactutil.ContactNameSourceKey:    contactutil.ContactNameSourceWAProfile,
			contactutil.ContactWAProfileNameKey: "Profile T",
		}),
		newContact("Dr Ali", manual),
	)
	laneFAssertName(t, merged, "Profile T", contactutil.ContactNameSourceWAProfile)
	assert.NotContains(t, merged.Metadata, contactutil.ContactWABusinessSavedNameKey,
		"the source's provenance does not relabel the kept name")
}

func TestLaneFAddressBookEventTime(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, timestamp := range []string{"", "  ", "abc", "0", "-5", "1.5"} {
		assert.True(t, coexistenceAddressBookEventTime(timestamp, now).IsZero(), "timestamp %q", timestamp)
	}
	assert.Equal(t, time.Unix(1_760_000_000, 0).UTC(), coexistenceAddressBookEventTime("1760000000", now))
	assert.Equal(t, now.Add(5*time.Minute), coexistenceAddressBookEventTime(
		strconv.FormatInt(now.AddDate(1, 0, 0).Unix(), 10), now), "a future time is capped")
	assert.Equal(t, now.Add(4*time.Minute), coexistenceAddressBookEventTime(
		strconv.FormatInt(now.Add(4*time.Minute).Unix(), 10), now), "small clock skew is kept")
}

// An add without a Meta time on a contact with no recorded event time applies
// but records no ordering time, so a later removal with an earlier real time
// still applies. (When a time is already recorded, an untimed add keeps it as
// the watermark; see TestLaneFSavedNameWatermarkSurvivesUntimedReAdd.)
func TestLaneFUntimedAddThenEarlierTimedRemoveApplies(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()

	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved A"})
	stored := laneFContactByPhone(t, env, phone)
	laneFAssertName(t, stored, "Saved A", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, true, stored.Metadata["coexistence_app_contact"])
	assert.NotContains(t, stored.Metadata, contactutil.ContactWABusinessSavedNameAtKey)
	assert.NotContains(t, stored.Metadata, "coexistence_app_contact_synced_at")

	laneFSync(t, env, laneFSyncItem{action: "remove", phone: phone, at: laneFAt(5)})
	stored = laneFContactByPhone(t, env, phone)
	assert.Equal(t, false, stored.Metadata["coexistence_app_contact"])
	assert.Equal(t, laneFAt(5).Format(time.RFC3339), stored.Metadata["coexistence_app_contact_removed_at"])
	assert.Equal(t, laneFAt(5).Format(time.RFC3339), stored.Metadata[contactutil.ContactWABusinessSavedNameRemovedAtKey])
	laneFAssertName(t, stored, "Saved A", contactutil.ContactNameSourceWABusiness)
	assert.True(t, env.app.buildContactResponse(&stored, env.account.OrganizationID).WABusinessSavedNameRemoved)
}

// A removal without a Meta time (missing or invalid) is still recorded, but
// it neither moves nor erases the ordering times real events recorded.
func TestLaneFUntimedRemoveKeepsRealCutoffs(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved A", at: laneFAt(10)})

	laneFSync(t, env, laneFSyncItem{action: "remove", phone: phone, timestamp: "not-a-time"})
	stored := laneFContactByPhone(t, env, phone)
	assert.Equal(t, false, stored.Metadata["coexistence_app_contact"])
	assert.NotContains(t, stored.Metadata, "coexistence_app_contact_removed_at")
	assert.Equal(t, "", stored.Metadata[contactutil.ContactWABusinessSavedNameRemovedAtKey])
	assert.Equal(t, laneFAt(10).Format(time.RFC3339), stored.Metadata[contactutil.ContactWABusinessSavedNameAtKey])
	assert.True(t, env.app.buildContactResponse(&stored, env.account.OrganizationID).WABusinessSavedNameRemoved)

	// The real add time still orders a redelivered older add...
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Older", at: laneFAt(5)})
	laneFAssertName(t, laneFContactByPhone(t, env, phone), "Saved A", contactutil.ContactNameSourceWABusiness)

	// ...and a newer one applies and clears the removal mark.
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved B", at: laneFAt(20)})
	stored = laneFContactByPhone(t, env, phone)
	laneFAssertName(t, stored, "Saved B", contactutil.ContactNameSourceWABusiness)
	assert.NotContains(t, stored.Metadata, contactutil.ContactWABusinessSavedNameRemovedAtKey)
	assert.Equal(t, true, stored.Metadata["coexistence_app_contact"])
}

// A far-future Meta time is capped at now plus five minutes, so it can hold
// back later events for no longer than that.
func TestLaneFFutureAddressBookTimeIsCapped(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	before := time.Now().UTC()
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Future", at: before.AddDate(1, 0, 0)})
	stored := laneFContactByPhone(t, env, phone)
	laneFAssertName(t, stored, "Future", contactutil.ContactNameSourceWABusiness)
	for _, key := range []string{contactutil.ContactWABusinessSavedNameAtKey, "coexistence_app_contact_synced_at"} {
		value, _ := stored.Metadata[key].(string)
		recorded, err := time.Parse(time.RFC3339, value)
		require.NoError(t, err, key)
		assert.False(t, recorded.After(time.Now().UTC().Add(5*time.Minute)), "%s is capped", key)
		assert.True(t, recorded.After(before), "%s is near now", key)
	}
}

// laneFSavedNameState runs saved-name address-book events through the same
// helpers the state-sync handler uses, without a database.
type laneFSavedNameState struct {
	displayName string
	metadata    models.JSONB
}

func (s *laneFSavedNameState) add(name string, at time.Time) {
	s.displayName = applyCoexistenceSavedContactName(s.displayName, s.metadata, name, at)
}

func (s *laneFSavedNameState) remove(at time.Time) {
	markCoexistenceSavedContactNameRemoved(s.metadata, at)
}

// An add without a Meta time clears the removal mark but must not lower the
// saved name's ordering watermark below the removal's time: a delayed add or
// remove older than that removal changes nothing, a newer add still applies,
// and same-second arrival order is kept.
func TestLaneFSavedNameWatermarkSurvivesUntimedReAdd(t *testing.T) {
	newState := func(t *testing.T) *laneFSavedNameState {
		t.Helper()
		s := &laneFSavedNameState{metadata: models.JSONB{}}
		s.add("A", laneFAt(10))
		s.remove(laneFAt(20))
		s.add("B", time.Time{})
		require.Equal(t, "B", s.displayName)
		require.NotContains(t, s.metadata, contactutil.ContactWABusinessSavedNameRemovedAtKey, "the re-add clears the badge")
		require.Equal(t, laneFAt(20).Format(time.RFC3339), s.metadata[contactutil.ContactWABusinessSavedNameAtKey],
			"the removal's time stays the watermark")
		return s
	}

	t.Run("delayed add", func(t *testing.T) {
		s := newState(t)
		s.add("Stale", laneFAt(15))
		assert.Equal(t, "B", s.displayName)
		assert.Equal(t, "B", s.metadata[contactutil.ContactWABusinessSavedNameKey])
		s.add("Newer", laneFAt(21))
		assert.Equal(t, "Newer", s.displayName)
		assert.Equal(t, laneFAt(21).Format(time.RFC3339), s.metadata[contactutil.ContactWABusinessSavedNameAtKey])
	})
	t.Run("delayed remove", func(t *testing.T) {
		s := newState(t)
		s.remove(laneFAt(15))
		assert.NotContains(t, s.metadata, contactutil.ContactWABusinessSavedNameRemovedAtKey, "no removed badge")
		s.add("Newer", laneFAt(21))
		assert.Equal(t, "Newer", s.displayName)
	})
	t.Run("same second keeps arrival order", func(t *testing.T) {
		s := newState(t)
		s.remove(laneFAt(20))
		assert.Equal(t, laneFAt(20).Format(time.RFC3339), s.metadata[contactutil.ContactWABusinessSavedNameRemovedAtKey])
		s.add("Same Second", laneFAt(20))
		assert.Equal(t, "Same Second", s.displayName)
		assert.NotContains(t, s.metadata, contactutil.ContactWABusinessSavedNameRemovedAtKey)
	})
	t.Run("manual name keeps precedence", func(t *testing.T) {
		s := &laneFSavedNameState{
			displayName: "Dr Ali",
			metadata:    models.JSONB{contactutil.ContactNameSourceKey: contactutil.ContactNameSourceManual},
		}
		s.add("A", laneFAt(10))
		s.remove(laneFAt(20))
		s.add("B", time.Time{})
		s.add("Stale", laneFAt(15))
		assert.Equal(t, "Dr Ali", s.displayName)
		assert.Equal(t, "B", s.metadata[contactutil.ContactWABusinessSavedNameKey])
		assert.Equal(t, contactutil.ContactNameSourceManual, s.metadata[contactutil.ContactNameSourceKey])
	})
	t.Run("the carried watermark keeps the future cap", func(t *testing.T) {
		now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
		capped := coexistenceAddressBookEventTime(strconv.FormatInt(now.AddDate(1, 0, 0).Unix(), 10), now)
		require.Equal(t, now.Add(5*time.Minute), capped)

		s := &laneFSavedNameState{metadata: models.JSONB{}}
		s.add("A", now)
		s.remove(capped)
		s.add("B", time.Time{})
		assert.Equal(t, "B", s.displayName)
		assert.Equal(t, capped.Format(time.RFC3339), s.metadata[contactutil.ContactWABusinessSavedNameAtKey],
			"the untimed re-add carries the capped removal time, not the raw future one")

		s.add("Stale", now.Add(4*time.Minute))
		assert.Equal(t, "B", s.displayName)
		s.add("Later", now.Add(6*time.Minute))
		assert.Equal(t, "Later", s.displayName, "an event after the cap applies")
	})
	t.Run("untimed add without a prior time records none", func(t *testing.T) {
		s := &laneFSavedNameState{metadata: models.JSONB{}}
		s.add("A", time.Time{})
		assert.Equal(t, "A", s.displayName)
		assert.NotContains(t, s.metadata, contactutil.ContactWABusinessSavedNameAtKey)
	})
}

// The same sequences end to end through smb_app_state_sync: add A at t10,
// remove at t20, untimed add of B, then a delayed add at t15. The saved name
// and the app-contact flag agree, and an add at t21 still applies.
func TestLaneFUntimedReAddKeepsRemovalCutoffAgainstDelayedAdd(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved A", at: laneFAt(10)})
	laneFSync(t, env, laneFSyncItem{action: "remove", phone: phone, at: laneFAt(20)})
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved B"})
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Stale", at: laneFAt(15)})

	stored := laneFContactByPhone(t, env, phone)
	laneFAssertName(t, stored, "Saved B", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, "Saved B", stored.Metadata[contactutil.ContactWABusinessSavedNameKey])
	assert.NotContains(t, stored.Metadata, contactutil.ContactWABusinessSavedNameRemovedAtKey)
	assert.Equal(t, true, stored.Metadata["coexistence_app_contact"])
	assert.False(t, env.app.buildContactResponse(&stored, env.account.OrganizationID).WABusinessSavedNameRemoved)

	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved C", at: laneFAt(21)})
	stored = laneFContactByPhone(t, env, phone)
	laneFAssertName(t, stored, "Saved C", contactutil.ContactNameSourceWABusiness)
	assert.Equal(t, true, stored.Metadata["coexistence_app_contact"])
}

// As above with a delayed remove at t15: no removed badge appears and the
// app-contact flag stays set; an add at t21 still applies.
func TestLaneFUntimedReAddKeepsRemovalCutoffAgainstDelayedRemove(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved A", at: laneFAt(10)})
	laneFSync(t, env, laneFSyncItem{action: "remove", phone: phone, at: laneFAt(20)})
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved B"})
	laneFSync(t, env, laneFSyncItem{action: "remove", phone: phone, at: laneFAt(15)})

	stored := laneFContactByPhone(t, env, phone)
	laneFAssertName(t, stored, "Saved B", contactutil.ContactNameSourceWABusiness)
	assert.NotContains(t, stored.Metadata, contactutil.ContactWABusinessSavedNameRemovedAtKey)
	assert.Equal(t, true, stored.Metadata["coexistence_app_contact"])
	assert.False(t, env.app.buildContactResponse(&stored, env.account.OrganizationID).WABusinessSavedNameRemoved)

	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved C", at: laneFAt(21)})
	laneFAssertName(t, laneFContactByPhone(t, env, phone), "Saved C", contactutil.ContactNameSourceWABusiness)
}
