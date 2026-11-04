package database_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Retain the unpartitioned definition as an independent result oracle. It must
// not call the new query builder or derive its partition predicates.
const unpartitionedMixedContactCount = `SELECT count(*)::bigint FROM (
 SELECT m.organization_id, m.contact_id
 FROM public.messages m
 JOIN public.contacts c ON c.organization_id=m.organization_id AND c.id=m.contact_id
 JOIN public.scheduled_jobs j ON j.organization_id=m.organization_id
  AND j.kind='inbound_message.continuation' AND j.aggregate_id=m.id
 WHERE m.direction='incoming'
 GROUP BY m.organization_id,m.contact_id
 HAVING count(DISTINCT COALESCE(NULLIF(j.payload->'message'->>'from_user_id',''),
  NULLIF(j.payload->'message'->>'from_parent_user_id',''))) > 1
) mixed`

func mixedPartitionContact(t *testing.T, f catalogFixture, org uuid.UUID, id uuid.UUID) {
	t.Helper()
	contact := models.Contact{BaseModel: models.BaseModel{ID: id}, OrganizationID: org, PhoneNumber: "60" + strings.ReplaceAll(id.String(), "-", ""), WhatsAppAccount: uuid.NewString(), Metadata: models.JSONB{}}
	// Raw insert preserves the nil UUID boundary; the model's BeforeCreate hook
	// would replace it with a random UUID.
	require.NoError(t, f.owner.Exec(`INSERT INTO public.contacts (id,organization_id,phone_number,whats_app_account,metadata) VALUES (?,?,?,?,?::jsonb)`, contact.ID, contact.OrganizationID, contact.PhoneNumber, contact.WhatsAppAccount, "{}").Error)
	for sender := 0; sender < 2; sender++ {
		message := models.Message{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org, ContactID: id, WhatsAppAccount: contact.WhatsAppAccount, Direction: models.Direction("incoming"), MessageType: models.MessageType("text")}
		require.NoError(t, f.owner.Create(&message).Error)
		payload := models.JSONB{"message": map[string]any{"from_user_id": fmt.Sprintf("synthetic-sender-%d", sender)}}
		if sender == 1 {
			payload = models.JSONB{"message": map[string]any{"from_user_id": "", "from_parent_user_id": "synthetic-sender-1"}}
		}
		job := models.ScheduledJob{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org, Kind: "inbound_message.continuation", AggregateType: "any-type", AggregateID: &message.ID, RunAt: time.Now(), IdempotencyKey: uuid.NewString(), Payload: payload}
		require.NoError(t, f.owner.Create(&job).Error)
	}
}

func TestTenantRLS_DataGatePartitionBoundaries(t *testing.T) {
	f := catalogClone(t)
	dataGatePoolOne(t, f)
	var first models.Organization
	require.NoError(t, f.owner.First(&first).Error)
	second := models.Organization{BaseModel: models.BaseModel{ID: uuid.New()}, Name: "Synthetic partition tenant", Slug: "partition-" + uuid.NewString(), Settings: models.JSONB{}}
	require.NoError(t, f.owner.Create(&second).Error)
	for bucket := 0; bucket < 16; bucket++ {
		// Lower endpoint and the UUID immediately below the next interval,
		// plus their adjacent UUIDs in the other tenant: 64 complete groups.
		for tenant, org := range []uuid.UUID{first.ID, second.ID} {
			lower := uuid.MustParse(fmt.Sprintf("%x0000000-0000-0000-0000-00000000000%d", bucket, tenant))
			upper := uuid.MustParse(fmt.Sprintf("%xfffffff-ffff-ffff-ffff-fffffffffff%x", bucket, 15-tenant))
			mixedPartitionContact(t, f, org, lower)
			mixedPartitionContact(t, f, org, upper)
		}
	}
	var reference int64
	require.NoError(t, f.owner.Raw(unpartitionedMixedContactCount).Row().Scan(&reference))
	require.Equal(t, int64(64), reference)
	before, session := dataGateRows(t, f), dataGateOwnerState(t, f)
	output, err := dataGateOutput(t, func() error { return database.ObserveMigrationDataGatesForTest(f.owner, catalogTemplate.runtime) })
	require.NoError(t, err)
	require.Equal(t, dataGateLines([3]int64{0, reference, 0}), output)
	require.Equal(t, before, dataGateRows(t, f))
	require.Equal(t, session, dataGateOwnerState(t, f))
}

