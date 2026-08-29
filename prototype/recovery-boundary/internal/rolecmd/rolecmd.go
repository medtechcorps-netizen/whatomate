// Package rolecmd exposes only an inert Gate-A role descriptor.
//
// The protocol behavior lives in the model and protocol packages. Gate A does
// not start a listener or contact any external service; a later Gate-C review
// must authorize and prove the substrate before runtime wiring can exist.
package rolecmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const DescribeArgument = "--describe"

var ErrInert = errors.New("gate-a prototype is inert")

type Descriptor struct {
	PrototypeOnly bool   `json:"prototype_only"`
	ClaimScope    string `json:"claim_scope"`
	Role          string `json:"role"`
	Network       string `json:"network"`
	LiveRuntime   bool   `json:"live_runtime"`
}

var roles = map[string]string{
	"writer-authority":   "none",
	"writer-broker":      "synthetic-source-only",
	"observer-authority": "none",
	"observer-broker":    "synthetic-recovery-only",
}

// Run accepts exactly one non-mutating descriptor action. Unknown or absent
// arguments fail closed, so these images cannot accidentally become a server
// or generic command runner.
func Run(role string, args []string, stdout, stderr io.Writer) error {
	network, ok := roles[role]
	if !ok || stdout == nil || stderr == nil {
		return ErrInert
	}
	if len(args) != 1 || args[0] != DescribeArgument {
		_, _ = fmt.Fprintln(stderr, "gate-a: inert prototype; only --describe is available")
		return ErrInert
	}
	descriptor := Descriptor{
		PrototypeOnly: true,
		ClaimScope:    "gate-c-nonproduction-only",
		Role:          role,
		Network:       network,
		LiveRuntime:   false,
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(true)
	return encoder.Encode(descriptor)
}
