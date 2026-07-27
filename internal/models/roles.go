package models

import (
	"github.com/google/uuid"
)

// Permission represents a granular permission for a specific resource and action
type Permission struct {
	BaseModel
	Resource    string `gorm:"size:50;not null;uniqueIndex:idx_permission_resource_action" json:"resource"`
	Action      string `gorm:"size:20;not null;uniqueIndex:idx_permission_resource_action" json:"action"`
	Description string `gorm:"size:200" json:"description"`
}

func (Permission) TableName() string {
	return "permissions"
}

// CustomRole represents a role with specific permissions
type CustomRole struct {
	BaseModel
	OrganizationID uuid.UUID    `gorm:"type:uuid;index;not null" json:"organization_id"`
	Name           string       `gorm:"size:100;not null" json:"name"`
	Description    string       `gorm:"size:500" json:"description"`
	IsSystem       bool         `gorm:"default:false" json:"is_system"`  // true for default admin/manager/agent
	IsDefault      bool         `gorm:"default:false" json:"is_default"` // default role for new users in org
	Permissions    []Permission `gorm:"many2many:role_permissions;" json:"permissions"`

	// Relations
	Organization *Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
}

func (CustomRole) TableName() string {
	return "custom_roles"
}

// RolePermission is the junction table for CustomRole and Permission many-to-many relationship
type RolePermission struct {
	CustomRoleID uuid.UUID `gorm:"type:uuid;primaryKey" json:"custom_role_id"`
	PermissionID uuid.UUID `gorm:"type:uuid;primaryKey" json:"permission_id"`
}

func (RolePermission) TableName() string {
	return "role_permissions"
}

// PermissionResource constants for available resources
const (
	ResourceUsers                = "users"
	ResourceTeams                = "teams"
	ResourceRoles                = "roles"
	ResourceSettingsGeneral      = "settings.general"
	ResourceSettingsChatbot      = "settings.chatbot"
	ResourceSettingsSSO          = "settings.sso"
	ResourceSettingsCalling      = "settings.calling"
	ResourceSettingsNotification = "settings.notification"
	// Chatbot sub-resources — used only as audit_log resource_type values
	// for per-tab activity feeds, not checked by the permission system.
	ResourceSettingsChatbotMessages = "settings.chatbot.messages"
	ResourceSettingsChatbotAgents   = "settings.chatbot.agents"
	ResourceSettingsChatbotHours    = "settings.chatbot.hours"
	ResourceSettingsChatbotSLA      = "settings.chatbot.sla"
	ResourceSettingsChatbotAI       = "settings.chatbot.ai"
	ResourceAccounts                = "accounts"
	ResourceTemplates               = "templates"
	ResourceFlowsWhatsApp           = "flows.whatsapp"
	ResourceFlowsChatbot            = "flows.chatbot"
	ResourceCampaigns               = "campaigns"
	ResourceChatbotKeywords         = "chatbot.keywords"
	ResourceChatbotAI               = "chatbot.ai"
	ResourceChat                    = "chat"
	ResourceChatAssign              = "chat.assign"
	ResourceContacts                = "contacts"
	ResourceTags                    = "tags"
	ResourceAnalytics               = "analytics"
	ResourceAnalyticsAgents         = "analytics.agents"
	ResourceTransfers               = "transfers"
	ResourceWebhooks                = "webhooks"
	ResourceAPIKeys                 = "api_keys"
	ResourceCannedResponses         = "canned_responses"
	ResourceCustomActions           = "custom_actions"
	ResourceOrganizations           = "organizations"
	ResourceCallLogs                = "call_logs"
	ResourceIVRFlows                = "ivr_flows"
	ResourceCallTransfers           = "call_transfers"
	ResourceOutgoingCalls           = "outgoing_calls"
	ResourceAuditLogs               = "audit_logs"
	ResourceBilling                 = "billing"
	ResourceEntitlements            = "entitlements"
	ResourceOnboarding              = "onboarding"
	ResourceWorkspaceTemplates      = "workspace_templates"
	ResourcePrivacySettings         = "privacy.settings"
	ResourcePrivacyConsents         = "privacy.consents"
	ResourcePrivacyRequests         = "privacy.requests"
	ResourcePrivacyIncidents        = "privacy.incidents"
	ResourceSupport                 = "support"
	ResourceOperations              = "operations"
	ResourceCRMPipelines            = "crm.pipelines"
	ResourceCRMLeads                = "crm.leads"
	ResourceTasks                   = "tasks"
	ResourceBookings                = "bookings"
	ResourceBookingSettings         = "booking.settings"
	ResourcePackages                = "packages"
	ResourceCredits                 = "credits"
	ResourcePayments                = "payments"
	ResourcePaymentRefunds          = "payments.refunds"
	ResourceCopilot                 = "copilot"
	ResourceChannelAccounts         = "channel_accounts"
	ResourceConversations           = "conversations"
)

