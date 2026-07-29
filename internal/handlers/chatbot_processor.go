package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/models"
	qwenapi "github.com/shridarpatil/whatomate/internal/qwen"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func redactURLForLog(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid_url>"
	}

	if parsed.Scheme == "" || parsed.Host == "" {
		return parsed.Path
	}

	return parsed.Scheme + "://" + parsed.Host + parsed.Path
}

func truncateLogValue(value string, maxLen int) string {
	if len(value) <= maxLen {
		return value
	}

	return value[:maxLen] + "...(truncated)"
}

// IncomingTextMessage represents a text, interactive, or media message from the webhook
type IncomingTextMessage struct {
	From       string `json:"from"`
	FromUserID string `json:"from_user_id,omitempty"` // BSUID
	To         string `json:"to,omitempty"`
	ID         string `json:"id"`
	Timestamp  string `json:"timestamp"`
	Type       string `json:"type"`
	Text       *struct {
		Body string `json:"body"`
	} `json:"text,omitempty"`
	Interactive *struct {
		Type        string `json:"type"`
		ButtonReply *struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"button_reply,omitempty"`
		ListReply *struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"list_reply,omitempty"`
		NFMReply *struct {
			ResponseJSON string `json:"response_json"`
			Body         string `json:"body"`
			Name         string `json:"name"`
		} `json:"nfm_reply,omitempty"`
		CallPermissionReply *struct {
			Response            string      `json:"response"`
			IsPermanent         bool        `json:"is_permanent"`
			ExpirationTimestamp json.Number `json:"expiration_timestamp,omitempty"`
			ResponseSource      string      `json:"response_source"`
		} `json:"call_permission_reply,omitempty"`
	} `json:"interactive,omitempty"`
	Image *struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
		SHA256   string `json:"sha256"`
		Caption  string `json:"caption,omitempty"`
	} `json:"image,omitempty"`
	Document *struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
		SHA256   string `json:"sha256"`
		Filename string `json:"filename,omitempty"`
		Caption  string `json:"caption,omitempty"`
	} `json:"document,omitempty"`
	Audio *struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
	} `json:"audio,omitempty"`
	Video *struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
		SHA256   string `json:"sha256"`
		Caption  string `json:"caption,omitempty"`
	} `json:"video,omitempty"`
	Sticker *struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
		SHA256   string `json:"sha256"`
		Animated bool   `json:"animated,omitempty"`
	} `json:"sticker,omitempty"`
	Context *struct {
		From string `json:"from"`
		ID   string `json:"id"` // WhatsApp message ID being replied to
	} `json:"context,omitempty"`
	Reaction *struct {
		MessageID string `json:"message_id"` // WhatsApp message ID being reacted to
		Emoji     string `json:"emoji"`      // The emoji reaction (empty string = remove reaction)
	} `json:"reaction,omitempty"`
	Location *struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Name      string  `json:"name,omitempty"`
		Address   string  `json:"address,omitempty"`
	} `json:"location,omitempty"`
	Button *struct {
		Text    string `json:"text"`
		Payload string `json:"payload"`
	} `json:"button,omitempty"`
	Contacts []struct {
		Name struct {
			FormattedName string `json:"formatted_name"`
			FirstName     string `json:"first_name,omitempty"`
			LastName      string `json:"last_name,omitempty"`
		} `json:"name"`
		Phones []struct {
			Phone string `json:"phone"`
			Type  string `json:"type,omitempty"`
		} `json:"phones,omitempty"`
	} `json:"contacts,omitempty"`
}

// persistedIncomingMessage is the durable hand-off between Meta's webhook ACK
// path and the asynchronous media/chatbot continuation. Every field needed by
// the continuation is copied only after the contact, message, lifecycle event,
// and outbox row have committed.
type persistedIncomingMessage struct {
	OrganizationID uuid.UUID
	PhoneNumberID  string
	Message        IncomingTextMessage
	Account        models.WhatsAppAccount
	Contact        models.Contact
	Extracted      ExtractedMessage
	Persisted      models.Message
}

// updateContactBSUID persists Meta's optional business-scoped user ID without
// allowing a metadata-write failure to poison the surrounding inbound-message
// transaction. GORM implements nested transactions with a PostgreSQL savepoint,
// so rolling this callback back restores the outer tenant transaction.
func (a *App) updateContactBSUID(contact *models.Contact, bsuid string) {
	if contact == nil || bsuid == "" || contact.BSUID == bsuid {
		return
	}

	err := a.DB.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&models.Contact{}).
			Where("id = ?", contact.ID).
			Update("bs_uid", bsuid).Error
	})
	if err != nil {
		a.Log.Warn("Failed to update contact BSUID; continuing inbound message processing",
			"error", err,
			"contact_id", contact.ID,
		)
		return
	}
	contact.BSUID = bsuid
}

// processIncomingMessageFull processes incoming WhatsApp messages with chatbot logic
func (a *App) processIncomingMessageFull(phoneNumberID string, msg IncomingTextMessage, profileName string) {
	a.Log.Info("Processing incoming message",
		"phone_number_id", phoneNumberID,
		"from", msg.From,
		"type", msg.Type,
		"profile_name", profileName,
	)

	// Find the WhatsApp account by phone_number_id (use cache)
	account, err := a.getWhatsAppAccountCached(phoneNumberID)
	if err != nil {
		a.Log.Error("WhatsApp account not found", "phone_id", phoneNumberID, "error", err)
		return
	}

	// Handle reaction messages specially - they update existing messages, not create new ones
	if msg.Type == "reaction" && msg.Reaction != nil {
		a.handleIncomingReaction(account, msg.From, msg.Reaction.MessageID, msg.Reaction.Emoji, profileName)
		return
	}

	work, duplicate, err := a.persistIncomingMessageForAccount(
		phoneNumberID,
		msg,
		profileName,
		account,
	)
	if err != nil {
		a.Log.Error("Failed to persist incoming message", "from", msg.From, "error", err)
		return
	}
	if duplicate {
		a.startPersistedIncomingMessageContinuation(work)
		return
	}
	a.startPersistedIncomingMessageContinuation(work)
}

