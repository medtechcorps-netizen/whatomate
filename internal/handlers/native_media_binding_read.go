package handlers

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
)

const nativeMediaBindingKey = models.MessageNativeMediaRetryMetadataKey

type nativeMediaIdentity struct {
	Version        int
	OrganizationID uuid.UUID
	AccountID      uuid.UUID
	PhoneID        string
	MessageID      uuid.UUID
	ContactID      uuid.UUID
	WAMID          string
	MediaID        string
	SHA256         string
	MessageType    models.MessageType
	MIME           string
	Filename       string
	Content        string
	AdmissionID    uuid.UUID
	OriginJobID    uuid.UUID
	OriginKey      string
	Revision       string
}
type nativeMediaBinding struct {
	Identity  nativeMediaIdentity
	Phase     string
	Epoch     uuid.UUID
	Owner     string
	HandoffID uuid.UUID
	ObjectKey string
	// PublicationEpoch identifies the committed origin/handoff lineage. Recovery
	// advances only Epoch, which fences physical upload attempts.
	PublicationEpoch uuid.UUID
}

func nativeMediaDeclared(metadata models.JSONB) bool {
	_, ok := metadata[nativeMediaBindingKey]
	return ok
}

func nativeMediaContentRevision(message *models.Message, mediaID, mediaSHA string) string {
	encoded, _ := json.Marshal([]any{message.MessageType, mediaID, mediaSHA, message.MediaMimeType, message.MediaFilename, message.Content})
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func readNativeMediaBinding(message *models.Message) (*nativeMediaBinding, error) {
	if message == nil || !nativeMediaDeclared(message.Metadata) {
		return nil, errors.New("native media binding absent")
	}
	encoded, err := json.Marshal(message.Metadata[nativeMediaBindingKey])
	if err != nil {
		return nil, err
	}
	var binding nativeMediaBinding
	if err = json.Unmarshal(encoded, &binding); err != nil {
		return nil, err
	}
	id := binding.Identity
	if id.Version != 1 || id.OrganizationID == uuid.Nil || id.AccountID == uuid.Nil || id.PhoneID == "" || id.MessageID == uuid.Nil || id.ContactID == uuid.Nil || id.WAMID == "" || id.MediaID == "" || id.AdmissionID == uuid.Nil || id.OriginJobID == uuid.Nil || id.Revision == "" || id.OriginKey != "inbound-message-continuation:"+uuid.NewSHA1(id.AccountID, []byte(id.WAMID)).String() || id.MessageID != message.ID || id.OrganizationID != message.OrganizationID || id.WAMID != message.WhatsAppMessageID || message.Direction != models.DirectionIncoming {
		return nil, errors.New("native media binding identity invalid")
	}
	switch binding.Phase {
	case "admitted", "superseded", "revoked":
	case "inline", "recovery", "ready":
		if binding.Epoch == uuid.Nil || binding.PublicationEpoch == uuid.Nil {
			return nil, errors.New("native media active lineage is missing")
		}
	default:
		return nil, errors.New("native media binding phase invalid")
	}
	return &binding, nil
}

func nativeMediaCurrent(message *models.Message, binding *nativeMediaBinding) bool {
	if message.DeletedAt.Valid || message.Metadata[coexistenceMediaRevokedMetadataKey] == true || binding.Phase == "superseded" || binding.Phase == "revoked" {
		return false
	}
	id := binding.Identity
	if binding.Phase == "ready" && (binding.ObjectKey == "" || message.MediaURL != binding.ObjectKey) {
		return false
	}
	if nativeMediaContentRevision(message, id.MediaID, id.SHA256) != id.Revision {
		return false
	}
	if actual := coexistenceMediaMetadataString(message.Metadata, coexistenceMediaProviderIDMetadataKey); actual != "" && actual != id.MediaID {
		return false
	}
	if actual := coexistenceMediaMetadataString(message.Metadata, "coexistence_media_sha256"); actual != "" && actual != id.SHA256 {
		return false
	}
	return true
}
