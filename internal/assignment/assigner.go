package assignment

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/zerodha/logf"
	"gorm.io/gorm"
)

// LoadCounter counts the number of active items per agent. Callers provide a
// domain-specific implementation: chat transfers count AgentTransfer rows while
// call transfers count CallTransfer rows.
type LoadCounter func(db *gorm.DB, orgID uuid.UUID, agentIDs []uuid.UUID) map[uuid.UUID]int64

// Assigner provides team-based agent assignment with caching.
type Assigner struct {
	db    *gorm.DB
	redis *redis.Client
	log   logf.Logger
}

// New creates a new Assigner.
func New(db *gorm.DB, redis *redis.Client, log logf.Logger) *Assigner {
	return &Assigner{db: db, redis: redis, log: log}
}

// WithDB returns an assigner that shares cache/log dependencies but performs
// database work through the supplied request-scoped transaction.
func (a *Assigner) WithDB(db *gorm.DB) *Assigner {
	if a == nil {
		return nil
	}
	return &Assigner{db: db, redis: a.redis, log: a.log}
}

// AssignToTeam dispatches to the appropriate strategy based on the team's
// AssignmentStrategy. excludeAgentIDs allows callers to skip agents that have
// already been tried (e.g., during call transfer rotation). Pass nil for normal
// use. loadCounter provides the domain-specific load counting function for the
// load_balanced strategy.
func (a *Assigner) AssignToTeam(teamID, orgID uuid.UUID, excludeAgentIDs []uuid.UUID, loadCounter LoadCounter) *uuid.UUID {
	cfg := a.GetTeamConfig(teamID)
	if cfg == nil {
		return nil
	}

	switch cfg.Strategy {
	case models.AssignmentStrategyRoundRobin:
		return a.assignRoundRobin(teamID, orgID, cfg.MemberIDs, excludeAgentIDs)
	case models.AssignmentStrategyLoadBalanced:
		return a.assignLoadBalanced(orgID, cfg.MemberIDs, excludeAgentIDs, loadCounter)
	case models.AssignmentStrategyManual:
		return nil
	default:
		return a.assignRoundRobin(teamID, orgID, cfg.MemberIDs, excludeAgentIDs)
	}
}

// AssignChatToTeamChecked selects an agent only after every required SQL operation
// succeeds. It preserves the original SQL error for the caller's retry policy;
// even a failed round-robin timestamp update returns no selected agent. The caller
// owns the transaction, its lock bounds, and whether a failure is retryable.
func (a *Assigner) AssignChatToTeamChecked(teamID, orgID uuid.UUID, excludeAgentIDs []uuid.UUID) (*uuid.UUID, error) {
	cfg, err := a.getTeamConfig(teamID, true)
	if err != nil || cfg == nil {
		return nil, err
	}
	if cfg.Strategy == models.AssignmentStrategyManual {
		return nil, nil
	}
	available, err := a.filterAvailableChecked(cfg.MemberIDs, excludeAgentIDs)
	if err != nil {
		return nil, fmt.Errorf("read team agent availability: %w", err)
	}
	if len(available) == 0 {
		return nil, nil
	}
	if cfg.Strategy == models.AssignmentStrategyLoadBalanced {
		loads, err := chatLoadCounts(a.db, orgID, available)
		if err != nil {
			return nil, fmt.Errorf("read team agent chat loads: %w", err)
		}
		return a.selectLeastLoaded(available, loads), nil
	}
	agentID, err := a.selectRoundRobin(teamID, available)
	if err != nil {
		return nil, err
	}
	return agentID, nil
}

// GetAvailableAgents returns the user IDs of available agents in the team,
// excluding the given IDs. Used for broadcast fallback after rotation exhausts
// individual agents.
func (a *Assigner) GetAvailableAgents(teamID uuid.UUID, excludeAgentIDs []uuid.UUID) []uuid.UUID {
	cfg := a.GetTeamConfig(teamID)
	if cfg == nil {
		return nil
	}

	return a.filterAvailable(cfg.MemberIDs, excludeAgentIDs)
}

// assignRoundRobin selects the available agent with the oldest last_assigned_at
// from the cached member list and updates their timestamp.
func (a *Assigner) assignRoundRobin(teamID, orgID uuid.UUID, memberIDs []uuid.UUID, excludeAgentIDs []uuid.UUID) *uuid.UUID {
	available := a.filterAvailable(memberIDs, excludeAgentIDs)
	if len(available) == 0 {
		a.log.Debug("No available agents for round-robin", "team_id", teamID)
		return nil
	}
	agentID, _ := a.selectRoundRobin(teamID, available)
	return agentID
}

