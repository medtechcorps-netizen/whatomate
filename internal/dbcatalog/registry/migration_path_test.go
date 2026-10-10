package registry

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This is conservative source protection, not path-sensitive proof of the
// future-profile branch. It includes all CLI branches, reached package init,
// callbacks, and the explicit reached-model GORM method contract. A baseline
// data step can also belong to this protected path: its baseline designation
// does not waive migration-path protection before a compat-backed lift.
const migrationPathFileCap = 228

var migrationPathRoots = []string{
	modulePath + "/cmd/whatomate.requiredStartupDatabaseContract",
	modulePath + "/cmd/whatomate.runRLSMigration",
	modulePath + "/cmd/whatomate.validateServerMigrationMode",
	modulePath + "/cmd/whatomate.verifyRLSMigrationRuntime",
	modulePath + "/cmd/whatomate.verifyStartupDatabaseContract",
	databasePath + "compiledRLSMigrationPhase",
}

type migrationPathContract struct {
	Version       int      `json:"version"`
	Roots         []string `json:"roots"`
	ClosureFiles  []string `json:"closure_files"`
	ClosureSHA256 string   `json:"closure_sha256"`
}

func TestMigrationPathClosureContract(t *testing.T) {
	result, err := registryProgram(t).closureWithGORMModels(modulePath+"/internal/models", migrationPathRoots...)
	if err != nil {
		t.Fatal(err)
	}
	// Regeneration cannot expand the separately reviewed literal cap.
	if err := requireClosureCap(result, migrationPathFileCap); err != nil {
		t.Fatal(err)
	}
	// These were concrete holes in a named-entrypoint-only guard. Keep a direct
	// regression that the real root reaches them, not only their source file.
	for _, name := range []string{"NewPostgres", "NewPostgresWithContext", "verifyLegacyAdditiveSchemaContract", "BackfillLastInboundAt"} {
		want := "internal/database/postgres.go:" + databasePath + name
		found := false
		for _, declaration := range result.Declarations {
			if declaration == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("migration path omits reached helper %s", name)
		}
	}
	want := migrationPathContract{1, migrationPathRoots, result.Files, result.SHA256}
	compareOrUpdate(t, "migration_path.json", "REREPLY_UPDATE_MIGRATION_PATH", want)
	var got migrationPathContract
	strictRead(t, "migration_path.json", &got)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("migration path contract differs from resolved source")
	}
	t.Logf("migration path: %d/%d files, %d declarations, sha256=%s", len(result.Files), migrationPathFileCap, len(result.Declarations), result.SHA256)
}

func TestMigrationPathClosureCapCannotExpand(t *testing.T) {
	t.Setenv("REREPLY_UPDATE_MIGRATION_PATH", "1")
	files := make([]string, migrationPathFileCap+1)
	for i := range files {
		files[i] = "synthetic.go"
	}
	if err := requireClosureCap(closureResult{Files: files}, migrationPathFileCap); err == nil {
		t.Fatal("update opt-in bypassed migration path cap")
	}
}

func TestMigrationPathClosureProtectsHelperAndData(t *testing.T) {
	const fixture = "package main\nconst compiledPhase = 3\nvar policy = 4\nfunc helper() int { return policy }\nfunc runRLSMigration() int { return helper()+compiledPhase }\nfunc unrelated() string { return \"unchanged\" }\n"
	for _, tc := range []struct {
		name, before, after string
		changed             bool
	}{
		{"transitive-helper", "return policy", "return policy+1", true},
		{"reached-package-variable", "policy = 4", "policy = 5", true},
		{"phase-constant", "compiledPhase = 3", "compiledPhase = 2", true},
		{"unrelated-same-file-body", "return \"unchanged\"", "return \"longer unrelated body\"", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(name, value string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("go.mod", "module migrationfixture.test\n\ngo 1.26.0\n")
			write("main.go", fixture)
			capture := func() closureResult {
				t.Helper()
				p, err := loadClosureProgram(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				result, err := p.closure("migrationfixture.test.runRLSMigration", "migrationfixture.test.compiledPhase")
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			before := capture()
			if strings.Count(fixture, tc.before) != 1 {
				t.Fatal("fixture mutation is ambiguous")
			}
			write("main.go", strings.Replace(fixture, tc.before, tc.after, 1))
			after := capture()
			if (before.SHA256 != after.SHA256) != tc.changed || !reflect.DeepEqual(before.Files, after.Files) {
				t.Fatal("migration closure changed the wrong declaration boundary")
			}
		})
	}
}
