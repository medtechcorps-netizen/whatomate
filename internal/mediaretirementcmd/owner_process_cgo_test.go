//go:build linux && cgo

package mediaretirementcmd

// Ensure this test really links cgo, including builds using netgo/osusergo.
import _ "runtime/cgo"

const mediaOwnerTestCGO = true
