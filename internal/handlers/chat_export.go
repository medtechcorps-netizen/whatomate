package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/access"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// Limit snapshot holders independently of users/replicas. The Redis owner lock
// additionally prevents a user's overlapping requests on different replicas.
// Slots cover preview/build/revalidation, never a slow network download.
var chatExportSlots = make(chan struct{}, 2)
var chatExportArchives = make(chan struct{}, 2)

const chatExportStreamDuration = 3 * time.Minute

// Cancellation and response ownership race through one Redis state transition.
// Once delivery owns the response, cancellation must not claim preparation was
// cancelled. The browser can still abort its download.
const chatExportCancelScript = `local state=redis.call('GET',KEYS[1]); if not state or state=='running' or state=='cancelled' then redis.call('SET',KEYS[1],'cancelled','EX',600); return 1 end return 0`
const chatExportTransitionScript = `if redis.call('GET',KEYS[1])==ARGV[1] then redis.call('SET',KEYS[1],ARGV[2],'EX',600); return 1 end return 0`
const chatExportOwnerReleaseScript = `if redis.call('GET',KEYS[1])==ARGV[1] then return redis.call('DEL',KEYS[1]) end return 0`

func chatExportAcquire() bool {
	select {
	case chatExportSlots <- struct{}{}:
		return true
	default:
		return false
	}
}
func chatExportRelease() { <-chatExportSlots }

// Export never falls back to the JWT's default workspace when a captured
// workspace header becomes inaccessible. The initial actor lookup is bounded
// too, before acquiring an archive or snapshot permit.
func (a *App) chatExportActor(r *fastglue.Request) (uuid.UUID, uuid.UUID, error) {
	ctx, cancel := context.WithTimeout(requestContext(r), 5*time.Second)
	defer cancel()
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return uuid.Nil, uuid.Nil, errChatExportDenied
	}
	scoped := root.scopedApp(root.DB.WithContext(ctx), uuid.Nil)
	scoped.Redis = nil
	orgID, err := scoped.getExplicitOrgID(r)
	if err != nil {
		return uuid.Nil, uuid.Nil, errChatExportDenied
	}
	var userID uuid.UUID
	switch value := r.RequestCtx.UserValue("user_id").(type) {
	case uuid.UUID:
		userID = value
	case string:
		userID, err = uuid.Parse(value)
	default:
		err = errChatExportDenied
	}
	if err != nil || userID == uuid.Nil {
		return uuid.Nil, uuid.Nil, errChatExportDenied
	}
	var user models.User
	if scoped.DB.Select("id", "is_super_admin").Where("id = ? AND is_active = ?", userID, true).First(&user).Error != nil {
		return uuid.Nil, uuid.Nil, errChatExportDenied
	}
	if !user.IsSuperAdmin {
		if _, ok := access.OrganizationMembership(scoped.DB, userID, orgID); !ok {
			return uuid.Nil, uuid.Nil, errChatExportDenied
		}
	}
	return orgID, userID, nil
}

// These routes must be registered directly on App, not through Tenant: this
// function owns the read-only snapshot and releases it before revalidation.
func (a *App) chatExportReadTransaction(ctx context.Context, orgID uuid.UUID, repeatable bool, fn func(*App) error) error {
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return errors.New("export database unavailable")
	}
	if _, nested := root.DB.Statement.ConnPool.(gorm.TxCommitter); nested {
		return errors.New("export requires a root connection")
	}
	isolation := sql.LevelReadCommitted
	if repeatable {
		isolation = sql.LevelRepeatableRead
	}
	return root.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if a.rlsEnabled() {
			if err := database.SetTenantContext(tx, orgID); err != nil {
				return err
			}
		}
		if err := requireOrdinaryTenantOrganization(tx, orgID); err != nil {
			return err
		}
		scoped := root.scopedApp(tx, orgID)
		// Fresh DB-only permission and policy reads. This never mutates caches.
		scoped.Redis = nil
		return fn(scoped)
	}, &sql.TxOptions{Isolation: isolation, ReadOnly: true})
}

