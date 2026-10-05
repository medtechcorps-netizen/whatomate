package main

import (
	"bytes"
	"go/ast"
	"go/token"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBootstrapToolStaysOutOfReleaseBinaries pins the two facts that keep
// this tool out of every release binary. It does not repeat the generic
// placement check:
//
//   - the tool is a main package, which Go refuses to import;
//   - its import path is under the module's release/ tree, and
//     release/staging/graphstub's TestReleaseBinariesDoNotDependOnReleaseTree
//     rejects every package under release/ in the go list -deps output of
//     each release binary (the targets docker/release/*.Dockerfile builds).
//
// It also requires that generic test to exist and to reject release/, so
// removing it cannot leave this tool unchecked.
func TestBootstrapToolStaysOutOfReleaseBinaries(t *testing.T) {
	tool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("the go command is not on PATH; this placement test must not be skipped")
	}
	command := exec.Command(tool, "list", "-f", "{{.ImportPath}} {{.Name}} {{.Module.Path}}", ".")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	require.NoError(t, err, stderr.String())
	fields := strings.Fields(string(output))
	require.Len(t, fields, 3, "go list . returned %q", output)
	importPath, name, module := fields[0], fields[1], fields[2]
	require.Equal(t, module+"/release/staging/bootstrap", importPath)
	require.Equal(t, "main", name)

	generic := parseGoFile(t, filepath.Join("..", "graphstub", "placement_test.go"))
	rejectsReleaseTree := false
	ast.Inspect(generic.function(t, "TestReleaseBinariesDoNotDependOnReleaseTree").Body, func(node ast.Node) bool {
		if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING && literal.Value == `"/release/"` {
			rejectsReleaseTree = true
		}
		return true
	})
	require.True(t, rejectsReleaseTree,
		"TestReleaseBinariesDoNotDependOnReleaseTree no longer rejects release/; this tool's placement is unchecked")
}
