package models

import (
	"time"

	"github.com/google/uuid"
)

// Channel identifies the customer-facing transport used for a conversation.
// Provider-specific names belong in ChannelAccount.Provider so, for example,
// multiple WhatsApp BSPs can implement the same channel.
type Channel string

const (
	ChannelWhatsApp  Channel = "whatsapp"
	ChannelInstagram Channel = "instagram"
	ChannelMessenger Channel = "messenger"
	ChannelThreads   Channel = "threads"
	ChannelWebChat   Channel = "webchat"
	ChannelEmail     Channel = "email"
	ChannelSMS       Channel = "sms"
	ChannelTelegram  Channel = "telegram"
	ChannelTikTok    Channel = "tiktok"
)

type ChannelAccountStatus string

const (
	ChannelAccountStatusPending      ChannelAccountStatus = "pending"
	ChannelAccountStatusActive       ChannelAccountStatus = "active"
	ChannelAccountStatusDegraded     ChannelAccountStatus = "degraded"
	ChannelAccountStatusSuspended    ChannelAccountStatus = "suspended"
	ChannelAccountStatusDisconnected ChannelAccountStatus = "disconnected"
)

type ChannelCredentialKind string

const (
	ChannelCredentialKindPrimary      ChannelCredentialKind = "primary"
	ChannelCredentialKindOAuth        ChannelCredentialKind = "oauth"
	ChannelCredentialKindWebhook      ChannelCredentialKind = "webhook"
	ChannelCredentialKindSigning      ChannelCredentialKind = "signing"
	ChannelCredentialKindRefreshToken ChannelCredentialKind = "refresh_token"
)

type ChannelCredentialStatus string

const (
	ChannelCredentialStatusActive   ChannelCredentialStatus = "active"
	ChannelCredentialStatusExpiring ChannelCredentialStatus = "expiring"
	ChannelCredentialStatusExpired  ChannelCredentialStatus = "expired"
	ChannelCredentialStatusRevoked  ChannelCredentialStatus = "revoked"
	ChannelCredentialStatusInvalid  ChannelCredentialStatus = "invalid"
)

type InboxConversationStatus string

const (
	InboxConversationStatusOpen     InboxConversationStatus = "open"
	InboxConversationStatusPending  InboxConversationStatus = "pending"
	InboxConversationStatusSnoozed  InboxConversationStatus = "snoozed"
	InboxConversationStatusResolved InboxConversationStatus = "resolved"
	InboxConversationStatusArchived InboxConversationStatus = "archived"
)

type ConversationParticipantRole string

const (
	ConversationParticipantRoleCustomer ConversationParticipantRole = "customer"
	ConversationParticipantRoleAgent    ConversationParticipantRole = "agent"
	ConversationParticipantRoleBot      ConversationParticipantRole = "bot"
	ConversationParticipantRoleSystem   ConversationParticipantRole = "system"
	ConversationParticipantRoleCC       ConversationParticipantRole = "cc"
)

type MessagePartType string

const (
	MessagePartTypeText        MessagePartType = "text"
	MessagePartTypeImage       MessagePartType = "image"
	MessagePartTypeVideo       MessagePartType = "video"
	MessagePartTypeAudio       MessagePartType = "audio"
	MessagePartTypeDocument    MessagePartType = "document"
	MessagePartTypeLocation    MessagePartType = "location"
	MessagePartTypeContact     MessagePartType = "contact"
	MessagePartTypeInteractive MessagePartType = "interactive"
	MessagePartTypeTemplate    MessagePartType = "template"
	MessagePartTypeReaction    MessagePartType = "reaction"
	MessagePartTypeHTML        MessagePartType = "html"
)

type MessagePartStatus string

const (
	MessagePartStatusPending MessagePartStatus = "pending"
	MessagePartStatusReady   MessagePartStatus = "ready"
	MessagePartStatusFailed  MessagePartStatus = "failed"
)

type InboundEventStatus string

const (
	InboundEventStatusPending    InboundEventStatus = "pending"
	InboundEventStatusProcessing InboundEventStatus = "processing"
	InboundEventStatusProcessed  InboundEventStatus = "processed"
	InboundEventStatusIgnored    InboundEventStatus = "ignored"
	InboundEventStatusFailed     InboundEventStatus = "failed"
)

type MessageEventType string

