package handlers

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// These tests need no database: they cover the guard paths of the
// membership-aware agent lookup and the SQL it generates (GORM dry run).

func dryRunPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.New(postgres.Config{
		DSN:                  "host=127.0.0.1 port=1 dbname=dryrun sslmode=disable",
		PreferSimpleProtocol: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	require.NoError(t, err)
	return db
}

func TestOrgMemberUserQuery_SQL(t *testing.T) {
	db := dryRunPostgres(t)
	userID, orgID := uuid.New(), uuid.New()

	for _, activeOnly := range []bool{true, false} {
		sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
			var user models.User
			return orgMemberUserQuery(tx, userID, orgID, activeOnly).First(&user)
		})
		normalized := strings.Join(strings.Fields(sql), " ")

		assert.Contains(t, normalized, "users.id = '"+userID.String()+"'")
		assert.Contains(t, normalized, "users.organization_id = '"+orgID.String()+"' OR EXISTS")
		assert.Contains(t, normalized, "uo.user_id = users.id AND uo.organization_id = '"+orgID.String()+"' AND uo.deleted_at IS NULL")
		assert.Contains(t, normalized, `"users"."deleted_at" IS NULL`, "soft-deleted users must stay excluded")
		assert.NotContains(t, normalized, "JOIN", "EXISTS keeps one row per user")
		if activeOnly {
			assert.Contains(t, normalized, "users.is_active = true")
		} else {
			assert.NotContains(t, normalized, "is_active")
		}
	}
}

func TestLookupOrgMemberUser_GuardsWithoutQuery(t *testing.T) {
	id := uuid.New()

	_, err := lookupOrgMemberUser(nil, id, id, true)
	require.ErrorIs(t, err, gorm.ErrInvalidDB)
	db := dryRunPostgres(t)
	_, err = lookupOrgMemberUser(db, uuid.Nil, id, true)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	_, err = lookupOrgMemberUser(db, id, uuid.Nil, true)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestOrgMemberMembershipValid_FailsClosed(t *testing.T) {
	orgID := uuid.New()
	homeUser := &models.User{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID}

	require.ErrorIs(t, checkOrgMemberMembership(dryRunPostgres(t), nil, orgID), gorm.ErrRecordNotFound)
	require.ErrorIs(t, checkOrgMemberMembership(nil, homeUser, orgID), gorm.ErrInvalidDB)
	require.ErrorIs(t, checkOrgMemberMembership(dryRunPostgres(t), homeUser, uuid.Nil), gorm.ErrRecordNotFound)
	failing := dryRunPostgres(t)
	failing.Error = errors.New("membership query failed")
	require.ErrorIs(t, checkOrgMemberMembership(failing, homeUser, orgID), failing.Error)

}

func TestOrgMemberMembershipValid_HomeOrgFallbackWithoutAnyRow(t *testing.T) {
	// A dry-run query returns no rows: the user never had a membership row,
	// so only the legacy home organization counts.
	db := dryRunPostgres(t)
	orgID := uuid.New()

	homeUser := &models.User{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID}
	assert.NoError(t, checkOrgMemberMembership(db, homeUser, orgID))

	otherUser := &models.User{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: uuid.New()}
	assert.ErrorIs(t, checkOrgMemberMembership(db, otherUser, orgID), gorm.ErrRecordNotFound)

	// No super-admin bypass.
	superAdmin := &models.User{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: uuid.New(), IsSuperAdmin: true}
	assert.ErrorIs(t, checkOrgMemberMembership(db, superAdmin, orgID), gorm.ErrRecordNotFound)
}

func TestOrgMembershipRowsValid(t *testing.T) {
	db := dryRunPostgres(t)
	orgID := uuid.New()
	homeUser := &models.User{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID}
	crossUser := &models.User{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: uuid.New()}
	deleted := gorm.DeletedAt{Time: time.Now(), Valid: true}
	resellerMemberID := uuid.New()

	row := func(user *models.User, source string, deletedAt gorm.DeletedAt) models.UserOrganization {
		r := models.UserOrganization{UserID: user.ID, OrganizationID: orgID, Source: source}
		r.ID = uuid.New()
		r.DeletedAt = deletedAt
		if source == models.MembershipSourceReseller {
			r.ResellerMemberID = &resellerMemberID
		}
		return r
	}

	cases := []struct {
		name string
		user *models.User
		rows []models.UserOrganization
		want bool
	}{
		{"no row, home org", homeUser, nil, true},
		{"no row, other home org", crossUser, nil, false},
		{"live direct row", crossUser, []models.UserOrganization{row(crossUser, models.MembershipSourceDirect, gorm.DeletedAt{})}, true},
		{"live legacy row without source", crossUser, []models.UserOrganization{row(crossUser, "", gorm.DeletedAt{})}, true},
		{"removed membership in home org", homeUser, []models.UserOrganization{row(homeUser, models.MembershipSourceDirect, deleted)}, false},
		{"removed cross-org membership", crossUser, []models.UserOrganization{row(crossUser, models.MembershipSourceDirect, deleted)}, false},
		{"removed then re-added", crossUser, []models.UserOrganization{
			row(crossUser, models.MembershipSourceDirect, deleted),
			row(crossUser, models.MembershipSourceDirect, gorm.DeletedAt{}),
		}, true},
		// The dry-run reseller lookup counts nothing, like a suspended reseller.
		{"live reseller row, reseller inactive", crossUser, []models.UserOrganization{row(crossUser, models.MembershipSourceReseller, gorm.DeletedAt{})}, false},
		{"unknown source", crossUser, []models.UserOrganization{row(crossUser, "imported", gorm.DeletedAt{})}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			valid, err := orgMembershipRowsValid(db, tc.user, orgID, tc.rows)
			require.NoError(t, err)
			assert.Equal(t, tc.want, valid)
		})
	}

	valid, err := orgMembershipRowsValid(db, nil, orgID, nil)
	require.NoError(t, err)
	assert.False(t, valid, "nil user")
}

func TestLoadOrgMembershipRows_IncludesSoftDeletedRows(t *testing.T) {
	db := dryRunPostgres(t)
	orgID, userID := uuid.New(), uuid.New()

	byUser, err := loadOrgMembershipRows(db, nil, orgID)
	require.NoError(t, err)
	assert.Empty(t, byUser, "no users means no query")

	var captured []string
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:capture_sql", func(tx *gorm.DB) {
		captured = append(captured, tx.Statement.SQL.String())
	}))

	byUser, err = loadOrgMembershipRows(db, []uuid.UUID{userID}, orgID)
	require.NoError(t, err)
	assert.Empty(t, byUser)
	require.Len(t, captured, 1)

	sql := captured[0]
	assert.Contains(t, sql, "user_organizations")
	assert.Contains(t, sql, "organization_id = $1 AND user_id IN ($2)")
	assert.NotContains(t, sql, "deleted_at", "soft-deleted rows must be loaded to detect removed members")
}

func TestFindOrgMemberUser_SendsSameNotFoundEnvelope(t *testing.T) {
	for _, label := range []string{"Agent", "User"} {
		req := testutil.NewGETRequest(t)
		user, err := findOrgMemberUser(dryRunPostgres(t), req, uuid.Nil, uuid.New(), label, true)

		assert.Nil(t, user)
		assert.ErrorIs(t, err, errEnvelopeSent)
		assert.Equal(t, fasthttp.StatusNotFound, req.RequestCtx.Response.StatusCode())

		var body map[string]any
		require.NoError(t, json.Unmarshal(req.RequestCtx.Response.Body(), &body))
		assert.Equal(t, label+" not found", body["message"])
	}
}