func chatExportError(r *fastglue.Request, err error) error {
	status, message, code := fasthttp.StatusServiceUnavailable, "Export could not be completed. No partial archive was downloaded.", "chat_export_failed"
	switch {
	case errors.Is(err, errChatExportUnavailable):
		status, message, code = fasthttp.StatusForbidden, "Chat export is not available yet.", "chat_export_unavailable"
	case errors.Is(err, errChatExportDenied):
		status, message, code = fasthttp.StatusForbidden, "You do not have permission to export these chats.", "chat_export_denied"
	case errors.Is(err, errChatExportTooLarge):
		status, message, code = fasthttp.StatusRequestEntityTooLarge, errChatExportTooLarge.Error(), "chat_export_too_large"
	case errors.Is(err, errChatExportScopeChanged):
		status, message, code = fasthttp.StatusConflict, errChatExportScopeChanged.Error(), "chat_export_scope_changed"
	case errors.Is(err, errChatExportCancelled), errors.Is(err, context.Canceled):
		status, message, code = fasthttp.StatusRequestTimeout, "Export cancelled. No archive was downloaded.", "chat_export_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		status, message, code = fasthttp.StatusRequestTimeout, "This export took too long. Choose a smaller date range or account and try again.", "chat_export_timeout"
	}
	return r.SendErrorEnvelope(status, message, nil, fastglue.ErrorType(code))
}

func parseChatExportRequest(r *fastglue.Request) (chatExportRequest, error) {
	var req chatExportRequest
	if len(r.RequestCtx.PostBody()) > 256<<10 {
		return req, errors.New("export selection is too large")
	}
	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return req, errors.New("invalid export request")
	}
	return req, req.validate()
}

// GetChatExportCapabilities reports availability without returning messages or
// granting a missing permission. Even superadmins require installed vocabulary.
func (a *App) GetChatExportCapabilities(r *fastglue.Request) error {
	orgID, userID, err := a.chatExportActor(r)
	if err != nil {
		return chatExportError(r, errChatExportDenied)
	}
	ctx, cancel := context.WithTimeout(requestContext(r), 5*time.Second)
	defer cancel()
	var access chatExportAccess
	err = a.chatExportReadTransaction(ctx, orgID, false, func(scoped *App) error { var e error; access, e = scoped.chatExportAccess(orgID, userID); return e })
	if err != nil && !errors.Is(err, errChatExportDenied) && !errors.Is(err, errChatExportUnavailable) {
		return chatExportError(r, err)
	}
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	return r.SendEnvelope(map[string]any{"available": err == nil, "native_inbox": err == nil && access.NativeInbox, "max_messages": chatExportMaxMessages, "max_bytes": chatExportMaxBytes, "max_selected": chatExportMaxSelected})
}

func (a *App) PreviewChatExport(r *fastglue.Request) error {
	orgID, userID, err := a.chatExportActor(r)
	if err != nil {
		return chatExportError(r, errChatExportDenied)
	}
	req, err := parseChatExportRequest(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "chat_export_invalid")
	}
	if !chatExportAcquire() {
		return r.SendErrorEnvelope(fasthttp.StatusTooManyRequests, "Other exports are being prepared. Try again shortly.", nil, "chat_export_busy")
	}
	defer chatExportRelease()
	ctx, cancel := context.WithTimeout(requestContext(r), 30*time.Second)
	defer cancel()
	var preview chatExportPreview
	err = a.chatExportReadTransaction(ctx, orgID, true, func(scoped *App) error {
		access, e := scoped.chatExportAccess(orgID, userID)
		if e != nil {
			return e
		}
		preview, e = scoped.chatExportPreview(orgID, userID, req, access)
		return e
	})
	if err != nil {
		return chatExportError(r, err)
	}
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	return r.SendEnvelope(preview)
}

func chatExportJobKey(orgID, userID, requestID uuid.UUID) string {
	return "chat-export:v1:" + orgID.String() + ":" + userID.String() + ":" + requestID.String()
}
func chatExportOwnerKey(orgID, userID uuid.UUID) string {
	return "chat-export-owner:v1:" + orgID.String() + ":" + userID.String()
}

