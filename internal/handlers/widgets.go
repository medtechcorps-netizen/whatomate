package handlers

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// WidgetRequest represents the request body for creating/updating a widget
type WidgetRequest struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	DataSource   string         `json:"data_source"`    // messages, contacts, campaigns, transfers, sessions
	Metric       string         `json:"metric"`         // count, sum, avg
	Field        string         `json:"field"`          // Field for sum/avg
	Filters      []FilterInput  `json:"filters"`        // Filter conditions
	DisplayType  string         `json:"display_type"`   // number, percentage, chart
	ChartType    string         `json:"chart_type"`     // line, bar, pie
	GroupByField string         `json:"group_by_field"` // Field to group by
	ShowChange   *bool          `json:"show_change"`
	Color        string         `json:"color"`
	Size         string         `json:"size"` // small, medium, large
	Config       map[string]any `json:"config"`
	IsShared     *bool          `json:"is_shared"`
	GridX        *int           `json:"grid_x"`
	GridY        *int           `json:"grid_y"`
	GridW        *int           `json:"grid_w"`
	GridH        *int           `json:"grid_h"`
}

// FilterInput represents a filter condition from the request
type FilterInput struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

// WidgetResponse represents the response for a widget
type WidgetResponse struct {
	ID           uuid.UUID      `json:"id"`
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	DataSource   string         `json:"data_source"`
	Metric       string         `json:"metric"`
	Field        string         `json:"field"`
	Filters      []FilterInput  `json:"filters"`
	DisplayType  string         `json:"display_type"`
	ChartType    string         `json:"chart_type"`
	GroupByField string         `json:"group_by_field"`
	ShowChange   bool           `json:"show_change"`
	Color        string         `json:"color"`
	Size         string         `json:"size"`
	DisplayOrder int            `json:"display_order"`
	GridX        int            `json:"grid_x"`
	GridY        int            `json:"grid_y"`
	GridW        int            `json:"grid_w"`
	GridH        int            `json:"grid_h"`
	Config       map[string]any `json:"config"`
	IsShared     bool           `json:"is_shared"`
	IsDefault    bool           `json:"is_default"`
	IsOwner      bool           `json:"is_owner"` // True if current user created this widget
	CreatedBy    string         `json:"created_by"`
	CreatedAt    string         `json:"created_at"`
	UpdatedAt    string         `json:"updated_at"`
}

// TableRow represents a single row in a table widget
type TableRow struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	SubLabel  string `json:"sub_label"`
	Status    string `json:"status"`
	Direction string `json:"direction,omitempty"`
	CreatedAt string `json:"created_at"`
}

// WidgetDataResponse represents the computed data for a widget
type WidgetDataResponse struct {
	WidgetID      uuid.UUID          `json:"widget_id"`
	Value         float64            `json:"value"`
	Change        float64            `json:"change"`         // Percentage change from previous period
	ChartData     []ChartPoint       `json:"chart_data"`     // For chart display type
	PrevValue     float64            `json:"prev_value"`     // Previous period value
	DataPoints    []DataPoint        `json:"data_points"`    // Breakdown data
	GroupedSeries *GroupedSeriesData `json:"grouped_series"` // For grouped time-series (line charts with group_by)
	TableRows     []TableRow         `json:"table_rows"`     // For table display type
}

type WidgetDataError struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

// GroupedSeriesData represents multiple datasets for grouped time-series charts
type GroupedSeriesData struct {
	Labels   []string               `json:"labels"`
	Datasets []GroupedSeriesDataset `json:"datasets"`
}

// GroupedSeriesDataset represents a single series in a grouped chart
type GroupedSeriesDataset struct {
	Label string    `json:"label"`
	Data  []float64 `json:"data"`
}

// ChartPoint represents a data point for charts
type ChartPoint struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

// DataPoint represents a breakdown data point
type DataPoint struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Color string  `json:"color,omitempty"`
}

// Available data sources and their filterable/groupable fields. These values
// are API names only; SQL identifiers are resolved through the separate,
// server-owned whitelists below.
var widgetDataSources = map[string][]string{
	"messages":  {"status", "direction", "message_type", "whatsapp_account"},
	"contacts":  {"assigned_user_id", "whatsapp_account", "is_read"},
	"campaigns": {"status", "message_status", "template_name", "created_by_id", "whatsapp_account"},
	"transfers": {"status", "source", "team_id", "agent_id"},
	"sessions":  {"status", "current_flow_id"},
	"crm_leads": {"status", "pipeline_id", "stage_id", "owner_user_id", "source", "currency"},
	"bookings":  {"status", "event_id", "source", "contact_package_id"},
	"invoices":  {"status", "currency"},
	"payments":  {"status", "type", "currency", "provider_account_id"},
	"packages":  {"status", "package_definition_id", "currency", "source"},
}

type widgetAggregateField struct {
	Expression string
}

// widgetAggregateFields maps public field names to fixed SQL expressions.
// Never interpolate the public field name itself.
var widgetAggregateFields = map[string]map[string]widgetAggregateField{
	"transfers": {
		"resolution_time": {
			Expression: "CASE WHEN status = 'resumed' AND resumed_at IS NOT NULL THEN EXTRACT(EPOCH FROM (resumed_at - transferred_at)) / 60 END",
		},
	},
	"crm_leads": {
		"value_minor": {Expression: "value_minor"},
	},
	"invoices": {
		"total_minor": {Expression: "total_minor"},
		"due_minor":   {Expression: "due_minor"},
	},
	"payments": {
		"amount_minor": {Expression: "amount_minor"},
	},
	"packages": {
		// Unlimited entitlements are deliberately excluded: representing them
		// as zero or a sentinel would corrupt sums and averages.
		"available_credits": {
			Expression: `(SELECT COALESCE(SUM(cb.available), 0)
				FROM credit_balances cb
				JOIN package_entitlements pe
				  ON pe.id = cb.package_entitlement_id
				 AND pe.organization_id = cb.organization_id
				 AND pe.deleted_at IS NULL
				WHERE cb.organization_id = contact_packages.organization_id
				  AND cb.contact_package_id = contact_packages.id
				  AND cb.deleted_at IS NULL
				  AND pe.is_unlimited = false)`,
		},
	},
}

type widgetSourceAccess struct {
	Resource string
	Action   string
}

var widgetSourceAccessRules = map[string]widgetSourceAccess{
	"messages":  {Resource: models.ResourceChat},
	"contacts":  {Resource: models.ResourceContacts},
	"campaigns": {Resource: models.ResourceCampaigns},
	// Transfer analytics is organization-wide. The transfers handlers reserve
	// that visibility for transfers:write; transfers:read is assignment/team
	// scoped and cannot safely back an unscoped aggregate.
	"transfers": {Resource: models.ResourceTransfers, Action: models.ActionWrite},
	"sessions":  {Resource: models.ResourceFlowsChatbot},
	"crm_leads": {Resource: models.ResourceCRMLeads},
	"bookings":  {Resource: models.ResourceBookings},
	"invoices":  {Resource: models.ResourcePayments},
	"payments":  {Resource: models.ResourcePayments},
	"packages":  {Resource: models.ResourcePackages},
}

