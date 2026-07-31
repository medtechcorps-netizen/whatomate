package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

const (
	// Cache TTLs based on granularity
	metaAnalyticsCacheHalfHourTTL = 1 * time.Hour
	metaAnalyticsCacheDayTTL      = 3 * time.Hour
	metaAnalyticsCacheMonthTTL    = 6 * time.Hour

	// Cache key prefix
	metaAnalyticsCachePrefix = "meta:analytics:"
)

// MetaAnalyticsRequest represents the request parameters for Meta analytics
type MetaAnalyticsRequest struct {
	AccountID     string `json:"account_id"`     // Optional: specific account ID or empty for all
	AnalyticsType string `json:"analytics_type"` // Required: analytics, pricing_analytics, template_analytics, call_analytics
	Start         string `json:"start"`          // Required: YYYY-MM-DD format
	End           string `json:"end"`            // Required: YYYY-MM-DD format
	Granularity   string `json:"granularity"`    // Optional: HALF_HOUR, DAY, MONTH (default: DAY)
}

// MetaAnalyticsResponse represents the response for Meta analytics
type MetaAnalyticsResponse struct {
	AccountID     string                          `json:"account_id"`
	AccountName   string                          `json:"account_name"`
	Data          *whatsapp.MetaAnalyticsResponse `json:"data"`
	TemplateNames map[string]string               `json:"template_names,omitempty"` // meta_template_id -> template name
	IsMock        bool                            `json:"is_mock"`
}

type metaAnalyticsLogicalAccount struct {
	ID                uuid.UUID
	Name              string
	PhoneID           string
	IsProviderAccount bool
	HasSnapshot       bool
	IsMock            bool
}

type metaAnalyticsAccountProjection struct {
	ID      uuid.UUID
	Name    string
	PhoneID string
}

