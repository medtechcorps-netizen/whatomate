package handlers_test

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// Both assignment directories must page deterministically even when an import
// gives hundreds of users/members the same creation timestamp. This exercises
// the real PostgreSQL handlers, including search, membership and read authority.
func TestAssignmentDirectoriesStablePagination(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	otherOrg := testutil.CreateTestOrganization(t, app.DB)
	denied := testutil.CreateTestUser(t, app.DB, org.ID)
	prefix := "assignment-page-" + uuid.NewString() + "-"
	created := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	const count = 201
	people := make([]models.User, count)
	members := make([]models.UserOrganization, count)
	for index := range people {
		homeOrg := org.ID
		if index == count-1 {
			homeOrg = otherOrg.ID // A valid cross-organization member must be included.
		}
		people[index] = models.User{
			BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: created, UpdatedAt: created},
			OrganizationID: homeOrg,
			Email:          testutil.UniqueEmail("assignment-page"),
			PasswordHash:   denied.PasswordHash,
			FullName:       fmt.Sprintf("%s%03d", prefix, index),
			IsActive:       true,
		}
		members[index] = models.UserOrganization{
			BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: created, UpdatedAt: created},
			UserID:         people[index].ID,
			OrganizationID: org.ID,
		}
	}
	require.NoError(t, app.DB.Create(&people).Error)
	require.NoError(t, app.DB.Create(&members).Error)
	// Matching data in an unrelated tenant must not affect totals or pages.
	foreign := testutil.CreateTestUser(t, app.DB, otherOrg.ID, testutil.WithFullName(prefix+"foreign"))
	foreignRole := testutil.CreateTestRoleWithKeys(t, app.DB, otherOrg.ID, "foreign-directory-reader", []string{"users:read", "organizations:read"})
	foreignReader := testutil.CreateTestUser(t, app.DB, otherOrg.ID, testutil.WithRoleID(&foreignRole.ID))

	for _, resource := range []string{"users", "organizations"} {
		t.Run(resource, func(t *testing.T) {
			role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "directory-reader", []string{resource + ":read"})
			reader := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
			var handler func(*fastglue.Request) error
			var otherHandler func(*fastglue.Request) error
			if resource == "users" {
				handler, otherHandler = app.ListUsers, app.ListOrganizationMembers
			} else {
				handler, otherHandler = app.ListOrganizationMembers, app.ListUsers
			}
			indexes := make([]int, count)
			for index := range indexes {
				indexes[index] = index
			}
			sort.Slice(indexes, func(i, j int) bool {
				if resource == "users" {
					return people[indexes[i]].ID.String() > people[indexes[j]].ID.String()
				}
				return members[indexes[i]].ID.String() > members[indexes[j]].ID.String()
			})
			expected := make([]uuid.UUID, count)
			for index, source := range indexes {
				expected[index] = people[source].ID
			}

			fetch := func(page int, search string) ([]uuid.UUID, int) {
				t.Helper()
				req := testutil.NewGETRequest(t)
				testutil.SetAuthContext(req, org.ID, reader.ID)
				testutil.SetQueryParam(req, "page", page)
				testutil.SetQueryParam(req, "limit", 100)
				testutil.SetQueryParam(req, "search", search)
				require.NoError(t, handler(req))
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
				var response struct {
					Data struct {
						Users   []handlers.UserResponse   `json:"users"`
						Members []handlers.MemberResponse `json:"members"`
						Total   int                       `json:"total"`
						Page    int                       `json:"page"`
						Limit   int                       `json:"limit"`
					} `json:"data"`
				}
				require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
				require.Equal(t, page, response.Data.Page)
				require.Equal(t, 100, response.Data.Limit)
				ids := []uuid.UUID{}
				for _, user := range response.Data.Users {
					ids = append(ids, user.ID)
				}
				for _, member := range response.Data.Members {
					require.Equal(t, org.ID, member.OrganizationID)
					ids = append(ids, member.UserID)
				}
				return ids, response.Data.Total
			}

			all := []uuid.UUID{}
			for page, length := range []int{100, 100, 1, 0} {
				ids, total := fetch(page+1, prefix)
				require.Equal(t, count, total)
				require.Len(t, ids, length)
				all = append(all, ids...)
			}
			require.Equal(t, expected, all, "equal timestamps must use the unique ID tie-breaker")
			require.NotContains(t, all, foreign.ID)
			require.Contains(t, all, people[count-1].ID, "cross-organization member must remain assignable")
			again, _ := fetch(2, prefix)
			require.Equal(t, expected[100:200], again)
			// Server search must find an entry that is beyond the first page.
			lastIndex := indexes[count-1]
			searched, total := fetch(1, people[lastIndex].FullName)
			require.Equal(t, 1, total)
			require.Equal(t, []uuid.UUID{people[lastIndex].ID}, searched)

			for _, caller := range []*models.User{denied, foreignReader} {
				req := testutil.NewGETRequest(t)
				testutil.SetAuthContext(req, org.ID, caller.ID)
				testutil.SetQueryParam(req, "search", prefix)
				require.NoError(t, handler(req))
				require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
			}
			// A directory-specific reader does not gain the other read permission.
			req := testutil.NewGETRequest(t)
			testutil.SetAuthContext(req, org.ID, reader.ID)
			require.NoError(t, otherHandler(req))
			require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
		})
	}
}