// Available metrics
var widgetMetrics = []string{"count", "sum", "avg"}

// Available display types
var widgetDisplayTypes = []string{"number", "percentage", "chart", "table", "shortcuts"}

// Static display types that don't need a data source
var staticDisplayTypes = map[string]bool{
	"shortcuts": true,
}

// ListWidgets returns all widgets for the user (their own + shared)
func (a *App) ListWidgets(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	// Check analytics read permission
	if !a.HasPermission(userID, models.ResourceAnalytics, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You don't have permission to view analytics", nil, "")
	}

	// Get user's own widgets + shared widgets from org
	var widgets []models.Widget
	if err := a.DB.Where(
		"organization_id = ? AND (user_id = ? OR is_shared = true)",
		orgID, userID,
	).Order("display_order ASC, created_at ASC").Find(&widgets).Error; err != nil {
		a.Log.Error("Failed to list widgets", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list widgets", nil, "")
	}

	// Convert to response format
	response := make([]WidgetResponse, len(widgets))
	for i, w := range widgets {
		response[i] = widgetToResponse(w, userID)
	}

	return r.SendEnvelope(map[string]any{
		"widgets": response,
	})
}

// GetWidget returns a single widget
func (a *App) GetWidget(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	// Check analytics read permission
	if !a.HasPermission(userID, models.ResourceAnalytics, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You don't have permission to view analytics", nil, "")
	}

	id, err := parsePathUUID(r, "id", "widget")
	if err != nil {
		return nil
	}

	var widget models.Widget
	if err := a.DB.Where(
		"id = ? AND organization_id = ? AND (user_id = ? OR is_shared = true)",
		id, orgID, userID,
	).First(&widget).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Widget not found", nil, "")
	}

	return r.SendEnvelope(widgetToResponse(widget, userID))
}

// CreateWidget creates a new widget
func (a *App) CreateWidget(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	// Check analytics write permission
	if !a.HasPermission(userID, models.ResourceAnalytics, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You don't have permission to create widgets", nil, "")
	}

	var req WidgetRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	// Validate required fields
	if req.Name == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Name is required", nil, "")
	}

	// Validate display type
	displayType := req.DisplayType
	if displayType == "" {
		displayType = "number"
	}
	if !contains(widgetDisplayTypes, displayType) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid display type", nil, "")
	}

	// For static display types (e.g. shortcuts), auto-set data_source and metric
	if staticDisplayTypes[displayType] {
		req.DataSource = displayType
		req.Metric = "count"
	} else {
		if req.DataSource == "" {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Data source is required", nil, "")
		}
		if req.Metric == "" {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Metric is required", nil, "")
		}

		// Validate data source
		if _, ok := widgetDataSources[req.DataSource]; !ok {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid data source", nil, "")
		}

		// Validate metric
		if !contains(widgetMetrics, req.Metric) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid metric", nil, "")
		}
	}

	// Get max display order
	var maxOrder int
	a.DB.Model(&models.Widget{}).
		Where("organization_id = ? AND user_id = ?", orgID, userID).
		Select("COALESCE(MAX(display_order), 0)").
		Scan(&maxOrder)

	// Convert filters to JSONBArray
	filters := make(models.JSONBArray, len(req.Filters))
	for i, f := range req.Filters {
		filters[i] = map[string]any{
			"field":    f.Field,
			"operator": f.Operator,
			"value":    f.Value,
		}
	}

	showChange := true
	if req.ShowChange != nil {
		showChange = *req.ShowChange
	}

	isShared := false
	if req.IsShared != nil {
		isShared = *req.IsShared
	}

	size := req.Size
	if size == "" {
		size = "small"
	}

	// Validate group_by_field if provided (only for non-static types)
	if req.GroupByField != "" && !staticDisplayTypes[displayType] {
		fields := widgetDataSources[req.DataSource]
		if !contains(fields, req.GroupByField) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid group by field for this data source", nil, "")
		}
	}
	if err := validateWidgetQueryDefinition(
		req.DataSource,
		req.Metric,
		req.Field,
		displayType,
		req.GroupByField,
		req.Filters,
	); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	// Default grid sizes based on display type
	gridW := 3
	gridH := 3
	switch displayType {
	case "chart":
		gridW = 6
		gridH = 5
	case "table", "shortcuts":
		gridW = 6
		gridH = 8
	}
	gridX := 0
	gridY := 0
	if req.GridX != nil {
		gridX = *req.GridX
	}
	if req.GridY != nil {
		gridY = *req.GridY
	}
	if req.GridW != nil {
		gridW = *req.GridW
	}
	if req.GridH != nil {
		gridH = *req.GridH
	}

	// Build config
	widgetConfig := models.JSONB{}
	if req.Config != nil {
		widgetConfig = models.JSONB(req.Config)
	}

	widget := models.Widget{
		OrganizationID: orgID,
		UserID:         &userID,
		Name:           req.Name,
		Description:    req.Description,
		DataSource:     req.DataSource,
		Metric:         req.Metric,
		Field:          req.Field,
		Filters:        filters,
		DisplayType:    displayType,
		ChartType:      req.ChartType,
		GroupByField:   req.GroupByField,
		ShowChange:     showChange,
		Color:          req.Color,
		Size:           size,
		Config:         widgetConfig,
		DisplayOrder:   maxOrder + 1,
		GridX:          gridX,
		GridY:          gridY,
		GridW:          gridW,
		GridH:          gridH,
		IsShared:       isShared,
	}

	if err := a.DB.Create(&widget).Error; err != nil {
		a.Log.Error("Failed to create widget", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create widget", nil, "")
	}

	return r.SendEnvelope(widgetToResponse(widget, userID))
}

