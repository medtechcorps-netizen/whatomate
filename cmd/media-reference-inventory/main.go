package main

import (
	"os"

	"github.com/shridarpatil/whatomate/internal/mediaretirementcmd"
)

func main() {
	os.Exit(mediaretirementcmd.Run(os.Args[1:]))
}