const (
	MessageEventTypeReceived  MessageEventType = "received"
	MessageEventTypeAccepted  MessageEventType = "accepted"
	MessageEventTypeSent      MessageEventType = "sent"
	MessageEventTypeDelivered MessageEventType = "delivered"
	MessageEventTypeRead      MessageEventType = "read"
	MessageEventTypeFailed    MessageEventType = "failed"
	MessageEventTypeDeleted   MessageEventType = "deleted"
	MessageEventTypeReaction  MessageEventType = "reaction"
)

type OutboxJobStatus string

const (
	OutboxJobStatusPending    OutboxJobStatus = "pending"
	OutboxJobStatusProcessing OutboxJobStatus = "processing"
	// Dispatching is a delivery fence: policy-cancellation transactions may
	// cancel processing work, but a job that atomically reached dispatching
	// has won the race and is allowed to make its single provider attempt.
	OutboxJobStatusDispatching OutboxJobStatus = "dispatching"
	OutboxJobStatusRetrying    OutboxJobStatus = "retrying"
	OutboxJobStatusSent        OutboxJobStatus = "sent"
	OutboxJobStatusFailed      OutboxJobStatus = "failed"
	OutboxJobStatusCancelled   OutboxJobStatus = "cancelled"
)

type ChannelPreferenceStatus string

const (
	ChannelPreferenceStatusUnknown  ChannelPreferenceStatus = "unknown"
	ChannelPreferenceStatusOptedIn  ChannelPreferenceStatus = "opted_in"
	ChannelPreferenceStatusOptedOut ChannelPreferenceStatus = "opted_out"
	ChannelPreferenceStatusBlocked  ChannelPreferenceStatus = "blocked"
)

type ChannelPreferencePurpose string

const (
	ChannelPreferencePurposeService       ChannelPreferencePurpose = "service"
	ChannelPreferencePurposeTransactional ChannelPreferencePurpose = "transactional"
	ChannelPreferencePurposeMarketing     ChannelPreferencePurpose = "marketing"
)

// ChannelAccount is a tenant-owned connection to a provider account, page,
// mailbox, phone number, or web-chat installation.
type ChannelAccount struct {
	BaseModel
	OrganizationID    uuid.UUID            `gorm:"type:uuid;not null;index;index:idx_channel_accounts_org_channel,priority:1;index:idx_channel_accounts_org_status,priority:1" json:"organization_id"`
	Channel           Channel              `gorm:"size:32;not null;index:idx_channel_accounts_org_channel,priority:2" json:"channel"`
	Provider          string               `gorm:"size:100;not null;index" json:"provider"`
	Name              string               `gorm:"size:120;not null" json:"name"`
	ExternalAccountID string               `gorm:"size:255;not null" json:"external_account_id"`
	Status            ChannelAccountStatus `gorm:"size:24;not null;default:'pending';index:idx_channel_accounts_org_status,priority:2" json:"status"`
	Capabilities      JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"capabilities"`
	Config            JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"config"`
	Metadata          JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`
	IsDefaultIncoming bool                 `gorm:"not null;default:false" json:"is_default_incoming"`
	IsDefaultOutgoing bool                 `gorm:"not null;default:false" json:"is_default_outgoing"`
	ConnectedAt       *time.Time           `json:"connected_at,omitempty"`
	LastHealthCheckAt *time.Time           `json:"last_health_check_at,omitempty"`
	LastInboundAt     *time.Time           `json:"last_inbound_at,omitempty"`
	LastOutboundAt    *time.Time           `json:"last_outbound_at,omitempty"`
	LastErrorAt       *time.Time           `json:"last_error_at,omitempty"`
	LastError         string               `gorm:"type:text" json:"last_error,omitempty"`
	CreatedByID       *uuid.UUID           `gorm:"type:uuid;index" json:"created_by_id,omitempty"`
	UpdatedByID       *uuid.UUID           `gorm:"type:uuid;index" json:"updated_by_id,omitempty"`

	Organization *Organization       `gorm:"foreignKey:OrganizationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"organization,omitempty"`
	Credentials  []ChannelCredential `gorm:"foreignKey:ChannelAccountID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"-"`
	CreatedBy    *User               `gorm:"foreignKey:CreatedByID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"created_by,omitempty"`
	UpdatedBy    *User               `gorm:"foreignKey:UpdatedByID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"updated_by,omitempty"`
}

func (ChannelAccount) TableName() string {
	return "channel_accounts"
}