// UpdateWidget updates a widget
func (a *App) UpdateWidget(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	// Check analytics write permission
	if !a.HasPermission(userID, models.ResourceAnalytics, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You don't have permission to edit widgets", nil, "")
	}

	id, err := parsePathUUID(r, "id", "widget")
	if err != nil {
		return nil
	}

	// Find the widget - must belong to same organization
	widget, err := findByIDAndOrg[models.Widget](a.DB, r, id, orgID, "Widget")
	if err != nil {
		return nil
	}

	// Only the owner can edit the widget
	if widget.UserID == nil || *widget.UserID != userID {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Only the widget owner can edit this widget", nil, "")
	}

	var req WidgetRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	// Update fields
	if req.Name != "" {
		widget.Name = req.Name
	}
	if req.Description != "" {
		widget.Description = req.Description
	}
	if req.DataSource != "" {
		if _, ok := widgetDataSources[req.DataSource]; !ok {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid data source", nil, "")
		}
		widget.DataSource = req.DataSource
	}
	if req.Metric != "" {
		if !contains(widgetMetrics, req.Metric) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid metric", nil, "")
		}
		widget.Metric = req.Metric
	}
	if req.Field != "" {
		widget.Field = req.Field
	}
	if req.Filters != nil {
		filters := make(models.JSONBArray, len(req.Filters))
		for i, f := range req.Filters {
			filters[i] = map[string]any{
				"field":    f.Field,
				"operator": f.Operator,
				"value":    f.Value,
			}
		}
		widget.Filters = filters
	}
	if req.DisplayType != "" {
		if !contains(widgetDisplayTypes, req.DisplayType) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid display type", nil, "")
		}
		widget.DisplayType = req.DisplayType
	}
	if req.ChartType != "" {
		widget.ChartType = req.ChartType
	}
	// Always update group_by_field (empty string clears it)
	if req.GroupByField != "" {
		ds := widget.DataSource
		if req.DataSource != "" {
			ds = req.DataSource
		}
		fields := widgetDataSources[ds]
		if !contains(fields, req.GroupByField) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid group by field for this data source", nil, "")
		}
	}
	widget.GroupByField = req.GroupByField
	if req.ShowChange != nil {
		widget.ShowChange = *req.ShowChange
	}
	if req.Color != "" {
		widget.Color = req.Color
	}
	if req.Size != "" {
		widget.Size = req.Size
	}
	if req.Config != nil {
		widget.Config = models.JSONB(req.Config)
	}
	if req.IsShared != nil {
		widget.IsShared = *req.IsShared
	}
	if req.GridX != nil {
		widget.GridX = *req.GridX
	}
	if req.GridY != nil {
		widget.GridY = *req.GridY
	}
	if req.GridW != nil {
		widget.GridW = *req.GridW
	}
	if req.GridH != nil {
		widget.GridH = *req.GridH
	}
	if err := validateWidgetQueryDefinition(
		widget.DataSource,
		widget.Metric,
		widget.Field,
		widget.DisplayType,
		widget.GroupByField,
		widgetFiltersFromModel(*widget),
	); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	if err := a.DB.Save(widget).Error; err != nil {
		a.Log.Error("Failed to update widget", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update widget", nil, "")
	}

	return r.SendEnvelope(widgetToResponse(*widget, userID))
}

// DeleteWidget deletes a widget
func (a *App) DeleteWidget(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	// Check analytics delete permission
	if !a.HasPermission(userID, models.ResourceAnalytics, models.ActionDelete, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You don't have permission to delete widgets", nil, "")
	}

	id, err := parsePathUUID(r, "id", "widget")
	if err != nil {
		return nil
	}

	// Find the widget - must belong to same organization
	widget, err := findByIDAndOrg[models.Widget](a.DB, r, id, orgID, "Widget")
	if err != nil {
		return nil
	}

	// Only the owner can delete the widget
	if widget.UserID == nil || *widget.UserID != userID {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Only the widget owner can delete this widget", nil, "")
	}

	if err := a.DB.Delete(widget).Error; err != nil {
		a.Log.Error("Failed to delete widget", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete widget", nil, "")
	}

	return r.SendEnvelope(map[string]string{"message": "Widget deleted successfully"})
}

// SaveWidgetLayout bulk saves grid positions for all widgets
func (a *App) SaveWidgetLayout(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	if !a.HasPermission(userID, models.ResourceAnalytics, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You don't have permission to edit widgets", nil, "")
	}

	var req struct {
		Layout []struct {
			ID    uuid.UUID `json:"id"`
			GridX int       `json:"grid_x"`
			GridY int       `json:"grid_y"`
			GridW int       `json:"grid_w"`
			GridH int       `json:"grid_h"`
		} `json:"layout"`
	}
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	if len(req.Layout) == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Layout is required", nil, "")
	}

	// Update all widgets in a transaction
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		for i, item := range req.Layout {
			result := tx.Model(&models.Widget{}).
				Where("id = ? AND organization_id = ? AND user_id = ?", item.ID, orgID, userID).
				Updates(map[string]any{
					"grid_x":        item.GridX,
					"grid_y":        item.GridY,
					"grid_w":        item.GridW,
					"grid_h":        item.GridH,
					"display_order": i,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return gorm.ErrRecordNotFound
			}
		}
		return nil
	})

	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Widget not found", nil, "")
		}
		a.Log.Error("Failed to save widget layout", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to save layout", nil, "")
	}

	return r.SendEnvelope(map[string]string{"message": "Layout saved successfully"})
}

// GetWidgetDataSources returns available data sources and their filterable fields
func (a *App) GetWidgetDataSources(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceAnalytics, models.ActionRead)
	if err != nil {
		return nil
	}
	sourceNames := make([]string, 0, len(widgetDataSources))
	for source := range widgetDataSources {
		sourceNames = append(sourceNames, source)
	}
	sort.Strings(sourceNames)

	sources := make([]map[string]any, 0, len(sourceNames))
	for _, source := range sourceNames {
		status, _, accessErr := a.widgetSourceAccessStatus(orgID, userID, source, "number")
		if accessErr != nil {
			a.Log.Error("Failed to evaluate widget data source entitlement", "error", accessErr, "data_source", source)
			return r.SendErrorEnvelope(
				fasthttp.StatusInternalServerError,
				"Failed to load widget data sources",
				nil,
				"",
			)
		}
		if status != 0 {
			continue
		}
		sources = append(sources, map[string]any{
			"name":             source,
			"label":            formatLabel(source),
			"fields":           widgetDataSources[source],
			"aggregate_fields": widgetAggregateFieldNames(source),
		})
	}

	return r.SendEnvelope(map[string]any{
		"data_sources":  sources,
		"metrics":       widgetMetrics,
		"display_types": widgetDisplayTypes,
		"operators": []map[string]string{
			{"value": "equals", "label": "Equals"},
			{"value": "not_equals", "label": "Not Equals"},
			{"value": "contains", "label": "Contains"},
			{"value": "gt", "label": "Greater Than"},
			{"value": "lt", "label": "Less Than"},
			{"value": "gte", "label": "Greater Than or Equal"},
			{"value": "lte", "label": "Less Than or Equal"},
		},
	})
}

// Helper functions

func widgetToResponse(w models.Widget, currentUserID uuid.UUID) WidgetResponse {
	// Parse filters from JSONBArray
	filters := make([]FilterInput, 0)
	for _, f := range w.Filters {
		if filterMap, ok := f.(map[string]any); ok {
			filters = append(filters, FilterInput{
				Field:    widgetGetString(filterMap, "field"),
				Operator: widgetGetString(filterMap, "operator"),
				Value:    widgetGetString(filterMap, "value"),
			})
		}
	}

	config := map[string]any(w.Config)
	if config == nil {
		config = map[string]any{}
	}

	return WidgetResponse{
		ID:           w.ID,
		Name:         w.Name,
		Description:  w.Description,
		DataSource:   w.DataSource,
		Metric:       w.Metric,
		Field:        w.Field,
		Filters:      filters,
		DisplayType:  w.DisplayType,
		ChartType:    w.ChartType,
		GroupByField: w.GroupByField,
		ShowChange:   w.ShowChange,
		Color:        w.Color,
		Size:         w.Size,
		DisplayOrder: w.DisplayOrder,
		GridX:        w.GridX,
		GridY:        w.GridY,
		GridW:        w.GridW,
		GridH:        w.GridH,
		Config:       config,
		IsShared:     w.IsShared,
		IsDefault:    w.IsDefault,
		IsOwner:      w.UserID != nil && *w.UserID == currentUserID,
		CreatedAt:    w.CreatedAt.Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    w.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}
}