// GetMetaAnalytics fetches Meta WhatsApp analytics with Redis caching. Logical
// accounts backed by snapshots are always served locally and never fall
// through to account secret decryption or the Meta client.
func (a *App) GetMetaAnalytics(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	// Check permission
	if !a.HasPermission(userID, "analytics", "read", orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Permission denied", nil, "")
	}

	// Parse request parameters
	accountID := string(r.RequestCtx.QueryArgs().Peek("account_id"))
	analyticsType := string(r.RequestCtx.QueryArgs().Peek("analytics_type"))
	startStr := string(r.RequestCtx.QueryArgs().Peek("start"))
	endStr := string(r.RequestCtx.QueryArgs().Peek("end"))
	granularity := string(r.RequestCtx.QueryArgs().Peek("granularity"))

	// Validate required parameters
	if analyticsType == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "analytics_type is required", nil, "")
	}
	if !whatsapp.ValidateAnalyticsType(analyticsType) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid analytics_type. Must be one of: analytics, pricing_analytics, template_analytics, call_analytics", nil, "")
	}
	if startStr == "" || endStr == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "start and end dates are required (YYYY-MM-DD format)", nil, "")
	}

	// Parse dates
	startDate, endDate, errMsg := parseDateRange(startStr, endStr)
	if errMsg != "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, errMsg, nil, "")
	}

	// Validate date range
	if endDate.Before(startDate) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "End date must be after start date", nil, "")
	}

	// Set default granularity (use DAY as standard input, will be normalized per endpoint)
	if granularity == "" {
		granularity = "DAY"
	}
	if !whatsapp.ValidateGranularity(granularity) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid granularity. Must be one of: HALF_HOUR, DAY, MONTH", nil, "")
	}

	originalGranularity := granularity
	switch granularity {
	case "DAILY":
		granularity = "DAY"
	case "MONTHLY":
		granularity = "MONTH"
	}

	// Auto-adjust granularity based on date range to avoid Meta API errors
	inclusiveDays := int(endDate.Sub(startDate).Hours()/24) + 1

	// MONTHLY requires at least 30 days
	if granularity == "MONTH" && inclusiveDays < 30 {
		granularity = "DAY"
		a.Log.Debug("Auto-adjusted granularity from MONTH to DAY due to small date range",
			"days", inclusiveDays,
			"original", originalGranularity,
		)
	}

	// HALF_HOUR only makes sense for ranges up to 7 days (too much data otherwise)
	if granularity == "HALF_HOUR" && inclusiveDays > 7 {
		granularity = "DAY"
		a.Log.Debug("Auto-adjusted granularity from HALF_HOUR to DAY due to large date range",
			"days", inclusiveDays,
			"original", originalGranularity,
		)
	}

	// Template analytics have a 90-day lookback limit
	if analyticsType == string(whatsapp.AnalyticsTypeTemplate) {
		nowUTC := time.Now().UTC()
		ninetyDaysAgo := time.Date(
			nowUTC.Year(),
			nowUTC.Month(),
			nowUTC.Day(),
			0,
			0,
			0,
			0,
			time.UTC,
		).AddDate(0, 0, -90)
		if startDate.Before(ninetyDaysAgo) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Template analytics have a 90-day lookback limit", nil, "")
		}
		granularity = "DAY"
	}

	// Convert dates to Unix timestamps
	startUnix := startDate.Unix()
	endUnix := endDate.Unix()

	var requestedTemplateIDs []string
	if analyticsType == string(whatsapp.AnalyticsTypeTemplate) {
		requestedTemplateIDs, err = parseRequestedMetaTemplateIDs(
			string(r.RequestCtx.QueryArgs().Peek("template_ids")),
		)
		if err != nil {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"template_ids must be a JSON array of at most 100 numeric template IDs",
				nil,
				"",
			)
		}
	}

	// Resolve both provider-backed accounts and snapshot-only logical accounts.
	accounts, err := a.loadMetaAnalyticsLogicalAccounts(orgID, accountID)
	if err != nil {
		a.Log.Error("Failed to fetch analytics accounts", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to fetch accounts", nil, "")
	}

	if len(accounts) == 0 {
		if accountID != "" {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Account not found", nil, "")
		}
		return r.SendEnvelope(map[string]any{
			"accounts": []MetaAnalyticsResponse{},
			"message":  "No WhatsApp accounts found",
			"demo_data": false,
		})
	}
	demoData := logicalAccountsContainMockData(accounts)
	snapshotBackedRequest := logicalAccountsContainSnapshots(accounts)

	// Build cache key
	cacheKey := a.buildMetaAnalyticsCacheKey(
		orgID,
		accountID,
		analyticsType,
		startUnix,
		endUnix,
		granularity,
		requestedTemplateIDs,
	)

	// Snapshot-backed requests always read the database source of truth. They
	// deliberately bypass Redis so a stale provider response can never mask a
	// newly introduced fixture for the same logical account.
	ctx := context.Background()
	if !snapshotBackedRequest {
		cached, cacheErr := a.Redis.Get(ctx, cacheKey).Result()
		if cacheErr == nil && cached != "" {
			var cachedResponse []MetaAnalyticsResponse
			if err := json.Unmarshal([]byte(cached), &cachedResponse); err == nil {
				a.Log.Debug("Meta analytics cache hit", "cache_key", cacheKey)
				response := map[string]any{
					"accounts":  cachedResponse,
					"cached":    true,
					"demo_data": demoData || metaAnalyticsResponsesContainMockData(cachedResponse),
				}
				if granularity != originalGranularity {
					response["adjusted_granularity"] = granularity
					response["original_granularity"] = originalGranularity
				}
				return r.SendEnvelope(response)
			}
		}
	}

	// Cache miss - fetch from Meta API
	a.Log.Debug("Meta analytics cache miss", "cache_key", cacheKey)

	snapshotsByAccount, err := a.loadMetaAnalyticsSnapshots(
		orgID,
		accountID,
		analyticsType,
		granularity,
		startDate,
		time.Unix(endUnix, 0).UTC(),
	)
	if err != nil {
		a.Log.Error("Failed to fetch Meta analytics snapshots", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to fetch analytics snapshots", nil, "")
	}

	var results []MetaAnalyticsResponse
	for _, logicalAccount := range accounts {
		accountSnapshots := snapshotsByAccount[logicalAccount.ID]
		if len(accountSnapshots) > 0 {
			data, templateNames, snapshotErr := mergeMetaAnalyticsSnapshots(
				accountSnapshots,
				analyticsType,
				startUnix,
				endUnix,
				requestedTemplateIDs,
			)
			if snapshotErr != nil {
				a.Log.Error(
					"Failed to decode Meta analytics snapshot",
					"error", snapshotErr,
					"account_id", logicalAccount.ID,
					"analytics_type", analyticsType,
				)
				return r.SendErrorEnvelope(
					fasthttp.StatusInternalServerError,
					"Stored analytics snapshot is invalid",
					nil,
					"",
				)
			}
			results = append(results, MetaAnalyticsResponse{
				AccountID:     logicalAccount.ID.String(),
				AccountName:   logicalAccount.Name,
				Data:          data,
				TemplateNames: templateNames,
				IsMock:        logicalAccount.IsMock,
			})
			continue
		}

		// Mock logical accounts are fail-closed: a missing type/range snapshot
		// returns no data and never falls through to a provider account.
		if logicalAccount.IsMock {
			results = append(results, MetaAnalyticsResponse{
				AccountID:   logicalAccount.ID.String(),
				AccountName: logicalAccount.Name,
				Data:        nil,
				IsMock:      true,
			})
			continue
		}

		if !logicalAccount.IsProviderAccount {
			continue
		}

		// Secret-bearing columns are fetched only for a provider request, never
		// while listing or resolving logical snapshot accounts.
		var account models.WhatsAppAccount
		if err := a.DB.
			Where("id = ? AND organization_id = ?", logicalAccount.ID, orgID).
			First(&account).Error; err != nil {
			a.Log.Error("Failed to load provider account", "error", err, "account_id", logicalAccount.ID)
			return r.SendErrorEnvelope(
				fasthttp.StatusInternalServerError,
				"Failed to load analytics account",
				nil,
				"",
			)
		}
		a.decryptAccountSecrets(&account)
		waAccount := a.toWhatsAppAccount(&account)

		req := &whatsapp.AnalyticsRequest{
			Start:       startUnix,
			End:         endUnix,
			Granularity: granularity,
		}

		// Get template IDs if this is template analytics (not template_group_analytics)
		// Note: template_group_analytics requires template_group_ids which are different from template IDs
		if analyticsType == string(whatsapp.AnalyticsTypeTemplate) {
			req.TemplateIDs = append(req.TemplateIDs, requestedTemplateIDs...)

			// If no template IDs provided, fetch from database
			if len(req.TemplateIDs) == 0 {
				var templates []models.Template
				if err := a.DB.Select("meta_template_id").
					Where("organization_id = ? AND whats_app_account = ? AND meta_template_id != '' AND meta_template_id IS NOT NULL",
						orgID, account.Name).
					Find(&templates).Error; err == nil {
					for _, t := range templates {
						if t.MetaTemplateID != "" {
							req.TemplateIDs = append(req.TemplateIDs, t.MetaTemplateID)
						}
					}
				}
				req.TemplateIDs, err = canonicalMetaTemplateIDs(req.TemplateIDs)
				if err != nil {
					a.Log.Error("Stored Meta template ID is invalid", "error", err, "account_id", account.ID)
					return r.SendErrorEnvelope(
						fasthttp.StatusInternalServerError,
						"Stored analytics template configuration is invalid",
						nil,
						"",
					)
				}
				a.Log.Debug("Auto-fetched template IDs for analytics",
					"account_name", account.Name,
					"template_count", len(req.TemplateIDs),
				)
			}

			// Skip if no templates found
			if len(req.TemplateIDs) == 0 {
				a.Log.Debug("No templates found for account, skipping template analytics",
					"account_id", account.ID,
					"account_name", account.Name,
				)
				results = append(results, MetaAnalyticsResponse{
					AccountID:   account.ID.String(),
					AccountName: account.Name,
					Data:        nil,
					IsMock:      false,
				})
				continue
			}
		}

		data, err := a.WhatsApp.GetAnalytics(ctx, waAccount, whatsapp.AnalyticsType(analyticsType), req)
		if err != nil {
			a.Log.Error("Failed to fetch Meta analytics",
				"error", err,
				"account_id", account.ID,
				"account_name", account.Name,
				"analytics_type", analyticsType,
				"start", startUnix,
				"end", endUnix,
				"granularity", granularity,
			)
			// Continue with other accounts
			results = append(results, MetaAnalyticsResponse{
				AccountID:   account.ID.String(),
				AccountName: account.Name,
				Data:        nil,
				IsMock:      false,
			})
			continue
		}

		// Log successful fetch with data point count
		dataPointCount := 0
		if data != nil {
			switch analyticsType {
			case string(whatsapp.AnalyticsTypeMessaging):
				if data.Analytics != nil {
					dataPointCount = len(data.Analytics.DataPoints)
				}
			case string(whatsapp.AnalyticsTypePricing):
				if data.PricingAnalytics != nil {
					dataPointCount = len(data.PricingAnalytics.DataPoints)
				}
			case string(whatsapp.AnalyticsTypeTemplate):
				if data.TemplateAnalytics != nil {
					dataPointCount = len(data.TemplateAnalytics.DataPoints)
				}
			case string(whatsapp.AnalyticsTypeCall):
				if data.CallAnalytics != nil {
					dataPointCount = len(data.CallAnalytics.DataPoints)
				}
			}
		}
		a.Log.Debug("Meta analytics fetched successfully",
			"account_id", account.ID,
			"account_name", account.Name,
			"analytics_type", analyticsType,
			"data_points", dataPointCount,
		)

		// Build template names map if this is template analytics
		var templateNames map[string]string
		if analyticsType == string(whatsapp.AnalyticsTypeTemplate) && data != nil {
			templateNames = make(map[string]string)

			// Get unique template IDs from the analytics data using a map for deduplication
			templateIDSet := make(map[string]struct{})
			if data.TemplateAnalytics != nil {
				for _, dp := range data.TemplateAnalytics.DataPoints {
					if dp.TemplateID != "" {
						templateIDSet[dp.TemplateID] = struct{}{}
					}
				}
			}

			// Convert set to slice
			templateIDs := make([]string, 0, len(templateIDSet))
			for id := range templateIDSet {
				templateIDs = append(templateIDs, id)
			}

			// Fetch template names from database
			if len(templateIDs) > 0 {
				var templates []models.Template
				if err := a.DB.Select("meta_template_id, name, display_name").
					Where("organization_id = ? AND meta_template_id IN ?", orgID, templateIDs).
					Find(&templates).Error; err == nil {
					for _, t := range templates {
						name := t.DisplayName
						if name == "" {
							name = t.Name
						}
						templateNames[t.MetaTemplateID] = name
					}
				}
			}
		}

		results = append(results, MetaAnalyticsResponse{
			AccountID:     account.ID.String(),
			AccountName:   account.Name,
			Data:          data,
			TemplateNames: templateNames,
			IsMock:        false,
		})
	}

	// Provider-only requests retain the existing Redis cache. Snapshot-backed
	// results are intentionally never written to Redis.
	if !snapshotBackedRequest {
		cacheTTL := a.getMetaAnalyticsCacheTTL(granularity)
		if cacheData, err := json.Marshal(results); err == nil {
			if err := a.Redis.Set(ctx, cacheKey, cacheData, cacheTTL).Err(); err != nil {
				a.Log.Error("Failed to cache Meta analytics", "error", err, "cache_key", cacheKey)
			}
		}
	}

	response := map[string]any{
		"accounts":  results,
		"cached":    false,
		"demo_data": demoData || metaAnalyticsResponsesContainMockData(results),
	}

	// Include adjusted granularity if it was changed
	if granularity != originalGranularity {
		response["adjusted_granularity"] = granularity
		response["original_granularity"] = originalGranularity
	}

	return r.SendEnvelope(response)
}

func (a *App) loadMetaAnalyticsLogicalAccounts(
	orgID uuid.UUID,
	accountID string,
) ([]metaAnalyticsLogicalAccount, error) {
	var selectedAccountID *uuid.UUID
	if accountID != "" {
		parsed, err := uuid.Parse(accountID)
		if err != nil {
			return []metaAnalyticsLogicalAccount{}, nil
		}
		selectedAccountID = &parsed
	}

	var realAccounts []metaAnalyticsAccountProjection
	realQuery := a.DB.
		Model(&models.WhatsAppAccount{}).
		Select("id, name, phone_id").
		Where("organization_id = ?", orgID)
	if selectedAccountID != nil {
		realQuery = realQuery.Where("id = ?", *selectedAccountID)
	}
	if err := realQuery.Order("name ASC").Find(&realAccounts).Error; err != nil {
		return nil, err
	}

	var snapshotRows []models.MetaAnalyticsSnapshot
	snapshotQuery := a.DB.
		Select("account_id, account_name, phone_id, is_mock").
		Where("organization_id = ?", orgID)
	if selectedAccountID != nil {
		snapshotQuery = snapshotQuery.Where("account_id = ?", *selectedAccountID)
	}
	if err := snapshotQuery.
		Order("account_name ASC, account_id ASC").
		Find(&snapshotRows).Error; err != nil {
		return nil, err
	}

	logicalAccounts := make([]metaAnalyticsLogicalAccount, 0, len(realAccounts)+len(snapshotRows))
	accountIndex := make(map[uuid.UUID]int, len(realAccounts)+len(snapshotRows))
	for i := range realAccounts {
		account := realAccounts[i]
		accountIndex[account.ID] = len(logicalAccounts)
		logicalAccounts = append(logicalAccounts, metaAnalyticsLogicalAccount{
			ID:                account.ID,
			Name:              account.Name,
			PhoneID:           account.PhoneID,
			IsProviderAccount: true,
		})
	}

	for _, snapshot := range snapshotRows {
		if index, exists := accountIndex[snapshot.AccountID]; exists {
			logicalAccounts[index].HasSnapshot = true
			logicalAccounts[index].IsMock = logicalAccounts[index].IsMock || snapshot.IsMock
			if logicalAccounts[index].Name == "" {
				logicalAccounts[index].Name = snapshot.AccountName
			}
			if logicalAccounts[index].PhoneID == "" {
				logicalAccounts[index].PhoneID = snapshot.PhoneID
			}
			continue
		}

		accountIndex[snapshot.AccountID] = len(logicalAccounts)
		logicalAccounts = append(logicalAccounts, metaAnalyticsLogicalAccount{
			ID:          snapshot.AccountID,
			Name:        snapshot.AccountName,
			PhoneID:     snapshot.PhoneID,
			HasSnapshot: true,
			IsMock:      snapshot.IsMock,
		})
	}

	sort.SliceStable(logicalAccounts, func(i, j int) bool {
		if logicalAccounts[i].Name == logicalAccounts[j].Name {
			return logicalAccounts[i].ID.String() < logicalAccounts[j].ID.String()
		}
		return logicalAccounts[i].Name < logicalAccounts[j].Name
	})
	return logicalAccounts, nil
}

func (a *App) loadMetaAnalyticsSnapshots(
	orgID uuid.UUID,
	accountID string,
	analyticsType string,
	granularity string,
	periodStart time.Time,
	periodEnd time.Time,
) (map[uuid.UUID][]models.MetaAnalyticsSnapshot, error) {
	query := a.DB.Where(
		"organization_id = ? AND analytics_type = ? AND granularity = ? AND period_start <= ? AND period_end >= ?",
		orgID,
		analyticsType,
		granularity,
		periodEnd,
		periodStart,
	)
	if accountID != "" {
		parsed, err := uuid.Parse(accountID)
		if err != nil {
			return map[uuid.UUID][]models.MetaAnalyticsSnapshot{}, nil
		}
		query = query.Where("account_id = ?", parsed)
	}

	var snapshots []models.MetaAnalyticsSnapshot
	if err := query.
		Order("account_id ASC, period_start ASC, updated_at ASC, id ASC").
		Find(&snapshots).Error; err != nil {
		return nil, err
	}

	byAccount := make(map[uuid.UUID][]models.MetaAnalyticsSnapshot)
	for _, snapshot := range snapshots {
		byAccount[snapshot.AccountID] = append(byAccount[snapshot.AccountID], snapshot)
	}
	return byAccount, nil
}

func logicalAccountsContainMockData(accounts []metaAnalyticsLogicalAccount) bool {
	for _, account := range accounts {
		if account.IsMock {
			return true
		}
	}
	return false
}

func logicalAccountsContainSnapshots(accounts []metaAnalyticsLogicalAccount) bool {
	for _, account := range accounts {
		if account.HasSnapshot {
			return true
		}
	}
	return false
}

func metaAnalyticsResponsesContainMockData(responses []MetaAnalyticsResponse) bool {
	for _, response := range responses {
		if response.IsMock {
			return true
		}
	}
	return false
}

func mergeMetaAnalyticsSnapshots(
	snapshots []models.MetaAnalyticsSnapshot,
	analyticsType string,
	periodStart int64,
	periodEnd int64,
	requestedTemplateIDs []string,
) (*whatsapp.MetaAnalyticsResponse, map[string]string, error) {
	if len(snapshots) == 0 {
		return nil, nil, nil
	}

	type messagingKey struct {
		Start int64
		End   int64
	}
	type pricingKey struct {
		Start           int64
		End             int64
		Country         string
		PricingType     string
		PricingCategory string
		Tier            string
	}
	type templateKey struct {
		TemplateID string
		Start      int64
		End        int64
	}
	type callKey struct {
		Start     int64
		End       int64
		Direction string
	}

	merged := &whatsapp.MetaAnalyticsResponse{}
	templateNames := make(map[string]string)
	messagingPoints := make(map[messagingKey]whatsapp.MessagingAnalyticsDataPoint)
	pricingPoints := make(map[pricingKey]whatsapp.PricingAnalyticsDataPoint)
	templatePoints := make(map[templateKey]whatsapp.TemplateAnalyticsDataPoint)
	callPoints := make(map[callKey]whatsapp.CallAnalyticsDataPoint)
	hasAnalyticsPayload := false
	requestedTemplateIDSet := make(map[string]struct{}, len(requestedTemplateIDs))
	for _, templateID := range requestedTemplateIDs {
		requestedTemplateIDSet[templateID] = struct{}{}
	}

	for _, snapshot := range snapshots {
		encoded, err := json.Marshal(snapshot.Payload)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal snapshot %s: %w", snapshot.ID, err)
		}
		var data whatsapp.MetaAnalyticsResponse
		if err := json.Unmarshal(encoded, &data); err != nil {
			return nil, nil, fmt.Errorf("decode snapshot %s: %w", snapshot.ID, err)
		}
		if merged.ID == "" && data.ID != "" {
			merged.ID = data.ID
		}

		for templateID, value := range snapshot.TemplateNames {
			if name, ok := value.(string); ok && name != "" {
				templateNames[templateID] = name
			}
		}

		switch analyticsType {
		case string(whatsapp.AnalyticsTypeMessaging):
			if data.Analytics == nil {
				continue
			}
			hasAnalyticsPayload = true
			for _, point := range data.Analytics.DataPoints {
				if metaAnalyticsPointOverlaps(point.Start, point.End, periodStart, periodEnd) {
					messagingPoints[messagingKey{Start: point.Start, End: point.End}] = point
				}
			}
		case string(whatsapp.AnalyticsTypePricing):
			if data.PricingAnalytics == nil {
				continue
			}
			hasAnalyticsPayload = true
			for _, point := range data.PricingAnalytics.DataPoints {
				if !metaAnalyticsPointOverlaps(point.Start, point.End, periodStart, periodEnd) {
					continue
				}
				key := pricingKey{
					Start:           point.Start,
					End:             point.End,
					Country:         point.Country,
					PricingType:     point.PricingType,
					PricingCategory: point.PricingCategory,
					Tier:            point.Tier,
				}
				pricingPoints[key] = point
			}
		case string(whatsapp.AnalyticsTypeTemplate):
			if data.TemplateAnalytics == nil {
				continue
			}
			hasAnalyticsPayload = true
			for _, point := range data.TemplateAnalytics.DataPoints {
				if !metaAnalyticsPointOverlaps(point.Start, point.End, periodStart, periodEnd) {
					continue
				}
				if len(requestedTemplateIDSet) > 0 {
					if _, requested := requestedTemplateIDSet[point.TemplateID]; !requested {
						continue
					}
				}
				templatePoints[templateKey{
					TemplateID: point.TemplateID,
					Start:      point.Start,
					End:        point.End,
				}] = point
			}
		case string(whatsapp.AnalyticsTypeCall):
			if data.CallAnalytics == nil {
				continue
			}
			hasAnalyticsPayload = true
			for _, point := range data.CallAnalytics.DataPoints {
				if !metaAnalyticsPointOverlaps(point.Start, point.End, periodStart, periodEnd) {
					continue
				}
				callPoints[callKey{
					Start:     point.Start,
					End:       point.End,
					Direction: point.Direction,
				}] = point
			}
		}
	}

	if !hasAnalyticsPayload {
		return nil, emptyMapAsNil(templateNames), nil
	}

	switch analyticsType {
	case string(whatsapp.AnalyticsTypeMessaging):
		points := make([]whatsapp.MessagingAnalyticsDataPoint, 0, len(messagingPoints))
		for _, point := range messagingPoints {
			points = append(points, point)
		}
		sort.Slice(points, func(i, j int) bool {
			if points[i].Start == points[j].Start {
				return points[i].End < points[j].End
			}
			return points[i].Start < points[j].Start
		})
		merged.Analytics = &whatsapp.MessagingAnalytics{
			Granularity: snapshots[0].Granularity,
			DataPoints:  points,
		}
	case string(whatsapp.AnalyticsTypePricing):
		points := make([]whatsapp.PricingAnalyticsDataPoint, 0, len(pricingPoints))
		for _, point := range pricingPoints {
			points = append(points, point)
		}
		sort.Slice(points, func(i, j int) bool {
			if points[i].Start != points[j].Start {
				return points[i].Start < points[j].Start
			}
			if points[i].PricingCategory != points[j].PricingCategory {
				return points[i].PricingCategory < points[j].PricingCategory
			}
			if points[i].PricingType != points[j].PricingType {
				return points[i].PricingType < points[j].PricingType
			}
			return points[i].Country < points[j].Country
		})
		merged.PricingAnalytics = &whatsapp.PricingAnalytics{
			Granularity: snapshots[0].Granularity,
			DataPoints:  points,
		}
	case string(whatsapp.AnalyticsTypeTemplate):
		points := make([]whatsapp.TemplateAnalyticsDataPoint, 0, len(templatePoints))
		for _, point := range templatePoints {
			points = append(points, point)
		}
		sort.Slice(points, func(i, j int) bool {
			if points[i].Start != points[j].Start {
				return points[i].Start < points[j].Start
			}
			return points[i].TemplateID < points[j].TemplateID
		})
		merged.TemplateAnalytics = &whatsapp.TemplateAnalytics{
			Granularity: snapshots[0].Granularity,
			DataPoints:  points,
		}
	case string(whatsapp.AnalyticsTypeCall):
		points := make([]whatsapp.CallAnalyticsDataPoint, 0, len(callPoints))
		for _, point := range callPoints {
			points = append(points, point)
		}
		sort.Slice(points, func(i, j int) bool {
			if points[i].Start != points[j].Start {
				return points[i].Start < points[j].Start
			}
			return points[i].Direction < points[j].Direction
		})
		merged.CallAnalytics = &whatsapp.CallAnalytics{
			Granularity: snapshots[0].Granularity,
			DataPoints:  points,
		}
	}

	return merged, emptyMapAsNil(templateNames), nil
}