// ChannelCredential stores an encrypted provider credential envelope. The
// blob and its relationship from ChannelAccount are deliberately excluded from
// JSON to avoid accidental API or log disclosure.
type ChannelCredential struct {
	BaseModel
	OrganizationID   uuid.UUID               `gorm:"type:uuid;not null;index;uniqueIndex:idx_channel_credentials_version,priority:1;index:idx_channel_credentials_org_account,priority:1" json:"organization_id"`
	ChannelAccountID uuid.UUID               `gorm:"type:uuid;not null;index;uniqueIndex:idx_channel_credentials_version,priority:2;index:idx_channel_credentials_org_account,priority:2;index:idx_channel_credentials_account_status,priority:1" json:"channel_account_id"`
	Kind             ChannelCredentialKind   `gorm:"size:32;not null;uniqueIndex:idx_channel_credentials_version,priority:3" json:"kind"`
	Version          int                     `gorm:"not null;default:1;uniqueIndex:idx_channel_credentials_version,priority:4" json:"version"`
	CredentialBlob   JSONB                   `gorm:"type:jsonb;not null" json:"-"`
	Status           ChannelCredentialStatus `gorm:"size:24;not null;default:'active';index:idx_channel_credentials_account_status,priority:2" json:"status"`
	KeyVersion       string                  `gorm:"size:100;not null" json:"key_version"`
	ExpiresAt        *time.Time              `gorm:"index" json:"expires_at,omitempty"`
	LastValidatedAt  *time.Time              `json:"last_validated_at,omitempty"`
	RotatedAt        *time.Time              `json:"rotated_at,omitempty"`
	RevokedAt        *time.Time              `json:"revoked_at,omitempty"`
	ValidationError  string                  `gorm:"type:text" json:"validation_error,omitempty"`
	Metadata         JSONB                   `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`

	Organization   *Organization   `gorm:"foreignKey:OrganizationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"organization,omitempty"`
	ChannelAccount *ChannelAccount `gorm:"foreignKey:ChannelAccountID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"channel_account,omitempty"`
}

func (ChannelCredential) TableName() string {
	return "channel_credentials"
}

// MetaDeauthorizationEvent is the non-secret durable journal for Meta's
// signed deauthorization callback. It deliberately stores only the digest and
// canonical provider identifiers, never the signed_request or any access
// token. The callback is global, so this journal is not tenant-scoped; target
// mutations remain inside exact tenant transactions.
type MetaDeauthorizationEvent struct {
	Digest            string     `gorm:"size:64;primaryKey" json:"-"`
	PlatformAppID     string     `gorm:"size:64;not null;index" json:"-"`
	AuthorizingUserID string     `gorm:"size:64;not null;index" json:"-"`
	IssuedAt          time.Time  `gorm:"not null" json:"-"`
	VerifiedAt        time.Time  `gorm:"not null" json:"-"`
	State             string     `gorm:"size:24;not null;default:'verified';index" json:"-"`
	LastAttemptAt     *time.Time `json:"-"`
	CompletedAt       *time.Time `json:"-"`
}

func (MetaDeauthorizationEvent) TableName() string {
	return "meta_deauthorization_events"
}

// MetaInstagramDataDeletionEvent is the non-secret durable journal for the
// managed Instagram Login application's data-deletion callback. It is
// intentionally separate from MetaDeauthorizationEvent: a deletion request
// creates a privacy workflow and status URL, while deauthorization only
// revokes an authorization. The callback is assigned to the deployment-owned
// tenant immediately after its app signature has been verified. Keeping the
// tenant non-null from the first insert lets PostgreSQL RLS protect the privacy
// journal even when there is no current channel account. Exact targets stay in
// their clinic tenant; no-target/ambiguous requests use the separate platform
// compliance tenant and store only HMAC-bound provider identities.
type MetaInstagramDataDeletionEvent struct {
	Digest            string     `gorm:"size:64;primaryKey" json:"-"`
	PlatformAppID     string     `gorm:"size:64;not null;index" json:"-"`
	AuthorizingUserID string     `gorm:"size:64;not null;index" json:"-"`
	IssuedAt          time.Time  `gorm:"not null" json:"-"`
	VerifiedAt        time.Time  `gorm:"not null;index" json:"-"`
	State             string     `gorm:"size:24;not null;default:'verified';index" json:"-"`
	OrganizationID    uuid.UUID  `gorm:"type:uuid;not null;index" json:"-"`
	TargetResolution  string     `gorm:"size:32;not null;default:'exact_target'" json:"-"`
	IdentityHashed    bool       `gorm:"not null;default:false" json:"-"`
	PrivacyRequestID  *uuid.UUID `gorm:"type:uuid;index" json:"-"`
	RequestNumber     string     `gorm:"size:64;index" json:"-"`
	LastAttemptAt     *time.Time `json:"-"`
	CompletedAt       *time.Time `gorm:"index" json:"-"`
}