func widgetGetString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func formatLabel(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	if len(s) > 0 {
		return strings.ToUpper(s[:1]) + s[1:]
	}
	return s
}

var widgetFilterOperators = map[string]bool{
	"equals":     true,
	"not_equals": true,
	"contains":   true,
	"gt":         true,
	"lt":         true,
	"gte":        true,
	"lte":        true,
}

func widgetAggregateFieldNames(dataSource string) []string {
	fields := widgetAggregateFields[dataSource]
	result := make([]string, 0, len(fields))
	for field := range fields {
		result = append(result, field)
	}
	sort.Strings(result)
	return result
}

func validateWidgetQueryDefinition(
	dataSource, metric, field, displayType, groupByField string,
	filters []FilterInput,
) error {
	if staticDisplayTypes[displayType] || dataSource == "shortcuts" {
		return nil
	}
	if _, ok := widgetDataSources[dataSource]; !ok {
		return fmt.Errorf("invalid data source")
	}
	if _, _, ok := resolveDataSourceTable(dataSource); !ok {
		return fmt.Errorf("invalid data source")
	}
	if !contains(widgetMetrics, metric) {
		return fmt.Errorf("invalid metric")
	}
	if metric == "sum" || metric == "avg" {
		if field == "" {
			return fmt.Errorf("field is required for %s", metric)
		}
		if _, ok := widgetAggregateFields[dataSource][field]; !ok {
			return fmt.Errorf("invalid aggregate field for this data source")
		}
	}
	if groupByField != "" {
		if _, ok := allowedGroupByFields[dataSource][groupByField]; !ok {
			return fmt.Errorf("invalid group by field for this data source")
		}
	}
	for _, filter := range filters {
		if _, ok := allowedFilterFields[dataSource][filter.Field]; !ok {
			return fmt.Errorf("invalid filter field for this data source")
		}
		if !widgetFilterOperators[filter.Operator] {
			return fmt.Errorf("invalid filter operator")
		}
	}
	return nil
}

func widgetFiltersFromModel(widget models.Widget) []FilterInput {
	filters := make([]FilterInput, 0, len(widget.Filters))
	for _, filter := range widget.Filters {
		if filterMap, ok := filter.(map[string]any); ok {
			filters = append(filters, FilterInput{
				Field:    widgetGetString(filterMap, "field"),
				Operator: widgetGetString(filterMap, "operator"),
				Value:    widgetGetString(filterMap, "value"),
			})
		}
	}
	return filters
}

// widgetSourceAccessStatus performs the second authorization layer required
// for dashboard queries. Analytics access alone never grants access to an
// underlying CRM, booking, commerce, or operational data source.
func (a *App) widgetSourceAccessStatus(
	orgID, userID uuid.UUID,
	dataSource, displayType string,
) (int, string, error) {
	if staticDisplayTypes[displayType] || dataSource == "shortcuts" {
		return 0, "", nil
	}
	rule, ok := widgetSourceAccessRules[dataSource]
	if !ok {
		return fasthttp.StatusBadRequest, "Invalid widget data source", nil
	}
	action := rule.Action
	if action == "" {
		action = models.ActionRead
	}
	if !a.HasPermission(userID, rule.Resource, action, orgID) {
		return fasthttp.StatusForbidden, "Insufficient permission for widget data source", nil
	}
	// Message table widgets return organization-wide conversation previews.
	// Chat read access can be assignment-scoped, so it is not sufficient for
	// this unscoped table query without contact-wide read access as well.
	if dataSource == "messages" &&
		displayType == "table" &&
		!a.HasPermission(userID, models.ResourceContacts, models.ActionRead, orgID) {
		return fasthttp.StatusForbidden, "Contact-wide read permission is required for message table widgets", nil
	}
	if entitlementKey, gated := ProductEntitlementKeyForResource(rule.Resource); gated {
		allowed, err := a.HasProductEntitlement(userID, orgID, entitlementKey)
		if err != nil {
			return fasthttp.StatusInternalServerError, "Product entitlement could not be evaluated", err
		}
		if !allowed {
			return fasthttp.StatusPaymentRequired, "Widget data source is not included in the active plan", nil
		}
	}
	return 0, "", nil
}

// GetWidgetData executes the widget query and returns the data
func (a *App) GetWidgetData(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceAnalytics, models.ActionRead)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "widget")
	if err != nil {
		return nil
	}

	// Parse date range from query params
	fromStr := string(r.RequestCtx.QueryArgs().Peek("from"))
	toStr := string(r.RequestCtx.QueryArgs().Peek("to"))

	// Get the widget
	var widget models.Widget
	if err := a.DB.Where(
		"id = ? AND organization_id = ? AND (user_id = ? OR is_shared = true)",
		id, orgID, userID,
	).First(&widget).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Widget not found", nil, "")
	}
	if status, message, accessErr := a.widgetSourceAccessStatus(
		orgID,
		userID,
		widget.DataSource,
		widget.DisplayType,
	); status != 0 {
		if accessErr != nil {
			a.Log.Error("Failed to authorize widget data source", "error", accessErr)
		}
		return r.SendErrorEnvelope(status, message, nil, "")
	}

	// Execute the query
	data, err := a.executeWidgetQuery(orgID, widget, fromStr, toStr)
	if err != nil {
		a.Log.Error("Failed to execute widget query", "error", err, "widget_id", id)
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	data.WidgetID = widget.ID
	return r.SendEnvelope(data)
}

// GetAllWidgetsData returns data for all user's widgets in a single request
func (a *App) GetAllWidgetsData(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceAnalytics, models.ActionRead)
	if err != nil {
		return nil
	}

	// Parse date range from query params
	fromStr := string(r.RequestCtx.QueryArgs().Peek("from"))
	toStr := string(r.RequestCtx.QueryArgs().Peek("to"))

	// Get user's widgets
	var widgets []models.Widget
	if err := a.DB.Where(
		"organization_id = ? AND (user_id = ? OR is_shared = true)",
		orgID, userID,
	).Order("display_order ASC").Find(&widgets).Error; err != nil {
		a.Log.Error("Failed to list widgets", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list widgets", nil, "")
	}

	// Execute queries for all widgets
	results := make(map[string]WidgetDataResponse)
	queryErrors := make(map[string]WidgetDataError)
	for _, widget := range widgets {
		if status, message, accessErr := a.widgetSourceAccessStatus(
			orgID,
			userID,
			widget.DataSource,
			widget.DisplayType,
		); status != 0 {
			if accessErr != nil {
				a.Log.Error(
					"Failed to authorize widget data source",
					"error",
					accessErr,
					"widget_id",
					widget.ID,
				)
			}
			queryErrors[widget.ID.String()] = WidgetDataError{Status: status, Message: message}
			continue
		}
		data, err := a.executeWidgetQuery(orgID, widget, fromStr, toStr)
		if err != nil {
			a.Log.Error("Failed to execute widget query", "error", err, "widget_id", widget.ID)
			queryErrors[widget.ID.String()] = WidgetDataError{
				Status:  fasthttp.StatusBadRequest,
				Message: err.Error(),
			}
			continue
		}
		data.WidgetID = widget.ID
		results[widget.ID.String()] = data
	}

	return r.SendEnvelope(map[string]any{
		"data":   results,
		"errors": queryErrors,
	})
}