func metaAnalyticsPointOverlaps(start, end, periodStart, periodEnd int64) bool {
	if end == 0 || end == start {
		return start >= periodStart && start <= periodEnd
	}
	// Meta interval endpoints are half-open: a point ending exactly when the
	// requested period starts belongs to the preceding interval.
	return start <= periodEnd && end > periodStart
}

func emptyMapAsNil(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	return values
}

// ListMetaAccountsForAnalytics lists WhatsApp accounts available for analytics
func (a *App) ListMetaAccountsForAnalytics(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	// Check permission
	if !a.HasPermission(userID, "analytics", "read", orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Permission denied", nil, "")
	}

	type AccountInfo struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		PhoneID string `json:"phone_id"`
		IsMock  bool   `json:"is_mock"`
	}

	accounts, err := a.loadMetaAnalyticsLogicalAccounts(orgID, "")
	if err != nil {
		a.Log.Error("Failed to fetch accounts", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to fetch accounts", nil, "")
	}

	result := make([]AccountInfo, 0, len(accounts))
	for _, acc := range accounts {
		result = append(result, AccountInfo{
			ID:      acc.ID.String(),
			Name:    acc.Name,
			PhoneID: acc.PhoneID,
			IsMock:  acc.IsMock,
		})
	}

	return r.SendEnvelope(map[string]any{
		"accounts":  result,
		"demo_data": logicalAccountsContainMockData(accounts),
	})
}