// continuePersistedIncomingMessage performs work that is intentionally outside
// Meta's acknowledgement critical path. Media download and every chatbot or
// provider call happen here, after the normalized inbound fact is durable.
func (a *App) continuePersistedIncomingMessage(
	ctx context.Context,
	work *persistedIncomingMessage,
) error {
	if work == nil {
		return nil
	}

	account := &work.Account
	contact, err := contactutil.ResolveCanonicalContact(
		a.DB,
		work.OrganizationID,
		work.Contact.ID,
	)
	if err != nil {
		a.Log.Error(
			"Failed to resolve inbound contact for asynchronous continuation",
			"error", err,
			"contact_id", work.Contact.ID,
			"message_id", work.Persisted.ID,
		)
		return fmt.Errorf("resolve inbound continuation contact: %w", err)
	}

	a.hydratePersistedIncomingMedia(ctx, work, account)
	// Broadcast only after optional hydration so a media message reaches the UI
	// once, with its durable object-store URL when download succeeded.
	a.broadcastNewMessage(work.OrganizationID, &work.Persisted, contact)

	msg := work.Message
	extracted := work.Extracted
	messageText := extracted.Text
	buttonID := extracted.ButtonID
	flowResponseData := extracted.FlowResponseData

	// Clear chatbot tracking since client has replied
	a.ClearContactChatbotTracking(contact.ID)

	// Check for active agent transfer - skip chatbot processing if transferred
	if a.hasActiveAgentTransfer(account.OrganizationID, contact.ID) {
		a.Log.Info("Contact has active agent transfer, skipping chatbot processing",
			"contact_id", contact.ID,
			"phone_number", contact.PhoneNumber)
		return nil
	}

	// Check if chatbot is enabled for this account (use cache)
	settings, err := a.getChatbotSettingsCached(account.OrganizationID, account.Name)
	if err != nil {
		a.Log.Error("Failed to load chatbot settings", "error", err, "account", account.Name, "org_id", account.OrganizationID)
		return fmt.Errorf("load inbound continuation chatbot settings: %w", err)
	}
	if !settings.IsEnabled {
		a.Log.Debug("Chatbot not enabled for this account, creating transfer for agent queue", "account", account.Name, "settings_id", settings.ID)
		// Create transfer to agent queue when chatbot is disabled
		a.createTransferToQueue(account, contact, models.TransferSourceChatbotDisabled)
		return nil
	}
	a.Log.Info("Chatbot settings loaded", "settings_id", settings.ID, "is_enabled", settings.IsEnabled, "ai_enabled", settings.AI.Enabled, "ai_provider", settings.AI.Provider, "default_response", settings.DefaultResponse)

	// Check business hours if enabled
	if settings.BusinessHours.Enabled && len(settings.BusinessHours.Hours) > 0 {
		if !a.isWithinBusinessHours(settings.BusinessHours.Hours) {
			// If automated responses are not allowed outside hours, send out-of-hours message and stop
			if !settings.BusinessHours.AllowAutomatedOutside {
				a.Log.Info("Outside business hours, sending out of hours message")
				if settings.BusinessHours.OutOfHoursMessage != "" {
					if err := a.sendAndSaveTextMessage(account, contact, settings.BusinessHours.OutOfHoursMessage); err != nil {
						a.Log.Error("Failed to send out of hours message", "error", err, "contact", contact.PhoneNumber)
						return fmt.Errorf("send out-of-hours response: %w", err)
					}
				}
				return nil
			}
			// AllowAutomatedOutsideHours is true, continue processing flows/keywords/AI
			a.Log.Info("Outside business hours but automated responses allowed, continuing")
		}
	}

	// Only process text and interactive messages for chatbot
	if messageText == "" {
		a.Log.Debug("Skipping message with no text content for chatbot", "type", msg.Type)
		return nil
	}

	a.Log.Info("Processing message", "text", messageText, "buttonID", buttonID, "from", msg.From)

	// Get or create active session for this contact
	session, isNewSession, err := a.getOrCreateSession(
		account.OrganizationID,
		contact.ID,
		account.Name,
		msg.From,
		settings.SessionTimeoutMins,
	)
	if err != nil || session == nil {
		a.Log.Error(
			"Failed to get or create chatbot session",
			"error", err,
			"contact_id", contact.ID,
			"account", account.Name,
		)
		if err == nil {
			err = errors.New("chatbot session was not returned")
		}
		return fmt.Errorf("get inbound continuation chatbot session: %w", err)
	}

	// Log incoming message to session
	a.logSessionMessage(session.ID, models.DirectionIncoming, messageText, "keyword_check")

	// Check for transfer keyword BEFORE sending greeting (transfer takes priority)
	keywordResponse, keywordMatched := a.matchKeywordRules(account.OrganizationID, account.Name, messageText)
	if keywordMatched && keywordResponse.ResponseType == models.ResponseTypeTransfer {
		a.Log.Info("Transfer keyword matched", "response", keywordResponse.Body)
		// Check business hours - if outside hours, send out of hours message instead
		if settings.BusinessHours.Enabled && len(settings.BusinessHours.Hours) > 0 {
			if !a.isWithinBusinessHours(settings.BusinessHours.Hours) {
				a.Log.Info("Outside business hours, sending out of hours message instead of transfer")
				if settings.BusinessHours.OutOfHoursMessage != "" {
					if err := a.sendAndSaveTextMessage(account, contact, settings.BusinessHours.OutOfHoursMessage); err != nil {
						a.Log.Error("Failed to send out of hours message", "error", err, "contact", contact.PhoneNumber)
						return fmt.Errorf(
							"send transfer out-of-hours response: %w",
							err,
						)
					}
				}
				return nil
			}
		}
		// Within business hours - send transfer message and create transfer
		if keywordResponse.Body != "" {
			if err := a.sendAndSaveTextMessage(account, contact, keywordResponse.Body); err != nil {
				a.Log.Error("Failed to send transfer message", "error", err, "contact", contact.PhoneNumber)
				return fmt.Errorf("send transfer response: %w", err)
			}
		}
		a.createTransferFromKeyword(account, contact)
		return nil
	}

	// Check if user is in an active flow. After Phase 4.2 every flow has
	// a v2 Graph populated; any flow without one is a misconfiguration
	// (manual DB edit or failed backfill) — log and exit cleanly.
	if session.CurrentFlowID != nil {
		flow, err := a.getChatbotFlowByIDCached(account.OrganizationID, *session.CurrentFlowID)
		if err != nil || flow == nil {
			a.Log.Error("Active chatbot flow not loadable", "error", err, "session", session.ID, "flow", session.CurrentFlowID)
			a.exitFlow(session)
			if err != nil {
				return fmt.Errorf("load active chatbot flow: %w", err)
			}
			return nil
		}
		if flow.Graph == nil {
			a.Log.Error("Active chatbot flow has no v2 graph; ignoring inbound", "session", session.ID, "flow", flow.ID)
			a.exitFlow(session)
			return nil
		}
		if err := a.runChatGraph(account, contact, session, flow, messageText, buttonID, flowResponseData); err != nil {
			a.Log.Error("Chat graph runner failed", "error", err, "session", session.ID, "flow", flow.ID)
			return fmt.Errorf("run active chatbot flow: %w", err)
		}
		return nil
	}

	// Try to match flow trigger keywords first (before greeting to avoid duplicate messages)
	if flow := a.matchFlowTrigger(account.OrganizationID, messageText); flow != nil {
		if flow.Graph == nil {
			a.Log.Error("Triggered chatbot flow has no v2 graph; ignoring", "flow", flow.ID)
			return nil
		}
		session.CurrentFlowID = &flow.ID
		session.CurrentStep = ""
		session.StepRetries = 0
		session.SessionData = models.JSONB{
			"_flow_id":   flow.ID.String(),
			"_flow_name": flow.Name,
		}
		if err := a.runChatGraph(account, contact, session, flow, messageText, buttonID, flowResponseData); err != nil {
			a.Log.Error("Chat graph runner failed at flow start", "error", err, "session", session.ID, "flow", flow.ID)
			return fmt.Errorf("start chatbot flow: %w", err)
		}
		return nil
	}

	// Send greeting message for new sessions (only if no flow was triggered)
	if isNewSession && settings.DefaultResponse != "" {
		a.Log.Info("New session - sending greeting message", "contact", contact.PhoneNumber)
		if len(settings.GreetingButtons) > 0 {
			greetingButtons := make([]map[string]any, 0)
			for _, btn := range settings.GreetingButtons {
				if btnMap, ok := btn.(map[string]any); ok {
					greetingButtons = append(greetingButtons, btnMap)
				}
			}
			if len(greetingButtons) > 0 {
				if err := a.sendAndSaveInteractiveButtons(account, contact, settings.DefaultResponse, greetingButtons); err != nil {
					a.Log.Error("Failed to send greeting buttons", "error", err, "contact", contact.PhoneNumber)
					return fmt.Errorf("send chatbot greeting buttons: %w", err)
				}
			} else {
				if err := a.sendAndSaveTextMessage(account, contact, settings.DefaultResponse); err != nil {
					a.Log.Error("Failed to send greeting message", "error", err, "contact", contact.PhoneNumber)
					return fmt.Errorf("send chatbot greeting: %w", err)
				}
			}
		} else {
			if err := a.sendAndSaveTextMessage(account, contact, settings.DefaultResponse); err != nil {
				a.Log.Error("Failed to send greeting message", "error", err, "contact", contact.PhoneNumber)
				return fmt.Errorf("send chatbot greeting: %w", err)
			}
		}
		a.logSessionMessage(session.ID, models.DirectionOutgoing, settings.DefaultResponse, "greeting")
		return nil // After greeting, don't process further for new sessions
	}

	// Handle non-transfer keyword matches (transfer was already handled above)
	if keywordMatched && keywordResponse.ResponseType != models.ResponseTypeTransfer {
		a.Log.Info("Keyword rule matched", "response_type", keywordResponse.ResponseType, "response", keywordResponse.Body)

		// Handle regular text response
		if len(keywordResponse.Buttons) > 0 {
			if err := a.sendAndSaveInteractiveButtons(account, contact, keywordResponse.Body, keywordResponse.Buttons); err != nil {
				a.Log.Error("Failed to send interactive buttons", "error", err, "contact", contact.PhoneNumber)
				return fmt.Errorf("send keyword response buttons: %w", err)
			}
		} else {
			if err := a.sendAndSaveTextMessage(account, contact, keywordResponse.Body); err != nil {
				a.Log.Error("Failed to send text message", "error", err, "contact", contact.PhoneNumber)
				return fmt.Errorf("send keyword response: %w", err)
			}
		}
		// Log outgoing message
		a.logSessionMessage(session.ID, models.DirectionOutgoing, keywordResponse.Body, "keyword_response")
		return nil
	}

	// If no keyword matched, try AI response if enabled
	if settings.AI.Enabled && settings.AI.Provider != "" && settings.AI.APIKey != "" {
		a.Log.Info("Attempting AI response", "provider", settings.AI.Provider, "model", settings.AI.Model)
		aiResult, actionErr := a.runInboundContinuationCapturedAction(
			"legacy_ai_generate",
			func() models.JSONB {
				aiResponse, generationErr := a.generateAIResponse(
					settings,
					session,
					messageText,
				)
				result := models.JSONB{"outcome": "empty"}
				switch {
				case generationErr != nil:
					result["outcome"] = "error"
					a.Log.Error("AI response failed", "error", generationErr, "provider", settings.AI.Provider, "model", settings.AI.Model)
				case aiResponse != "":
					result["outcome"] = "generated"
					// Store only the user-facing answer needed for recovery;
					// prompts, API keys, and request payloads are excluded.
					result["answer"] = aiResponse
				}
				return result
			},
		)
		if actionErr != nil {
			return fmt.Errorf("generate durable AI response: %w", actionErr)
		}
		aiResponse, _ := aiResult["answer"].(string)
		aiOutcome, _ := aiResult["outcome"].(string)
		if aiOutcome == "error" {
			a.Log.Error(
				"AI response failed",
				"provider", settings.AI.Provider,
				"model", settings.AI.Model,
			)
			// Fall through to default response
		} else if aiResponse != "" {
			a.Log.Info("AI response generated successfully", "response_length", len(aiResponse))
			if err := a.sendAndSaveTextMessage(account, contact, aiResponse); err != nil {
				a.Log.Error("Failed to send AI response", "error", err, "contact", contact.PhoneNumber)
				return fmt.Errorf("send AI response: %w", err)
			}
			a.logSessionMessage(session.ID, models.DirectionOutgoing, aiResponse, "ai_response")
			return nil
		} else {
			a.Log.Warn("AI returned empty response")
		}
	} else {
		a.Log.Info("AI not configured", "ai_enabled", settings.AI.Enabled, "has_provider", settings.AI.Provider != "", "has_api_key", settings.AI.APIKey != "")
	}

	// If no AI response or AI not enabled, send fallback message (for existing sessions)
	// Greeting is already sent for new sessions above
	if settings.FallbackMessage != "" && !isNewSession {
		a.Log.Info("Sending fallback message", "response", settings.FallbackMessage)
		if len(settings.FallbackButtons) > 0 {
			fallbackButtons := make([]map[string]any, 0)
			for _, btn := range settings.FallbackButtons {
				if btnMap, ok := btn.(map[string]any); ok {
					fallbackButtons = append(fallbackButtons, btnMap)
				}
			}
			if len(fallbackButtons) > 0 {
				if err := a.sendAndSaveInteractiveButtons(account, contact, settings.FallbackMessage, fallbackButtons); err != nil {
					a.Log.Error("Failed to send fallback buttons", "error", err, "contact", contact.PhoneNumber)
					return fmt.Errorf("send fallback buttons: %w", err)
				}
			} else {
				if err := a.sendAndSaveTextMessage(account, contact, settings.FallbackMessage); err != nil {
					a.Log.Error("Failed to send fallback message", "error", err, "contact", contact.PhoneNumber)
					return fmt.Errorf("send fallback response: %w", err)
				}
			}
		} else {
			if err := a.sendAndSaveTextMessage(account, contact, settings.FallbackMessage); err != nil {
				a.Log.Error("Failed to send fallback message", "error", err, "contact", contact.PhoneNumber)
				return fmt.Errorf("send fallback response: %w", err)
			}
		}
		a.logSessionMessage(session.ID, models.DirectionOutgoing, settings.FallbackMessage, "fallback_response")
	} else if !isNewSession {
		a.Log.Info("No fallback message configured for existing session")
	}
	return nil
}