// CancelChatExport sets only an owner-bound transient control key. It performs
// no customer-data mutation; current membership is still required. It is valid
// before a delayed download request arrives, preventing that request starting.
func (a *App) CancelChatExport(r *fastglue.Request) error {
	orgID, userID, err := a.chatExportActor(r)
	if err != nil {
		return chatExportError(r, errChatExportDenied)
	}
	id, err := parsePathUUID(r, "id", "export")
	if err != nil {
		return nil
	}
	if a.rootApp().Redis == nil {
		return chatExportError(r, errors.New("export coordination unavailable"))
	}
	ctx, cancel := context.WithTimeout(requestContext(r), 5*time.Second)
	defer cancel()
	accepted, err := a.rootApp().Redis.Eval(ctx, chatExportCancelScript, []string{chatExportJobKey(orgID, userID, id)}).Int()
	if err != nil {
		return chatExportError(r, err)
	}
	return r.SendEnvelope(map[string]any{"cancelled": accepted == 1, "preparation_finished": accepted == 0})
}

type chatExportDownload struct {
	io.Reader
	archive *chatExportArchive
	once    sync.Once
	cleanup func()
	mu      sync.Mutex
	timer   *time.Timer
}

func (d *chatExportDownload) expireAfter(duration time.Duration) {
	d.mu.Lock()
	d.timer = time.AfterFunc(duration, func() { _ = d.Close() })
	d.mu.Unlock()
}

func (d *chatExportDownload) Close() error {
	d.once.Do(func() {
		d.mu.Lock()
		if d.timer != nil {
			d.timer.Stop()
		}
		d.mu.Unlock()
		d.archive.cleanup()
		if d.cleanup != nil {
			d.cleanup()
		}
	})
	return nil
}

