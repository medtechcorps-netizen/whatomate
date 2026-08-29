package main

import (
	"os"

	"github.com/shridarpatil/whatomate/prototype/recovery-boundary/internal/rolecmd"
)

func main() {
	if err := rolecmd.Run("writer-authority", os.Args[1:], os.Stdout, os.Stderr); err != nil {
		os.Exit(64)
	}
}