func (MetaInstagramDataDeletionEvent) TableName() string {
	return "meta_instagram_data_deletion_events"
}

// ContactIdentity maps an internal CRM contact to one provider identity.
type ContactIdentity struct {
	BaseModel
	OrganizationID    uuid.UUID  `gorm:"type:uuid;not null;index;uniqueIndex:idx_contact_identities_external,priority:1;index:idx_contact_identities_org_contact,priority:1" json:"organization_id"`
	ContactID         uuid.UUID  `gorm:"type:uuid;not null;index;index:idx_contact_identities_org_contact,priority:2" json:"contact_id"`
	ChannelAccountID  uuid.UUID  `gorm:"type:uuid;not null;index;uniqueIndex:idx_contact_identities_external,priority:2" json:"channel_account_id"`
	Channel           Channel    `gorm:"size:32;not null;index" json:"channel"`
	ExternalID        string     `gorm:"size:255;not null;uniqueIndex:idx_contact_identities_external,priority:3" json:"external_id"`
	Address           string     `gorm:"size:320;index" json:"address,omitempty"`
	NormalizedAddress string     `gorm:"size:320;index" json:"normalized_address,omitempty"`
	DisplayName       string     `gorm:"size:255" json:"display_name,omitempty"`
	AvatarURL         string     `gorm:"type:text" json:"avatar_url,omitempty"`
	IsPrimary         bool       `gorm:"not null;default:false" json:"is_primary"`
	IsVerified        bool       `gorm:"not null;default:false" json:"is_verified"`
	FirstSeenAt       *time.Time `json:"first_seen_at,omitempty"`
	LastSeenAt        *time.Time `gorm:"index" json:"last_seen_at,omitempty"`
	Metadata          JSONB      `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`

	Organization   *Organization   `gorm:"foreignKey:OrganizationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"organization,omitempty"`
	Contact        *Contact        `gorm:"foreignKey:ContactID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"contact,omitempty"`
	ChannelAccount *ChannelAccount `gorm:"foreignKey:ChannelAccountID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"channel_account,omitempty"`
}

func (ContactIdentity) TableName() string {
	return "contact_identities"
}

// InboxConversation is the provider-independent work item shown in the shared
// inbox. ExternalConversationID is the provider's thread/session identifier.
type InboxConversation struct {
	BaseModel
	OrganizationID         uuid.UUID               `gorm:"type:uuid;not null;index;uniqueIndex:idx_inbox_conversations_external,priority:1;index:idx_inbox_conversations_org_status,priority:1;index:idx_inbox_conversations_org_activity,priority:1" json:"organization_id"`
	ChannelAccountID       uuid.UUID               `gorm:"type:uuid;not null;index;uniqueIndex:idx_inbox_conversations_external,priority:2" json:"channel_account_id"`
	ContactID              uuid.UUID               `gorm:"type:uuid;not null;index" json:"contact_id"`
	ContactIdentityID      *uuid.UUID              `gorm:"type:uuid;index" json:"contact_identity_id,omitempty"`
	Channel                Channel                 `gorm:"size:32;not null;index" json:"channel"`
	ExternalConversationID string                  `gorm:"size:512;not null;uniqueIndex:idx_inbox_conversations_external,priority:3" json:"external_conversation_id"`
	Status                 InboxConversationStatus `gorm:"size:24;not null;default:'open';index:idx_inbox_conversations_org_status,priority:2" json:"status"`
	Subject                string                  `gorm:"size:998" json:"subject,omitempty"`
	Priority               int                     `gorm:"not null;default:0;index" json:"priority"`
	AssignedUserID         *uuid.UUID              `gorm:"type:uuid;index" json:"assigned_user_id,omitempty"`
	AssignedTeamID         *uuid.UUID              `gorm:"type:uuid;index" json:"assigned_team_id,omitempty"`
	UnreadCount            int                     `gorm:"not null;default:0" json:"unread_count"`
	LastMessagePreview     string                  `gorm:"type:text" json:"last_message_preview,omitempty"`
	OpenedAt               time.Time               `gorm:"not null;index" json:"opened_at"`
	LastMessageAt          *time.Time              `gorm:"index:idx_inbox_conversations_org_activity,priority:2,sort:desc" json:"last_message_at,omitempty"`
	LastInboundAt          *time.Time              `json:"last_inbound_at,omitempty"`
	LastOutboundAt         *time.Time              `json:"last_outbound_at,omitempty"`
	ServiceWindowEndsAt    *time.Time              `gorm:"index" json:"service_window_ends_at,omitempty"`
	SnoozedUntil           *time.Time              `gorm:"index" json:"snoozed_until,omitempty"`
	ResolvedAt             *time.Time              `json:"resolved_at,omitempty"`
	ArchivedAt             *time.Time              `json:"archived_at,omitempty"`
	Config                 JSONB                   `gorm:"type:jsonb;not null;default:'{}'" json:"config"`
	Metadata               JSONB                   `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`

	Organization    *Organization             `gorm:"foreignKey:OrganizationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"organization,omitempty"`
	ChannelAccount  *ChannelAccount           `gorm:"foreignKey:ChannelAccountID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT" json:"channel_account,omitempty"`
	Contact         *Contact                  `gorm:"foreignKey:ContactID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT" json:"contact,omitempty"`
	ContactIdentity *ContactIdentity          `gorm:"foreignKey:ContactIdentityID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"contact_identity,omitempty"`
	AssignedUser    *User                     `gorm:"foreignKey:AssignedUserID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"assigned_user,omitempty"`
	AssignedTeam    *Team                     `gorm:"foreignKey:AssignedTeamID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"assigned_team,omitempty"`
	Participants    []ConversationParticipant `gorm:"foreignKey:ConversationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"participants,omitempty"`
}