func (a *App) DownloadChatExport(r *fastglue.Request) error {
	orgID, userID, err := a.chatExportActor(r)
	if err != nil {
		return chatExportError(r, errChatExportDenied)
	}
	req, err := parseChatExportRequest(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "chat_export_invalid")
	}
	if req.RequestID == uuid.Nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "An export request ID is required", nil, "chat_export_invalid")
	}
	if !chatExportAcquire() {
		return r.SendErrorEnvelope(fasthttp.StatusTooManyRequests, "Other exports are being prepared. Try again shortly.", nil, "chat_export_busy")
	}
	defer chatExportRelease()
	select {
	case chatExportArchives <- struct{}{}:
	default:
		return r.SendErrorEnvelope(fasthttp.StatusTooManyRequests, "Other chat archives are downloading. Try again shortly.", nil, "chat_export_busy")
	}
	archiveOwnedByResponse := false
	defer func() {
		if !archiveOwnedByResponse {
			<-chatExportArchives
		}
	}()
	root := a.rootApp()
	if root.Redis == nil {
		return chatExportError(r, errors.New("export coordination unavailable"))
	}
	ctx, cancel := context.WithTimeout(requestContext(r), chatExportMaxDuration)
	defer cancel()
	ownerKey := chatExportOwnerKey(orgID, userID)
	jobKey := chatExportJobKey(orgID, userID, req.RequestID)
	claimed, err := root.Redis.SetNX(ctx, ownerKey, req.RequestID.String(), chatExportMaxDuration+chatExportStreamDuration+time.Minute).Result()
	if err != nil {
		return chatExportError(r, err)
	}
	if !claimed {
		return r.SendErrorEnvelope(fasthttp.StatusTooManyRequests, "You already have an export preparing or downloading. Cancel it or wait for it to finish.", nil, "chat_export_busy")
	}
	releaseOwner := func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = root.Redis.Eval(cleanupCtx, chatExportOwnerReleaseScript, []string{ownerKey}, req.RequestID.String()).Err()
	}
	defer func() {
		if !archiveOwnedByResponse {
			releaseOwner()
		}
	}()
	started, err := root.Redis.SetNX(ctx, jobKey, "running", 10*time.Minute).Result()
	if err != nil {
		return chatExportError(r, err)
	}
	if !started {
		return chatExportError(r, errChatExportCancelled)
	}
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				state, e := root.Redis.Get(ctx, jobKey).Result()
				if e == nil && state == "delivering" {
					return
				}
				if e != nil || state != "running" {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-watchDone
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = root.Redis.Eval(cleanupCtx, chatExportTransitionScript, []string{jobKey}, "running", "finished").Err()
	}()
	var archive *chatExportArchive
	defer func() {
		if archive != nil {
			archive.cleanup()
		}
	}()
	var initialAccess chatExportAccess
	err = a.chatExportReadTransaction(ctx, orgID, true, func(scoped *App) error {
		var e error
		initialAccess, e = scoped.chatExportAccess(orgID, userID)
		if e != nil {
			return e
		}
		preview, e := scoped.chatExportPreview(orgID, userID, req, initialAccess)
		if e != nil {
			return e
		}
		if !preview.CanDownload {
			if preview.Messages == 0 {
				return errors.New("no messages match export scope")
			}
			return errChatExportTooLarge
		}
		var snapshot time.Time
		if e = scoped.DB.Raw("SELECT transaction_timestamp()").Scan(&snapshot).Error; e != nil {
			return e
		}
		archive, e = newChatExportArchive(snapshot, req)
		if e != nil {
			return e
		}
		if e = scoped.buildChatExport(ctx, orgID, userID, req, initialAccess, archive); e != nil {
			return e
		}
		if int64(len(archive.IDs)) != preview.Messages {
			return errChatExportScopeChanged
		}
		return nil
	})
	if err != nil {
		return chatExportError(r, err)
	}
	if err = archive.finish(); err != nil {
		return chatExportError(r, err)
	}
	// The snapshot is committed and released. This fresh READ COMMITTED owner
	// uses one connection and DB-only grants, never the original stale snapshot.
	err = a.revalidateChatExport(ctx, orgID, userID, req, initialAccess, archive.IDs)
	if err != nil {
		return chatExportError(r, err)
	}
	if err = ctx.Err(); err != nil {
		return chatExportError(r, err)
	}
	info, err := archive.File.Stat()
	if err != nil {
		return chatExportError(r, err)
	}
	// This is the final preparation commit. A cancellation acknowledged before
	// this atomic claim prevents every archive byte, even between watcher polls.
	delivering, err := root.Redis.Eval(ctx, chatExportTransitionScript, []string{jobKey}, "running", "delivering").Int()
	if err != nil {
		return chatExportError(r, err)
	}
	if delivering != 1 {
		return chatExportError(r, errChatExportCancelled)
	}
	response := &chatExportDownload{Reader: archive.File, archive: archive, cleanup: func() {
		defer func() { <-chatExportArchives }()
		defer releaseOwner()
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = root.Redis.Eval(cleanupCtx, chatExportTransitionScript, []string{jobKey}, "delivering", "finished").Err()
	}}
	response.expireAfter(chatExportStreamDuration)
	archiveOwnedByResponse = true
	archive = nil // Response owns cleanup, including a client-aborted stream.
	r.RequestCtx.Response.Header.Set("Content-Type", "application/zip")
	r.RequestCtx.Response.Header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="rereply-chats-%s.zip"`, time.Now().UTC().Format("20060102-150405")))
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store, private")
	r.RequestCtx.Response.Header.Set("X-Content-Type-Options", "nosniff")
	r.RequestCtx.Response.SetBodyStream(response, int(info.Size()))
	return nil
}

func (a *App) revalidateChatExport(ctx context.Context, orgID, userID uuid.UUID, req chatExportRequest, initialAccess chatExportAccess, messageIDs []uuid.UUID) error {
	return a.chatExportReadTransaction(ctx, orgID, false, func(scoped *App) error {
		current, e := scoped.chatExportAccess(orgID, userID)
		if e != nil {
			return e
		}
		if !initialAccess.MaskPhones && current.MaskPhones {
			return errChatExportScopeChanged
		}
		// One statement observes assignment/account/history access for the whole
		// archive at one instant. JSON binds one bounded parameter rather than
		// exceeding PostgreSQL's parameter limit for a large export.
		ids, e := json.Marshal(messageIDs)
		if e != nil {
			return e
		}
		var count int64
		if e = scoped.chatExportQuery(orgID, userID, req, current).Where("m.id IN (SELECT jsonb_array_elements_text(?::jsonb)::uuid)", string(ids)).Count(&count).Error; e != nil {
			return e
		}
		if count != int64(len(messageIDs)) {
			return errChatExportScopeChanged
		}
		latest, e := scoped.chatExportAccess(orgID, userID)
		if e != nil {
			return e
		}
		if latest != current {
			return errChatExportScopeChanged
		}
		return nil
	})
}