// KeywordResponse holds the response content and optional buttons
type KeywordResponse struct {
	Body         string
	Buttons      []map[string]any
	ResponseType models.ResponseType // text, transfer
}

// matchKeywordRules checks if the message matches any keyword rules
func (a *App) matchKeywordRules(orgID uuid.UUID, accountName, messageText string) (*KeywordResponse, bool) {
	// Use cached keyword rules (includes both account-specific and global rules)
	rules, err := a.getKeywordRulesCached(orgID, accountName)
	if err != nil {
		a.Log.Error("Failed to fetch keyword rules", "error", err)
		return nil, false
	}

	messageLower := strings.ToLower(messageText)

	for _, rule := range rules {
		for _, keyword := range rule.Keywords {
			keywordLower := strings.ToLower(keyword)
			matched := false

			switch rule.MatchType {
			case models.MatchTypeExact:
				if rule.CaseSensitive {
					matched = messageText == keyword
				} else {
					matched = messageLower == keywordLower
				}
			case models.MatchTypeContains:
				if rule.CaseSensitive {
					matched = strings.Contains(messageText, keyword)
				} else {
					matched = strings.Contains(messageLower, keywordLower)
				}
			case models.MatchTypeStartsWith:
				if rule.CaseSensitive {
					matched = strings.HasPrefix(messageText, keyword)
				} else {
					matched = strings.HasPrefix(messageLower, keywordLower)
				}
			case models.MatchTypeRegex:
				re, err := regexp.Compile(keyword)
				if err == nil {
					matched = re.MatchString(messageText)
				}
			default:
				// Default to contains
				matched = strings.Contains(messageLower, keywordLower)
			}

			if matched {
				response := &KeywordResponse{
					ResponseType: rule.ResponseType,
				}

				// For transfer type, use body as the transfer message
				if rule.ResponseType == models.ResponseTypeTransfer {
					if body, ok := rule.ResponseContent["body"].(string); ok {
						response.Body = body
					}
					return response, true
				}

				// Get response body
				if body, ok := rule.ResponseContent["body"].(string); ok {
					response.Body = body
				}

				// Get buttons if present
				if buttons, ok := rule.ResponseContent["buttons"].([]any); ok && len(buttons) > 0 {
					response.Buttons = make([]map[string]any, 0, len(buttons))
					for _, btn := range buttons {
						if btnMap, ok := btn.(map[string]any); ok {
							response.Buttons = append(response.Buttons, btnMap)
						}
					}
				}

				if response.Body != "" {
					return response, true
				}
			}
		}
	}

	return nil, false
}

// sendAndSaveTextMessage sends a text message and saves it to the database
// Uses the unified SendOutgoingMessage for consistent behavior
func (a *App) sendAndSaveTextMessage(account *models.WhatsAppAccount, contact *models.Contact, message string) error {
	return a.runInboundContinuationSend(
		"text",
		map[string]any{
			"contact_id": contact.ID,
			"content":    message,
		},
		func() (*models.Message, error) {
			return a.SendOutgoingMessage(
				context.Background(),
				OutgoingMessageRequest{
					Account: account,
					Contact: contact,
					Type:    models.MessageTypeText,
					Content: message,
				},
				ChatbotSendOptions(),
			)
		},
	)
}

// sendAndSaveInteractiveButtons sends an interactive button message and saves it to the database.
// Buttons with type "url" are automatically separated and sent as CTA URL messages,
// since WhatsApp doesn't allow mixing reply buttons and URL buttons in the same message.
func (a *App) sendAndSaveInteractiveButtons(account *models.WhatsAppAccount, contact *models.Contact, bodyText string, buttons []map[string]any) error {
	// Separate reply buttons from CTA buttons (url / phone)
	replyButtons := make([]map[string]any, 0, len(buttons))
	ctaButtons := make([]map[string]any, 0)
	for _, btn := range buttons {
		btnType, _ := btn["type"].(string)
		switch btnType {
		case "url":
			ctaButtons = append(ctaButtons, btn)
		case "phone":
			// Convert phone button to CTA URL with tel: scheme
			phoneNumber, _ := btn["phone_number"].(string)
			if phoneNumber != "" {
				ctaButtons = append(ctaButtons, map[string]any{
					"title": btn["title"],
					"url":   "tel:" + phoneNumber,
				})
			}
		default:
			replyButtons = append(replyButtons, btn)
		}
	}

	// WhatsApp doesn't allow mixing reply and CTA buttons.
	// If both exist (legacy configs), ignore CTA buttons.
	if len(replyButtons) > 0 && len(ctaButtons) > 0 {
		ctaButtons = nil
	}

	// Send reply buttons (with the body text)
	if len(replyButtons) > 0 {
		waButtons := make([]whatsapp.Button, 0, len(replyButtons))
		for i, btn := range replyButtons {
			if i >= 10 {
				break
			}
			buttonID, _ := btn["id"].(string)
			buttonTitle, _ := btn["title"].(string)
			if buttonID == "" {
				buttonID = fmt.Sprintf("btn_%d", i+1)
			}
			if buttonTitle == "" {
				continue
			}
			waButtons = append(waButtons, whatsapp.Button{
				ID:    buttonID,
				Title: buttonTitle,
			})
		}

		if len(waButtons) > 0 {
			interactiveType := "button"
			if len(waButtons) > 3 {
				interactiveType = "list"
			}
			if err := a.runInboundContinuationSend(
				"interactive_buttons",
				map[string]any{
					"contact_id":       contact.ID,
					"interactive_type": interactiveType,
					"body":             bodyText,
					"buttons":          waButtons,
				},
				func() (*models.Message, error) {
					return a.SendOutgoingMessage(
						context.Background(),
						OutgoingMessageRequest{
							Account:         account,
							Contact:         contact,
							Type:            models.MessageTypeInteractive,
							InteractiveType: interactiveType,
							BodyText:        bodyText,
							Buttons:         waButtons,
						},
						ChatbotSendOptions(),
					)
				},
			); err != nil {
				return err
			}
		}
	}

	// Send CTA-only buttons (no reply buttons mixed in)
	// WhatsApp allows max 2 CTA buttons, each sent as a separate cta_url message.
	if len(ctaButtons) > 2 {
		ctaButtons = ctaButtons[:2]
	}
	for i, ctaBtn := range ctaButtons {
		btnTitle, _ := ctaBtn["title"].(string)
		btnURL, _ := ctaBtn["url"].(string)
		if btnTitle != "" && btnURL != "" {
			// First CTA button carries the body text
			ctaBody := bodyText
			if i > 0 {
				ctaBody = btnTitle
			}
			if err := a.sendAndSaveCTAURLButton(account, contact, ctaBody, btnTitle, btnURL); err != nil {
				return err
			}
		}
	}

	// No buttons at all — fall back to text
	if len(replyButtons) == 0 && len(ctaButtons) == 0 {
		return a.sendAndSaveTextMessage(account, contact, bodyText)
	}

	return nil
}

// sendAndSaveCTAURLButton sends a CTA URL button message and saves it to the database
// Uses the unified SendOutgoingMessage for consistent behavior
func (a *App) sendAndSaveCTAURLButton(account *models.WhatsAppAccount, contact *models.Contact, bodyText, buttonText, url string) error {
	return a.runInboundContinuationSend(
		"cta_url",
		map[string]any{
			"contact_id": contact.ID,
			"body":       bodyText,
			"button":     buttonText,
			"url":        url,
		},
		func() (*models.Message, error) {
			return a.SendOutgoingMessage(
				context.Background(),
				OutgoingMessageRequest{
					Account:         account,
					Contact:         contact,
					Type:            models.MessageTypeInteractive,
					InteractiveType: "cta_url",
					BodyText:        bodyText,
					ButtonText:      buttonText,
					URL:             url,
				},
				ChatbotSendOptions(),
			)
		},
	)
}

// sendAndSaveFlowMessage sends a WhatsApp Flow message and saves it to the database
// Uses the unified SendOutgoingMessage for consistent behavior
func (a *App) sendAndSaveFlowMessage(account *models.WhatsAppAccount, contact *models.Contact, flowID, headerText, bodyText, ctaText, flowToken, firstScreen string) error {
	return a.runInboundContinuationSend(
		"whatsapp_flow",
		map[string]any{
			"contact_id":   contact.ID,
			"flow_id":      flowID,
			"header":       headerText,
			"body":         bodyText,
			"cta":          ctaText,
			"first_screen": firstScreen,
		},
		func() (*models.Message, error) {
			return a.SendOutgoingMessage(
				context.Background(),
				OutgoingMessageRequest{
					Account:         account,
					Contact:         contact,
					Type:            models.MessageTypeFlow,
					FlowID:          flowID,
					FlowHeader:      headerText,
					BodyText:        bodyText,
					FlowCTA:         ctaText,
					FlowToken:       flowToken,
					FlowFirstScreen: firstScreen,
				},
				ChatbotSendOptions(),
			)
		},
	)
}

