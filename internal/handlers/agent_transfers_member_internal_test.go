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

	_, ok := lookupOrgMemberUser(nil, id, id, true)
	assert.False(t, ok, "nil db")

	db := dryRunPostgres(t)
	_, ok = lookupOrgMemberUser(db, uuid.Nil, id, true)
	assert.False(t, ok, "nil user id")
	_, ok = lookupOrgMemberUser(db, id, uuid.Nil, true)
	assert.False(t, ok, "nil org id")
}

func TestOrgMemberMembershipValid_FailsClosed(t *testing.T) {
	orgID := uuid.New()
	homeUser := &models.User{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID}

	assert.False(t, orgMemberMembershipValid(dryRunPostgres(t), nil, orgID), "nil user")
	assert.False(t, orgMemberMembershipValid(nil, homeUser, orgID), "nil db must not accept the home org")
	assert.False(t, orgMemberMembershipValid(dryRunPostgres(t), homeUser, uuid.Nil), "nil org")

	failing := dryRunPostgres(t)
	failing.Error = errors.New("membership query failed")
	assert.False(t, orgMemberMembershipValid(failing, homeUser, orgID),
		"a failed membership query must reject, not fall back to the home org")
}

func TestOrgMemberMembershipValid_HomeOrgFallbackWithoutAnyRow(t *testing.T) {
	// A dry-run query returns no rows: the user never had a membership row,
	// so only the legacy home organization counts.
	db := dryRunPostgres(t)
	orgID := uuid.New()

	homeUser := &models.User{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID}
	assert.True(t, orgMemberMembershipValid(db, homeUser, orgID))

	otherUser := &models.User{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: uuid.New()}
	assert.False(t, orgMemberMembershipValid(db, otherUser, orgID))

	// No super-admin bypass.
	superAdmin := &models.User{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: uuid.New(), IsSuperAdmin: true}
	assert.False(t, orgMemberMembershipValid(db, superAdmin, orgID))
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
			assert.Equal(t, tc.want, orgMembershipRowsValid(db, tc.user, orgID, tc.rows))
		})
	}

	assert.False(t, orgMembershipRowsValid(db, nil, orgID, nil), "nil user")
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
		user, err := findOrgMemberUser(nil, req, uuid.New(), uuid.New(), label, true)

		assert.Nil(t, user)
		assert.ErrorIs(t, err, errEnvelopeSent)
		assert.Equal(t, fasthttp.StatusNotFound, req.RequestCtx.Response.StatusCode())

		var body map[string]any
		require.NoError(t, json.Unmarshal(req.RequestCtx.Response.Body(), &body))
		assert.Equal(t, label+" not found", body["message"])
	}
}
