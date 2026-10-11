package database_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const compatFixtureInputLimit = 16 * 1024
const compatFixtureResultPrefix = "COMPAT_FIXTURE_JSON:"

// This is a private, bounded stdin protocol for an explicitly invoked test
// binary, not a release command. The controller generates every credential.
// No credential, URL, SQL, driver error or private input is printed or saved.
// Fixed compat names are for OwnedProcesses; namespace-derived names let real
// tests use disjoint disposable databases and roles in the same local cluster.
type compatFixtureInput struct {
	Version           int    `json:"version"`
	Namespace         string `json:"namespace"`
	OwnerDSN          string `json:"owner_dsn"`
	RuntimeDSN        string `json:"runtime_dsn"`
	OrganizationID    string `json:"organization_id"`
	Password          string `json:"password"`
	EncryptionKey     string `json:"encryption_key"`
	GraphAccessToken  string `json:"graph_access_token"`
	PhoneNumberID     string `json:"phone_number_id"`
	BusinessAccountID string `json:"business_account_id"`
}

type compatFixtureResult struct {
	Version           int    `json:"version"`
	Namespace         string `json:"namespace"`
	OrganizationID    string `json:"organization_id"`
	UserID            string `json:"user_id"`
	AccountID         string `json:"account_id"`
	ChannelAccountID  string `json:"channel_account_id"`
	AccountName       string `json:"account_name"`
	Email             string `json:"email"`
	PhoneNumberID     string `json:"phone_number_id"`
	BusinessAccountID string `json:"business_account_id"`
	CatalogSHA256     string `json:"catalog_sha256"`
}

var compatFixtureNamespace = regexp.MustCompile(`^rereply-compat-[0-9a-f]{12}$`)
var compatFixtureSecret = regexp.MustCompile(`^[0-9a-f]{64}$`)
var compatFixtureGraphID = regexp.MustCompile(`^999[0-9]{12}$`)

func compatFixtureRefusal(code string) error { return errors.New("compat-fixture:" + code) }

func decodeCompatFixtureInput(reader io.Reader) (compatFixtureInput, error) {
	var input compatFixtureInput
	raw, err := io.ReadAll(io.LimitReader(reader, compatFixtureInputLimit+1))
	if err != nil || len(raw) == 0 || len(raw) > compatFixtureInputLimit {
		return input, compatFixtureRefusal("input")
	}
	// Reject duplicate top-level keys before the typed decoder, including a
	// repeated key whose final value happens to be valid. There are no nested
	// input objects; the typed decoder rejects them, nulls and nonfinite values.
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return input, compatFixtureRefusal("input")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return input, compatFixtureRefusal("input")
		}
		seen[key] = true
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return input, compatFixtureRefusal("input")
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') || len(seen) != 10 {
		return input, compatFixtureRefusal("input")
	}
	if d.Decode(new(any)) != io.EOF {
		return input, compatFixtureRefusal("input")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil {
		return input, compatFixtureRefusal("input")
	}
	if _, _, err = compatFixtureURLs(input); err != nil {
		return compatFixtureInput{}, err
	}
	return input, nil
}

func compatFixtureURLs(input compatFixtureInput) (*url.URL, *url.URL, error) {
	refuse := func() (*url.URL, *url.URL, error) { return nil, nil, compatFixtureRefusal("input") }
	organization, err := uuid.Parse(input.OrganizationID)
	if input.Version != 1 || !compatFixtureNamespace.MatchString(input.Namespace) || err != nil ||
		organization == uuid.Nil || organization.String() != input.OrganizationID || organization.Version() != 4 ||
		!compatFixtureSecret.MatchString(input.Password) || !compatFixtureSecret.MatchString(input.EncryptionKey) ||
		!compatFixtureSecret.MatchString(input.GraphAccessToken) || !compatFixtureGraphID.MatchString(input.PhoneNumberID) ||
		!compatFixtureGraphID.MatchString(input.BusinessAccountID) || input.PhoneNumberID == input.BusinessAccountID {
		return refuse()
	}
	owner, err := catalogTestURL(input.OwnerDSN)
	if err != nil {
		return refuse()
	}
	runtime, err := catalogTestURL(input.RuntimeDSN)
	if err != nil || owner.Host != runtime.Host || owner.Path != runtime.Path || owner.Scheme != runtime.Scheme ||
		owner.Fragment != "" || runtime.Fragment != "" || owner.Opaque != "" || runtime.Opaque != "" {
		return refuse()
	}
	token := strings.TrimPrefix(input.Namespace, "rereply-compat-")
	dbName, ownerName, runtimeName := "compat", "compat_owner", "compat_runtime"
	if owner.Path != "/compat" {
		dbName, ownerName, runtimeName = "compat_fixture_"+token, "compat_fixture_owner_"+token, "compat_fixture_runtime_"+token
	}
	if owner.Path != "/"+dbName || owner.User.Username() != ownerName || runtime.User.Username() != runtimeName {
		return refuse()
	}
	for _, u := range []*url.URL{owner, runtime} {
		password, present := u.User.Password()
		if !present || !compatFixtureSecret.MatchString(password) || u.Query().Get("sslmode") != "disable" ||
			u.Query().Get("default_transaction_read_only") != "" {
			return refuse()
		}
	}
	return owner, runtime, nil
}