// getOrCreateSession finds an active session or creates a new one
// Returns the session and a boolean indicating if it's a new session
func (a *App) getOrCreateSession(
	orgID, contactID uuid.UUID,
	accountName, phoneNumber string,
	timeoutMins int,
) (*models.ChatbotSession, bool, error) {
	if timeoutMins <= 0 {
		timeoutMins = 30
	}
	now := time.Now()
	timeout := now.Add(-time.Duration(timeoutMins) * time.Minute)
	var result models.ChatbotSession
	isNew := false

	err := canonicalContactWriteTransaction(a.DB, func(tx *gorm.DB) error {
		canonical, err := contactutil.ResolveCanonicalContactForUpdate(tx, orgID, contactID)
		if err != nil {
			return err
		}

		var activeSessions []models.ChatbotSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"organization_id = ? AND contact_id = ? AND whats_app_account = ? AND status = ?",
				orgID,
				canonical.ID,
				accountName,
				models.SessionStatusActive,
			).
			Order("last_activity_at DESC, created_at DESC, id DESC").
			Find(&activeSessions).Error; err != nil {
			return err
		}

		chosen := -1
		staleIDs := make([]uuid.UUID, 0, len(activeSessions))
		duplicateLiveIDs := make([]uuid.UUID, 0, len(activeSessions))
		for i := range activeSessions {
			if activeSessions[i].LastActivityAt.After(timeout) {
				if chosen == -1 {
					chosen = i
				} else {
					duplicateLiveIDs = append(duplicateLiveIDs, activeSessions[i].ID)
				}
				continue
			}
			staleIDs = append(staleIDs, activeSessions[i].ID)
		}

		if len(staleIDs) > 0 {
			if err := tx.Model(&models.ChatbotSession{}).
				Where("id IN ? AND status = ?", staleIDs, models.SessionStatusActive).
				Updates(map[string]any{
					"status":       models.SessionStatusTimeout,
					"completed_at": now,
				}).Error; err != nil {
				return err
			}
		}
		if len(duplicateLiveIDs) > 0 {
			if err := tx.Model(&models.ChatbotSession{}).
				Where("id IN ? AND status = ?", duplicateLiveIDs, models.SessionStatusActive).
				Updates(map[string]any{
					"status":       models.SessionStatusCancelled,
					"completed_at": now,
				}).Error; err != nil {
				return err
			}
		}

		if chosen >= 0 {
			result = activeSessions[chosen]
			if err := tx.Model(&models.ChatbotSession{}).
				Where("id = ? AND status = ?", result.ID, models.SessionStatusActive).
				Update("last_activity_at", now).Error; err != nil {
				return err
			}
			result.LastActivityAt = now
			isNew = false
			return nil
		}

		canonicalPhone := canonical.PhoneNumber
		if canonicalPhone == "" {
			canonicalPhone = phoneNumber
		}
		result = models.ChatbotSession{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			OrganizationID:  orgID,
			ContactID:       canonical.ID,
			WhatsAppAccount: accountName,
			PhoneNumber:     canonicalPhone,
			Status:          models.SessionStatusActive,
			SessionData:     models.JSONB{},
			StartedAt:       now,
			LastActivityAt:  now,
		}
		if err := tx.Create(&result).Error; err != nil {
			return err
		}
		isNew = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return &result, isNew, nil
}

// logSessionMessage logs a message to the chatbot session
func (a *App) logSessionMessage(sessionID uuid.UUID, direction models.Direction, message, stepName string) {
	msg := models.ChatbotSessionMessage{
		BaseModel: models.BaseModel{ID: uuid.New()},
		SessionID: sessionID,
		Direction: direction,
		Message:   message,
		StepName:  stepName,
	}
	if err := a.DB.Create(&msg).Error; err != nil {
		a.Log.Error("Failed to log session message", "error", err)
	}
}

// matchFlowTrigger checks if the message triggers any flow
func (a *App) matchFlowTrigger(orgID uuid.UUID, messageText string) *models.ChatbotFlow {
	// Use cached flows (includes steps)
	flows, err := a.getChatbotFlowsCached(orgID)
	if err != nil {
		a.Log.Error("Failed to fetch chatbot flows", "error", err)
		return nil
	}

	messageLower := strings.ToLower(messageText)

	for _, flow := range flows {
		for _, keyword := range flow.TriggerKeywords {
			if strings.Contains(messageLower, strings.ToLower(keyword)) {
				return &flow
			}
		}
	}
	return nil
}

// startFlow initiates a chatbot flow for a user
func (a *App) exitFlow(session *models.ChatbotSession) {
	now := time.Now()
	a.DB.Model(session).Updates(map[string]any{
		"current_step": "",
		"step_retries": 0,
		"status":       models.SessionStatusCompleted,
		"completed_at": now,
	})

	// Clear chatbot tracking so SLA doesn't fire after flow exit
	a.ClearContactChatbotTracking(session.ContactID)
}

// closeSession ends the chatbot session and clears contact tracking
// It takes the full flow to find next steps when skipping
// executeConfiguredAPI builds and executes an HTTP request from a chatbot API config.
// replaceVar is called to substitute variables in the URL, body, and header values.
// Returns the response body and status code.
func (a *App) executeConfiguredAPI(apiConfig models.JSONB, replaceVar func(string) string) ([]byte, int, error) {
	apiURL, ok := apiConfig["url"].(string)
	if !ok || apiURL == "" {
		return nil, 0, fmt.Errorf("API URL is required")
	}
	apiURL = replaceVar(apiURL)
	logURL := redactURLForLog(apiURL)

	method := "GET"
	if m, ok := apiConfig["method"].(string); ok && m != "" {
		method = strings.ToUpper(m)
	}

	var bodyReader io.Reader
	if bodyTemplate, ok := apiConfig["body"].(string); ok && bodyTemplate != "" {
		bodyReader = strings.NewReader(replaceVar(bodyTemplate))
	}

	req, err := http.NewRequest(method, apiURL, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if headers, ok := apiConfig["headers"].(map[string]any); ok {
		for key, value := range headers {
			if strVal, ok := value.(string); ok {
				req.Header.Set(key, replaceVar(strVal))
			}
		}
	}

	a.Log.Info("Executing configured API request", "method", method, "url", logURL)

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		a.Log.Error("Configured API request failed", "method", method, "url", logURL, "error", err)
		return nil, 0, fmt.Errorf("API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	limitReader := io.LimitReader(resp.Body, 1024*1024)
	body, err := io.ReadAll(limitReader)
	if err != nil {
		a.Log.Error("Failed to read configured API response", "method", method, "url", logURL, "status_code", resp.StatusCode, "error", err)
		return nil, 0, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		a.Log.Warn(
			"Configured API request returned non-2xx",
			"method", method,
			"url", logURL,
			"status_code", resp.StatusCode,
			"response_preview", truncateLogValue(string(body), 300),
		)
	} else {
		a.Log.Info(
			"Configured API request completed",
			"method", method,
			"url", logURL,
			"status_code", resp.StatusCode,
			"response_bytes", len(body),
		)
	}

	return body, resp.StatusCode, nil
}

type ApiResponse struct {
	Message      string
	Buttons      []map[string]any
	MappedData   map[string]any // Data extracted via response_mapping
	ResponseData map[string]any // Full API response data
}

// fetchApiResponse fetches a response from an external API, supporting message + buttons
// and response_mapping for storing API data in session variables.
//
// Mirrors fetchAPIContext in seeding implicit variables (phone_number) so flow-step
// API templates can interpolate {{phone_number}} just like AI-context API templates.
func (a *App) generateAIResponse(settings *models.ChatbotSettings, session *models.ChatbotSession, userMessage string) (string, error) {
	// Build context from AIContext entries
	contextData := a.buildAIContext(settings.OrganizationID, session, userMessage)

	switch settings.AI.Provider {
	case models.AIProviderOpenAI:
		return a.generateOpenAIResponse(settings, session, userMessage, contextData)
	case models.AIProviderAnthropic:
		return a.generateAnthropicResponse(settings, session, userMessage, contextData)
	case models.AIProviderGoogle:
		return a.generateGoogleResponse(settings, session, userMessage, contextData)
	case models.AIProviderQwen:
		return a.generateQwenResponse(settings, session, userMessage, contextData)
	default:
		return "", fmt.Errorf("unsupported AI provider: %s", settings.AI.Provider)
	}
}

// buildAIContext fetches and combines all AI context data
func (a *App) buildAIContext(orgID uuid.UUID, session *models.ChatbotSession, userMessage string) string {
	// Get WhatsApp account for cache key
	whatsAppAccount := ""
	if session != nil {
		whatsAppAccount = session.WhatsAppAccount
	}

	// Use cached AI contexts
	contexts, err := a.getAIContextsCached(orgID, whatsAppAccount)
	if err != nil || len(contexts) == 0 {
		return ""
	}

	var contextParts []string

	for _, ctx := range contexts {
		var content string

		switch ctx.ContextType {
		case models.ContextTypeStatic:
			content = ctx.StaticContent

		case models.ContextTypeAPI:
			// Start with static content/prompt if provided
			content = ctx.StaticContent

			// Fetch data from external API and append
			apiContent, err := a.fetchAPIContext(ctx.ApiConfig, session, userMessage)
			if err != nil {
				a.Log.Error("Failed to fetch API context", "context_name", ctx.Name, "error", err)
				// Still use static content if API fails
			} else if apiContent != "" {
				if content != "" {
					content = content + "\n\nData:\n" + apiContent
				} else {
					content = apiContent
				}
			}
		}

		if content != "" {
			contextParts = append(contextParts, fmt.Sprintf("### %s\n%s", ctx.Name, content))
		}
	}

	if len(contextParts) == 0 {
		return ""
	}

	return "## Context Information\n\n" + strings.Join(contextParts, "\n\n")
}

// fetchAPIContext fetches context data from an external API
func (a *App) fetchAPIContext(apiConfig models.JSONB, session *models.ChatbotSession, userMessage string) (string, error) {
	if apiConfig == nil {
		return "", fmt.Errorf("API config is empty")
	}

	// Build session data for variable replacement
	sessionData := models.JSONB{}
	if session != nil {
		sessionData = session.SessionData
		if sessionData == nil {
			sessionData = models.JSONB{}
		}
		sessionData["phone_number"] = session.PhoneNumber
		sessionData["user_message"] = userMessage
	}

	replaceVar := func(s string) string { return processTemplate(s, sessionData) }
	respBody, statusCode, err := a.executeConfiguredAPI(apiConfig, replaceVar)
	if err != nil {
		return "", err
	}

	if statusCode < 200 || statusCode >= 300 {
		return "", fmt.Errorf("API returned status %d", statusCode)
	}

	// Check for response_path to extract specific field
	if responsePath, ok := apiConfig["response_path"].(string); ok && responsePath != "" {
		var jsonResp map[string]any
		if err := json.Unmarshal(respBody, &jsonResp); err == nil {
			if value := getNestedValue(jsonResp, responsePath); value != nil {
				return formatValue(value), nil
			}
		}
	}

	return string(respBody), nil
}

// generateOpenAICompatibleResponse calls an OpenAI-compatible chat completions
// endpoint. Qwen uses this protocol through Alibaba Cloud Model Studio.
func (a *App) generateOpenAICompatibleResponse(providerName, url string, settings *models.ChatbotSettings, session *models.ChatbotSession, userMessage string, contextData string, extraPayload map[string]any) (string, error) {
	// Build messages array
	messages := []map[string]string{}

	// Build system prompt with context
	systemPrompt := settings.AI.SystemPrompt
	if contextData != "" {
		if systemPrompt != "" {
			systemPrompt = systemPrompt + "\n\n" + contextData
		} else {
			systemPrompt = contextData
		}
	}

	// Add system prompt if configured
	if systemPrompt != "" {
		messages = append(messages, map[string]string{
			"role":    "system",
			"content": systemPrompt,
		})
	}

	// Add conversation history if enabled
	if settings.AI.IncludeHistory && session != nil {
		history := a.getSessionHistory(session.ID, settings.AI.HistoryLimit)
		for _, msg := range history {
			role := "user"
			if msg.Direction == models.DirectionOutgoing {
				role = "assistant"
			}
			messages = append(messages, map[string]string{
				"role":    role,
				"content": msg.Message,
			})
		}
	}

	// Add current user message
	messages = append(messages, map[string]string{
		"role":    "user",
		"content": userMessage,
	})

	payload := map[string]any{
		"model":      settings.AI.Model,
		"messages":   messages,
		"max_tokens": settings.AI.MaxTokens,
	}
	for key, value := range extraPayload {
		payload[key] = value
	}

	if settings.AI.Temperature > 0 {
		payload["temperature"] = settings.AI.Temperature
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+settings.AI.APIKey)

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	const maxAIResponseBytes = 4 << 20
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAIResponseBytes+1))
	if readErr != nil {
		return "", fmt.Errorf("failed to read %s response: %w", providerName, readErr)
	}
	if len(body) > maxAIResponseBytes {
		return "", fmt.Errorf("%s response exceeded the size limit", providerName)
	}

	if resp.StatusCode != 200 {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &errResp)
		if errResp.Error.Message == "" {
			errResp.Error.Message = strings.TrimSpace(string(body))
		}
		return "", fmt.Errorf("%s API error: %s", providerName, errResp.Error.Message)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(result.Choices) > 0 {
		return strings.TrimSpace(result.Choices[0].Message.Content), nil
	}

	return "", fmt.Errorf("no response from %s", providerName)
}

// generateOpenAIResponse generates a response using OpenAI API.
func (a *App) generateOpenAIResponse(settings *models.ChatbotSettings, session *models.ChatbotSession, userMessage string, contextData string) (string, error) {
	return a.generateOpenAICompatibleResponse(
		"OpenAI",
		"https://api.openai.com/v1/chat/completions",
		settings,
		session,
		userMessage,
		contextData,
		nil,
	)
}

// generateQwenResponse generates a low-latency customer-service response using
// Alibaba Cloud Model Studio's OpenAI-compatible Qwen API. Thinking is disabled
// for routine CRM replies to keep latency and token usage predictable.
func (a *App) generateQwenResponse(settings *models.ChatbotSettings, session *models.ChatbotSession, userMessage string, contextData string) (string, error) {
	messages := make([]qwenapi.Message, 0, 8)
	systemPrompt := settings.AI.SystemPrompt
	if contextData != "" {
		if systemPrompt != "" {
			systemPrompt += "\n\n" + contextData
		} else {
			systemPrompt = contextData
		}
	}
	if systemPrompt != "" {
		messages = append(messages, qwenapi.Message{Role: "system", Content: systemPrompt})
	}
	if settings.AI.IncludeHistory && session != nil {
		for _, message := range a.getSessionHistory(session.ID, settings.AI.HistoryLimit) {
			role := "user"
			if message.Direction == models.DirectionOutgoing {
				role = "assistant"
			}
			messages = append(messages, qwenapi.Message{Role: role, Content: message.Message})
		}
	}
	messages = append(messages, qwenapi.Message{Role: "user", Content: userMessage})

	baseURL := qwenapi.DefaultBaseURL
	if a.Config != nil && a.Config.AI.QwenBaseURL != "" {
		baseURL = a.Config.AI.QwenBaseURL
	}
	return qwenapi.Generate(context.Background(), a.HTTPClient, qwenapi.Options{
		APIKey:      settings.AI.APIKey,
		BaseURL:     baseURL,
		Model:       settings.AI.Model,
		MaxTokens:   settings.AI.MaxTokens,
		Temperature: settings.AI.Temperature,
		Messages:    messages,
	})
}

// generateAnthropicResponse generates a response using Anthropic API
func (a *App) generateAnthropicResponse(settings *models.ChatbotSettings, session *models.ChatbotSession, userMessage string, contextData string) (string, error) {
	url := "https://api.anthropic.com/v1/messages"

	// Build messages array
	messages := []map[string]string{}

	// Add conversation history if enabled
	if settings.AI.IncludeHistory && session != nil {
		history := a.getSessionHistory(session.ID, settings.AI.HistoryLimit)
		for _, msg := range history {
			role := "user"
			if msg.Direction == models.DirectionOutgoing {
				role = "assistant"
			}
			messages = append(messages, map[string]string{
				"role":    role,
				"content": msg.Message,
			})
		}
	}

	// Add current user message
	messages = append(messages, map[string]string{
		"role":    "user",
		"content": userMessage,
	})

	payload := map[string]any{
		"model":      settings.AI.Model,
		"messages":   messages,
		"max_tokens": settings.AI.MaxTokens,
	}

	// Build system prompt with context
	systemPrompt := settings.AI.SystemPrompt
	if contextData != "" {
		if systemPrompt != "" {
			systemPrompt = systemPrompt + "\n\n" + contextData
		} else {
			systemPrompt = contextData
		}
	}

	// Add system prompt if configured
	if systemPrompt != "" {
		payload["system"] = systemPrompt
	}

	if settings.AI.Temperature > 0 {
		payload["temperature"] = settings.AI.Temperature
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", settings.AI.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &errResp)
		return "", fmt.Errorf("anthropic API error: %s", errResp.Error.Message)
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	for _, content := range result.Content {
		if content.Type == "text" {
			return strings.TrimSpace(content.Text), nil
		}
	}

	return "", fmt.Errorf("no text response from Anthropic")
}

// generateGoogleResponse generates a response using Google Gemini API
func (a *App) generateGoogleResponse(settings *models.ChatbotSettings, session *models.ChatbotSession, userMessage string, contextData string) (string, error) {
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
		settings.AI.Model, settings.AI.APIKey)

	// Build contents array
	contents := []map[string]any{}

	// Add conversation history if enabled
	if settings.AI.IncludeHistory && session != nil {
		history := a.getSessionHistory(session.ID, settings.AI.HistoryLimit)
		for _, msg := range history {
			role := "user"
			if msg.Direction == models.DirectionOutgoing {
				role = "model"
			}
			contents = append(contents, map[string]any{
				"role": role,
				"parts": []map[string]string{
					{"text": msg.Message},
				},
			})
		}
	}

	// Add current user message
	contents = append(contents, map[string]any{
		"role": "user",
		"parts": []map[string]string{
			{"text": userMessage},
		},
	})

	payload := map[string]any{
		"contents": contents,
		"generationConfig": map[string]any{
			"maxOutputTokens": settings.AI.MaxTokens,
		},
	}

	// Build system prompt with context
	systemPrompt := settings.AI.SystemPrompt
	if contextData != "" {
		if systemPrompt != "" {
			systemPrompt = systemPrompt + "\n\n" + contextData
		} else {
			systemPrompt = contextData
		}
	}

	// Add system instruction if configured
	if systemPrompt != "" {
		payload["systemInstruction"] = map[string]any{
			"parts": []map[string]string{
				{"text": systemPrompt},
			},
		}
	}

	if settings.AI.Temperature > 0 {
		payload["generationConfig"].(map[string]any)["temperature"] = settings.AI.Temperature
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &errResp)
		return "", fmt.Errorf("google AI API error: %s", errResp.Error.Message)
	}

	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(result.Candidates) > 0 && len(result.Candidates[0].Content.Parts) > 0 {
		return strings.TrimSpace(result.Candidates[0].Content.Parts[0].Text), nil
	}

	return "", fmt.Errorf("no response from Google AI")
}

