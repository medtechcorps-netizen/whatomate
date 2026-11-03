package mediaretirementdb

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/mediaretirement"
)

const MediaInventoryApplication = "rereply-media-reference-inventory"

var mediaInventoryFamilies = [...]mediaretirement.Family{
	mediaretirement.Messages, mediaretirement.MessageParts, mediaretirement.CampaignPreviews,
	mediaretirement.Recordings, mediaretirement.InboundMedia, mediaretirement.IVRAudio,
	mediaretirement.OrganizationAudio, mediaretirement.CatalogArtwork, mediaretirement.IdentityAvatars,
}

// This closed relation set covers the nine projections and current-selector
// witness. All tenant relations use the shared canonical policy/dependency
// validator; organizations must be an owner-visible, non-RLS ordinary table.
var mediaInventoryTables = [...]string{
	"organizations", "whatsapp_accounts", "channel_accounts", "whatsapp_coexistence_states",
	"messages", "message_parts", "bulk_message_campaigns", "call_logs", "inbound_events",
	"ivr_flows", "catalog_products", "contact_identities",
}

type MediaInventoryScope struct {
	OrganizationID string
	AccountID      string
}

type mediaInventoryManifest struct {
	Version                       int                              `json:"version"`
	Purpose                       string                           `json:"purpose"`
	ReferenceScope                string                           `json:"reference_scope"`
	Scope                         mediaretirement.ScopeWitness     `json:"current_selectors"`
	HistoricalAttributionComplete bool                             `json:"historical_attribution_complete"`
	SameSnapshot                  bool                             `json:"same_snapshot"`
	SessionDisposed               bool                             `json:"session_disposed"`
	CompleteFamilies              []mediaretirement.Family         `json:"complete_families"`
	Counts                        map[mediaretirement.Family]int64 `json:"counts"`
	Pages                         []json.RawMessage                `json:"pages"`
}

type mediaInventorySessionWitness struct {
	PID          uint32
	BackendStart string
	Snapshot     string
}

// CollectMediaReferenceInventory takes ownership of conn and always closes it.
// The caller must connect using its explicitly configured owner credential;
// this function neither opens a connection nor falls back to another role.
// It starts its own read-only repeatable-read transaction and returns bytes
// only after rollback and connection disposal have both succeeded. It makes
// no object-storage requests and performs no data mutation or retirement.
//
// The manifest covers the nine fixed reference families for the organization,
// including unmatched legacy names. The selected account witness is current
// only. Global reseller/recovery overlap and historical attribution remain
// separate requirements, as do fresh transaction checks before any mutation.
func CollectMediaReferenceInventory(parent context.Context, conn *pgx.Conn, scope MediaInventoryScope) ([]byte, error) {
	if conn == nil {
		return nil, mediaretirement.ErrIncomplete
	}
	var tx pgx.Tx
	disposed := false
	dispose := func() bool {
		if disposed {
			return false
		}
		disposed = true
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		rollbackOK := true
		if tx != nil {
			rollbackOK = tx.Rollback(cleanup) == nil
		}
		closeErr := conn.Close(cleanup)
		cleanupDone := false
		select {
		case <-conn.PgConn().CleanupDone():
			cleanupDone = true
		case <-cleanup.Done():
		}
		return rollbackOK && closeErr == nil && conn.IsClosed() && cleanupDone && cleanup.Err() == nil
	}
	defer func() {
		if !disposed {
			dispose()
		}
	}()
	if parent == nil || parent.Err() != nil || conn.IsClosed() || conn.PgConn().TxStatus() != 'I' ||
		!mediaInventoryScopeValid(scope) {
		return nil, mediaretirement.ErrIncomplete
	}
	if _, ok := parent.Deadline(); !ok {
		return nil, mediaretirement.ErrIncomplete
	}
	ctx, stop := context.WithTimeout(parent, 30*time.Second)
	defer stop()
	var err error
	tx, err = conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	// Acquire the fixed relation locks before the first snapshot-bearing SELECT.
	// ONLY prevents following inherited child relations. No data rows are read
	// until the exact owner/RLS/dependency guards below have all passed.
	locks := make([]string, len(mediaInventoryTables))
	for i, table := range mediaInventoryTables {
		locks[i] = "ONLY public." + table
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE "+strings.Join(locks, ",")+" IN ACCESS SHARE MODE"); err != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	before, err := mediaInventorySession(ctx, tx)
	if err != nil || mediaInventoryVisibility(ctx, tx) != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	witness, err := mediaInventoryScope(ctx, tx, scope)
	if err != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	manifest := mediaInventoryManifest{
		Version: 1, Purpose: "media_reference_inventory", ReferenceScope: "organization_nine_families",
		Scope: witness, SameSnapshot: true, CompleteFamilies: make([]mediaretirement.Family, 0, len(mediaInventoryFamilies)),
		Counts: make(map[mediaretirement.Family]int64), Pages: make([]json.RawMessage, 0),
	}
	remaining := mediaretirement.MaxPrivateBytes - 4096 // reserve the final manifest envelope
	for _, family := range mediaInventoryFamilies {
		plan, err := mediaretirement.Projection(family)
		if err != nil || !mediaInventoryAdmittedTables(plan.Tables) {
			return nil, mediaretirement.ErrIncomplete
		}
		cursor := ""
		for {
			if ctx.Err() != nil || tx.Conn() != conn || conn.PgConn().TxStatus() != 'T' || len(manifest.Pages) >= 512 {
				return nil, mediaretirement.ErrIncomplete
			}
			var raw *string
			if err := tx.QueryRow(ctx, plan.SQL, scope.OrganizationID, cursor, remaining).Scan(&raw); err != nil || raw == nil {
				return nil, mediaretirement.ErrIncomplete
			}
			page, err := mediaretirement.DecodePage([]byte(*raw), family, scope.OrganizationID, cursor, remaining)
			if err != nil {
				return nil, mediaretirement.ErrIncomplete
			}
			remaining -= len(*raw)
			manifest.Pages = append(manifest.Pages, json.RawMessage(*raw))
			manifest.Counts[family] += int64(len(page.Records))
			if !page.More {
				manifest.CompleteFamilies = append(manifest.CompleteFamilies, family)
				break
			}
			cursor = page.LastID
		}
	}
	// These are same-snapshot consistency checks, never a claim of observing
	// changes committed after this snapshot. Retirement needs fresh guards.
	afterWitness, err := mediaInventoryScope(ctx, tx, scope)
	if err != nil || witness != afterWitness || mediaInventoryVisibility(ctx, tx) != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	after, err := mediaInventorySession(ctx, tx)
	if err != nil || before != after || ctx.Err() != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	if !dispose() || parent.Err() != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	manifest.SessionDisposed = true
	raw, err := json.Marshal(manifest)
	if err != nil || len(raw) > mediaretirement.MaxPrivateBytes || parent.Err() != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	return raw, nil
}

func mediaInventoryAdmittedTables(tables []string) bool {
	if len(tables) == 0 {
		return false
	}
	for _, table := range tables {
		found := false
		for _, admitted := range mediaInventoryTables {
			found = found || table == admitted
		}
		if !found {
			return false
		}
	}
	return true
}

func mediaInventoryScope(ctx context.Context, tx pgx.Tx, scope MediaInventoryScope) (mediaretirement.ScopeWitness, error) {
	plan := mediaretirement.ScopeProjection()
	if !mediaInventoryAdmittedTables(plan.Tables) {
		return mediaretirement.ScopeWitness{}, mediaretirement.ErrIncomplete
	}
	var raw *string
	if err := tx.QueryRow(ctx, plan.SQL, scope.OrganizationID, scope.AccountID).Scan(&raw); err != nil || raw == nil {
		return mediaretirement.ScopeWitness{}, mediaretirement.ErrIncomplete
	}
	return mediaretirement.DecodeScope([]byte(*raw), scope.OrganizationID, scope.AccountID)
}

func mediaInventoryVisibility(ctx context.Context, tx pgx.Tx) error {
	if database.ValidateMediaReferenceVisibility(ctx, tx) != nil {
		return mediaretirement.ErrIncomplete
	}
	return nil
}

func mediaInventoryScopeValid(scope MediaInventoryScope) bool {
	for _, value := range []string{scope.OrganizationID, scope.AccountID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return false
		}
	}
	return true
}