func compatFixtureAuthority(owner, runtime *gorm.DB, ownerName, runtimeName, dbName string) error {
	for _, check := range []struct {
		db   *gorm.DB
		name string
	}{{owner, ownerName}, {runtime, runtimeName}} {
		var safe bool
		err := check.db.Raw(`SELECT current_user=? AND session_user=current_user AND current_database()=?
 AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls
 AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles p
  WHERE (p.rolsuper OR p.rolcreatedb OR p.rolcreaterole OR p.rolreplication OR p.rolbypassrls)
   AND pg_catalog.pg_has_role(r.oid,p.oid,'MEMBER'))
 FROM pg_catalog.pg_roles r WHERE r.rolname=current_user`, check.name, dbName).Scan(&safe).Error
		if err != nil || !safe {
			return compatFixtureRefusal("authority")
		}
	}
	// The existing production-profile bootstrap checks database/schema ownership,
	// no runtime membership in the owner, and full public namespace emptiness.
	return nil
}

func compatFixtureCapture(ctx context.Context, runtimeDSN string) (dbcatalog.Snapshot, error) {
	known, err := dbcatalog.BuiltinCatalog()
	if err != nil {
		return dbcatalog.Snapshot{}, compatFixtureRefusal("catalog")
	}
	globals, err := dbcatalog.BuiltinGlobalTables()
	if err != nil {
		return dbcatalog.Snapshot{}, compatFixtureRefusal("catalog")
	}
	return dbcatalog.Capture(ctx, runtimeDSN, dbcatalog.Options{Catalog: known, GlobalTables: globals, IncludeSeeds: true, Timeout: 30 * time.Second})
}

