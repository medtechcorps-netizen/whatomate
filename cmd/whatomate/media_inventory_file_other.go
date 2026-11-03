//go:build !linux

package main

import "github.com/shridarpatil/whatomate/internal/mediaretirement"

// This operational command requires the Linux application's private file and
// atomic no-replace contract; unsupported hosts never receive inventory bytes.
func openMediaInventoryOutput(string) (mediaInventoryOutput, error) {
	return nil, mediaretirement.ErrIncomplete
}