// executeWidgetQuery executes the query for a widget and returns the data
func (a *App) executeWidgetQuery(orgID uuid.UUID, widget models.Widget, fromStr, toStr string) (WidgetDataResponse, error) {
	now := time.Now()

	var periodStart, periodEnd time.Time

	if fromStr != "" && toStr != "" {
		var errMsg string
		periodStart, periodEnd, errMsg = parseDateRange(fromStr, toStr)
		if errMsg != "" {
			// Fall back to current month on parse error
			periodStart = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			periodEnd = now
		}
	} else {
		// Default to current month
		periodStart = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		periodEnd = now
	}

	// Calculate previous period for comparison
	periodDuration := periodEnd.Sub(periodStart)
	previousPeriodStart := periodStart.Add(-periodDuration - time.Nanosecond)
	previousPeriodEnd := periodStart.Add(-time.Nanosecond)

	response := WidgetDataResponse{
		ChartData:  []ChartPoint{},
		DataPoints: []DataPoint{},
		TableRows:  []TableRow{},
	}
	filters := widgetFiltersFromModel(widget)
	if err := validateWidgetQueryDefinition(
		widget.DataSource,
		widget.Metric,
		widget.Field,
		widget.DisplayType,
		widget.GroupByField,
		filters,
	); err != nil {
		return response, err
	}

	// Early return for static display types (no data query needed)
	if staticDisplayTypes[widget.DisplayType] {
		return response, nil
	}

	// Handle table display type
	if widget.DisplayType == "table" {
		if widget.GroupByField != "" {
			// Grouped table: reuse existing getGroupedData to populate DataPoints
			response.DataPoints = a.getGroupedData(orgID, widget, filters, periodStart, periodEnd)
		} else {
			// Table rows: query last 10 records
			response.TableRows = a.getTableRows(orgID, widget, filters, periodStart, periodEnd)
		}
		return response, nil
	}

	currentValue, err := a.queryWidgetMetric(
		orgID,
		widget,
		filters,
		periodStart,
		periodEnd,
	)
	if err != nil {
		return response, err
	}
	previousValue, err := a.queryWidgetMetric(
		orgID,
		widget,
		filters,
		previousPeriodStart,
		previousPeriodEnd,
	)
	if err != nil {
		return response, err
	}

	response.Value = currentValue
	response.PrevValue = previousValue
	response.Change = calculatePercentageChange(int64(previousValue), int64(currentValue))

	// Get chart data if display type is chart
	if widget.DisplayType == "chart" {
		if widget.GroupByField != "" {
			if widget.ChartType == "line" {
				// Line chart with group by → grouped time-series
				groupedSeries := a.getGroupedTimeSeriesData(orgID, widget, filters, periodStart, periodEnd)
				response.GroupedSeries = &groupedSeries
			} else {
				// Bar/Pie chart with group by → data points (group → count)
				response.DataPoints = a.getGroupedData(orgID, widget, filters, periodStart, periodEnd)
			}
		} else {
			response.ChartData = a.getChartData(orgID, widget, filters, periodStart, periodEnd)
		}
	}

	return response, nil
}

// Query helper functions for each data source
func widgetMetricSQLExpression(widget models.Widget) (string, bool) {
	switch widget.Metric {
	case "count":
		return "COUNT(*)", true
	case "sum", "avg":
		aggregate, ok := widgetAggregateFields[widget.DataSource][widget.Field]
		if !ok {
			return "", false
		}
		return fmt.Sprintf(
			"COALESCE(%s(%s), 0)",
			strings.ToUpper(widget.Metric),
			aggregate.Expression,
		), true
	default:
		return "", false
	}
}

func (a *App) queryWidgetMetric(
	orgID uuid.UUID,
	widget models.Widget,
	filters []FilterInput,
	start, end time.Time,
) (float64, error) {
	tableName, dateField, ok := resolveDataSourceTable(widget.DataSource)
	if !ok {
		return 0, fmt.Errorf("invalid data source")
	}
	metricExpression, ok := widgetMetricSQLExpression(widget)
	if !ok {
		return 0, fmt.Errorf("invalid metric or aggregate field")
	}
	query := fmt.Sprintf(
		"SELECT %s AS value FROM %s WHERE organization_id = ? AND %s >= ? AND %s <= ?",
		metricExpression,
		tableName,
		dateField,
		dateField,
	)
	if widgetSourceUsesSoftDelete[widget.DataSource] {
		query += " AND deleted_at IS NULL"
	}
	args := []any{orgID, start, end}
	query, args = appendFilterSQL(widget.DataSource, query, args, filters)

	var result struct {
		Value float64
	}
	if err := a.DB.Raw(query, args...).Scan(&result).Error; err != nil {
		return 0, err
	}
	return result.Value, nil
}

func (a *App) getChartData(orgID uuid.UUID, widget models.Widget, filters []FilterInput, start, end time.Time) []ChartPoint {
	chartData := make([]ChartPoint, 0)

	tableName, dateField, ok := resolveDataSourceTable(widget.DataSource)
	if !ok {
		return chartData
	}
	metricExpression, ok := widgetMetricSQLExpression(widget)
	if !ok {
		return chartData
	}

	// Build raw query for daily aggregation
	query := fmt.Sprintf(`
		SELECT DATE_TRUNC('day', %s) as date, %s as value
		FROM %s
		WHERE organization_id = ? AND %s >= ? AND %s <= ?
	`, dateField, metricExpression, tableName, dateField, dateField)

	args := []any{orgID, start, end}
	if widgetSourceUsesSoftDelete[widget.DataSource] {
		query += " AND deleted_at IS NULL"
	}
	query, args = appendFilterSQL(widget.DataSource, query, args, filters)

	query += fmt.Sprintf(" GROUP BY DATE_TRUNC('day', %s) ORDER BY date ASC", dateField)

	type DailyValue struct {
		Date  time.Time
		Value float64
	}

	var results []DailyValue
	a.DB.Raw(query, args...).Scan(&results)

	for _, r := range results {
		chartData = append(chartData, ChartPoint{
			Label: r.Date.Format("Jan 02"),
			Value: r.Value,
		})
	}

	return chartData
}