// PermissionAction constants for available actions
const (
	ActionRead    = "read"
	ActionWrite   = "write"
	ActionDelete  = "delete"
	ActionSync    = "sync"
	ActionExecute = "execute"
	ActionImport  = "import"
	ActionExport  = "export"
	ActionPickup  = "pickup"
	ActionAssign  = "assign"
)

// DefaultPermissions returns the list of all available permissions to seed
func DefaultPermissions() []Permission {
	return []Permission{
		// Users
		{Resource: ResourceUsers, Action: ActionRead, Description: "View users"},
		{Resource: ResourceUsers, Action: ActionWrite, Description: "Create and edit users"},
		{Resource: ResourceUsers, Action: ActionDelete, Description: "Delete users"},

		// Teams
		{Resource: ResourceTeams, Action: ActionRead, Description: "View teams"},
		{Resource: ResourceTeams, Action: ActionWrite, Description: "Create and edit teams"},
		{Resource: ResourceTeams, Action: ActionDelete, Description: "Delete teams"},

		// Roles
		{Resource: ResourceRoles, Action: ActionRead, Description: "View roles"},
		{Resource: ResourceRoles, Action: ActionWrite, Description: "Create and edit roles"},
		{Resource: ResourceRoles, Action: ActionDelete, Description: "Delete roles"},

		// Settings
		{Resource: ResourceSettingsGeneral, Action: ActionRead, Description: "View general settings"},
		{Resource: ResourceSettingsGeneral, Action: ActionWrite, Description: "Edit general settings"},
		{Resource: ResourceSettingsChatbot, Action: ActionRead, Description: "View chatbot settings"},
		{Resource: ResourceSettingsChatbot, Action: ActionWrite, Description: "Edit chatbot settings"},
		{Resource: ResourceSettingsSSO, Action: ActionRead, Description: "View SSO settings"},
		{Resource: ResourceSettingsSSO, Action: ActionWrite, Description: "Edit SSO settings"},

		// Accounts
		{Resource: ResourceAccounts, Action: ActionRead, Description: "View WhatsApp accounts"},
		{Resource: ResourceAccounts, Action: ActionWrite, Description: "Create and edit WhatsApp accounts"},
		{Resource: ResourceAccounts, Action: ActionDelete, Description: "Delete WhatsApp accounts"},

		// Templates
		{Resource: ResourceTemplates, Action: ActionRead, Description: "View message templates"},
		{Resource: ResourceTemplates, Action: ActionWrite, Description: "Create and edit templates"},
		{Resource: ResourceTemplates, Action: ActionDelete, Description: "Delete templates"},
		{Resource: ResourceTemplates, Action: ActionSync, Description: "Sync templates with Meta"},

		// WhatsApp Flows
		{Resource: ResourceFlowsWhatsApp, Action: ActionRead, Description: "View WhatsApp flows"},
		{Resource: ResourceFlowsWhatsApp, Action: ActionWrite, Description: "Create and edit WhatsApp flows"},
		{Resource: ResourceFlowsWhatsApp, Action: ActionDelete, Description: "Delete WhatsApp flows"},

		// Chatbot Flows
		{Resource: ResourceFlowsChatbot, Action: ActionRead, Description: "View chatbot flows"},
		{Resource: ResourceFlowsChatbot, Action: ActionWrite, Description: "Create and edit chatbot flows"},
		{Resource: ResourceFlowsChatbot, Action: ActionDelete, Description: "Delete chatbot flows"},

		// Campaigns
		{Resource: ResourceCampaigns, Action: ActionRead, Description: "View campaigns"},
		{Resource: ResourceCampaigns, Action: ActionWrite, Description: "Create and edit campaigns"},
		{Resource: ResourceCampaigns, Action: ActionDelete, Description: "Delete campaigns"},
		{Resource: ResourceCampaigns, Action: ActionExecute, Description: "Execute campaigns"},

		// Chatbot Keywords
		{Resource: ResourceChatbotKeywords, Action: ActionRead, Description: "View keyword rules"},
		{Resource: ResourceChatbotKeywords, Action: ActionWrite, Description: "Create and edit keyword rules"},
		{Resource: ResourceChatbotKeywords, Action: ActionDelete, Description: "Delete keyword rules"},

		// Chatbot AI
		{Resource: ResourceChatbotAI, Action: ActionRead, Description: "View AI contexts"},
		{Resource: ResourceChatbotAI, Action: ActionWrite, Description: "Create and edit AI contexts"},
		{Resource: ResourceChatbotAI, Action: ActionDelete, Description: "Delete AI contexts"},

		// Chat
		{Resource: ResourceChat, Action: ActionRead, Description: "View chat conversations"},
		{Resource: ResourceChat, Action: ActionWrite, Description: "Send messages"},
		{Resource: ResourceChatAssign, Action: ActionWrite, Description: "Assign conversations to agents"},

		// Contacts
		{Resource: ResourceContacts, Action: ActionRead, Description: "View contacts"},
		{Resource: ResourceContacts, Action: ActionWrite, Description: "Create and edit contacts"},
		{Resource: ResourceContacts, Action: ActionDelete, Description: "Delete contacts"},
		{Resource: ResourceContacts, Action: ActionImport, Description: "Import contacts"},
		{Resource: ResourceContacts, Action: ActionExport, Description: "Export contacts"},

		// Tags
		{Resource: ResourceTags, Action: ActionRead, Description: "View tags"},
		{Resource: ResourceTags, Action: ActionWrite, Description: "Create and edit tags"},
		{Resource: ResourceTags, Action: ActionDelete, Description: "Delete tags"},

		// Analytics
		{Resource: ResourceAnalytics, Action: ActionRead, Description: "View analytics dashboard"},
		{Resource: ResourceAnalytics, Action: ActionWrite, Description: "Create and edit dashboard widgets"},
		{Resource: ResourceAnalytics, Action: ActionDelete, Description: "Delete dashboard widgets"},
		{Resource: ResourceAnalyticsAgents, Action: ActionRead, Description: "View agent analytics"},

		// Transfers
		{Resource: ResourceTransfers, Action: ActionRead, Description: "View agent transfers"},
		{Resource: ResourceTransfers, Action: ActionWrite, Description: "Create transfers"},
		{Resource: ResourceTransfers, Action: ActionPickup, Description: "Pickup transfers from queue"},

		// Webhooks
		{Resource: ResourceWebhooks, Action: ActionRead, Description: "View webhooks"},
		{Resource: ResourceWebhooks, Action: ActionWrite, Description: "Create and edit webhooks"},
		{Resource: ResourceWebhooks, Action: ActionDelete, Description: "Delete webhooks"},

		// API Keys
		{Resource: ResourceAPIKeys, Action: ActionRead, Description: "View API keys"},
		{Resource: ResourceAPIKeys, Action: ActionWrite, Description: "Create API keys"},
		{Resource: ResourceAPIKeys, Action: ActionDelete, Description: "Delete API keys"},

		// Canned Responses
		{Resource: ResourceCannedResponses, Action: ActionRead, Description: "View canned responses"},
		{Resource: ResourceCannedResponses, Action: ActionWrite, Description: "Create and edit canned responses"},
		{Resource: ResourceCannedResponses, Action: ActionDelete, Description: "Delete canned responses"},

		// Custom Actions
		{Resource: ResourceCustomActions, Action: ActionRead, Description: "View custom actions"},
		{Resource: ResourceCustomActions, Action: ActionWrite, Description: "Create and edit custom actions"},
		{Resource: ResourceCustomActions, Action: ActionDelete, Description: "Delete custom actions"},

		// Organizations
		{Resource: ResourceOrganizations, Action: ActionRead, Description: "View organizations"},
		{Resource: ResourceOrganizations, Action: ActionWrite, Description: "Create organizations"},
		{Resource: ResourceOrganizations, Action: ActionDelete, Description: "Delete organizations"},
		{Resource: ResourceOrganizations, Action: ActionAssign, Description: "Manage organization members"},

		// Call Logs
		{Resource: ResourceCallLogs, Action: ActionRead, Description: "View call logs"},

		// IVR Flows
		{Resource: ResourceIVRFlows, Action: ActionRead, Description: "View IVR flows"},
		{Resource: ResourceIVRFlows, Action: ActionWrite, Description: "Create and edit IVR flows"},
		{Resource: ResourceIVRFlows, Action: ActionDelete, Description: "Delete IVR flows"},

		// Call Transfers
		{Resource: ResourceCallTransfers, Action: ActionRead, Description: "View call transfers"},
		{Resource: ResourceCallTransfers, Action: ActionWrite, Description: "Accept and manage call transfers"},

		// Outgoing Calls
		{Resource: ResourceOutgoingCalls, Action: ActionRead, Description: "View outgoing call status"},
		{Resource: ResourceOutgoingCalls, Action: ActionWrite, Description: "Initiate outgoing calls"},

		// Audit Logs
		{Resource: ResourceAuditLogs, Action: ActionRead, Description: "View audit logs"},

		// Commercial operations
		{Resource: ResourceBilling, Action: ActionRead, Description: "View subscription and invoices"},
		{Resource: ResourceBilling, Action: ActionWrite, Description: "Manage subscription and billing profile"},
		{Resource: ResourceEntitlements, Action: ActionRead, Description: "View effective feature entitlements"},
		{Resource: ResourceEntitlements, Action: ActionWrite, Description: "Manage entitlement overrides"},
		{Resource: ResourceOnboarding, Action: ActionRead, Description: "View workspace onboarding"},
		{Resource: ResourceOnboarding, Action: ActionWrite, Description: "Complete workspace onboarding"},
		{Resource: ResourceWorkspaceTemplates, Action: ActionRead, Description: "View workspace templates"},
		{Resource: ResourceWorkspaceTemplates, Action: ActionWrite, Description: "Create and apply workspace templates"},

		// Privacy and support
		{Resource: ResourcePrivacySettings, Action: ActionRead, Description: "View privacy and retention settings"},
		{Resource: ResourcePrivacySettings, Action: ActionWrite, Description: "Manage privacy and retention settings"},
		{Resource: ResourcePrivacyConsents, Action: ActionRead, Description: "View consent evidence"},
		{Resource: ResourcePrivacyConsents, Action: ActionWrite, Description: "Record consent evidence"},
		{Resource: ResourcePrivacyRequests, Action: ActionRead, Description: "View privacy requests"},
		{Resource: ResourcePrivacyRequests, Action: ActionWrite, Description: "Process privacy requests"},
		{Resource: ResourcePrivacyRequests, Action: ActionExecute, Description: "Reserved for approved privacy fulfillment workers"},
		{Resource: ResourcePrivacyIncidents, Action: ActionRead, Description: "View privacy incidents"},
		{Resource: ResourcePrivacyIncidents, Action: ActionWrite, Description: "Manage privacy incidents"},
		{Resource: ResourceSupport, Action: ActionRead, Description: "View support cases and tenant health"},
		{Resource: ResourceSupport, Action: ActionWrite, Description: "Create and update support cases"},
		{Resource: ResourceOperations, Action: ActionRead, Description: "View recovery and delivery operations"},
		{Resource: ResourceOperations, Action: ActionExecute, Description: "Reserved for approved recovery workers"},

		// CRM and bookings
		{Resource: ResourceCRMPipelines, Action: ActionRead, Description: "View CRM pipelines"},
		{Resource: ResourceCRMPipelines, Action: ActionWrite, Description: "Manage CRM pipelines and stages"},
		{Resource: ResourceCRMPipelines, Action: ActionDelete, Description: "Delete CRM pipelines and stages"},
		{Resource: ResourceCRMLeads, Action: ActionRead, Description: "View CRM leads"},
		{Resource: ResourceCRMLeads, Action: ActionWrite, Description: "Create, edit, and move CRM leads"},
		{Resource: ResourceCRMLeads, Action: ActionDelete, Description: "Archive CRM leads"},
		{Resource: ResourceTasks, Action: ActionRead, Description: "View follow-up tasks"},
		{Resource: ResourceTasks, Action: ActionWrite, Description: "Create and complete follow-up tasks"},
		{Resource: ResourceTasks, Action: ActionDelete, Description: "Cancel follow-up tasks"},
		{Resource: ResourceBookings, Action: ActionRead, Description: "View bookings and classes"},
		{Resource: ResourceBookings, Action: ActionWrite, Description: "Create and transition bookings"},
		{Resource: ResourceBookings, Action: ActionDelete, Description: "Cancel booking records"},
		{Resource: ResourceBookingSettings, Action: ActionRead, Description: "View services, resources, and availability"},
		{Resource: ResourceBookingSettings, Action: ActionWrite, Description: "Manage services, resources, and availability"},
		{Resource: ResourcePackages, Action: ActionRead, Description: "View packages"},
		{Resource: ResourcePackages, Action: ActionWrite, Description: "Manage packages and customer purchases"},
		{Resource: ResourcePackages, Action: ActionDelete, Description: "Retire packages"},
		{Resource: ResourceCredits, Action: ActionRead, Description: "View package credit balances"},
		{Resource: ResourceCredits, Action: ActionExecute, Description: "Adjust or redeem package credits"},
		{Resource: ResourcePayments, Action: ActionRead, Description: "View invoices and payments"},
		{Resource: ResourcePayments, Action: ActionWrite, Description: "Create invoices and payment intents"},
		{Resource: ResourcePaymentRefunds, Action: ActionExecute, Description: "Issue payment refunds"},

		// AI and omnichannel
		{Resource: ResourceCopilot, Action: ActionRead, Description: "View Qwen Copilot runs"},
		{Resource: ResourceCopilot, Action: ActionExecute, Description: "Generate and approve Copilot suggestions"},
		{Resource: ResourceChannelAccounts, Action: ActionRead, Description: "View channel connections"},
		{Resource: ResourceChannelAccounts, Action: ActionWrite, Description: "Connect and configure channels"},
		{Resource: ResourceChannelAccounts, Action: ActionDelete, Description: "Disconnect channels"},
		{Resource: ResourceConversations, Action: ActionRead, Description: "View omnichannel conversations"},
		{Resource: ResourceConversations, Action: ActionWrite, Description: "Reply to and manage omnichannel conversations"},
	}
}