func (InboxConversation) TableName() string {
	return "inbox_conversations"
}

// ConversationParticipant captures both internal users and external channel
// participants. ParticipantKey is a stable caller-supplied key (for example,
// "user:<uuid>" or "external:<provider-id>") used for idempotent upserts.
type ConversationParticipant struct {
	BaseModel
	OrganizationID    uuid.UUID                   `gorm:"type:uuid;not null;index;uniqueIndex:idx_conversation_participants_key,priority:1;index:idx_conversation_participants_org_conversation,priority:1" json:"organization_id"`
	ConversationID    uuid.UUID                   `gorm:"type:uuid;not null;index;uniqueIndex:idx_conversation_participants_key,priority:2;index:idx_conversation_participants_org_conversation,priority:2" json:"conversation_id"`
	ParticipantKey    string                      `gorm:"size:512;not null;uniqueIndex:idx_conversation_participants_key,priority:3" json:"participant_key"`
	Role              ConversationParticipantRole `gorm:"size:24;not null;index" json:"role"`
	UserID            *uuid.UUID                  `gorm:"type:uuid;index" json:"user_id,omitempty"`
	ContactIdentityID *uuid.UUID                  `gorm:"type:uuid;index" json:"contact_identity_id,omitempty"`
	ExternalID        string                      `gorm:"size:512;index" json:"external_id,omitempty"`
	DisplayName       string                      `gorm:"size:255" json:"display_name,omitempty"`
	Address           string                      `gorm:"size:320" json:"address,omitempty"`
	JoinedAt          time.Time                   `gorm:"not null" json:"joined_at"`
	LeftAt            *time.Time                  `json:"left_at,omitempty"`
	Metadata          JSONB                       `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`

	Organization    *Organization      `gorm:"foreignKey:OrganizationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"organization,omitempty"`
	Conversation    *InboxConversation `gorm:"foreignKey:ConversationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"conversation,omitempty"`
	User            *User              `gorm:"foreignKey:UserID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"user,omitempty"`
	ContactIdentity *ContactIdentity   `gorm:"foreignKey:ContactIdentityID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"contact_identity,omitempty"`
}

func (ConversationParticipant) TableName() string {
	return "conversation_participants"
}