// provisionCompatFixture does not create roles/databases, contact the Graph
// stub, start servers, or adopt an existing bootstrap. Failed partial targets
// are retained for the owning controller to inspect/clean, never auto-retried.
func provisionCompatFixture(input compatFixtureInput) (result compatFixtureResult, err error) {
	ownerURL, runtimeURL, err := compatFixtureURLs(input)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	owner, err := catalogConnect(ownerURL)
	if err != nil {
		return result, compatFixtureRefusal("connect")
	}
	defer func() {
		if catalogClose(owner) != nil {
			err = compatFixtureRefusal("close")
		}
	}()
	runtime, err := catalogConnect(runtimeURL)
	if err != nil {
		return result, compatFixtureRefusal("connect")
	}
	defer func() {
		if catalogClose(runtime) != nil {
			err = compatFixtureRefusal("close")
		}
	}()
	owner, runtime = owner.WithContext(ctx), runtime.WithContext(ctx)
	if err = compatFixtureAuthority(owner, runtime, ownerURL.User.Username(), runtimeURL.User.Username(), strings.TrimPrefix(ownerURL.Path, "/")); err != nil {
		return result, err
	}
	if BootstrapProductionShapeEmptyForTest(owner, runtime, runtimeURL.User.Username()) != nil || applyProductionOverlayForTest(owner) != nil {
		return result, compatFixtureRefusal("bootstrap")
	}
	profile, err := loadProductionProfile()
	if err != nil {
		return result, compatFixtureRefusal("catalog")
	}
	before, err := compatFixtureCapture(ctx, runtimeURL.String())
	if err != nil || before.SHA256 != profile.LiveSHA256 || len(before.Unknown) != 0 {
		return result, compatFixtureRefusal("catalog")
	}
	organizationID := uuid.MustParse(input.OrganizationID)
	result = compatFixtureResult{Version: 1, Namespace: input.Namespace, OrganizationID: input.OrganizationID,
		AccountName: input.Namespace + "-wa", Email: input.Namespace + "-client@example.test",
		PhoneNumberID: input.PhoneNumberID, BusinessAccountID: input.BusinessAccountID, CatalogSHA256: before.SHA256}
	hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	if err != nil {
		return compatFixtureResult{}, compatFixtureRefusal("fixture")
	}
	accessToken, err := crypto.Encrypt(input.GraphAccessToken, input.EncryptionKey)
	if err != nil || !crypto.IsEncrypted(accessToken) {
		return compatFixtureResult{}, compatFixtureRefusal("fixture")
	}
	err = database.WithTenant(owner, organizationID, func(tx *gorm.DB) error {
		var reseller models.Reseller
		if err := tx.Where("slug = ?", database.PlatformResellerSlug).First(&reseller).Error; err != nil {
			return err
		}
		org := models.Organization{BaseModel: models.BaseModel{ID: organizationID}, ResellerID: &reseller.ID,
			Name: input.Namespace, Slug: input.Namespace, Settings: models.JSONB{}}
		if err := tx.Create(&org).Error; err != nil {
			return err
		}
		if err := database.LockOrganizationPolicyScope(tx, organizationID); err != nil {
			return err
		}
		if err := database.SeedSystemRolesForOrg(tx, organizationID); err != nil {
			return err
		}
		var role models.CustomRole
		if err := tx.Where("organization_id = ? AND name = ? AND is_system = ?", organizationID, "admin", true).First(&role).Error; err != nil {
			return err
		}
		// Same model/association contract as testutil.CreateTestUser; avoid that
		// helper's Fatal/driver-error output on the private export entrypoint.
		user := models.User{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationID,
			Email: result.Email, PasswordHash: string(hash), FullName: input.Namespace, RoleID: &role.ID,
			IsActive: true, IsAvailable: true, IsSuperAdmin: false, Settings: models.JSONB{}}
		membership := models.UserOrganization{BaseModel: models.BaseModel{ID: uuid.New()}, UserID: user.ID,
			OrganizationID: organizationID, RoleID: &role.ID, IsDefault: true, Source: "direct"}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		if err := tx.Create(&membership).Error; err != nil {
			return err
		}
		result.UserID = user.ID.String()
		// Match the existing enableLegacyReplyOmnichannel fixture's real durable
		// entitlement models, without an override or a fabricated gate result.
		plan := models.Plan{BaseModel: models.BaseModel{ID: uuid.New()}, ScopeKey: "platform", Code: input.Namespace,
			Name: input.Namespace, Status: models.CommercialPlanStatusActive, Vertical: "general", Metadata: models.JSONB{}, CreatedByID: &user.ID}
		billing := models.BillingAccount{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationID,
			Provider: models.BillingProviderManual, Status: models.BillingAccountStatusActive, DefaultCurrency: "MYR",
			BillingProfile: models.JSONB{}, ProviderData: models.JSONB{}, Metadata: models.JSONB{}}
		start, end := time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(7*24*time.Hour)
		subscription := models.Subscription{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationID,
			BillingAccountID: billing.ID, PlanID: plan.ID, Provider: models.BillingProviderManual,
			Status: models.SubscriptionStatusActive, Quantity: 1, CollectionMethod: "send_invoice",
			EntitlementsSnapshot: models.JSONB{channel.OmnichannelEntitlementKey: true}, ProviderData: models.JSONB{},
			CurrentPeriodStart: &start, CurrentPeriodEnd: &end, CreatedByID: &user.ID}
		for _, value := range []any{&plan, &billing, &subscription} {
			if err := tx.Create(value).Error; err != nil {
				return err
			}
		}
		// OwnedProcesses supplies the owned Graph app ID/secret through its fixed
		// app config. No workspace override/ChannelCredential is necessary; the
		// normal legacy Meta resolver uses that explicitly configured fallback.
		account := models.WhatsAppAccount{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationID,
			Name: result.AccountName, PhoneID: input.PhoneNumberID, BusinessID: input.BusinessAccountID,
			AccessToken: accessToken, APIVersion: "v25.0", Status: "active", IsDefaultIncoming: true, IsDefaultOutgoing: true,
			CreatedByID: &user.ID}
		if err := tx.Create(&account).Error; err != nil {
			return err
		}
		shadow, err := channel.EnsureLegacyMetaWhatsAppAccount(tx, channel.LegacyMetaAccountRef{ID: account.ID,
			OrganizationID: organizationID, Name: account.Name, Status: account.Status, PhoneID: account.PhoneID, BusinessID: account.BusinessID})
		if err != nil {
			return err
		}
		result.AccountID, result.ChannelAccountID = account.ID.String(), shadow.ID.String()
		settings := models.ChatbotSettings{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationID, WhatsAppAccount: "",
			IsEnabled: false, AI: models.AIConfig{Enabled: false}}
		return tx.Create(&settings).Error
	})
	if err != nil {
		return compatFixtureResult{}, compatFixtureRefusal("fixture")
	}
	if database.VerifyTenantRLS(runtime, runtimeURL.User.Username()) != nil {
		return compatFixtureResult{}, compatFixtureRefusal("runtime")
	}
	after, err := compatFixtureCapture(ctx, runtimeURL.String())
	beforeSeeds, _ := json.Marshal(before.Seeds)
	afterSeeds, _ := json.Marshal(after.Seeds)
	if err != nil || after.SHA256 != before.SHA256 || len(after.Unknown) != 0 || !bytes.Equal(beforeSeeds, afterSeeds) {
		return compatFixtureResult{}, compatFixtureRefusal("catalog")
	}
	return result, nil
}