// RefreshMetaAnalyticsCache invalidates the cache for Meta analytics
func (a *App) RefreshMetaAnalyticsCache(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	// Check permission
	if !a.HasPermission(userID, "analytics", "write", orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Permission denied", nil, "")
	}

	// Delete all cached analytics for this organization
	ctx := context.Background()
	pattern := fmt.Sprintf("%s%s:*", metaAnalyticsCachePrefix, orgID.String())
	a.deleteKeysByPattern(ctx, pattern)

	return r.SendEnvelope(map[string]any{
		"message": "Analytics cache cleared successfully",
	})
}

// buildMetaAnalyticsCacheKey builds a cache key for Meta analytics
func (a *App) buildMetaAnalyticsCacheKey(
	orgID uuid.UUID,
	accountID string,
	analyticsType string,
	start int64,
	end int64,
	granularity string,
	templateIDs []string,
) string {
	if accountID == "" {
		accountID = "all"
	}
	templateScope := "auto"
	if len(templateIDs) > 0 {
		templateScope = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(templateIDs, ","))))
	}
	return fmt.Sprintf("%s%s:%s:%s:%d:%d:%s:%s",
		metaAnalyticsCachePrefix,
		orgID.String(),
		accountID,
		analyticsType,
		start,
		end,
		granularity,
		templateScope,
	)
}