// ConversationRead stores a durable read cursor per participant. ReaderKey
// makes the cursor idempotent even when the reader is an external participant.
type ConversationRead struct {
	BaseModel
	OrganizationID     uuid.UUID  `gorm:"type:uuid;not null;index;uniqueIndex:idx_conversation_reads_reader,priority:1;index:idx_conversation_reads_org_conversation,priority:1" json:"organization_id"`
	ConversationID     uuid.UUID  `gorm:"type:uuid;not null;index;uniqueIndex:idx_conversation_reads_reader,priority:2;index:idx_conversation_reads_org_conversation,priority:2" json:"conversation_id"`
	ParticipantID      *uuid.UUID `gorm:"type:uuid;index" json:"participant_id,omitempty"`
	UserID             *uuid.UUID `gorm:"type:uuid;index" json:"user_id,omitempty"`
	ReaderKey          string     `gorm:"size:512;not null;uniqueIndex:idx_conversation_reads_reader,priority:3" json:"reader_key"`
	LastReadMessageID  *uuid.UUID `gorm:"type:uuid;index" json:"last_read_message_id,omitempty"`
	LastReadExternalID string     `gorm:"size:512" json:"last_read_external_id,omitempty"`
	// LastReadIngestedAt is the server-order half of the durable cursor. It is
	// nullable for rolling compatibility with older writers; unread queries
	// fall back to LastReadAt until those rows are advanced by the new binary.
	LastReadIngestedAt *time.Time `json:"last_read_ingested_at,omitempty"`
	LastReadAt         time.Time  `gorm:"not null;index" json:"last_read_at"`
	Metadata           JSONB      `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`

	Organization *Organization            `gorm:"foreignKey:OrganizationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"organization,omitempty"`
	Conversation *InboxConversation       `gorm:"foreignKey:ConversationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"conversation,omitempty"`
	Participant  *ConversationParticipant `gorm:"foreignKey:ParticipantID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"participant,omitempty"`
	User         *User                    `gorm:"foreignKey:UserID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"user,omitempty"`
	// Product integrity owns the composite tenant FK
	// (last_read_message_id, organization_id) -> messages(id, organization_id)
	// with ON DELETE RESTRICT. A PostgreSQL-14-compatible message-delete trigger
	// clears only the optional message ID before the constraint is checked.
	// Excluding this association from AutoMigrate
	// avoids a conflicting single-column SET NULL constraint while preserving
	// runtime preload behavior.
	LastReadMessage *Message `gorm:"foreignKey:LastReadMessageID;-:migration" json:"last_read_message,omitempty"`
}

func (ConversationRead) TableName() string {
	return "conversation_reads"
}

// MessagePart preserves ordered, provider-neutral rich-message content while
// the existing Message remains the message envelope during migration.
type MessagePart struct {
	BaseModel
	OrganizationID   uuid.UUID         `gorm:"type:uuid;not null;index;uniqueIndex:idx_message_parts_position,priority:1;index:idx_message_parts_org_message,priority:1" json:"organization_id"`
	MessageID        uuid.UUID         `gorm:"type:uuid;not null;index;uniqueIndex:idx_message_parts_position,priority:2;index:idx_message_parts_org_message,priority:2" json:"message_id"`
	ConversationID   uuid.UUID         `gorm:"type:uuid;not null;index" json:"conversation_id"`
	Position         int               `gorm:"not null;default:0;uniqueIndex:idx_message_parts_position,priority:3" json:"position"`
	Type             MessagePartType   `gorm:"size:24;not null" json:"type"`
	Status           MessagePartStatus `gorm:"size:20;not null;default:'ready'" json:"status"`
	Text             string            `gorm:"type:text" json:"text,omitempty"`
	Caption          string            `gorm:"type:text" json:"caption,omitempty"`
	MediaURL         string            `gorm:"type:text" json:"media_url,omitempty"`
	StorageKey       string            `gorm:"type:text" json:"storage_key,omitempty"`
	ProviderMediaRef string            `gorm:"size:512" json:"provider_media_ref,omitempty"`
	MimeType         string            `gorm:"size:255" json:"mime_type,omitempty"`
	Filename         string            `gorm:"size:512" json:"filename,omitempty"`
	SizeBytes        int64             `gorm:"not null;default:0" json:"size_bytes,omitempty"`
	Checksum         string            `gorm:"size:128" json:"checksum,omitempty"`
	Payload          JSONB             `gorm:"type:jsonb;not null;default:'{}'" json:"payload"`

	Organization *Organization      `gorm:"foreignKey:OrganizationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"organization,omitempty"`
	Message      *Message           `gorm:"foreignKey:MessageID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"message,omitempty"`
	Conversation *InboxConversation `gorm:"foreignKey:ConversationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"conversation,omitempty"`
}

func (MessagePart) TableName() string {
	return "message_parts"
}