// TestCompatFixtureExport is invoked only by an owning synthetic controller,
// before compatibility measurement. It shares the inspected PG container's
// network namespace so localhost remains literal; no service-alias exception.
// The caller owns database/roles, temporary process, timeout and cleanup. It
// must separately register the matching Graph account, configure legacy reply
// for this org, and prove actual login/API sweeps. This is no onboarding proof.
func TestCompatFixtureExport(t *testing.T) {
	if os.Getenv("COMPAT_FIXTURE_EXPORT") != "1" {
		t.Skip("COMPAT_FIXTURE_EXPORT not set")
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(key), "PG") {
			t.Fatal("synthetic compatibility fixture ambient PostgreSQL options refused")
		}
	}
	input, err := decodeCompatFixtureInput(os.Stdin)
	if err != nil {
		t.Fatal("synthetic compatibility fixture input refused")
	}
	result, err := provisionCompatFixture(input)
	if err != nil {
		t.Fatal("synthetic compatibility fixture export refused or failed")
	}
	raw, err := json.Marshal(result)
	if err != nil || len(raw) > 4096 {
		t.Fatal("synthetic compatibility fixture result refused")
	}
	fmt.Println(compatFixtureResultPrefix + string(raw))
}

func compatFixtureSyntheticInput() compatFixtureInput {
	token := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	owner := &url.URL{Scheme: "postgres", Host: "127.0.0.1:5432", Path: "/compat_fixture_" + token,
		User: url.UserPassword("compat_fixture_owner_"+token, strings.Repeat("a", 64)), RawQuery: "sslmode=disable"}
	runtime := *owner
	runtime.User = url.UserPassword("compat_fixture_runtime_"+token, strings.Repeat("b", 64))
	return compatFixtureInput{Version: 1, Namespace: "rereply-compat-" + token, OwnerDSN: owner.String(), RuntimeDSN: runtime.String(),
		OrganizationID: uuid.NewString(), Password: strings.Repeat("c", 64), EncryptionKey: strings.Repeat("d", 64),
		GraphAccessToken: strings.Repeat("e", 64), PhoneNumberID: "999000000000001", BusinessAccountID: "999000000000002"}
}

