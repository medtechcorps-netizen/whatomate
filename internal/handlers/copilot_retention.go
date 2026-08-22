package handlers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

const (
	defaultCopilotRetentionInterval  = time.Hour
	defaultCopilotRetentionBatchSize = 500
)

// CopilotRetentionProcessor permanently removes expired Copilot artifacts.
// The processor scopes every deletion to one organization so PostgreSQL RLS
// remains effective for background work. Deletions are idempotent, which makes
// concurrent application replicas safe: a second replica simply finds no rows.
type CopilotRetentionProcessor struct {
	app       *App
	interval  time.Duration
	batchSize int
	now       func() time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
	runMu    sync.Mutex
}

// NewCopilotRetentionProcessor creates the tenant-safe Copilot purge loop.
func NewCopilotRetentionProcessor(app *App, interval time.Duration) *CopilotRetentionProcessor {
	if interval <= 0 {
		interval = defaultCopilotRetentionInterval
	}
	return &CopilotRetentionProcessor{
		app:       app,
		interval:  interval,
		batchSize: defaultCopilotRetentionBatchSize,
		now: func() time.Time {
			return time.Now().UTC()
		},
		stopCh: make(chan struct{}),
	}
}

// Start purges once immediately and then repeats until cancellation.
func (p *CopilotRetentionProcessor) Start(ctx context.Context) {
	if p == nil || p.app == nil || p.app.DB == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.runAndLog(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.runAndLog(ctx)
		}
	}
}

func (p *CopilotRetentionProcessor) runAndLog(ctx context.Context) {
	purged, err := p.RunOnce(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.app.Log.Error("Copilot retention purge failed", "error", err)
		}
		return
	}
	if purged > 0 {
		p.app.Log.Info("Expired Copilot runs purged", "count", purged)
	}
}

// Stop is safe to call more than once.
func (p *CopilotRetentionProcessor) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
}

// RunOnce purges one bounded batch for every organization.
func (p *CopilotRetentionProcessor) RunOnce(ctx context.Context) (int64, error) {
	if p == nil || p.app == nil || p.app.DB == nil {
		return 0, errors.New("copilot retention processor requires an app database")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.runMu.Lock()
	defer p.runMu.Unlock()

	var organizationIDs []uuid.UUID
	if err := p.app.rootApp().DB.WithContext(ctx).
		Scopes(database.ExcludePlatformComplianceOrganizations).
		Model(&models.Organization{}).
		Order("id").
		Pluck("id", &organizationIDs).Error; err != nil {
		return 0, fmt.Errorf("list Copilot retention organizations: %w", err)
	}

	var total int64
	var runErrors []error
	for _, organizationID := range organizationIDs {
		purged, err := p.purgeOrganization(ctx, organizationID)
		total += purged
		if err != nil {
			runErrors = append(
				runErrors,
				fmt.Errorf("purge Copilot runs for organization %s: %w", organizationID, err),
			)
		}
	}
	return total, errors.Join(runErrors...)
}

func (p *CopilotRetentionProcessor) purgeOrganization(
	ctx context.Context,
	organizationID uuid.UUID,
) (int64, error) {
	if organizationID == uuid.Nil {
		return 0, errors.New("organization ID is required")
	}
	batchSize := p.batchSize
	if batchSize <= 0 {
		batchSize = defaultCopilotRetentionBatchSize
	}
	now := p.now().UTC()
	var purged int64
	err := p.app.WithTenantApp(organizationID, func(scoped *App) error {
		return scoped.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var runIDs []uuid.UUID
			if err := tx.Unscoped().Model(&models.CopilotRun{}).
				Where("organization_id = ? AND expires_at IS NOT NULL AND expires_at <= ?", organizationID, now).
				Order("expires_at ASC, id ASC").
				Limit(batchSize).
				Pluck("id", &runIDs).Error; err != nil {
				return fmt.Errorf("find expired runs: %w", err)
			}
			if len(runIDs) == 0 {
				return nil
			}

			if err := tx.Unscoped().
				Where("organization_id = ? AND run_id IN ?", organizationID, runIDs).
				Delete(&models.CopilotFeedback{}).Error; err != nil {
				return fmt.Errorf("delete expired run feedback: %w", err)
			}
			result := tx.Unscoped().
				Where("organization_id = ? AND id IN ?", organizationID, runIDs).
				Delete(&models.CopilotRun{})
			if result.Error != nil {
				return fmt.Errorf("delete expired runs: %w", result.Error)
			}
			purged = result.RowsAffected
			return nil
		})
	})
	return purged, err
}