func (a *Assigner) selectRoundRobin(teamID uuid.UUID, available []uuid.UUID) (*uuid.UUID, error) {
	// Among available agents, pick the one with oldest last_assigned_at
	var members []models.TeamMember
	err := a.db.
		Where("team_id = ? AND user_id IN ?", teamID, available).
		Order("last_assigned_at ASC NULLS FIRST").
		Find(&members).Error
	if err != nil {
		return nil, fmt.Errorf("read round-robin team members: %w", err)
	}
	if len(members) == 0 {
		a.log.Debug("No team members found for round-robin", "team_id", teamID)
		return nil, nil
	}

	selected := members[0]
	now := time.Now()
	err = a.db.Model(&selected).Update("last_assigned_at", now).Error

	a.log.Debug("Round-robin assigned to agent", "team_id", teamID, "user_id", selected.UserID)
	if err != nil {
		// The legacy wrapper retains its historical best-effort timestamp behavior;
		// the checked entry point discards this candidate when an error is returned.
		return &selected.UserID, fmt.Errorf("update round-robin assignment timestamp: %w", err)
	}
	return &selected.UserID, nil
}

// assignLoadBalanced selects the available agent with the fewest active items
// as counted by the provided LoadCounter.
func (a *Assigner) assignLoadBalanced(orgID uuid.UUID, memberIDs []uuid.UUID, excludeAgentIDs []uuid.UUID, loadCounter LoadCounter) *uuid.UUID {
	available := a.filterAvailable(memberIDs, excludeAgentIDs)
	if len(available) == 0 {
		a.log.Debug("No available agents for load-balanced")
		return nil
	}

	loadMap := loadCounter(a.db, orgID, available)
	return a.selectLeastLoaded(available, loadMap)
}

func (a *Assigner) selectLeastLoaded(available []uuid.UUID, loadMap map[uuid.UUID]int64) *uuid.UUID {
	var lowestUserID *uuid.UUID
	var lowestCount int64 = -1
	for _, uid := range available {
		count := loadMap[uid]
		if lowestCount < 0 || count < lowestCount {
			lowestCount = count
			id := uid
			lowestUserID = &id
		}
	}

	if lowestUserID != nil {
		a.log.Debug("Load-balanced assigned to agent", "user_id", *lowestUserID, "current_load", lowestCount)
	}
	return lowestUserID
}

// filterAvailable returns user IDs from memberIDs that are active, available,
// and not in the exclude list.
func (a *Assigner) filterAvailable(memberIDs []uuid.UUID, excludeAgentIDs []uuid.UUID) []uuid.UUID {
	available, _ := a.filterAvailableChecked(memberIDs, excludeAgentIDs)
	return available
}

func (a *Assigner) filterAvailableChecked(memberIDs []uuid.UUID, excludeAgentIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(memberIDs) == 0 {
		return nil, nil
	}

	// Build exclude set
	excludeSet := make(map[uuid.UUID]bool, len(excludeAgentIDs))
	for _, id := range excludeAgentIDs {
		excludeSet[id] = true
	}

	// Filter out excluded agents before querying
	candidates := make([]uuid.UUID, 0, len(memberIDs))
	for _, id := range memberIDs {
		if !excludeSet[id] {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Query DB for availability (this changes frequently, not cached)
	var availableIDs []uuid.UUID
	err := a.db.Model(&models.User{}).
		Select("id").
		Where("id IN ? AND is_available = ? AND is_active = ?", candidates, true, true).
		Pluck("id", &availableIDs).Error

	return availableIDs, err
}

// ResolvePerAgentTimeout returns the per-agent timeout in seconds using the
// resolution order: team (if > 0) → global default.
func ResolvePerAgentTimeout(teamTimeout int, _ int, globalDefault int) int {
	if teamTimeout > 0 {
		return teamTimeout
	}
	if globalDefault > 0 {
		return globalDefault
	}
	return 15 // fallback default
}

// InvalidateTeamCache removes the cached team config for the given team ID.
func (a *Assigner) InvalidateTeamCache(teamID uuid.UUID) {
	ctx := context.Background()
	key := teamCachePrefix + teamID.String()
	a.redis.Del(ctx, key)
	a.log.Debug("Invalidated team assignment cache", "team_id", teamID)
}