// getSessionHistory retrieves recent messages from the session
func (a *App) getSessionHistory(sessionID uuid.UUID, limit int) []models.ChatbotSessionMessage {
	var messages []models.ChatbotSessionMessage
	a.DB.Where("session_id = ?", sessionID).
		Order("created_at DESC").
		Limit(limit).
		Find(&messages)

	// Reverse to get chronological order
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	return messages
}

// Reaction represents a reaction on a message
type Reaction struct {
	Emoji     string `json:"emoji"`
	FromPhone string `json:"from_phone,omitempty"` // Phone number if from contact
	FromUser  string `json:"from_user,omitempty"`  // User ID if from agent
}

// handleIncomingReaction handles incoming reaction messages from WhatsApp
func (a *App) handleIncomingReaction(account *models.WhatsAppAccount, fromPhone, messageWAMID, emoji, profileName string) {
	a.Log.Info("Handling incoming reaction",
		"from", fromPhone,
		"message_wamid", messageWAMID,
		"emoji", emoji,
	)

	// Find the message being reacted to
	// WhatsApp encodes phone numbers in the WAMID prefix, so the same message
	// has different WAMIDs from sender vs recipient perspective.
	// We match on the suffix after "FQIA" + 4 chars (type indicator like "ERgS" or "EhgU")
	var message models.Message
	if err := a.DB.Where(
		"organization_id = ? AND whats_app_message_id = ?",
		account.OrganizationID,
		messageWAMID,
	).First(&message).Error; err != nil {
		// Try matching on WAMID suffix (the unique message ID part)
		if idx := strings.Index(messageWAMID, "FQIA"); idx != -1 {
			// Extract suffix after "FQIA" + 4 char type indicator (e.g., "ERgS", "EhgU")
			suffixStart := idx + 8
			if suffixStart < len(messageWAMID) {
				suffix := messageWAMID[suffixStart:]
				if err := a.DB.Where(
					"organization_id = ? AND whats_app_message_id LIKE ?",
					account.OrganizationID,
					"%"+suffix,
				).First(&message).Error; err != nil {
					a.Log.Warn("Message not found for reaction", "wamid", messageWAMID, "suffix", suffix)
					return
				}
			} else {
				a.Log.Warn("Message not found for reaction - invalid WAMID format", "wamid", messageWAMID)
				return
			}
		} else {
			a.Log.Warn("Message not found for reaction - no FQIA pattern", "wamid", messageWAMID)
			return
		}
	}

	// Keep any newly discovered contact and its durable lifecycle event atomic.
	contact, _, err := a.getOrCreateInboundContact(account, fromPhone, profileName, "")
	if err != nil {
		a.Log.Error("Failed to get or create reaction contact", "phone", fromPhone, "error", err)
		return
	}

	// Parse existing reactions from Metadata
	var metadata map[string]any
	if message.Metadata != nil {
		metadata = message.Metadata
	} else {
		metadata = make(map[string]any)
	}

	// Get or initialize reactions array
	var reactions []Reaction
	if reactionsRaw, ok := metadata["reactions"]; ok {
		if reactionsArray, ok := reactionsRaw.([]any); ok {
			for _, r := range reactionsArray {
				if rMap, ok := r.(map[string]any); ok {
					emoji, _ := rMap["emoji"].(string)
					reactions = append(reactions, Reaction{
						Emoji:     emoji,
						FromPhone: getStringFromMap(rMap, "from_phone"),
						FromUser:  getStringFromMap(rMap, "from_user"),
					})
				}
			}
		}
	}

	// Remove existing reaction from this contact (each contact can only have one reaction)
	var newReactions []Reaction
	for _, r := range reactions {
		if r.FromPhone != fromPhone {
			newReactions = append(newReactions, r)
		}
	}

	// Add new reaction if emoji is not empty (empty = remove reaction)
	if emoji != "" {
		newReactions = append(newReactions, Reaction{
			Emoji:     emoji,
			FromPhone: fromPhone,
		})
	}

	// Update metadata
	metadata["reactions"] = newReactions

	// Save to database
	if err := a.DB.Model(&message).Update("metadata", metadata).Error; err != nil {
		a.Log.Error("Failed to update message reactions", "error", err)
		return
	}

	a.Log.Info("Updated message reaction", "message_id", message.ID, "reactions_count", len(newReactions))

	// Broadcast via WebSocket
	a.broadcastReactionUpdate(account.OrganizationID, message.ID, contact.ID, newReactions)
}

