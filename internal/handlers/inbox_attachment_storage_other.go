//go:build !linux

package handlers

import "context"

// The local lane requires an anchored nofollow directory walk and
// nonblocking leaf open. Keep it unavailable without that implementation.
const inboxAttachmentLocalSupported = false

func readInboxAttachmentLocal(context.Context, string, []string, int64) ([]byte, error) {
	return nil, errInvalidStoredMediaKey
}