func mediaInventorySession(ctx context.Context, tx pgx.Tx) (mediaInventorySessionWitness, error) {
	var witness mediaInventorySessionWitness
	if ctx == nil || ctx.Err() != nil || tx == nil || tx.Conn() == nil || tx.Conn().PgConn().TxStatus() != 'T' {
		return witness, mediaretirement.ErrIncomplete
	}
	if _, ok := ctx.Deadline(); !ok {
		return witness, mediaretirement.ErrIncomplete
	}
	var eligible bool
	err := tx.QueryRow(ctx, `SELECT current_user=session_user AND NOT r.rolsuper
 AND d.datdba=r.oid AND a.usesysid=r.oid AND a.usename=session_user AND a.datid=d.oid
 AND pg_catalog.pg_backend_pid()=$1
 AND pg_catalog.current_setting('application_name')=$2
 AND pg_catalog.current_setting('server_version_num')::integer/10000=17
 AND NOT pg_catalog.pg_is_in_recovery()
 AND pg_catalog.current_setting('transaction_read_only')='on'
 AND pg_catalog.current_setting('transaction_isolation')='repeatable read'
 AND pg_catalog.current_setting('search_path')='pg_catalog'
 AND pg_catalog.current_setting('server_encoding')='UTF8'
 AND pg_catalog.current_setting('session_replication_role')='origin'
 AND pg_catalog.current_setting('statement_timeout')::interval > interval '0 seconds'
 AND pg_catalog.current_setting('statement_timeout')::interval <= interval '3 seconds'
 AND pg_catalog.current_setting('lock_timeout')::interval > interval '0 seconds'
 AND pg_catalog.current_setting('lock_timeout')::interval <= interval '1 second'
 AND pg_catalog.current_setting('idle_in_transaction_session_timeout')::interval > interval '0 seconds'
 AND pg_catalog.current_setting('idle_in_transaction_session_timeout')::interval <= interval '5 seconds',
 a.pid::bigint,a.backend_start::text,pg_catalog.pg_current_snapshot()::text
 FROM pg_catalog.pg_roles r JOIN pg_catalog.pg_database d ON d.datname=pg_catalog.current_database()
 JOIN pg_catalog.pg_stat_activity a ON a.pid=pg_catalog.pg_backend_pid()
 WHERE r.rolname=current_user`, tx.Conn().PgConn().PID(), MediaInventoryApplication).Scan(&eligible, &witness.PID, &witness.BackendStart, &witness.Snapshot)
	if err != nil || !eligible || witness.PID == 0 || witness.BackendStart == "" || witness.Snapshot == "" || ctx.Err() != nil {
		return mediaInventorySessionWitness{}, mediaretirement.ErrIncomplete
	}
	return witness, nil
}
