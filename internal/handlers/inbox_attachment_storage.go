package handlers

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/storage"
)

const inboxForwardedLocalDirectory = "inbound-forwarded"

func inboxForwardedLocalMediaDir(orgID, messageID uuid.UUID) string {
	return path.Join(inboxForwardedLocalDirectory, orgID.String(), messageID.String())
}

// The caller establishes the message and tenant scope before reading. A local
// Coexistence key additionally needs an exact revision-derived proof from that
// row because its historical disk directory does not contain the tenant ID.
func (a *App) readInboxAttachment(ctx context.Context, orgID uuid.UUID, key string, maxBytes int64, provedLegacyCoexKey string) ([]byte, string, error) {
	if a == nil || ctx == nil || maxBytes <= 0 || maxBytes == 1<<63-1 {
		return nil, "", errors.New("invalid inbox attachment read request")
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	object, parts := inboxAttachmentStorageKey(orgID, key, provedLegacyCoexKey)
	if object {
		bounded, ok := a.ObjectStore.(storage.BoundedObjectReader)
		if !ok {
			return nil, "", errors.New("inbox attachment storage does not support bounded reads")
		}
		body, contentType, err := bounded.GetLimited(ctx, key, maxBytes)
		if err != nil {
			return nil, "", err
		}
		if int64(len(body)) > maxBytes {
			return nil, "", storage.ErrObjectTooLarge
		}
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		return body, contentType, nil
	}

	if len(parts) == 0 || a.Config == nil {
		return nil, "", errInvalidStoredMediaKey
	}
	body, err := readInboxAttachmentLocal(ctx, a.getMediaStoragePath(), parts, maxBytes)
	return body, "", err
}

func isInboxAttachmentStorageKey(orgID uuid.UUID, key, provedLegacyCoexKey string) bool {
	object, parts := inboxAttachmentStorageKey(orgID, key, provedLegacyCoexKey)
	return object || len(parts) != 0
}

func inboxAttachmentStorageKey(orgID uuid.UUID, key, provedLegacyCoexKey string) (object bool, localParts []string) {
	if orgID == uuid.Nil || len(key) > 1024 || !fs.ValidPath(key) {
		return false, nil
	}
	for _, c := range key {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '/' || c == '.' || c == '-' || c == '_' {
			continue
		}
		return false, nil
	}

	if strings.HasPrefix(key, "organizations/"+orgID.String()+"/messages/") {
		return true, nil
	}
	if !inboxAttachmentLocalSupported {
		return false, nil
	}
	parts := strings.Split(key, "/")
	if len(parts) == 4 && parts[1] == orgID.String() && isInboxAttachmentUUID(parts[2]) {
		stem, suffix, hasSuffix := strings.Cut(parts[3], ".")
		if hasSuffix && isInboxAttachmentUUID(stem) &&
			((parts[0] == "native" && suffix == "bin") ||
				(parts[0] == inboxForwardedLocalDirectory && suffix != "" && !strings.Contains(suffix, "."))) {
			return false, parts
		}
	}
	if len(parts) == 2 && parts[0] == "coexistence" && key == provedLegacyCoexKey {
		stem, ok := strings.CutSuffix(parts[1], ".bin")
		const uuidLen = 36
		if ok && len(stem) == 2*uuidLen+1 && stem[uuidLen] == '-' &&
			isInboxAttachmentUUID(stem[:uuidLen]) && isInboxAttachmentUUID(stem[uuidLen+1:]) {
			return false, parts
		}
	}
	// Older images/documents/audio/video paths are global and have no
	// revision-derived ownership proof at this boundary.
	return false, nil
}

func isInboxAttachmentUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}