type partitionFaultState struct {
	mode   string
	seen   int
	cancel context.CancelFunc
}

type partitionFaultConnector struct {
	driver.Connector
	state *partitionFaultState
}

func (c *partitionFaultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	dc, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	pc, ok := dc.(*stdlib.Conn)
	if !ok {
		_ = dc.Close()
		return nil, errors.New("partition fixture requires pgx stdlib")
	}
	return &partitionFaultConn{Conn: pc, state: c.state}, nil
}

type partitionFaultConn struct {
	*stdlib.Conn
	state *partitionFaultState
}

func (c *partitionFaultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, " AS mixed_contacts") && strings.Contains(query, " AND m.contact_id >= '") {
		c.state.seen++
		if c.state.mode == "overflow" && c.state.seen == 1 {
			return &partitionCountRows{value: int64(9223372036854775807)}, nil
		}
		if c.state.mode == "overflow" && c.state.seen == 2 {
			return &partitionCountRows{value: int64(1)}, nil
		}
		if c.state.seen == 3 {
			switch c.state.mode {
			case "sql_error":
				// A real PG error aborts the transaction. The production savepoint
				// must recover it before the third gate and outer restoration.
				return c.Conn.QueryContext(ctx, "SELECT 1/0", nil)
			case "negative":
				return &partitionCountRows{value: int64(-1)}, nil
			case "cancel":
				c.state.cancel()
				return c.Conn.QueryContext(ctx, query, args)
			}
		}
	}
	return c.Conn.QueryContext(ctx, query, args)
}

type partitionCountRows struct {
	value int64
	done  bool
}

func (r *partitionCountRows) Columns() []string { return []string{"count"} }
func (r *partitionCountRows) Close() error      { return nil }
func (r *partitionCountRows) Next(out []driver.Value) error {
	if r.done {
		return io.EOF
	}
	out[0] = r.value
	r.done = true
	return nil
}

func TestTenantRLS_DataGatePartitionPartialFailure(t *testing.T) {
	for _, mode := range []string{"sql_error", "negative", "overflow", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := catalogClone(t)
			var org models.Organization
			require.NoError(t, f.owner.First(&org).Error)
			mixedPartitionContact(t, f, org.ID, uuid.MustParse("00000000-0000-0000-0000-000000000001"))
			cfg, err := pgx.ParseConfig(catalogRoleURL(catalogTemplate.base, f.name, catalogTemplate.owner, catalogTemplate.ownerPassword).String())
			require.NoError(t, err)
			cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			state := &partitionFaultState{mode: mode, cancel: cancel}
			pool := sql.OpenDB(&partitionFaultConnector{Connector: stdlib.GetConnector(*cfg), state: state})
			pool.SetMaxOpenConns(1)
			pool.SetMaxIdleConns(1)
			t.Cleanup(func() { require.NoError(t, pool.Close()) })
			observed, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Discard})
			require.NoError(t, err)
			var pid int
			require.NoError(t, observed.Raw("SELECT pg_backend_pid()").Row().Scan(&pid))
			before := dataGateRows(t, f)
			started := time.Now()
			output, runErr := dataGateOutput(t, func() error {
				return database.ObserveMigrationDataGatesForTest(observed.WithContext(ctx), catalogTemplate.runtime)
			})
			require.Less(t, time.Since(started), 17*time.Second)
			require.Contains(t, output, "data-gate empty-phone-mixed-sender-contacts error\n")
			require.NotContains(t, output, "empty-phone-mixed-sender-contacts count=")
			if mode != "cancel" {
				require.NoError(t, runErr)
				require.Equal(t, "data-gate empty-phone-legacy-contacts count=0 mode=observe\ndata-gate empty-phone-mixed-sender-contacts error\ndata-gate empty-phone-missing-continuation-messages count=0 mode=observe\n", output)
			} else {
				// Cancellation can dispose the driver connection. A restoration
				// error must not turn any observation into a known count.
				if runErr != nil {
					require.NotContains(t, output, "count=")
				}
			}
			if mode == "overflow" {
				require.Equal(t, 2, state.seen)
			} else {
				require.Equal(t, 3, state.seen)
			}
			require.Equal(t, before, dataGateRows(t, f))
			require.NoError(t, pool.Close())
			var locks int
			require.NoError(t, f.owner.Raw("SELECT count(*) FROM pg_locks WHERE pid=? AND locktype='advisory'", pid).Row().Scan(&locks))
			require.Zero(t, locks)
		})
	}
}
