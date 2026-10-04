package graphstub

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// releaseTargets are the only packages the release images build
// (docker/release/*.Dockerfile). TestReleaseDockerfilesBuildOnlyTheseTargets
// keeps this list honest.
var releaseTargets = []string{"./cmd/gmail-relay", "./cmd/meta-relay", "./cmd/whatomate"}

func goTool(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("the go command is not on PATH; these placement tests must not be skipped")
	}
	return path
}

// goList runs the go command for the release build platform, so build
// constraints resolve as they do in docker/release/*.Dockerfile.
func goList(t *testing.T, dir string, args ...string) []string {
	t.Helper()
	command := exec.Command(goTool(t), args...)
	command.Dir = dir
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

func moduleRoot(t *testing.T) (dir, module string) {
	t.Helper()
	fields := strings.SplitN(goList(t, ".", "list", "-m", "-f", "{{.Dir}}|{{.Path}}")[0], "|", 2)
	if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
		t.Fatalf("cannot resolve the main module: %v", fields)
	}
	return fields[0], fields[1]
}

// TestReleaseBinariesDoNotDependOnReleaseTree is the placement test from the
// Part A plan: nothing under release/ (this stub, the staging bootstrap tool
// or anything added later) may be compiled into a release binary.
func TestReleaseBinariesDoNotDependOnReleaseTree(t *testing.T) {
	root, module := moduleRoot(t)
	args := append([]string{"list", "-deps", "-tags", "timetzdata", "-f", "{{.ImportPath}}"}, releaseTargets...)
	dependencies := goList(t, root, args...)
	seen := map[string]bool{}
	for _, dependency := range dependencies {
		seen[dependency] = true
		if dependency == module+"/release" || strings.HasPrefix(dependency, module+"/release/") ||
			strings.Contains(dependency, "graphstub") || strings.Contains(dependency, "graph-stub") {
			t.Errorf("a release binary depends on %s", dependency)
		}
	}
	// Guard against a vacuous pass: the listing must contain the binaries
	// and the product packages they are known to import.
	for _, required := range []string{module + "/cmd/whatomate", module + "/cmd/meta-relay", module + "/cmd/gmail-relay",
		module + "/internal/handlers", module + "/pkg/whatsapp", module + "/internal/metarelay"} {
		if !seen[required] {
			t.Errorf("go list -deps did not list %s", required)
		}
	}
}

// TestGraphStubUsesTheStandardLibraryOnly lists every non-standard package
// the stub and its command import (tests excluded): only the stub itself.
func TestGraphStubUsesTheStandardLibraryOnly(t *testing.T) {
	root, module := moduleRoot(t)
	own := module + "/release/staging/graphstub"
	packages := goList(t, root, "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./release/staging/graphstub/...")
	if len(packages) < 2 {
		t.Fatalf("go list found too few stub packages: %v", packages)
	}
	for _, path := range packages {
		if path != own && !strings.HasPrefix(path, own+"/") {
			t.Errorf("the graph stub depends on %s", path)
		}
	}
}

func TestReleaseDockerfilesBuildOnlyTheseTargets(t *testing.T) {
	root, _ := moduleRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "docker", "release", "*.Dockerfile"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no release Dockerfiles: %v", err)
	}
	target := regexp.MustCompile(`^\./[A-Za-z0-9_./-]+$`)
	found := map[string]bool{}
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.ReplaceAll(strings.ReplaceAll(string(content), "\r\n", "\n"), "\\\n", " ")
		for _, line := range strings.Split(joined, "\n") {
			if !strings.Contains(line, "go build") {
				continue
			}
			for _, field := range strings.Fields(line) {
				if target.MatchString(field) {
					found[field] = true
				}
			}
		}
	}
	var got []string
	for path := range found {
		got = append(got, path)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(releaseTargets, ",") {
		t.Fatalf("release Dockerfiles build %v; update releaseTargets and re-check placement", got)
	}
}

func TestGraphStubDockerfile(t *testing.T) {
	root, _ := moduleRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "docker", "staging", "graph-stub.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := strings.ReplaceAll(string(content), "\r\n", "\n")
	froms := regexp.MustCompile(`(?m)^FROM\s+(.*)$`).FindAllStringSubmatch(dockerfile, -1)
	if len(froms) != 2 || strings.TrimSpace(froms[1][1]) != "scratch" {
		t.Fatalf("want a builder stage and a final scratch stage, got %v", froms)
	}
	if !regexp.MustCompile(`@sha256:[0-9a-f]{64}\s+AS builder$`).MatchString(froms[0][1]) {
		t.Fatalf("the builder base is not pinned by digest: %s", froms[0][1])
	}
	for _, required := range []string{"USER 65532:65532", "./release/staging/graphstub/cmd/graph-stub", "COPY release/staging/graphstub/ "} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("graph-stub.Dockerfile lacks %q", required)
		}
	}
	if regexp.MustCompile(`(?m)^COPY \. `).MatchString(dockerfile) {
		t.Error("graph-stub.Dockerfile copies the whole build context")
	}
	ignore, err := os.ReadFile(filepath.Join(root, "docker", "staging", "graph-stub.Dockerfile.dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(ignore))
	if len(lines) == 0 || lines[0] != "*" || !strings.Contains(string(ignore), "!release/staging/graphstub/") {
		t.Fatalf("graph-stub.Dockerfile.dockerignore must exclude everything but the stub: %q", ignore)
	}
}
