package assignment_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shridarpatil/whatomate/internal/assignment"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// The private GORM handle has its own callback registry. It shares only the
// fixture SQL pool, which it does not close or mutate. These tests do not run in
// parallel and never register an error injection on the shared test handle.
func checkedAssigner(t *testing.T) (*assignment.Assigner, *gorm.DB) {
	t.Helper()
	shared := testutil.SetupTestDB(t)
	rdb := testutil.SetupTestRedis(t)
	if rdb == nil {
		t.Skip("TEST_REDIS_URL not set, skipping")
	}
	pool, err := shared.DB()
	require.NoError(t, err)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: shared.Logger})
	require.NoError(t, err)
	return assignment.New(db, rdb, testutil.NopLogger()), db
}

// This is an error-origin regression, not a real lock-timeout experiment. It
// injects the original SQL error at each actual query boundary; the handler
// contention regressions separately exercise real PostgreSQL lock deadlines.
func TestAssignChatToTeamCheckedPreservesSQLError(t *testing.T) {
	for _, code := range []string{"55P03", "42501"} {
		for _, stage := range []string{"team", "config_members", "availability", "round_robin_members", "round_robin_update", "chat_load"} {
			t.Run(code+"/"+stage, func(t *testing.T) {
				a, db := checkedAssigner(t)
				org := testutil.CreateTestOrganization(t, db)
				strategy := models.AssignmentStrategyRoundRobin
				if stage == "chat_load" {
					strategy = models.AssignmentStrategyLoadBalanced
				}
				team, agents := createTeam(t, db, org.ID, strategy, 1, 0)
				cold := stage == "team" || stage == "config_members"
				if !cold {
					require.NotNil(t, a.GetTeamConfig(team.ID))
				}
				injected := &pgconn.PgError{Code: code, Message: "synthetic checked-assignment query failure"}
				var hits atomic.Int32
				callback := func(tx *gorm.DB) {
					table := tx.Statement.Table
					matches := (stage == "team" && table == "teams") ||
						((stage == "config_members" || stage == "round_robin_members" || stage == "round_robin_update") && table == "team_members") ||
						(stage == "availability" && table == "users") ||
						(stage == "chat_load" && table == "agent_transfers")
					if matches {
						hits.Add(1)
						_ = tx.AddError(injected)
					}
				}
				const name = "checked-assignment:original-error"
				var remove func() error
				switch stage {
				case "round_robin_update":
					require.NoError(t, db.Callback().Update().Before("gorm:update").Register(name, callback))
					remove = func() error { return db.Callback().Update().Remove(name) }
				case "chat_load":
					require.NoError(t, db.Callback().Row().Before("gorm:row").Register(name, callback))
					remove = func() error { return db.Callback().Row().Remove(name) }
				default:
					require.NoError(t, db.Callback().Query().Before("gorm:query").Register(name, callback))
					remove = func() error { return db.Callback().Query().Remove(name) }
				}
				t.Cleanup(func() { _ = remove() })
				selected, err := a.AssignChatToTeamChecked(team.ID, org.ID, nil)
				require.ErrorIs(t, err, injected)
				var cause *pgconn.PgError
				require.ErrorAs(t, err, &cause)
				assert.Same(t, injected, cause, "the original statement failure must survive")
				assert.EqualValues(t, 1, hits.Load())
				assert.Nil(t, selected, "failed reads and writes cannot supply a selected agent")
				if cold {
					rdb := testutil.SetupTestRedis(t)
					exists, err := rdb.Exists(context.Background(), "team:assignment:"+team.ID.String()).Result()
					require.NoError(t, err)
					assert.Zero(t, exists, "a failed member/config read must not poison the cache")
				}
				require.NoError(t, remove())
				remove = func() error { return nil }
				selected, err = a.AssignChatToTeamChecked(team.ID, org.ID, nil)
				require.NoError(t, err)
				require.NotNil(t, selected)
				assert.Equal(t, agents[0].ID, *selected, "retry reloads and selects the real member")
			})
		}
	}
}

func TestAssignChatToTeamCheckedLoadAndExclusion(t *testing.T) {
	a, db := checkedAssigner(t)
	org := testutil.CreateTestOrganization(t, db)
	team, agents := createTeam(t, db, org.ID, models.AssignmentStrategyLoadBalanced, 2, 0)
	contact := testutil.CreateTestContact(t, db, org.ID)
	require.NoError(t, db.Create(&models.AgentTransfer{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		ContactID: contact.ID, AgentID: &agents[0].ID, Status: models.TransferStatusActive,
	}).Error)
	selected, err := a.AssignChatToTeamChecked(team.ID, org.ID, nil)
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Equal(t, agents[1].ID, *selected)
	selected, err = a.AssignChatToTeamChecked(team.ID, org.ID, []uuid.UUID{agents[1].ID})
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Equal(t, agents[0].ID, *selected)
	setUnavailable(t, db, agents[0].ID)
	selected, err = a.AssignChatToTeamChecked(team.ID, org.ID, []uuid.UUID{agents[1].ID})
	require.NoError(t, err)
	assert.Nil(t, selected)
}

func TestAssignChatToTeamCheckedIntentionalNoAgent(t *testing.T) {
	a, db := checkedAssigner(t)
	org := testutil.CreateTestOrganization(t, db)
	team, _ := createTeam(t, db, org.ID, models.AssignmentStrategyManual, 1, 0)
	selected, err := a.AssignChatToTeamChecked(team.ID, org.ID, nil)
	require.NoError(t, err)
	assert.Nil(t, selected)
	a.InvalidateTeamCache(team.ID)
	require.NoError(t, db.Model(team).Update("is_active", false).Error)
	selected, err = a.AssignChatToTeamChecked(team.ID, org.ID, nil)
	require.NoError(t, err)
	assert.Nil(t, selected)
}

func TestLegacyAssignmentKeepsBestEffortTimestampBehavior(t *testing.T) {
	a, db := checkedAssigner(t)
	org := testutil.CreateTestOrganization(t, db)
	team, agents := createTeam(t, db, org.ID, models.AssignmentStrategyRoundRobin, 1, 0)
	const name = "checked-assignment:legacy-timestamp"
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "team_members" {
			_ = tx.AddError(&pgconn.PgError{Code: "55P03", Message: "synthetic timestamp lock failure"})
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Update().Remove(name) })
	selected := a.AssignToTeam(team.ID, org.ID, nil, assignment.ChatLoadCounter)
	require.NotNil(t, selected)
	assert.Equal(t, agents[0].ID, *selected)
}
