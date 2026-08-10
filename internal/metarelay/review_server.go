package metarelay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (s *Server) handleReviewMetaWebhook(
	w http.ResponseWriter,
	request *http.Request,
	webhookApp WebhookApp,
	raw []byte,
) {
	if webhookApp != WebhookAppMessenger {
		writeError(w, http.StatusNotFound, "unknown_webhook")
		return
	}
	if err := validateReviewWebhookTarget(raw, s.config.ReviewPageID); err != nil {
		switch {
		case errors.Is(err, ErrUnknownAccount):
			writeError(w, http.StatusNotFound, "unknown_account")
		case errors.Is(err, ErrUnsupportedObject):
			writeError(w, http.StatusUnprocessableEntity, "unsupported_object")
		default:
			writeError(w, http.StatusBadRequest, "invalid_payload")
		}
		return
	}

	// Meta expects acknowledgement within five seconds. The dynamic broker and
	// durable Redis acceptance share one four-second budget.
	ctx, cancel := context.WithTimeout(request.Context(), 4*time.Second)
	defer cancel()
	binding, err := s.reviewResolver.ResolveReviewBinding(ctx)
	if err != nil {
		status, code := reviewBindingErrorStatus(err)
		writeError(w, status, code)
		return
	}
	if err := validateReviewBinding(s.config, binding, s.now().UTC()); err != nil {
		writeError(w, http.StatusNotFound, "review_binding_rejected")
		return
	}
	account, err := binding.account()
	if err != nil {
		writeError(w, http.StatusNotFound, "review_binding_rejected")
		return
	}
	reviewConfig := &Config{
		byExternal: map[string]*AccountConfig{
			accountIndexKey(account.Channel, account.ExternalAccountID): account,
		},
		byKey: map[string]*AccountConfig{account.Key: account},
	}
	jobs, err := NormalizeInbound(reviewConfig, WebhookAppMessenger, raw)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknownAccount), errors.Is(err, ErrWebhookAppMismatch):
			writeError(w, http.StatusNotFound, "unknown_account")
		case errors.Is(err, ErrUnsupportedObject):
			writeError(w, http.StatusUnprocessableEntity, "unsupported_object")
		case errors.Is(err, ErrCanonicalJobTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "canonical_payload_too_large")
		default:
			writeError(w, http.StatusBadRequest, "invalid_payload")
		}
		return
	}
	for index := range jobs {
		jobs[index].ReviewGeneration = binding.Tuple.Generation
		jobs[index].ReviewCredentialID = binding.CredentialID
		jobs[index].ReviewCredentialVersion = binding.CredentialVersion
	}
	if _, err := s.store.AcceptInbound(ctx, digestHex(raw), jobs); err != nil {
		s.logger.Error("Meta review delivery was not durably accepted", "component", "inbound_accept")
		writeError(w, http.StatusServiceUnavailable, "durable_acceptance_failed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("EVENT_RECEIVED"))
}

func validateReviewWebhookTarget(raw []byte, expectedPageID string) error {
	var payload metaWebhook
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ErrInvalidMetaPayload
	}
	if strings.ToLower(strings.TrimSpace(payload.Object)) != "page" {
		return ErrUnsupportedObject
	}
	if len(payload.Entry) == 0 {
		return ErrInvalidMetaPayload
	}
	for _, entry := range payload.Entry {
		if strings.TrimSpace(entry.ID) != expectedPageID {
			return ErrUnknownAccount
		}
	}
	return nil
}