// Helper function to safely get string from map
func getStringFromMap(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// ExtractedMessage holds the derived content fields of a message.
type ExtractedMessage struct {
	Text             string
	Type             string // may differ from msg.Type, e.g. "button_reply"
	Media            *MediaInfo
	ButtonID         string         // used by chatbot routing only
	FlowResponseData map[string]any // used by chatbot routing only
}

// extractMessageContent walks an IncomingTextMessage and returns the derived
// fields, including a best-effort media download. The regular inbound webhook
// path uses extractMessageContentForPersistence directly so network I/O never
// delays Meta's acknowledgement.
func (a *App) extractMessageContent(ctx context.Context, msg IncomingTextMessage, account *models.WhatsAppAccount) ExtractedMessage {
	extracted := a.extractMessageContentForPersistence(msg)
	a.hydrateExtractedIncomingMedia(ctx, msg, account, &extracted)
	return extracted
}

// extractMessageContentForPersistence normalizes only data already present in
// Meta's payload. It must remain free of provider and object-store calls.
func (a *App) extractMessageContentForPersistence(msg IncomingTextMessage) ExtractedMessage {
	extracted := ExtractedMessage{
		Type: msg.Type,
	}

	if msg.Type == "text" && msg.Text != nil {
		extracted.Text = msg.Text.Body
	} else if msg.Type == "button" && msg.Button != nil {
		// Template quick_reply button click — WhatsApp sends type "button"
		extracted.Text = msg.Button.Text
		extracted.ButtonID = msg.Button.Payload
		extracted.Type = "button_reply"
	} else if msg.Type == "interactive" && msg.Interactive != nil {
		// Handle button reply
		if msg.Interactive.ButtonReply != nil {
			extracted.Text = msg.Interactive.ButtonReply.Title
			extracted.ButtonID = msg.Interactive.ButtonReply.ID
			extracted.Type = "button_reply"
		}
		// Handle list reply
		if msg.Interactive.ListReply != nil {
			extracted.Text = msg.Interactive.ListReply.Title
			extracted.ButtonID = msg.Interactive.ListReply.ID
			extracted.Type = "button_reply"
		}
		// Handle WhatsApp Flow reply (nfm_reply)
		if msg.Interactive.NFMReply != nil {
			extracted.Text = msg.Interactive.NFMReply.Body
			extracted.Type = "nfm_reply"
			// Parse the response JSON to extract form data
			if msg.Interactive.NFMReply.ResponseJSON != "" {
				var responseData map[string]any
				if err := json.Unmarshal([]byte(msg.Interactive.NFMReply.ResponseJSON), &responseData); err != nil {
					a.Log.Error("Failed to parse flow response JSON", "error", err, "response_json", msg.Interactive.NFMReply.ResponseJSON)
				} else {
					extracted.FlowResponseData = responseData
					a.Log.Info("Parsed WhatsApp Flow response", "data", extracted.FlowResponseData)
				}
			}
		}
	} else if msg.Type == "image" && msg.Image != nil {
		// Handle image message
		extracted.Text = msg.Image.Caption
		extracted.Media = &MediaInfo{
			MediaMimeType: msg.Image.MimeType,
		}
	} else if msg.Type == "document" && msg.Document != nil {
		// Handle document message
		extracted.Text = msg.Document.Caption
		extracted.Media = &MediaInfo{
			MediaMimeType: msg.Document.MimeType,
			MediaFilename: msg.Document.Filename,
		}
	} else if msg.Type == "video" && msg.Video != nil {
		// Handle video message
		extracted.Text = msg.Video.Caption
		extracted.Media = &MediaInfo{
			MediaMimeType: msg.Video.MimeType,
		}
	} else if msg.Type == "audio" && msg.Audio != nil {
		// Handle audio message
		extracted.Media = &MediaInfo{
			MediaMimeType: msg.Audio.MimeType,
		}
	} else if msg.Type == "sticker" && msg.Sticker != nil {
		// Handle sticker message (treat like image)
		extracted.Media = &MediaInfo{
			MediaMimeType: msg.Sticker.MimeType,
		}
	} else if msg.Type == "location" && msg.Location != nil {
		// Handle location message - store as JSON in content
		locationData := map[string]any{
			"latitude":  msg.Location.Latitude,
			"longitude": msg.Location.Longitude,
		}
		if msg.Location.Name != "" {
			locationData["name"] = msg.Location.Name
		}
		if msg.Location.Address != "" {
			locationData["address"] = msg.Location.Address
		}
		if jsonBytes, err := json.Marshal(locationData); err == nil {
			extracted.Text = string(jsonBytes)
		}
	} else if msg.Type == "contacts" && len(msg.Contacts) > 0 {
		// Handle contacts message - store as JSON in content
		contactsData := make([]map[string]any, 0, len(msg.Contacts))
		for _, c := range msg.Contacts {
			contactVal := map[string]any{
				"name": c.Name.FormattedName,
			}
			if len(c.Phones) > 0 {
				phones := make([]string, 0, len(c.Phones))
				for _, p := range c.Phones {
					phones = append(phones, p.Phone)
				}
				contactVal["phones"] = phones
			}
			contactsData = append(contactsData, contactVal)
		}
		if jsonBytes, err := json.Marshal(contactsData); err == nil {
			extracted.Text = string(jsonBytes)
		}
	}

	return extracted
}

// hydrateExtractedIncomingMedia performs the optional provider download after
// the payload-only representation has been normalized.
func (a *App) hydrateExtractedIncomingMedia(
	ctx context.Context,
	msg IncomingTextMessage,
	account *models.WhatsAppAccount,
	extracted *ExtractedMessage,
) {
	if account == nil || extracted == nil || extracted.Media == nil {
		return
	}

	var mediaID, mimeType, label string
	switch {
	case msg.Type == "image" && msg.Image != nil:
		mediaID, mimeType, label = msg.Image.ID, msg.Image.MimeType, "image"
	case msg.Type == "document" && msg.Document != nil:
		mediaID, mimeType, label = msg.Document.ID, msg.Document.MimeType, "document"
	case msg.Type == "video" && msg.Video != nil:
		mediaID, mimeType, label = msg.Video.ID, msg.Video.MimeType, "video"
	case msg.Type == "audio" && msg.Audio != nil:
		mediaID, mimeType, label = msg.Audio.ID, msg.Audio.MimeType, "audio"
	case msg.Type == "sticker" && msg.Sticker != nil:
		mediaID, mimeType, label = msg.Sticker.ID, msg.Sticker.MimeType, "sticker"
	default:
		return
	}
	if mediaID == "" {
		return
	}

	waAccount := a.toWhatsAppAccount(account)
	localPath, err := a.DownloadAndSaveMedia(
		ctx,
		account.OrganizationID,
		mediaID,
		mimeType,
		waAccount,
	)
	if err != nil {
		a.Log.Error(
			"Failed to hydrate incoming media",
			"error", err,
			"media_id", mediaID,
			"media_type", label,
		)
		return
	}
	extracted.Media.MediaURL = localPath
}

// MediaInfo holds media-related information for an incoming message
type MediaInfo struct {
	MediaURL      string
	MediaMimeType string
	MediaFilename string
}

var errIncomingMessageAlreadyProcessed = errors.New("incoming message already processed")

// persistIncomingMessageBeforeAck is the synchronous Meta webhook boundary for
// regular inbound messages. It returns only after the contact, normalized
// Message, customer activity, and durable outbox rows have committed. A replay
// is a successful no-op so Meta can safely stop retrying it.
func (a *App) persistIncomingMessageBeforeAck(
	phoneNumberID string,
	msg IncomingTextMessage,
	profileName string,
) (work *persistedIncomingMessage, duplicate bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			work = nil
			duplicate = false
			err = fmt.Errorf("panic while persisting incoming message: %v", recovered)
		}
	}()

	if strings.TrimSpace(phoneNumberID) == "" {
		return nil, false, errors.New("incoming message phone number ID is required")
	}
	if strings.TrimSpace(msg.ID) == "" {
		return nil, false, errors.New("incoming WhatsApp message ID is required")
	}
	if msg.Type == "reaction" {
		return nil, false, errors.New("reaction messages use the specialized inbound path")
	}

	if a.rlsEnabled() && !a.hasTenantScope() {
		err = a.withPhoneTenant(phoneNumberID, func(scoped *App) error {
			var scopedErr error
			work, duplicate, scopedErr = scoped.persistIncomingMessageForAccount(
				phoneNumberID,
				msg,
				profileName,
				nil,
			)
			return scopedErr
		})
		return work, duplicate, err
	}

	if a.rlsEnabled() {
		return a.persistIncomingMessageForAccount(
			phoneNumberID,
			msg,
			profileName,
			nil,
		)
	}

	// RLS supplies an outer tenant transaction in production. Keep the same
	// all-or-nothing boundary when RLS is disabled by binding a scoped clone to
	// one explicit transaction.
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		txApp := a.scopedApp(tx, uuid.Nil)
		var transactionErr error
		work, duplicate, transactionErr = txApp.persistIncomingMessageForAccount(
			phoneNumberID,
			msg,
			profileName,
			nil,
		)
		return transactionErr
	})
	return work, duplicate, err
}