func TestAssignmentDirectoriesExplicitOrganization(t *testing.T) {
	t.Parallel()
	for _, resource := range []string{"users", "organizations"} {
		t.Run(resource, func(t *testing.T) {
			app := newTestApp(t)
			app.Config.Database.RLSEnabled = true // Exercise the real transaction-scoped route wrapper.
			home := testutil.CreateTestOrganization(t, app.DB)
			selected := testutil.CreateTestOrganization(t, app.DB)
			foreign := testutil.CreateTestOrganization(t, app.DB)
			homeRole := testutil.CreateTestRoleWithKeys(t, app.DB, home.ID, "home-directory-reader", []string{resource + ":read"})
			selectedRole := testutil.CreateTestRoleWithKeys(t, app.DB, selected.ID, "selected-directory-reader", []string{resource + ":read"})
			caller := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithRoleID(&homeRole.ID))
			homePerson := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithFullName("Home workspace person"))
			selectedPerson := testutil.CreateTestUser(t, app.DB, selected.ID, testutil.WithFullName("Selected workspace person"))
			membership := models.UserOrganization{
				BaseModel: models.BaseModel{ID: uuid.New()},
				UserID:    caller.ID, OrganizationID: selected.ID, RoleID: &selectedRole.ID,
			}
			require.NoError(t, app.DB.Create(&membership).Error)
			method := (*handlers.App).ListUsers
			if resource == "organizations" {
				method = (*handlers.App).ListOrganizationMembers
			}
			handler := app.Tenant(method)
			request := func(header string, expectedStatus int) []uuid.UUID {
				t.Helper()
				req := testutil.NewGETRequest(t)
				// Auth middleware supplies the JWT default; the selected workspace
				// must be resolved from the actual override, not injected as context.
				testutil.SetAuthContext(req, home.ID, caller.ID)
				if header != "" {
					testutil.SetHeader(req, "X-Organization-ID", header)
				}
				testutil.SetQueryParam(req, "limit", 100)
				require.NoError(t, handler(req))
				require.Equal(t, expectedStatus, testutil.GetResponseStatusCode(req))
				if expectedStatus != fasthttp.StatusOK {
					require.NotContains(t, string(testutil.GetResponseBody(req)), homePerson.FullName)
					require.NotContains(t, string(testutil.GetResponseBody(req)), selectedPerson.FullName)
					return nil
				}
				var response struct {
					Data struct {
						Users   []handlers.UserResponse   `json:"users"`
						Members []handlers.MemberResponse `json:"members"`
					} `json:"data"`
				}
				require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
				ids := []uuid.UUID{}
				for _, user := range response.Data.Users {
					ids = append(ids, user.ID)
				}
				for _, member := range response.Data.Members {
					ids = append(ids, member.UserID)
				}
				return ids
			}
			require.ElementsMatch(t, []uuid.UUID{caller.ID, homePerson.ID}, request("", fasthttp.StatusOK))
			require.ElementsMatch(t, []uuid.UUID{caller.ID, homePerson.ID}, request(home.ID.String(), fasthttp.StatusOK))
			require.ElementsMatch(t, []uuid.UUID{caller.ID, selectedPerson.ID}, request(selected.ID.String(), fasthttp.StatusOK))
			request(foreign.ID.String(), fasthttp.StatusForbidden)
			request("not-an-organization", fasthttp.StatusBadRequest)
			request(uuid.Nil.String(), fasthttp.StatusBadRequest)
			// Home access remains valid after the selected membership is revoked.
			// The directory must refuse instead of returning those home people.
			require.NoError(t, app.DB.Delete(&membership).Error)
			request(selected.ID.String(), fasthttp.StatusForbidden)
			require.ElementsMatch(t, []uuid.UUID{caller.ID, homePerson.ID}, request("", fasthttp.StatusOK))
		})
	}
}