func TestCompatFixtureInputContract(t *testing.T) {
	input := compatFixtureSyntheticInput()
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	decoded, err := decodeCompatFixtureInput(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, input, decoded)
	for name, mutation := range map[string]func(compatFixtureInput) compatFixtureInput{
		"remote": func(v compatFixtureInput) compatFixtureInput {
			v.OwnerDSN = strings.Replace(v.OwnerDSN, "127.0.0.1", "provider.invalid", 1)
			return v
		},
		"query_override":   func(v compatFixtureInput) compatFixtureInput { v.RuntimeDSN += "&host=provider.invalid"; return v },
		"duplicate_option": func(v compatFixtureInput) compatFixtureInput { v.OwnerDSN += "&sslmode=require"; return v },
		"different_database": func(v compatFixtureInput) compatFixtureInput {
			v.RuntimeDSN = strings.Replace(v.RuntimeDSN, "/compat_fixture_", "/other_", 1)
			return v
		},
		"different_port": func(v compatFixtureInput) compatFixtureInput {
			v.RuntimeDSN = strings.Replace(v.RuntimeDSN, ":5432", ":6543", 1)
			return v
		},
		"wrong_owner": func(v compatFixtureInput) compatFixtureInput {
			v.OwnerDSN = strings.Replace(v.OwnerDSN, "compat_fixture_owner_", "postgres_", 1)
			return v
		},
		"namespace":          func(v compatFixtureInput) compatFixtureInput { v.Namespace = "customer"; return v },
		"nil_org":            func(v compatFixtureInput) compatFixtureInput { v.OrganizationID = uuid.Nil.String(); return v },
		"password":           func(v compatFixtureInput) compatFixtureInput { v.Password = "customer-password"; return v },
		"empty_key":          func(v compatFixtureInput) compatFixtureInput { v.EncryptionKey = ""; return v },
		"nonsynthetic_phone": func(v compatFixtureInput) compatFixtureInput { v.PhoneNumberID = "60123456789"; return v },
		"same_graph_ids":     func(v compatFixtureInput) compatFixtureInput { v.BusinessAccountID = v.PhoneNumberID; return v },
	} {
		t.Run(name, func(t *testing.T) {
			bad, err := json.Marshal(mutation(input))
			require.NoError(t, err)
			_, err = decodeCompatFixtureInput(bytes.NewReader(bad))
			require.EqualError(t, err, "compat-fixture:input")
		})
	}
	for name, bad := range map[string][]byte{
		"duplicate": append([]byte(`{"version":1,`), raw[1:]...),
		"unknown":   append([]byte(`{"unknown":1,`), raw[1:]...),
		"trailing":  append(append([]byte{}, raw...), []byte(`{}`)...),
		"null":      bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":null`), 1),
		"nonfinite": bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":NaN`), 1),
		"oversized": bytes.Repeat([]byte(" "), compatFixtureInputLimit+1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeCompatFixtureInput(bytes.NewReader(bad))
			require.EqualError(t, err, "compat-fixture:input")
		})
	}
	// Exercise the exact fixed production names without any database call.
	input.OwnerDSN = "postgres://compat_owner:" + strings.Repeat("a", 64) + "@127.0.0.1:5432/compat?sslmode=disable"
	input.RuntimeDSN = "postgres://compat_runtime:" + strings.Repeat("b", 64) + "@127.0.0.1:5432/compat?sslmode=disable"
	_, _, err = compatFixtureURLs(input)
	require.NoError(t, err)
}