func (a *App) persistIncomingMessageForAccount(
	phoneNumberID string,
	msg IncomingTextMessage,
	profileName string,
	account *models.WhatsAppAccount,
) (*persistedIncomingMessage, bool, error) {
	var err error
	if account == nil {
		account, err = a.getWhatsAppAccountCached(phoneNumberID)
		if err != nil {
			return nil, false, fmt.Errorf("load WhatsApp account: %w", err)
		}
	}

	// Existing merge aliases are locked and resolved before the message write.
	contact, _, err := a.getOrCreateInboundContact(
		account,
		msg.From,
		profileName,
		msg.FromUserID,
	)
	if err != nil {
		return nil, false, fmt.Errorf("get or create inbound contact: %w", err)
	}

	extracted := a.extractMessageContentForPersistence(msg)
	replyToWAMID := ""
	if msg.Context != nil {
		replyToWAMID = msg.Context.ID
	}
	message, err := a.persistIncomingMessageWithFlow(
		account,
		contact,
		msg.ID,
		extracted.Type,
		extracted.Text,
		extracted.Media,
		replyToWAMID,
		extracted.FlowResponseData,
	)
	if errors.Is(err, errIncomingMessageAlreadyProcessed) {
		a.Log.Info("Ignored duplicate incoming message", "whatsapp_message_id", msg.ID)
		var existing models.Message
		if loadErr := a.DB.Where(
			"organization_id = ? AND whats_app_account = ? AND whats_app_message_id = ?",
			account.OrganizationID,
			account.Name,
			msg.ID,
		).First(&existing).Error; loadErr != nil {
			return nil, false, fmt.Errorf(
				"load duplicate incoming message for continuation: %w",
				loadErr,
			)
		}
		if queueErr := a.ensureInboundContinuationJob(
			account,
			&existing,
			msg,
			profileName,
		); queueErr != nil {
			return nil, false, queueErr
		}
		return &persistedIncomingMessage{
			OrganizationID: account.OrganizationID,
			PhoneNumberID:  phoneNumberID,
			Message:        msg,
			Account:        *account,
			Contact:        *contact,
			Extracted:      extracted,
			Persisted:      existing,
		}, true, nil
	}
	if err != nil {
		return nil, false, err
	}

	work := &persistedIncomingMessage{
		OrganizationID: account.OrganizationID,
		PhoneNumberID:  phoneNumberID,
		Message:        msg,
		Account:        *account,
		Contact:        *contact,
		Extracted:      extracted,
		Persisted:      *message,
	}
	if err := a.ensureInboundContinuationJob(
		account,
		message,
		msg,
		profileName,
	); err != nil {
		return nil, false, err
	}
	return work, false, nil
}

func (a *App) hydratePersistedIncomingMedia(
	ctx context.Context,
	work *persistedIncomingMessage,
	account *models.WhatsAppAccount,
) {
	if work == nil || work.Extracted.Media == nil {
		return
	}

	hydrated := work.Extracted
	hydratedMedia := *work.Extracted.Media
	hydrated.Media = &hydratedMedia
	a.hydrateExtractedIncomingMedia(ctx, work.Message, account, &hydrated)
	if hydrated.Media.MediaURL == "" {
		return
	}

	updates := map[string]any{
		"media_url":       hydrated.Media.MediaURL,
		"media_mime_type": hydrated.Media.MediaMimeType,
		"media_filename":  hydrated.Media.MediaFilename,
	}
	if err := a.DB.Model(&models.Message{}).
		Where(
			"organization_id = ? AND id = ?",
			work.OrganizationID,
			work.Persisted.ID,
		).
		Updates(updates).Error; err != nil {
		a.Log.Error(
			"Failed to persist hydrated incoming media",
			"error", err,
			"message_id", work.Persisted.ID,
		)
		return
	}

	work.Extracted = hydrated
	work.Persisted.MediaURL = hydrated.Media.MediaURL
	work.Persisted.MediaMimeType = hydrated.Media.MediaMimeType
	work.Persisted.MediaFilename = hydrated.Media.MediaFilename
}

// getOrCreateInboundContact keeps the contact row, immutable CRM activity, and
// durable webhook outbox in one transaction. A unique-contact race aborts the
// losing PostgreSQL transaction, so retry it from a fresh transaction.
func (a *App) getOrCreateInboundContact(
	account *models.WhatsAppAccount,
	phoneNumber, profileName, bsuid string,
) (*models.Contact, bool, error) {
	if account == nil {
		return nil, false, errors.New("WhatsApp account is required")
	}

	var result models.Contact
	var isNew bool
	var err error
	for attempt := 0; attempt < canonicalContactWriteAttempts; attempt++ {
		err = a.DB.Transaction(func(tx *gorm.DB) error {
			contact, created, createErr := contactutil.GetOrCreateContact(
				tx,
				account.OrganizationID,
				phoneNumber,
				profileName,
			)
			if createErr != nil {
				return createErr
			}

			canonical, resolveErr := contactutil.ResolveCanonicalContactForUpdate(
				tx,
				account.OrganizationID,
				contact.ID,
			)
			if resolveErr != nil {
				return resolveErr
			}
			if profileName != "" && canonical.ProfileName != profileName {
				if updateErr := tx.Model(canonical).
					Update("profile_name", profileName).Error; updateErr != nil {
					return updateErr
				}
				canonical.ProfileName = profileName
			}
			if created {
				if _, activityErr := recordCustomerActivity(
					tx,
					account.OrganizationID,
					customerActivityInput{
						ContactID:        canonical.ID,
						EventType:        models.CustomerActivityContactCreated,
						Category:         models.CustomerActivityCategoryContact,
						Title:            "Contact created",
						Summary:          canonical.ProfileName,
						ActorType:        models.CustomerActivityActorContact,
						SourceObjectType: "contact",
						SourceObjectID:   &canonical.ID,
						OccurredAt:       time.Now().UTC(),
						Metadata: models.JSONB{
							"whatsapp_account": account.Name,
						},
						WebhookData: models.JSONB{
							"contact_phone":    canonical.PhoneNumber,
							"contact_name":     canonical.ProfileName,
							"whatsapp_account": account.Name,
						},
						IdempotencyKey: "contact-created:" + canonical.ID.String(),
					},
				); activityErr != nil {
					return activityErr
				}
			}

			result = *canonical
			isNew = created
			return nil
		})
		if err == nil {
			// BSUID is optional metadata. Isolate its update behind a
			// savepoint so a failure cannot roll back the durable contact,
			// CRM activity, or the inbound message written by the caller.
			a.updateContactBSUID(&result, bsuid)
			return &result, isNew, nil
		}
		if !isUniqueViolation(err) && !isRetryableCanonicalContactWrite(err) {
			return nil, false, err
		}
	}
	return nil, false, err
}