// resolveDataSourceTable returns the table name and date field for a data source
// allowedFilterFields enumerates the columns each data source is allowed
// to be filtered on. Filter column names get interpolated directly into
// raw SQL (column identifiers can't be parameterized), so anything not
// in this whitelist is rejected — otherwise an authenticated user with
// widget-write permission can craft a malicious filter and inject SQL.
//
// Frontends should keep their filter pickers in sync with this list; any
// filter whose `field` is not allowed will be silently dropped at query
// time.
var allowedFilterFields = map[string]map[string]string{
	"messages": {
		"status":           "status",
		"direction":        "direction",
		"message_type":     "message_type",
		"whatsapp_account": "whatsapp_account",
	},
	"contacts": {
		"assigned_user_id": "assigned_user_id",
		"whatsapp_account": "whatsapp_account",
		"is_read":          "is_read",
	},
	"campaigns": {
		"status":           "status",
		"template_name":    "template_name",
		"created_by_id":    "created_by_id",
		"whatsapp_account": "whatsapp_account",
	},
	"transfers": {
		"status":   "status",
		"source":   "source",
		"team_id":  "team_id",
		"agent_id": "agent_id",
	},
	"sessions": {
		"status":          "status",
		"current_flow_id": "current_flow_id",
	},
	"crm_leads": {
		"status":        "status",
		"pipeline_id":   "pipeline_id",
		"stage_id":      "stage_id",
		"owner_user_id": "owner_user_id",
		"source":        "source",
		"currency":      "currency",
	},
	"bookings": {
		"status":             "status",
		"event_id":           "event_id",
		"source":             "source",
		"contact_package_id": "contact_package_id",
	},
	"invoices": {
		"status":   "status",
		"currency": "currency",
	},
	"payments": {
		"status":              "status",
		"type":                "type",
		"currency":            "currency",
		"provider_account_id": "provider_account_id",
	},
	"packages": {
		"status":                "status",
		"package_definition_id": "package_definition_id",
		"currency":              "currency",
		"source":                "source",
	},
}

// allowedAggregateFields enumerates the columns each data source is
// allowed to be summed/averaged over (the `field` argument to metric
// types "sum" / "avg"). Same threat model as allowedFilterFields — the
// column name flows into raw SQL.
// allowedGroupByFields maps public API names to fixed SQL expressions. Query
// builders use only the mapped expression, never the request value.
var allowedGroupByFields = map[string]map[string]string{
	"messages": {
		"status": "status", "direction": "direction", "message_type": "message_type",
		"whatsapp_account": "whatsapp_account",
	},
	"contacts": {
		"assigned_user_id": "assigned_user_id", "whatsapp_account": "whatsapp_account", "is_read": "is_read",
	},
	"campaigns": {
		"status": "status", "message_status": "message_status", "template_name": "template_name",
		"created_by_id": "created_by_id", "whatsapp_account": "whatsapp_account",
	},
	"transfers": {
		"status": "status", "source": "source", "team_id": "team_id", "agent_id": "agent_id",
	},
	"sessions": {
		"status": "status", "current_flow_id": "current_flow_id",
	},
	"crm_leads": {
		"status": "status", "pipeline_id": "pipeline_id", "stage_id": "stage_id",
		"owner_user_id": "owner_user_id", "source": "source", "currency": "currency",
	},
	"bookings": {
		"status": "status", "event_id": "event_id", "source": "source", "contact_package_id": "contact_package_id",
	},
	"invoices": {
		"status": "status", "currency": "currency",
	},
	"payments": {
		"status": "status", "type": "type", "currency": "currency", "provider_account_id": "provider_account_id",
	},
	"packages": {
		"status": "status", "package_definition_id": "package_definition_id", "currency": "currency", "source": "source",
	},
}

var widgetSourceUsesSoftDelete = map[string]bool{
	"messages": true, "contacts": true, "campaigns": true, "transfers": true,
	"sessions": true, "crm_leads": true, "bookings": true, "invoices": true,
	"payments": false, "packages": true,
}

func resolveDataSourceTable(dataSource string) (tableName, dateField string, ok bool) {
	switch dataSource {
	case "messages":
		return "messages", "created_at", true
	case "contacts":
		return "contacts", "last_message_at", true
	case "campaigns":
		return "bulk_message_campaigns", "created_at", true
	case "transfers":
		return "agent_transfers", "transferred_at", true
	case "sessions":
		return "chatbot_sessions", "created_at", true
	case "crm_leads":
		return "crm_leads", "created_at", true
	case "bookings":
		return "bookings", "created_at", true
	case "invoices":
		return "commerce_invoices", "created_at", true
	case "payments":
		return "payment_transactions", "occurred_at", true
	case "packages":
		return "contact_packages", "created_at", true
	default:
		return "", "", false
	}
}

// appendFilterSQL appends filter conditions to a raw SQL query string and args slice.
// Filters whose field is not in allowedFilterFields[dataSource] are silently dropped —
// they would otherwise be a SQL injection vector since column names interpolate raw.
func appendFilterSQL(dataSource string, query string, args []any, filters []FilterInput) (string, []any) {
	for _, f := range filters {
		condition, value, ok := buildFilterSQL(dataSource, f)
		if !ok {
			continue
		}
		query += " AND " + condition
		args = append(args, value)
	}
	return query, args
}

// getGroupedData returns aggregated counts grouped by a field (for bar/pie charts)
func (a *App) getGroupedData(orgID uuid.UUID, widget models.Widget, filters []FilterInput, start, end time.Time) []DataPoint {
	dataPoints := make([]DataPoint, 0)

	// Special case: campaigns grouped by message_status uses pre-aggregated counters
	if widget.DataSource == "campaigns" && widget.GroupByField == "message_status" {
		return a.getCampaignMessageStatusData(orgID, filters, start, end)
	}

	tableName, dateField, ok := resolveDataSourceTable(widget.DataSource)
	if !ok {
		return dataPoints
	}

	groupExpression, ok := allowedGroupByFields[widget.DataSource][widget.GroupByField]
	if !ok {
		a.Log.Error("Invalid GroupByField", "field", widget.GroupByField)
		return dataPoints
	}
	metricExpression, ok := widgetMetricSQLExpression(widget)
	if !ok {
		return dataPoints
	}

	query := fmt.Sprintf(`
		SELECT COALESCE((%s)::text, '') as label, %s as value
		FROM %s
		WHERE organization_id = ? AND %s >= ? AND %s <= ?
	`, groupExpression, metricExpression, tableName, dateField, dateField)

	args := []any{orgID, start, end}
	if widgetSourceUsesSoftDelete[widget.DataSource] {
		query += " AND deleted_at IS NULL"
	}
	query, args = appendFilterSQL(widget.DataSource, query, args, filters)

	query += fmt.Sprintf(" GROUP BY %s ORDER BY value DESC", groupExpression)

	type GroupedValue struct {
		Label string
		Value float64
	}

	var results []GroupedValue
	a.DB.Raw(query, args...).Scan(&results)

	for _, r := range results {
		label := r.Label
		if label == "" {
			label = "(empty)"
		}
		dataPoints = append(dataPoints, DataPoint{
			Label: label,
			Value: r.Value,
		})
	}

	return dataPoints
}

// getCampaignMessageStatusData returns sent/delivered/read/failed totals from campaign counters
func (a *App) getCampaignMessageStatusData(orgID uuid.UUID, filters []FilterInput, start, end time.Time) []DataPoint {
	query := `
		SELECT
			COALESCE(SUM(sent_count), 0) as sent,
			COALESCE(SUM(delivered_count), 0) as delivered,
			COALESCE(SUM(read_count), 0) as read_count,
			COALESCE(SUM(failed_count), 0) as failed
		FROM bulk_message_campaigns
		WHERE organization_id = ? AND created_at >= ? AND created_at <= ?
		  AND deleted_at IS NULL
	`

	args := []any{orgID, start, end}
	query, args = appendFilterSQL("campaigns", query, args, filters)

	type CampaignCounts struct {
		Sent      int64
		Delivered int64
		ReadCount int64 `gorm:"column:read_count"`
		Failed    int64
	}

	var counts CampaignCounts
	a.DB.Raw(query, args...).Scan(&counts)

	return []DataPoint{
		{Label: "sent", Value: float64(counts.Sent)},
		{Label: "delivered", Value: float64(counts.Delivered)},
		{Label: "read", Value: float64(counts.ReadCount)},
		{Label: "failed", Value: float64(counts.Failed)},
	}
}