// InboundEvent is the durable, idempotent webhook inbox. Payload is retained
// for replay and must be subject to the tenant's retention policy.
type InboundEvent struct {
	BaseModel
	OrganizationID      uuid.UUID          `gorm:"type:uuid;not null;index;uniqueIndex:idx_inbound_events_dedupe,priority:1;index:idx_inbound_events_org_status,priority:1" json:"organization_id"`
	ChannelAccountID    uuid.UUID          `gorm:"type:uuid;not null;index;uniqueIndex:idx_inbound_events_dedupe,priority:2" json:"channel_account_id"`
	DedupeKey           string             `gorm:"size:512;not null;uniqueIndex:idx_inbound_events_dedupe,priority:3" json:"dedupe_key"`
	ProviderEventID     string             `gorm:"size:512;index" json:"provider_event_id,omitempty"`
	EventType           string             `gorm:"size:100;not null;index" json:"event_type"`
	Status              InboundEventStatus `gorm:"size:24;not null;default:'pending';index:idx_inbound_events_org_status,priority:2" json:"status"`
	SignatureValid      bool               `gorm:"not null;default:false" json:"signature_valid"`
	ReceivedAt          time.Time          `gorm:"not null;index" json:"received_at"`
	ProcessingStartedAt *time.Time         `json:"processing_started_at,omitempty"`
	ProcessedAt         *time.Time         `json:"processed_at,omitempty"`
	NextAttemptAt       *time.Time         `gorm:"index" json:"next_attempt_at,omitempty"`
	AttemptCount        int                `gorm:"not null;default:0" json:"attempt_count"`
	ErrorCode           string             `gorm:"size:100" json:"error_code,omitempty"`
	ErrorMessage        string             `gorm:"type:text" json:"error_message,omitempty"`
	Headers             JSONB              `gorm:"type:jsonb;not null;default:'{}'" json:"headers"`
	Payload             JSONB              `gorm:"type:jsonb;not null;default:'{}'" json:"payload"`

	Organization   *Organization   `gorm:"foreignKey:OrganizationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"organization,omitempty"`
	ChannelAccount *ChannelAccount `gorm:"foreignKey:ChannelAccountID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT" json:"channel_account,omitempty"`
}

func (InboundEvent) TableName() string {
	return "inbound_events"
}

// MessageEvent is an append-oriented timeline of delivery and interaction
// events for an internal or provider message.
type MessageEvent struct {
	BaseModel
	OrganizationID    uuid.UUID        `gorm:"type:uuid;not null;index;uniqueIndex:idx_message_events_provider,priority:1;index:idx_message_events_org_message,priority:1" json:"organization_id"`
	ChannelAccountID  uuid.UUID        `gorm:"type:uuid;not null;index;uniqueIndex:idx_message_events_provider,priority:2" json:"channel_account_id"`
	ConversationID    uuid.UUID        `gorm:"type:uuid;not null;index" json:"conversation_id"`
	MessageID         *uuid.UUID       `gorm:"type:uuid;index;index:idx_message_events_org_message,priority:2" json:"message_id,omitempty"`
	ProviderEventID   string           `gorm:"size:512;not null;uniqueIndex:idx_message_events_provider,priority:3" json:"provider_event_id"`
	ExternalMessageID string           `gorm:"size:512;index" json:"external_message_id,omitempty"`
	Type              MessageEventType `gorm:"size:24;not null;index" json:"type"`
	OccurredAt        time.Time        `gorm:"not null;index" json:"occurred_at"`
	ActorExternalID   string           `gorm:"size:512" json:"actor_external_id,omitempty"`
	ErrorCode         string           `gorm:"size:100" json:"error_code,omitempty"`
	ErrorMessage      string           `gorm:"type:text" json:"error_message,omitempty"`
	Payload           JSONB            `gorm:"type:jsonb;not null;default:'{}'" json:"payload"`

	Organization   *Organization      `gorm:"foreignKey:OrganizationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"organization,omitempty"`
	ChannelAccount *ChannelAccount    `gorm:"foreignKey:ChannelAccountID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT" json:"channel_account,omitempty"`
	Conversation   *InboxConversation `gorm:"foreignKey:ConversationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"conversation,omitempty"`
	Message        *Message           `gorm:"foreignKey:MessageID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"message,omitempty"`
}

func (MessageEvent) TableName() string {
	return "message_events"
}