// saveIncomingMessage saves an incoming message to the messages table
func (a *App) saveIncomingMessage(account *models.WhatsAppAccount, contact *models.Contact, whatsappMsgID, msgType, content string, mediaInfo *MediaInfo, replyToWAMID string) bool {
	return a.saveIncomingMessageWithFlow(account, contact, whatsappMsgID, msgType, content, mediaInfo, replyToWAMID, nil)
}

// saveIncomingMessageWithFlow persists the normalized WhatsApp Flow response
// together with the message. Keeping the submitted fields on the immutable
// inbound record is required for idempotent booking and CRM actions; session
// data alone can be overwritten by later chatbot steps.
func (a *App) saveIncomingMessageWithFlow(account *models.WhatsAppAccount, contact *models.Contact, whatsappMsgID, msgType, content string, mediaInfo *MediaInfo, replyToWAMID string, flowResponseData map[string]any) bool {
	message, err := a.persistIncomingMessageWithFlow(
		account,
		contact,
		whatsappMsgID,
		msgType,
		content,
		mediaInfo,
		replyToWAMID,
		flowResponseData,
	)
	if err != nil {
		if errors.Is(err, errIncomingMessageAlreadyProcessed) {
			a.Log.Info("Ignored duplicate incoming message", "whatsapp_message_id", whatsappMsgID)
		} else {
			a.Log.Error("Failed to save incoming message", "error", err)
		}
		return false
	}

	a.Log.Info("Saved incoming message", "message_id", message.ID, "contact_id", contact.ID, "media_url", message.MediaURL)
	a.broadcastNewMessage(account.OrganizationID, message, contact)
	return true
}

// persistIncomingMessageWithFlow commits the normalized message and its
// lifecycle/outbox fact but performs no provider or WebSocket work. Callers can
// therefore place an acknowledgement boundary immediately after this returns.
func (a *App) persistIncomingMessageWithFlow(account *models.WhatsAppAccount, contact *models.Contact, whatsappMsgID, msgType, content string, mediaInfo *MediaInfo, replyToWAMID string, flowResponseData map[string]any) (*models.Message, error) {
	if account == nil || contact == nil {
		return nil, errors.New("cannot save incoming message without account and contact")
	}

	message := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    account.OrganizationID,
		WhatsAppAccount:   account.Name,
		WhatsAppMessageID: whatsappMsgID,
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageType(msgType),
		Content:           content,
		Status:            models.MessageStatusReceived,
	}
	if len(flowResponseData) > 0 {
		message.FlowResponse = models.JSONB(flowResponseData)
	}

	// Add media fields if present
	if mediaInfo != nil {
		message.MediaURL = mediaInfo.MediaURL
		message.MediaMimeType = mediaInfo.MediaMimeType
		message.MediaFilename = mediaInfo.MediaFilename
	}

	now := time.Now().UTC()
	preview := content
	if len(preview) > 100 {
		preview = preview[:97] + "..."
	}
	if msgType != "text" && msgType != "button_reply" && msgType != "nfm_reply" {
		preview = "[" + msgType + "]"
	}

	idempotencyKey := "message-incoming:" +
		uuid.NewSHA1(account.ID, []byte(strings.TrimSpace(whatsappMsgID))).String()
	var canonicalContact models.Contact
	transactionErr := canonicalContactWriteTransaction(a.DB, func(tx *gorm.DB) error {
		message.Status = models.MessageStatusReceived
		message.IsReply = false
		message.ReplyToMessageID = nil

		canonical, err := contactutil.ResolveCanonicalContactForUpdate(
			tx,
			account.OrganizationID,
			contact.ID,
		)
		if err != nil {
			return err
		}
		canonicalContact = *canonical
		message.ContactID = canonical.ID

		var existingActivity models.CustomerActivityEvent
		existingErr := tx.Where(
			"organization_id = ? AND idempotency_key = ?",
			account.OrganizationID,
			idempotencyKey,
		).First(&existingActivity).Error
		if existingErr == nil {
			existingCanonical, resolveExistingErr := contactutil.ResolveCanonicalContact(
				tx,
				account.OrganizationID,
				existingActivity.ContactID,
			)
			if resolveExistingErr != nil {
				return resolveExistingErr
			}
			if existingCanonical.ID != canonical.ID ||
				existingActivity.EventType != models.CustomerActivityMessageIncoming ||
				existingActivity.SourceObjectType != "message" {
				return errors.New("incoming WhatsApp message ID was reused for a different contact or event")
			}
			return errIncomingMessageAlreadyProcessed
		}
		if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return existingErr
		}

		// Also suppress a legacy replay whose message predates the lifecycle
		// event stream. New concurrent deliveries are still protected by the
		// globally stable activity idempotency key below.
		var existingMessage models.Message
		existingMessageErr := tx.Where(
			"organization_id = ? AND whats_app_account = ? AND whats_app_message_id = ?",
			account.OrganizationID,
			account.Name,
			whatsappMsgID,
		).First(&existingMessage).Error
		if existingMessageErr == nil {
			existingCanonical, resolveExistingErr := contactutil.ResolveCanonicalContact(
				tx,
				account.OrganizationID,
				existingMessage.ContactID,
			)
			if resolveExistingErr != nil {
				return resolveExistingErr
			}
			if existingCanonical.ID != canonical.ID {
				return errors.New("incoming WhatsApp message ID was reused for a different contact")
			}
			return errIncomingMessageAlreadyProcessed
		}
		if !errors.Is(existingMessageErr, gorm.ErrRecordNotFound) {
			return existingMessageErr
		}

		// Handle reply context within the same tenant and transaction.
		if replyToWAMID != "" {
			var replyToMsg models.Message
			replyErr := tx.Where(
				"organization_id = ? AND whats_app_message_id = ?",
				account.OrganizationID,
				replyToWAMID,
			).First(&replyToMsg).Error
			if replyErr == nil {
				message.IsReply = true
				message.ReplyToMessageID = &replyToMsg.ID
			} else if !errors.Is(replyErr, gorm.ErrRecordNotFound) {
				return replyErr
			}
		}

		// Pre-mark bot-handled messages as read so the unread badge does not
		// flash before the bot reply. Transfer state is checked transactionally
		// against the canonical contact.
		settings, settingsErr := a.getChatbotSettingsCached(
			account.OrganizationID,
			account.Name,
		)
		if settingsErr == nil && settings.IsEnabled {
			var activeTransfers int64
			if err := tx.Model(&models.AgentTransfer{}).
				Where(
					"organization_id = ? AND contact_id = ? AND status = ?",
					account.OrganizationID,
					canonical.ID,
					models.TransferStatusActive,
				).
				Count(&activeTransfers).Error; err != nil {
				return err
			}
			if activeTransfers == 0 {
				message.Status = models.MessageStatusRead
			}
		}

		if err := tx.Create(&message).Error; err != nil {
			return err
		}
		if err := tx.Model(canonical).Updates(map[string]any{
			"last_message_at":      now,
			"last_message_preview": preview,
			"is_read":              false,
			"whats_app_account":    account.Name,
			"last_inbound_at":      now,
		}).Error; err != nil {
			return err
		}

		_, err = recordCustomerActivity(
			tx,
			account.OrganizationID,
			customerActivityInput{
				ContactID:        canonical.ID,
				EventType:        models.CustomerActivityMessageIncoming,
				Category:         models.CustomerActivityCategoryMessage,
				Title:            "Message received",
				Summary:          preview,
				ActorType:        models.CustomerActivityActorContact,
				SourceObjectType: "message",
				SourceObjectID:   &message.ID,
				OccurredAt:       now,
				Metadata: models.JSONB{
					"message_type":     msgType,
					"message_status":   string(message.Status),
					"whatsapp_account": account.Name,
				},
				WebhookData: models.JSONB{
					"message_id":       message.ID.String(),
					"contact_phone":    canonical.PhoneNumber,
					"contact_name":     canonical.ProfileName,
					"message_type":     models.MessageType(msgType),
					"content":          content,
					"whatsapp_account": account.Name,
					"direction":        models.DirectionIncoming,
				},
				IdempotencyKey: idempotencyKey,
			},
		)
		return err
	})
	if transactionErr != nil {
		return nil, transactionErr
	}

	canonicalContact.LastMessageAt = &now
	canonicalContact.LastMessagePreview = preview
	canonicalContact.IsRead = false
	canonicalContact.WhatsAppAccount = account.Name
	canonicalContact.LastInboundAt = &now
	*contact = canonicalContact
	a.mirrorLegacyWhatsAppMessage(account, message.ID)

	return &message, nil
}

// isWithinBusinessHours checks if current time is within configured business hours
func (a *App) isWithinBusinessHours(businessHours models.JSONBArray) bool {
	now := time.Now()
	currentDay := int(now.Weekday()) // 0 = Sunday, 1 = Monday, etc.
	currentTime := now.Format("15:04")

	for _, bh := range businessHours {
		bhMap, ok := bh.(map[string]any)
		if !ok {
			continue
		}

		// Get day (0-6, Sunday-Saturday)
		day, ok := bhMap["day"].(float64)
		if !ok {
			continue
		}

		if int(day) != currentDay {
			continue
		}

		// Check if enabled for this day
		enabled, ok := bhMap["enabled"].(bool)
		if !ok || !enabled {
			return false // Day exists but is disabled
		}

		// Get start and end times
		startTime, ok := bhMap["start_time"].(string)
		if !ok {
			continue
		}
		endTime, ok := bhMap["end_time"].(string)
		if !ok {
			continue
		}

		// Compare times (simple string comparison works for HH:MM format)
		if currentTime >= startTime && currentTime <= endTime {
			return true
		}
		return false // Found the day but outside hours
	}

	// If no matching day found, assume outside business hours
	return false
}