// This test owns only its unique namespace-derived DB/roles. The optional
// cluster URL is a disposable loopback administrator supplied by the caller.
func TestCompatFixtureExportRealPostgres(t *testing.T) {
	rawBase := os.Getenv("TEST_DATABASE_URL")
	if rawBase == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	base, err := catalogTestURL(rawBase)
	require.NoError(t, err)
	admin, err := catalogConnect(base)
	require.NoError(t, err)
	input := compatFixtureSyntheticInput()
	ownerURL, runtimeURL, err := compatFixtureURLs(input)
	require.NoError(t, err)
	ownerURL.Host, runtimeURL.Host = base.Host, base.Host
	input.OwnerDSN, input.RuntimeDSN = ownerURL.String(), runtimeURL.String()
	ownerName, runtimeName, dbName := ownerURL.User.Username(), runtimeURL.User.Username(), strings.TrimPrefix(ownerURL.Path, "/")
	createdOwner, createdRuntime, createdDB := false, false, false
	t.Cleanup(func() {
		if createdDB {
			require.NoError(t, admin.Exec("DROP DATABASE "+dbName+" WITH (FORCE)").Error)
		}
		if createdRuntime {
			require.NoError(t, admin.Exec("DROP ROLE "+runtimeName).Error)
		}
		if createdOwner {
			require.NoError(t, admin.Exec("DROP ROLE "+ownerName).Error)
		}
		require.NoError(t, catalogClose(admin))
	})
	for _, role := range []struct {
		name, password string
		created        *bool
	}{
		{ownerName, strings.Repeat("a", 64), &createdOwner}, {runtimeName, strings.Repeat("b", 64), &createdRuntime},
	} {
		require.NoError(t, admin.Exec("CREATE ROLE "+role.name+" LOGIN PASSWORD '"+role.password+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION").Error)
		*role.created = true
	}
	require.NoError(t, admin.Exec("CREATE DATABASE "+dbName+" OWNER "+ownerName+" TEMPLATE template0").Error)
	createdDB = true
	adminURL := *base
	adminURL.Path, adminURL.RawPath = "/"+dbName, ""
	dbAdmin, err := catalogConnect(&adminURL)
	require.NoError(t, err)
	require.NoError(t, dbAdmin.Exec("ALTER SCHEMA public OWNER TO "+ownerName).Error)
	t.Cleanup(func() { require.NoError(t, catalogClose(dbAdmin)) })
	countPublic := func() int64 {
		var count int64
		require.NoError(t, dbAdmin.Raw("SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'").Scan(&count).Error)
		return count
	}
	// Authority and nonempty refusals precede any profile/bootstrap mutation.
	for _, roleName := range []string{ownerName, runtimeName} {
		require.NoError(t, admin.Exec("ALTER ROLE "+roleName+" BYPASSRLS").Error)
		_, err = provisionCompatFixture(input)
		require.EqualError(t, err, "compat-fixture:authority")
		require.Zero(t, countPublic())
		require.NoError(t, admin.Exec("ALTER ROLE "+roleName+" NOBYPASSRLS").Error)
	}
	require.NoError(t, dbAdmin.Exec("CREATE TABLE public.fixture_nonempty(id integer)").Error)
	_, err = provisionCompatFixture(input)
	require.EqualError(t, err, "compat-fixture:bootstrap")
	require.EqualValues(t, 1, countPublic())
	require.NoError(t, dbAdmin.Exec("DROP TABLE public.fixture_nonempty").Error)
	inputBytes, err := json.Marshal(input)
	require.NoError(t, err)
	runExport := func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		binary, err := os.Executable()
		if err != nil {
			return nil, err
		}
		command := exec.CommandContext(ctx, binary, "-test.run=^TestCompatFixtureExport$", "-test.v", "-test.timeout=4m")
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			upper := strings.ToUpper(key)
			if !strings.HasPrefix(upper, "COMPAT_FIXTURE_") && !strings.HasPrefix(upper, "PG") && !strings.HasPrefix(upper, "WHATOMATE_") {
				command.Env = append(command.Env, entry)
			}
		}
		command.Env = append(command.Env, "COMPAT_FIXTURE_EXPORT=1")
		command.Stdin = bytes.NewReader(inputBytes)
		return command.CombinedOutput()
	}
	output, err := runExport()
	require.NoError(t, err, "synthetic fixture child failed; output withheld")
	for _, secret := range []string{input.Password, input.EncryptionKey, input.GraphAccessToken, strings.Repeat("a", 64), strings.Repeat("b", 64), input.OwnerDSN, input.RuntimeDSN} {
		require.False(t, bytes.Contains(output, []byte(secret)), "fixture output must not contain input secrets")
	}
	var result compatFixtureResult
	matches := 0
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, compatFixtureResultPrefix) {
			matches++
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, compatFixtureResultPrefix)), &result))
		}
	}
	require.Equal(t, 1, matches)
	require.Equal(t, input.Namespace, result.Namespace)
	profile, err := loadProductionProfile()
	require.NoError(t, err)
	require.Equal(t, profile.LiveSHA256, result.CatalogSHA256)
	runtime, err := catalogConnect(runtimeURL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalogClose(runtime)) })
	require.NoError(t, database.VerifyTenantRLS(runtime, runtimeName))
	org := uuid.MustParse(input.OrganizationID)
	require.NoError(t, database.WithTenant(runtime, org, func(tx *gorm.DB) error {
		var user models.User
		require.NoError(t, tx.Where("id = ?", result.UserID).First(&user).Error)
		require.Equal(t, result.Email, user.Email)
		require.Equal(t, org, user.OrganizationID)
		require.True(t, user.IsActive)
		require.False(t, user.IsSuperAdmin)
		require.NoError(t, bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(input.Password)))
		var membership models.UserOrganization
		require.NoError(t, tx.Where("user_id = ? AND organization_id = ?", user.ID, org).First(&membership).Error)
		require.True(t, membership.IsDefault)
		require.Equal(t, user.RoleID, membership.RoleID)
		var native models.WhatsAppAccount
		require.NoError(t, tx.Where("id = ?", result.AccountID).First(&native).Error)
		require.True(t, crypto.IsEncrypted(native.AccessToken))
		clear, err := crypto.Decrypt(native.AccessToken, input.EncryptionKey)
		require.NoError(t, err)
		require.True(t, clear == input.GraphAccessToken, "encrypted account token binding")
		var shadow models.ChannelAccount
		require.NoError(t, tx.Where("id = ?", result.ChannelAccountID).First(&shadow).Error)
		require.Equal(t, channel.LegacyMetaProvider, shadow.Provider)
		require.Equal(t, "legacy-account:"+native.ID.String(), shadow.ExternalAccountID)
		require.Equal(t, models.ChannelAccountStatusActive, shadow.Status)
		bound, err := channel.LegacyMetaWhatsAppAccountID(&shadow)
		require.NoError(t, err)
		require.Equal(t, native.ID, bound)
		allowed, err := channel.HasDurableOmnichannelEntitlement(tx, org, time.Now().UTC())
		require.NoError(t, err)
		require.True(t, allowed)
		for _, table := range []string{"contacts", "contact_identities", "messages", "message_parts", "inbox_conversations", "inbound_events", "outbox_jobs", "templates", "chatbot_flows", "channel_credentials", "provider_integrations"} {
			var count int64
			require.NoError(t, tx.Table(table).Where("organization_id = ?", org).Count(&count).Error)
			require.Zero(t, count, "Sweep-owned rows and unnecessary credentials are absent")
		}
		var settings models.ChatbotSettings
		require.NoError(t, tx.Where("organization_id = ? AND whats_app_account = ?", org, "").First(&settings).Error)
		require.False(t, settings.IsEnabled)
		require.False(t, settings.AI.Enabled)
		return nil
	}))
	var visible int64
	require.NoError(t, runtime.Model(&models.WhatsAppAccount{}).Count(&visible).Error)
	require.Zero(t, visible)
	require.ErrorContains(t, database.WithTenant(runtime, uuid.New(), func(tx *gorm.DB) error {
		require.NoError(t, tx.Model(&models.WhatsAppAccount{}).Count(&visible).Error)
		require.Zero(t, visible)
		forged := models.WhatsAppAccount{OrganizationID: org, Name: "forged", PhoneID: input.PhoneNumberID, AccessToken: "synthetic"}
		require.Error(t, tx.Create(&forged).Error)
		return errors.New("rollback deliberately refused foreign insert")
	}), "rollback deliberately refused foreign insert")
	before, err := compatFixtureCapture(context.Background(), runtimeURL.String())
	require.NoError(t, err)
	output, err = runExport()
	require.Error(t, err)
	require.Contains(t, string(output), "synthetic compatibility fixture export refused or failed")
	after, err := compatFixtureCapture(context.Background(), runtimeURL.String())
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoError(t, database.VerifyTenantRLS(runtime, runtimeName))
	var users, accounts int64
	require.NoError(t, dbAdmin.Model(&models.User{}).Where("organization_id = ?", org).Count(&users).Error)
	require.NoError(t, dbAdmin.Model(&models.WhatsAppAccount{}).Where("organization_id = ?", org).Count(&accounts).Error)
	require.EqualValues(t, 1, users)
	require.EqualValues(t, 1, accounts)
	t.Log("synthetic fixture: exact production profile, non-super login, durable entitlement, encrypted native account, genuine shadow, no Sweep rows, RLS isolation, repeat refusal")
}
