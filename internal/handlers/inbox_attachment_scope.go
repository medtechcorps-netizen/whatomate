package handlers

import (
	"bytes"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

type inboxAttachmentScope struct {
	conversationID uuid.UUID
	contactID      uuid.UUID
	nativeAccount  models.WhatsAppAccount
	cutoff         *time.Time
}

var errInboxAttachmentUnavailable = errors.New("inbox attachment unavailable")

// Keep the bounded reads and their response bodies under one process limit.
var inboxAttachmentSlots = make(chan struct{}, 2)

type inboxAttachmentBody struct {
	*bytes.Reader
	closeOnce sync.Once
}

func (body *inboxAttachmentBody) Close() error {
	body.closeOnce.Do(func() { <-inboxAttachmentSlots })
	return nil
}

func inboxAttachmentSelectedOrg(r *fastglue.Request) uuid.UUID {
	orgID, _ := uuid.Parse(string(r.RequestCtx.Request.Header.Peek("X-Organization-ID")))
	return orgID
}

func (a *App) inboxAttachmentScope(
	orgID, userID uuid.UUID,
	conversation *models.InboxConversation,
) (inboxAttachmentScope, error) {
	if conversation == nil || conversation.OrganizationID != orgID ||
		conversation.Channel != models.ChannelWhatsApp ||
		conversation.ContactID == uuid.Nil || conversation.ExternalConversationID != "legacy-contact:"+conversation.ContactID.String() ||
		conversation.ChannelAccount == nil || conversation.ChannelAccount.ID != conversation.ChannelAccountID ||
		conversation.ChannelAccount.OrganizationID != orgID {
		return inboxAttachmentScope{}, errInboxAttachmentUnavailable
	}
	accountID, err := channelapi.LegacyMetaWhatsAppAccountID(conversation.ChannelAccount)
	if err != nil {
		return inboxAttachmentScope{}, errInboxAttachmentUnavailable
	}
	permissions, err := a.getUserPermissionsCached(userID, orgID)
	if err != nil {
		return inboxAttachmentScope{}, errInboxAttachmentUnavailable
	}
	if !permissions.IsSuperAdmin {
		var role models.CustomRole
		if err := a.DB.Select("id").Where("id = ? AND organization_id = ?", permissions.RoleID, orgID).First(&role).Error; err != nil {
			return inboxAttachmentScope{}, err
		}
	}
	has := func(resource string) bool {
		if permissions.IsSuperAdmin {
			return true
		}
		for _, key := range permissions.Permissions {
			if key == resource+":"+models.ActionRead {
				return true
			}
		}
		return false
	}
	if !has(models.ResourceConversations) || !has(models.ResourceChat) {
		return inboxAttachmentScope{}, errInboxAttachmentUnavailable
	}
	allowed, err := a.HasProductEntitlement(userID, orgID, "omnichannel.enabled")
	if err != nil {
		return inboxAttachmentScope{}, err
	}
	if !allowed {
		return inboxAttachmentScope{}, errInboxAttachmentUnavailable
	}
	var contact models.Contact
	contactQuery := a.DB.Select("id").Where("id = ? AND organization_id = ?", conversation.ContactID, orgID)
	allContacts := has(models.ResourceContacts)
	if !allContacts {
		contactQuery = a.whereContactAssignedTo(contactQuery, userID, orgID)
	}
	if err := contactQuery.First(&contact).Error; err != nil {
		return inboxAttachmentScope{}, err
	}
	var native models.WhatsAppAccount
	if err := a.DB.Select("id", "organization_id", "name", "phone_id").
		Where("id = ? AND organization_id = ?", accountID, orgID).First(&native).Error; err != nil {
		return inboxAttachmentScope{}, err
	}
	if native.Name == "" || conversation.ChannelAccount.Metadata["legacy_account_name"] != native.Name {
		return inboxAttachmentScope{}, errInboxAttachmentUnavailable
	}

	scope := inboxAttachmentScope{
		conversationID: conversation.ID,
		contactID:      contact.ID, nativeAccount: native,
	}
	if !allContacts {
		var settings models.ChatbotSettings
		err := a.DB.Select("id", "agent_current_conversation_only").
			Where("organization_id = ? AND whats_app_account = ''", orgID).First(&settings).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return inboxAttachmentScope{}, err
		}
		if settings.AgentAssignment.CurrentConversationOnly {
			scope.cutoff, err = a.customerConversationCutoff(orgID, userID, []uuid.UUID{contact.ID})
			if err != nil {
				return inboxAttachmentScope{}, err
			}
			if scope.cutoff == nil {
				return inboxAttachmentScope{}, errInboxAttachmentUnavailable
			}
		}
	}
	return scope, nil
}
