//go:build !linux

package mediaretirementcmd

import (
	"context"
	"github.com/shridarpatil/whatomate/internal/mediaretirement"
	"os"
)

func readMediaOwnerInput(context.Context, *os.File) ([]byte, error) {
	return nil, mediaretirement.ErrIncomplete
}
