package handlers

import (
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

// The built-in playbooks intentionally provision workflow structure only.
// Prices, clinicians, treatment protocols, legal bases, and retention periods
// remain tenant decisions and are never inferred from a vertical label.
type productTemplateStageSeed struct {
	Key         string
	Name        string
	Color       string
	Kind        models.CRMPipelineStageKind
	Probability int
	SLAHours    int
}

type productTemplatePipelineSeed struct {
	Key         string
	Name        string
	Description string
	Stages      []productTemplateStageSeed
}

type productTemplateServiceSeed struct {
	Key             string
	Name            string
	Description     string
	Kind            models.BookingServiceKind
	DurationMinutes int
	DefaultCapacity int
}

type productTemplateProvisioningSummary struct {
	PipelineCreated bool
	StagesCreated   int
	ServicesCreated int
}

func productCommercialBuildBuiltInTemplates() []builtInWorkspaceTemplate {
	templates := []builtInWorkspaceTemplate{
		{
			Key:         "clinic",
			Name:        "Clinic Edition",
			Vertical:    "clinic",
			Description: "A privacy-conscious patient enquiry, appointment and follow-up workspace.",
			Highlights: []string{
				"New enquiry to attended pipeline",
				"Appointment and post-visit follow-ups",
				"Consent-first messaging defaults",
			},
			Pipeline: productTemplatePipelineSeed{
				Key:         "patient-journey",
				Name:        "Patient journey",
				Description: "From first enquiry through attendance and follow-up.",
				Stages: []productTemplateStageSeed{
					{Key: "new-enquiry", Name: "New enquiry", Color: "#3B82F6", Kind: models.CRMPipelineStageKindOpen, Probability: 10, SLAHours: 4},
					{Key: "qualified", Name: "Qualified", Color: "#6366F1", Kind: models.CRMPipelineStageKindOpen, Probability: 25, SLAHours: 24},
					{Key: "appointment-booked", Name: "Appointment booked", Color: "#8B5CF6", Kind: models.CRMPipelineStageKindOpen, Probability: 50, SLAHours: 48},
					{Key: "attended", Name: "Attended", Color: "#14B8A6", Kind: models.CRMPipelineStageKindOpen, Probability: 75, SLAHours: 24},
					{Key: "follow-up", Name: "Follow-up", Color: "#F59E0B", Kind: models.CRMPipelineStageKindOpen, Probability: 85, SLAHours: 72},
					{Key: "converted", Name: "Converted", Color: "#22C55E", Kind: models.CRMPipelineStageKindWon, Probability: 100},
					{Key: "not-proceeding", Name: "Not proceeding", Color: "#EF4444", Kind: models.CRMPipelineStageKindLost},
				},
			},
			Services: []productTemplateServiceSeed{
				{Key: "initial-assessment", Name: "Initial assessment", Description: "Starter appointment type; assign duration, price and practitioner before publishing.", Kind: models.BookingServiceKindAppointment, DurationMinutes: 60, DefaultCapacity: 1},
				{Key: "follow-up-appointment", Name: "Follow-up appointment", Description: "Starter follow-up type; review duration and price before publishing.", Kind: models.BookingServiceKindAppointment, DurationMinutes: 30, DefaultCapacity: 1},
			},
		},
		{
			Key:         "pharmacy",
			Name:        "Pharmacy Edition",
			Vertical:    "pharmacy",
			Description: "A structured product enquiry, collection and replenishment workspace.",
			Highlights: []string{
				"Product enquiry qualification",
				"Collection-ready notifications",
				"Repeat purchase follow-ups",
			},
			Pipeline: productTemplatePipelineSeed{
				Key:         "pharmacy-enquiries",
				Name:        "Pharmacy enquiries",
				Description: "From product enquiry through collection and repeat follow-up.",
				Stages: []productTemplateStageSeed{
					{Key: "new-enquiry", Name: "New enquiry", Color: "#3B82F6", Kind: models.CRMPipelineStageKindOpen, Probability: 10, SLAHours: 2},
					{Key: "product-confirmed", Name: "Product confirmed", Color: "#6366F1", Kind: models.CRMPipelineStageKindOpen, Probability: 35, SLAHours: 8},
					{Key: "collection-ready", Name: "Collection ready", Color: "#8B5CF6", Kind: models.CRMPipelineStageKindOpen, Probability: 65, SLAHours: 24},
					{Key: "repeat-follow-up", Name: "Repeat follow-up", Color: "#F59E0B", Kind: models.CRMPipelineStageKindOpen, Probability: 80, SLAHours: 168},
					{Key: "completed", Name: "Completed", Color: "#22C55E", Kind: models.CRMPipelineStageKindWon, Probability: 100},
					{Key: "not-proceeding", Name: "Not proceeding", Color: "#EF4444", Kind: models.CRMPipelineStageKindLost},
				},
			},
			Services: []productTemplateServiceSeed{
				{Key: "pharmacy-consultation", Name: "Pharmacy consultation", Description: "Starter consultation type; assign duration, price and pharmacist before publishing.", Kind: models.BookingServiceKindAppointment, DurationMinutes: 15, DefaultCapacity: 1},
			},
		},
		{
			Key:         "wellness",
			Name:        "Wellness Edition",
			Vertical:    "wellness",
			Description: "A consultation, class, package and retention workspace.",
			Highlights: []string{
				"Consultation-to-package pipeline",
				"Class and practitioner booking defaults",
				"Package renewal follow-ups",
			},
			Pipeline: productTemplatePipelineSeed{
				Key:         "wellness-membership",
				Name:        "Wellness membership",
				Description: "From new lead through consultation, trial and membership.",
				Stages: []productTemplateStageSeed{
					{Key: "new-lead", Name: "New lead", Color: "#3B82F6", Kind: models.CRMPipelineStageKindOpen, Probability: 10, SLAHours: 4},
					{Key: "consultation-booked", Name: "Consultation booked", Color: "#6366F1", Kind: models.CRMPipelineStageKindOpen, Probability: 35, SLAHours: 48},
					{Key: "trial-attended", Name: "Trial attended", Color: "#8B5CF6", Kind: models.CRMPipelineStageKindOpen, Probability: 60, SLAHours: 24},
					{Key: "package-offered", Name: "Package offered", Color: "#F59E0B", Kind: models.CRMPipelineStageKindOpen, Probability: 80, SLAHours: 72},
					{Key: "member-active", Name: "Member active", Color: "#22C55E", Kind: models.CRMPipelineStageKindWon, Probability: 100},
					{Key: "not-proceeding", Name: "Not proceeding", Color: "#EF4444", Kind: models.CRMPipelineStageKindLost},
				},
			},
			Services: []productTemplateServiceSeed{
				{Key: "wellness-consultation", Name: "Wellness consultation", Description: "Starter consultation type; assign duration, price and practitioner before publishing.", Kind: models.BookingServiceKindAppointment, DurationMinutes: 45, DefaultCapacity: 1},
				{Key: "group-class", Name: "Group class", Description: "Starter class type; review capacity, price and instructor before publishing.", Kind: models.BookingServiceKindClass, DurationMinutes: 60, DefaultCapacity: 8},
				{Key: "private-pilates", Name: "Private Pilates", Description: "Starter private session; review duration, price and instructor before publishing.", Kind: models.BookingServiceKindAppointment, DurationMinutes: 60, DefaultCapacity: 1},
			},
		},
	}

	for index := range templates {
		templates[index].Manifest = productCommercialBuiltInManifest(templates[index])
	}
	return templates
}

func productCommercialBuiltInManifest(template builtInWorkspaceTemplate) models.JSONB {
	highlights := make([]any, 0, len(template.Highlights))
	for _, highlight := range template.Highlights {
		highlights = append(highlights, highlight)
	}

	stages := make([]any, 0, len(template.Pipeline.Stages))
	for _, stage := range template.Pipeline.Stages {
		stages = append(stages, models.JSONB{
			"key":         stage.Key,
			"name":        stage.Name,
			"color":       stage.Color,
			"kind":        string(stage.Kind),
			"probability": stage.Probability,
			"sla_hours":   stage.SLAHours,
		})
	}

	services := make([]any, 0, len(template.Services))
	for _, service := range template.Services {
		services = append(services, models.JSONB{
			"key":              service.Key,
			"name":             service.Name,
			"description":      service.Description,
			"kind":             string(service.Kind),
			"duration_minutes": service.DurationMinutes,
			"default_capacity": service.DefaultCapacity,
		})
	}

	return models.JSONB{
		"schema":     "workspace.v2",
		"vertical":   template.Vertical,
		"highlights": highlights,
		"pipeline": models.JSONB{
			"key":         template.Pipeline.Key,
			"name":        template.Pipeline.Name,
			"description": template.Pipeline.Description,
			"stages":      stages,
		},
		"booking_services": services,
	}
}

func productCommercialProvisionBuiltInTemplateResources(
	tx *gorm.DB,
	organization models.Organization,
	application models.WorkspaceTemplateApplication,
	version models.WorkspaceTemplateVersion,
	template builtInWorkspaceTemplate,
	userID uuid.UUID,
) (productTemplateProvisioningSummary, error) {
	summary := productTemplateProvisioningSummary{}
	currency := productCommercialTemplateCurrency(organization.Settings)

	var defaultPipelineCount int64
	if err := tx.Model(&models.CRMPipeline{}).
		Where("organization_id = ? AND is_default = ? AND is_active = ?", organization.ID, true, true).
		Count(&defaultPipelineCount).Error; err != nil {
		return summary, err
	}

	pipeline := models.CRMPipeline{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Name:           template.Pipeline.Name,
		Description:    template.Pipeline.Description,
		IsDefault:      defaultPipelineCount == 0,
		IsActive:       true,
		DisplayOrder:   0,
		Version:        1,
		CreatedByID:    &userID,
		UpdatedByID:    &userID,
	}
	if err := tx.Create(&pipeline).Error; err != nil {
		return summary, err
	}
	if err := productCommercialCreateTemplateResourceMap(
		tx,
		organization.ID,
		application.ID,
		"crm.pipeline."+template.Pipeline.Key,
		"crm_pipeline",
		pipeline.ID,
		version.Checksum,
		models.JSONB{"is_default": pipeline.IsDefault},
	); err != nil {
		return summary, err
	}
	summary.PipelineCreated = true

	for order, seed := range template.Pipeline.Stages {
		stage := models.CRMPipelineStage{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: organization.ID,
			PipelineID:     pipeline.ID,
			Name:           seed.Name,
			Color:          seed.Color,
			DisplayOrder:   order,
			Kind:           seed.Kind,
			Probability:    seed.Probability,
			SLAHours:       seed.SLAHours,
			IsActive:       true,
			Version:        1,
			CreatedByID:    &userID,
			UpdatedByID:    &userID,
		}
		if err := tx.Create(&stage).Error; err != nil {
			return summary, err
		}
		if err := productCommercialCreateTemplateResourceMap(
			tx,
			organization.ID,
			application.ID,
			"crm.pipeline."+template.Pipeline.Key+".stage."+seed.Key,
			"crm_pipeline_stage",
			stage.ID,
			version.Checksum,
			models.JSONB{"display_order": order, "kind": string(seed.Kind)},
		); err != nil {
			return summary, err
		}
		summary.StagesCreated++
	}

	for _, seed := range template.Services {
		service := models.BookingService{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			OrganizationID:  organization.ID,
			Name:            seed.Name,
			Description:     seed.Description,
			Kind:            seed.Kind,
			DurationMinutes: seed.DurationMinutes,
			DefaultCapacity: seed.DefaultCapacity,
			PriceMinor:      0,
			Currency:        currency,
			ReminderPolicy:  models.JSONB{},
			IsActive:        false,
			Metadata:        models.JSONB{"template_key": template.Key, "requires_review": true},
			Version:         1,
			CreatedByID:     &userID,
			UpdatedByID:     &userID,
		}
		if err := tx.Create(&service).Error; err != nil {
			return summary, err
		}
		// GORM applies the model's default:true tag to a false zero value during
		// Create. Starter services must remain unavailable until the tenant has
		// reviewed their duration, capacity, and pricing.
		if err := tx.Model(&service).UpdateColumn("is_active", false).Error; err != nil {
			return summary, err
		}
		if err := productCommercialCreateTemplateResourceMap(
			tx,
			organization.ID,
			application.ID,
			"booking.service."+seed.Key,
			"booking_service",
			service.ID,
			version.Checksum,
			models.JSONB{"active": false, "requires_review": true},
		); err != nil {
			return summary, err
		}
		summary.ServicesCreated++
	}

	return summary, nil
}

func productCommercialCreateTemplateResourceMap(
	tx *gorm.DB,
	organizationID uuid.UUID,
	applicationID uuid.UUID,
	key string,
	resourceType string,
	resourceID uuid.UUID,
	checksum string,
	metadata models.JSONB,
) error {
	resourceMap := models.WorkspaceTemplateResourceMap{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      organizationID,
		ApplicationID:       applicationID,
		TemplateResourceKey: key,
		ResourceType:        resourceType,
		ResourceID:          resourceID.String(),
		Action:              "created",
		Status:              "active",
		SourceChecksum:      checksum,
		Metadata:            metadata,
	}
	return tx.Create(&resourceMap).Error
}

func productCommercialTemplateCurrency(settings models.JSONB) string {
	currency, _ := settings["currency"].(string)
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if len(currency) != 3 {
		return "MYR"
	}
	for _, character := range currency {
		if character < 'A' || character > 'Z' {
			return "MYR"
		}
	}
	return currency
}