func parseRequestedMetaTemplateIDs(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var templateIDs []string
	if err := json.Unmarshal([]byte(raw), &templateIDs); err != nil {
		return nil, err
	}
	return canonicalMetaTemplateIDs(templateIDs)
}

func canonicalMetaTemplateIDs(templateIDs []string) ([]string, error) {
	if len(templateIDs) > 100 {
		return nil, fmt.Errorf("too many template IDs")
	}
	seen := make(map[string]struct{}, len(templateIDs))
	canonical := make([]string, 0, len(templateIDs))
	for _, rawID := range templateIDs {
		templateID := strings.TrimSpace(rawID)
		if templateID == "" || len(templateID) > 32 {
			return nil, fmt.Errorf("invalid template ID length")
		}
		for _, character := range templateID {
			if character < '0' || character > '9' {
				return nil, fmt.Errorf("template ID %q is not numeric", templateID)
			}
		}
		if _, exists := seen[templateID]; exists {
			continue
		}
		seen[templateID] = struct{}{}
		canonical = append(canonical, templateID)
	}
	sort.Strings(canonical)
	return canonical, nil
}

// getMetaAnalyticsCacheTTL returns the appropriate cache TTL based on granularity
func (a *App) getMetaAnalyticsCacheTTL(granularity string) time.Duration {
	switch granularity {
	case "HALF_HOUR":
		return metaAnalyticsCacheHalfHourTTL
	case "DAY":
		return metaAnalyticsCacheDayTTL
	case "MONTH":
		return metaAnalyticsCacheMonthTTL
	default:
		return metaAnalyticsCacheDayTTL
	}
}