// getGroupedTimeSeriesData returns time-series data grouped by a field (for line charts with group_by)
func (a *App) getGroupedTimeSeriesData(orgID uuid.UUID, widget models.Widget, filters []FilterInput, start, end time.Time) GroupedSeriesData {
	result := GroupedSeriesData{
		Labels:   make([]string, 0),
		Datasets: make([]GroupedSeriesDataset, 0),
	}

	// Special case: campaigns grouped by message_status over time
	if widget.DataSource == "campaigns" && widget.GroupByField == "message_status" {
		return a.getCampaignMessageStatusTimeSeries(orgID, filters, start, end)
	}

	tableName, dateField, ok := resolveDataSourceTable(widget.DataSource)
	if !ok {
		return result
	}
	groupExpression, ok := allowedGroupByFields[widget.DataSource][widget.GroupByField]
	if !ok {
		return result
	}
	metricExpression, ok := widgetMetricSQLExpression(widget)
	if !ok {
		return result
	}

	query := fmt.Sprintf(`
		SELECT DATE_TRUNC('day', %s) as date,
		       COALESCE((%s)::text, '') as group_value,
		       %s as value
		FROM %s
		WHERE organization_id = ? AND %s >= ? AND %s <= ?
	`, dateField, groupExpression, metricExpression, tableName, dateField, dateField)

	args := []any{orgID, start, end}
	if widgetSourceUsesSoftDelete[widget.DataSource] {
		query += " AND deleted_at IS NULL"
	}
	query, args = appendFilterSQL(widget.DataSource, query, args, filters)

	query += fmt.Sprintf(" GROUP BY DATE_TRUNC('day', %s), %s ORDER BY date ASC", dateField, groupExpression)

	type GroupedRow struct {
		Date       time.Time
		GroupValue string
		Value      float64
	}

	var rows []GroupedRow
	a.DB.Raw(query, args...).Scan(&rows)

	// Collect unique dates and groups
	dateSet := make(map[string]bool)
	groupSet := make(map[string]bool)
	dateOrder := make([]string, 0)
	groupOrder := make([]string, 0)

	for _, row := range rows {
		dateLabel := row.Date.Format("Jan 02")
		if !dateSet[dateLabel] {
			dateSet[dateLabel] = true
			dateOrder = append(dateOrder, dateLabel)
		}
		gv := row.GroupValue
		if gv == "" {
			gv = "(empty)"
		}
		if !groupSet[gv] {
			groupSet[gv] = true
			groupOrder = append(groupOrder, gv)
		}
	}

	result.Labels = dateOrder

	// Build a lookup: group → date → count
	lookup := make(map[string]map[string]float64)
	for _, row := range rows {
		gv := row.GroupValue
		if gv == "" {
			gv = "(empty)"
		}
		dateLabel := row.Date.Format("Jan 02")
		if lookup[gv] == nil {
			lookup[gv] = make(map[string]float64)
		}
		lookup[gv][dateLabel] = row.Value
	}

	// Build datasets
	for _, group := range groupOrder {
		data := make([]float64, len(dateOrder))
		for i, dateLabel := range dateOrder {
			data[i] = lookup[group][dateLabel]
		}
		result.Datasets = append(result.Datasets, GroupedSeriesDataset{
			Label: group,
			Data:  data,
		})
	}

	return result
}

// getCampaignMessageStatusTimeSeries returns daily sent/delivered/read/failed from campaign counters over time
func (a *App) getCampaignMessageStatusTimeSeries(orgID uuid.UUID, filters []FilterInput, start, end time.Time) GroupedSeriesData {
	result := GroupedSeriesData{
		Labels:   make([]string, 0),
		Datasets: make([]GroupedSeriesDataset, 0),
	}

	query := `
		SELECT DATE_TRUNC('day', created_at) as date,
			COALESCE(SUM(sent_count), 0) as sent,
			COALESCE(SUM(delivered_count), 0) as delivered,
			COALESCE(SUM(read_count), 0) as read_count,
			COALESCE(SUM(failed_count), 0) as failed
		FROM bulk_message_campaigns
		WHERE organization_id = ? AND created_at >= ? AND created_at <= ?
		  AND deleted_at IS NULL
	`

	args := []any{orgID, start, end}
	query, args = appendFilterSQL("campaigns", query, args, filters)

	query += " GROUP BY DATE_TRUNC('day', created_at) ORDER BY date ASC"

	type DailyCampaignCounts struct {
		Date      time.Time
		Sent      int64
		Delivered int64
		ReadCount int64 `gorm:"column:read_count"`
		Failed    int64
	}

	var rows []DailyCampaignCounts
	a.DB.Raw(query, args...).Scan(&rows)

	labels := make([]string, len(rows))
	sentData := make([]float64, len(rows))
	deliveredData := make([]float64, len(rows))
	readData := make([]float64, len(rows))
	failedData := make([]float64, len(rows))

	for i, row := range rows {
		labels[i] = row.Date.Format("Jan 02")
		sentData[i] = float64(row.Sent)
		deliveredData[i] = float64(row.Delivered)
		readData[i] = float64(row.ReadCount)
		failedData[i] = float64(row.Failed)
	}

	result.Labels = labels
	result.Datasets = []GroupedSeriesDataset{
		{Label: "sent", Data: sentData},
		{Label: "delivered", Data: deliveredData},
		{Label: "read", Data: readData},
		{Label: "failed", Data: failedData},
	}

	return result
}

// applyFilter chains a single user-supplied filter onto a GORM query. Skips
// filters whose field isn't in the data-source whitelist — that's the SQL
// injection guard. The query is unchanged in that case.
func applyFilter(dataSource string, query *gorm.DB, filter FilterInput) *gorm.DB {
	condition, value, ok := buildFilterSQL(dataSource, filter)
	if !ok {
		return query
	}
	return query.Where(condition, value)
}

// buildFilterSQL turns a user-supplied filter into a parameterized SQL
// fragment. The value is always parameterized (`?`); the column name is
// interpolated raw, which is why we whitelist the field against
// allowedFilterFields[dataSource]. Returns ok=false (no condition, nil
// value) for any field that isn't in the whitelist for this data source.
func buildFilterSQL(dataSource string, filter FilterInput) (string, any, bool) {
	field, ok := allowedFilterFields[dataSource][filter.Field]
	if !ok {
		return "", nil, false
	}
	value := filter.Value

	switch filter.Operator {
	case "equals":
		return fmt.Sprintf("%s = ?", field), value, true
	case "not_equals":
		return fmt.Sprintf("%s != ?", field), value, true
	case "contains":
		return fmt.Sprintf("%s ILIKE ?", field), "%" + value + "%", true
	case "gt":
		return fmt.Sprintf("%s > ?", field), value, true
	case "lt":
		return fmt.Sprintf("%s < ?", field), value, true
	case "gte":
		return fmt.Sprintf("%s >= ?", field), value, true
	case "lte":
		return fmt.Sprintf("%s <= ?", field), value, true
	default:
		return "", nil, false
	}
}