// OutboxJob is a transactional delivery job. IdempotencyKey is scoped to the
// tenant and channel account and is reused across retries.
type OutboxJob struct {
	BaseModel
	OrganizationID   uuid.UUID                `gorm:"type:uuid;not null;index;uniqueIndex:idx_outbox_jobs_idempotency,priority:1;index:idx_outbox_jobs_org_status_available,priority:1" json:"organization_id"`
	ChannelAccountID uuid.UUID                `gorm:"type:uuid;not null;index;uniqueIndex:idx_outbox_jobs_idempotency,priority:2" json:"channel_account_id"`
	ConversationID   uuid.UUID                `gorm:"type:uuid;not null;index" json:"conversation_id"`
	MessageID        *uuid.UUID               `gorm:"type:uuid;index" json:"message_id,omitempty"`
	IdempotencyKey   string                   `gorm:"size:512;not null;uniqueIndex:idx_outbox_jobs_idempotency,priority:3" json:"idempotency_key"`
	PayloadDigest    string                   `gorm:"size:64;not null;default:''" json:"payload_digest"`
	Purpose          ChannelPreferencePurpose `gorm:"size:32;not null;default:'';index" json:"purpose"`
	Status           OutboxJobStatus          `gorm:"size:24;not null;default:'pending';index:idx_outbox_jobs_org_status_available,priority:2" json:"status"`
	Priority         int                      `gorm:"not null;default:0;index" json:"priority"`
	AvailableAt      time.Time                `gorm:"not null;index;index:idx_outbox_jobs_org_status_available,priority:3" json:"available_at"`
	LockedAt         *time.Time               `json:"locked_at,omitempty"`
	LockedBy         string                   `gorm:"size:255" json:"locked_by,omitempty"`
	AttemptCount     int                      `gorm:"not null;default:0" json:"attempt_count"`
	MaxAttempts      int                      `gorm:"not null;default:8" json:"max_attempts"`
	LastAttemptAt    *time.Time               `json:"last_attempt_at,omitempty"`
	SentAt           *time.Time               `json:"sent_at,omitempty"`
	FailedAt         *time.Time               `json:"failed_at,omitempty"`
	LastErrorCode    string                   `gorm:"size:100" json:"last_error_code,omitempty"`
	LastError        string                   `gorm:"type:text" json:"last_error,omitempty"`
	ProviderState    JSONB                    `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	Payload          JSONB                    `gorm:"type:jsonb;not null;default:'{}'" json:"payload"`

	Organization   *Organization      `gorm:"foreignKey:OrganizationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"organization,omitempty"`
	ChannelAccount *ChannelAccount    `gorm:"foreignKey:ChannelAccountID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT" json:"channel_account,omitempty"`
	Conversation   *InboxConversation `gorm:"foreignKey:ConversationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"conversation,omitempty"`
	Message        *Message           `gorm:"foreignKey:MessageID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"message,omitempty"`
}

func (OutboxJob) TableName() string {
	return "outbox_jobs"
}

// ContactChannelPreference records consent separately for each purpose because
// service, transactional, and marketing permissions have different legal and
// provider-policy bases.
type ContactChannelPreference struct {
	BaseModel
	OrganizationID    uuid.UUID                `gorm:"type:uuid;not null;index;uniqueIndex:idx_contact_channel_preferences_scope,priority:1;index:idx_contact_channel_preferences_org_contact,priority:1" json:"organization_id"`
	ContactID         uuid.UUID                `gorm:"type:uuid;not null;index;uniqueIndex:idx_contact_channel_preferences_scope,priority:2;index:idx_contact_channel_preferences_org_contact,priority:2" json:"contact_id"`
	ChannelAccountID  uuid.UUID                `gorm:"type:uuid;not null;index;uniqueIndex:idx_contact_channel_preferences_scope,priority:3" json:"channel_account_id"`
	ContactIdentityID *uuid.UUID               `gorm:"type:uuid;index" json:"contact_identity_id,omitempty"`
	Channel           Channel                  `gorm:"size:32;not null;index" json:"channel"`
	Purpose           ChannelPreferencePurpose `gorm:"size:32;not null;uniqueIndex:idx_contact_channel_preferences_scope,priority:4" json:"purpose"`
	Status            ChannelPreferenceStatus  `gorm:"size:24;not null;default:'unknown';index" json:"status"`
	Source            string                   `gorm:"size:100" json:"source,omitempty"`
	ProofReference    string                   `gorm:"type:text" json:"proof_reference,omitempty"`
	OptedInAt         *time.Time               `json:"opted_in_at,omitempty"`
	OptedOutAt        *time.Time               `json:"opted_out_at,omitempty"`
	ConfirmedAt       *time.Time               `json:"confirmed_at,omitempty"`
	LastContactedAt   *time.Time               `json:"last_contacted_at,omitempty"`
	QuietHours        JSONB                    `gorm:"type:jsonb;not null;default:'{}'" json:"quiet_hours"`
	Config            JSONB                    `gorm:"type:jsonb;not null;default:'{}'" json:"config"`
	Metadata          JSONB                    `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`

	Organization    *Organization    `gorm:"foreignKey:OrganizationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"organization,omitempty"`
	Contact         *Contact         `gorm:"foreignKey:ContactID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"contact,omitempty"`
	ChannelAccount  *ChannelAccount  `gorm:"foreignKey:ChannelAccountID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"channel_account,omitempty"`
	ContactIdentity *ContactIdentity `gorm:"foreignKey:ContactIdentityID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"contact_identity,omitempty"`
}

func (ContactChannelPreference) TableName() string {
	return "contact_channel_preferences"
}
