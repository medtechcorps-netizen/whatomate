//go:build !linux

package mediaretirementcmd

import "github.com/shridarpatil/whatomate/internal/mediaretirement"

func protectMediaOwnerProcess() error {
	return mediaretirement.ErrIncomplete
}