// tableQuerySQL maps each data source to its SELECT + WHERE clause and ORDER BY suffix.
// Each query must select: id, label, sub_label, status, direction, created_at
// and use positional args: $1=orgID, $2=start, $3=end.
var tableQuerySQL = map[string]struct{ base, orderBy string }{
	"messages": {
		base: `SELECT m.id,
			COALESCE((
				SELECT COALESCE(NULLIF(c.profile_name, ''), c.phone_number)
				FROM contacts c
				WHERE c.id = m.contact_id
				  AND c.organization_id = m.organization_id
				  AND c.deleted_at IS NULL
				LIMIT 1
			), m.contact_id::text) as label,
			LEFT(m.content, 80) as sub_label, m.status, m.direction, m.created_at
			FROM messages m
			WHERE m.organization_id = ? AND m.created_at >= ? AND m.created_at <= ?
			  AND m.deleted_at IS NULL`,
		orderBy: " ORDER BY m.created_at DESC LIMIT 10",
	},
	"contacts": {
		base: `SELECT id, COALESCE(NULLIF(profile_name, ''), phone_number) as label,
			phone_number as sub_label, '' as status, '' as direction, last_message_at as created_at
			FROM contacts
			WHERE organization_id = ? AND last_message_at >= ? AND last_message_at <= ?
			  AND deleted_at IS NULL`,
		orderBy: " ORDER BY last_message_at DESC LIMIT 10",
	},
	"campaigns": {
		base: `SELECT id, name as label, status as sub_label, status, '' as direction, created_at
			FROM bulk_message_campaigns
			WHERE organization_id = ? AND created_at >= ? AND created_at <= ?
			  AND deleted_at IS NULL`,
		orderBy: " ORDER BY created_at DESC LIMIT 10",
	},
	"transfers": {
		base: `SELECT t.id,
			COALESCE((
				SELECT COALESCE(NULLIF(c.profile_name, ''), c.phone_number)
				FROM contacts c
				WHERE c.id = t.contact_id
				  AND c.organization_id = t.organization_id
				  AND c.deleted_at IS NULL
				LIMIT 1
			), t.contact_id::text) as label,
			t.source as sub_label, t.status, '' as direction, t.transferred_at as created_at
			FROM agent_transfers t
			WHERE t.organization_id = ? AND t.transferred_at >= ? AND t.transferred_at <= ?
			  AND t.deleted_at IS NULL`,
		orderBy: " ORDER BY t.transferred_at DESC LIMIT 10",
	},
	"sessions": {
		base: `SELECT s.id,
			COALESCE((
				SELECT COALESCE(NULLIF(c.profile_name, ''), c.phone_number)
				FROM contacts c
				WHERE c.id = s.contact_id
				  AND c.organization_id = s.organization_id
				  AND c.deleted_at IS NULL
				LIMIT 1
			), s.contact_id::text) as label,
			s.status as sub_label, s.status, '' as direction, s.created_at
			FROM chatbot_sessions s
			WHERE s.organization_id = ? AND s.created_at >= ? AND s.created_at <= ?
			  AND s.deleted_at IS NULL`,
		orderBy: " ORDER BY s.created_at DESC LIMIT 10",
	},
	"crm_leads": {
		base: `SELECT id, title as label, source as sub_label, status,
			'' as direction, created_at
			FROM crm_leads
			WHERE organization_id = ? AND created_at >= ? AND created_at <= ?
			  AND deleted_at IS NULL`,
		orderBy: " ORDER BY created_at DESC LIMIT 10",
	},
	"bookings": {
		base: `SELECT id, contact_id::text as label, event_id::text as sub_label,
			status, '' as direction, created_at
			FROM bookings
			WHERE organization_id = ? AND created_at >= ? AND created_at <= ?
			  AND deleted_at IS NULL`,
		orderBy: " ORDER BY created_at DESC LIMIT 10",
	},
	"invoices": {
		base: `SELECT id, invoice_number as label,
			currency || ' ' || total_minor::text as sub_label,
			status, '' as direction, created_at
			FROM commerce_invoices
			WHERE organization_id = ? AND created_at >= ? AND created_at <= ?
			  AND deleted_at IS NULL`,
		orderBy: " ORDER BY created_at DESC LIMIT 10",
	},
	"payments": {
		base: `SELECT id,
			COALESCE(NULLIF(provider_transaction_id, ''), id::text) as label,
			currency || ' ' || amount_minor::text as sub_label,
			status, '' as direction, occurred_at as created_at
			FROM payment_transactions
			WHERE organization_id = ? AND occurred_at >= ? AND occurred_at <= ?`,
		orderBy: " ORDER BY occurred_at DESC LIMIT 10",
	},
	"packages": {
		base: `SELECT cp.id,
			COALESCE((
				SELECT pd.name
				FROM package_definitions pd
				WHERE pd.id = cp.package_definition_id
				  AND pd.organization_id = cp.organization_id
				  AND pd.deleted_at IS NULL
				LIMIT 1
			), cp.package_definition_id::text) as label,
			cp.currency || ' ' || cp.purchase_amount_minor::text as sub_label,
			cp.status, '' as direction, cp.created_at
			FROM contact_packages cp
			WHERE cp.organization_id = ? AND cp.created_at >= ? AND cp.created_at <= ?
			  AND cp.deleted_at IS NULL`,
		orderBy: " ORDER BY cp.created_at DESC LIMIT 10",
	},
}

// getTableRows returns the last 10 rows for a table widget based on the data source.
func (a *App) getTableRows(orgID uuid.UUID, widget models.Widget, filters []FilterInput, periodStart, periodEnd time.Time) []TableRow {
	sql, ok := tableQuerySQL[widget.DataSource]
	if !ok {
		return []TableRow{}
	}

	query := sql.base
	args := []any{orgID, periodStart, periodEnd}
	query, args = appendFilterSQL(widget.DataSource, query, args, filters)
	query += sql.orderBy

	type row struct {
		ID        string
		Label     string
		SubLabel  string `gorm:"column:sub_label"`
		Status    string
		Direction string
		CreatedAt time.Time
	}
	var results []row
	a.DB.Raw(query, args...).Scan(&results)

	tableRows := make([]TableRow, len(results))
	for i, r := range results {
		label := r.Label
		subLabel := r.SubLabel
		switch widget.DataSource {
		case "contacts":
			label, subLabel = a.MaskContactFields(orgID, label, subLabel)
		case "messages", "transfers", "sessions":
			label, _ = a.MaskContactFields(orgID, label, label)
		}
		tableRows[i] = TableRow{
			ID:        r.ID,
			Label:     label,
			SubLabel:  subLabel,
			Status:    r.Status,
			Direction: r.Direction,
			CreatedAt: r.CreatedAt.Format(time.RFC3339),
		}
	}
	return tableRows
}
