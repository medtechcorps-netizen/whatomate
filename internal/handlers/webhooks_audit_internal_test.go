package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
)

func TestWebhookURLAuditChangeMasksSameOriginRoute(t *testing.T) {
	oldURL := "https://hooks.example.com/tenant/old-private-token"
	newURL := "https://hooks.example.com/tenant/new-private-token"
	oldSnap := webhookAuditSnapshot(&models.Webhook{URL: oldURL})
	newSnap := webhookAuditSnapshot(&models.Webhook{URL: newURL})

	changes := webhookURLAuditChange(oldURL, newURL, oldSnap, newSnap)
	if len(changes) != 1 {
		t.Fatalf("expected one masked URL change, got %d", len(changes))
	}
	if got := changes[0]["field"]; got != "url" {
		t.Fatalf("expected url field, got %v", got)
	}
	if got := changes[0]["old_value"]; got != "[redacted]" {
		t.Fatalf("expected redacted old value, got %v", got)
	}
	if got := changes[0]["new_value"]; got != "[changed]" {
		t.Fatalf("expected changed marker, got %v", got)
	}

	encoded, err := json.Marshal(changes)
	if err != nil {
		t.Fatalf("marshal changes: %v", err)
	}
	for _, sensitive := range []string{oldURL, newURL, "old-private-token", "new-private-token"} {
		if strings.Contains(string(encoded), sensitive) {
			t.Fatalf("audit change leaked sensitive URL material %q: %s", sensitive, encoded)
		}
	}
}

func TestWebhookURLAuditChangeDefersOriginChangesToSnapshot(t *testing.T) {
	oldURL := "https://old.example.com/private-route"
	newURL := "https://new.example.com/private-route"
	oldSnap := webhookAuditSnapshot(&models.Webhook{URL: oldURL})
	newSnap := webhookAuditSnapshot(&models.Webhook{URL: newURL})

	if changes := webhookURLAuditChange(oldURL, newURL, oldSnap, newSnap); len(changes) != 0 {
		t.Fatalf("expected origin change to be represented only by the snapshot, got %v", changes)
	}
}
