package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// releaseTargets are the only packages the release images build
// (docker/release/*.Dockerfile; release/staging/graphstub/placement_test.go
// keeps that list honest).
var releaseTargets = []string{"./cmd/gmail-relay", "./cmd/meta-relay", "./cmd/whatomate"}

// goList runs the go command for the release build platform, so build
// constraints resolve as they do in docker/release/*.Dockerfile.
func goList(t *testing.T, args ...string) []string {
	t.Helper()
	tool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("the go command is not on PATH; these placement tests must not be skipped")
	}
	command := exec.Command(tool, args...)
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOFLAGS=-mod=readonly")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestReleaseBinariesDoNotContainTheBootstrapTool is the Part A placement
// test for this tool: no release binary compiles it in. The listing runs from
// the module root, so it sees the release targets exactly as the release
// Dockerfiles build them.
func TestReleaseBinariesDoNotContainTheBootstrapTool(t *testing.T) {
	self := goList(t, "list", "-f", "{{.ImportPath}} {{.Name}} {{.Module.Dir}}", ".")
	if len(self) != 1 {
		t.Fatalf("go list . returned %v", self)
	}
	fields := strings.SplitN(self[0], " ", 3)
	if len(fields) != 3 || !strings.HasSuffix(fields[0], "/release/staging/bootstrap") || fields[1] != "main" {
		t.Fatalf("this package is not the release/staging/bootstrap command: %v", fields)
	}
	tool, root := fields[0], fields[2]
	module := strings.TrimSuffix(tool, "/release/staging/bootstrap")

	args := append([]string{"-C", root, "list", "-deps", "-tags", "timetzdata", "-f", "{{.ImportPath}}"}, releaseTargets...)
	dependencies := goList(t, args...)
	seen := map[string]bool{}
	for _, dependency := range dependencies {
		seen[dependency] = true
		if dependency == tool || strings.HasPrefix(dependency, tool+"/") ||
			dependency == module+"/release" || strings.HasPrefix(dependency, module+"/release/") {
			t.Errorf("a release binary depends on %s", dependency)
		}
	}
	// Not vacuous: the listing holds the binaries and the packages the tool
	// itself drives, which the release binaries share with it.
	for _, required := range []string{module + "/cmd/whatomate", module + "/cmd/meta-relay", module + "/cmd/gmail-relay",
		module + "/internal/database", module + "/internal/handlers", module + "/internal/channel"} {
		if !seen[required] {
			t.Errorf("go list -deps did not list %s", required)
		}
	}
}