// SystemRolePermissions returns the default permission mappings for system roles
func SystemRolePermissions() map[string][]string {
	// Format: "resource:action"
	allPermissions := []string{}
	for _, p := range DefaultPermissions() {
		allPermissions = append(allPermissions, p.Resource+":"+p.Action)
	}

	managerPermissions := []string{
		// Teams (read only)
		"teams:read",
		// Settings
		"settings.general:read", "settings.general:write",
		"settings.chatbot:read", "settings.chatbot:write",
		// Accounts
		"accounts:read", "accounts:write", "accounts:delete",
		// Templates
		"templates:read", "templates:write", "templates:delete", "templates:sync",
		// Flows
		"flows.whatsapp:read", "flows.whatsapp:write", "flows.whatsapp:delete",
		"flows.chatbot:read", "flows.chatbot:write", "flows.chatbot:delete",
		// Campaigns
		"campaigns:read", "campaigns:write", "campaigns:delete", "campaigns:execute",
		// Chatbot
		"chatbot.keywords:read", "chatbot.keywords:write", "chatbot.keywords:delete",
		"chatbot.ai:read", "chatbot.ai:write", "chatbot.ai:delete",
		// Chat
		"chat:read", "chat:write", "chat.assign:write",
		// Contacts
		"contacts:read", "contacts:write", "contacts:delete", "contacts:import", "contacts:export",
		// Tags
		"tags:read", "tags:write", "tags:delete",
		// Analytics
		"analytics:read", "analytics.agents:read",
		// Transfers
		"transfers:read", "transfers:write", "transfers:pickup",
		// Webhooks
		"webhooks:read", "webhooks:write", "webhooks:delete",
		// Canned Responses
		"canned_responses:read", "canned_responses:write", "canned_responses:delete",
		// Custom Actions
		"custom_actions:read", "custom_actions:write", "custom_actions:delete",
		// Organizations (read only)
		"organizations:read",
		// Calling
		"call_logs:read",
		"ivr_flows:read", "ivr_flows:write", "ivr_flows:delete",
		"call_transfers:read", "call_transfers:write",
		"outgoing_calls:read", "outgoing_calls:write",
		// Commercial workspace
		"billing:read", "entitlements:read",
		"onboarding:read", "onboarding:write",
		"workspace_templates:read",
		"privacy.settings:read", "privacy.settings:write",
		"privacy.consents:read", "privacy.consents:write",
		"privacy.requests:read", "privacy.requests:write",
		"support:read", "support:write",
		// CRM, bookings and commerce
		"crm.pipelines:read", "crm.pipelines:write",
		"crm.leads:read", "crm.leads:write", "crm.leads:delete",
		"tasks:read", "tasks:write", "tasks:delete",
		"bookings:read", "bookings:write", "bookings:delete",
		"booking.settings:read", "booking.settings:write",
		"packages:read", "packages:write",
		"credits:read",
		"payments:read", "payments:write",
		"copilot:read", "copilot:execute",
		"channel_accounts:read", "channel_accounts:write",
		"conversations:read", "conversations:write",
	}

	agentPermissions := []string{
		// Accounts (read only)
		"accounts:read",
		// Chat
		"chat:read", "chat:write",
		// Contacts (read only)
		"contacts:read",
		// Tags (read only - agents can see tags on contacts)
		"tags:read",
		// Analytics (own)
		"analytics.agents:read",
		// Transfers
		"transfers:read", "transfers:write", "transfers:pickup",
		// Canned Responses (read only)
		"canned_responses:read",
		// Call Logs: agents see only their own (no call_logs:read permission)
		// Call Transfers
		"call_transfers:read", "call_transfers:write",
		// Outgoing Calls
		"outgoing_calls:read", "outgoing_calls:write",
		// CRM workspace
		"crm.pipelines:read",
		"crm.leads:read", "crm.leads:write",
		"tasks:read", "tasks:write",
		"bookings:read", "bookings:write",
		"booking.settings:read",
		"packages:read", "credits:read", "payments:read",
		"copilot:read", "copilot:execute",
		"channel_accounts:read",
		"conversations:read", "conversations:write",
	}

	return map[string][]string{
		"admin":   allPermissions,
		"manager": managerPermissions,
		"agent":   agentPermissions,
	}
}
