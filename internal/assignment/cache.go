package assignment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

const (
	teamCachePrefix = "team:assignment:"
	teamCacheTTL    = 6 * time.Hour
)

// TeamConfig holds the cached team metadata used for assignment decisions.
type TeamConfig struct {
	Strategy            models.AssignmentStrategy `json:"strategy"`
	PerAgentTimeoutSecs int                       `json:"per_agent_timeout_secs"`
	MemberIDs           []uuid.UUID               `json:"member_ids"` // agent-role members only
}

// GetTeamConfig returns the team's assignment config, reading from cache first.
func (a *Assigner) GetTeamConfig(teamID uuid.UUID) *TeamConfig {
	cfg, _ := a.getTeamConfig(teamID, false)
	return cfg
}

// checkMemberError keeps the legacy accessor's best-effort member loading while
// checked chat selection refuses a failed read before publishing a cache entry.
func (a *Assigner) getTeamConfig(teamID uuid.UUID, checkMemberError bool) (*TeamConfig, error) {
	ctx := context.Background()
	key := teamCachePrefix + teamID.String()

	// Try cache first
	cached, err := a.redis.Get(ctx, key).Result()
	if err == nil && cached != "" {
		var cfg TeamConfig
		if err := json.Unmarshal([]byte(cached), &cfg); err == nil {
			return &cfg, nil
		}
	}

	// Cache miss — load from DB
	var team models.Team
	if err := a.db.Where("id = ? AND is_active = ?", teamID, true).First(&team).Error; err != nil {
		a.log.Error("Failed to get team for assignment", "error", err, "team_id", teamID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("read assignment team: %w", err)
	}

	// Get agent-role member IDs
	var memberIDs []uuid.UUID
	err = a.db.Table("team_members").
		Select("user_id").
		Where("team_id = ? AND role = ? AND deleted_at IS NULL", teamID, models.TeamRoleAgent).
		Pluck("user_id", &memberIDs).Error
	if err != nil && checkMemberError {
		return nil, fmt.Errorf("read assignment team members: %w", err)
	}

	cfg := &TeamConfig{
		Strategy:            team.AssignmentStrategy,
		PerAgentTimeoutSecs: team.PerAgentTimeoutSecs,
		MemberIDs:           memberIDs,
	}

	// Store in cache
	data, err := json.Marshal(cfg)
	if err == nil {
		a.redis.Set(ctx, key, string(data), teamCacheTTL)
	}

	return cfg, nil
}
