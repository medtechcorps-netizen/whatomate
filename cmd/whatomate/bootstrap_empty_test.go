package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
)

// No test in this file selects a connection-producing command path. Keep real
// config parsing while isolating ambient WHATOMATE overrides; no loader mock.
func bootstrapCommandTestEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(key), "WHATOMATE_") {
			t.Setenv(key, value)
			if err := os.Unsetenv(key); err != nil {
				t.Fatal("could not isolate bootstrap configuration environment")
			}
		}
	}
}

func TestRLSBootstrapEmptyArgumentGrammar(t *testing.T) {
	for _, tc := range []struct {
		name, config, shape string
		args                []string
	}{
		{"defaults", "config.toml", "", nil},
		{"config", "synthetic.toml", "", []string{"-config", "synthetic.toml"}},
		{"inline", "synthetic.toml", "v0", []string{"-config=synthetic.toml", "-production-shape=v0"}},
		{"shape-first", "synthetic.toml", "v0", []string{"-production-shape", "v0", "-config", "synthetic.toml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, shape, err := parseRLSBootstrapEmptyArgs(tc.args)
			if err != nil || path != tc.config || shape != tc.shape {
				t.Fatal("closed bootstrap arguments did not preserve expected values")
			}
		})
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown", []string{"-force"}},
		{"positional", []string{"synthetic.toml"}},
		{"missing-config", []string{"-config"}},
		{"missing-shape", []string{"-production-shape"}},
		{"empty-config", []string{"-config="}},
		{"blank-config", []string{"-config", "   "}},
		{"empty-shape", []string{"-production-shape="}},
		{"unsupported-shape", []string{"-production-shape", "v1"}},
		{"case-shape", []string{"-production-shape", "V0"}},
		{"duplicate-config", []string{"-config=a", "-config=b"}},
		{"duplicate-shape", []string{"-production-shape=v0", "-production-shape=v0"}},
		{"double-dash", []string{"--config=a"}},
		{"separator", []string{"--"}},
		{"flag-as-value", []string{"-config", "-production-shape"}},
		{"trailing-positional", []string{"-production-shape=v0", "extra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := parseRLSBootstrapEmptyArgs(tc.args); err == nil {
				t.Fatal("unsupported bootstrap arguments accepted")
			}
		})
	}
}

func TestRLSBootstrapEmptyArgumentsPrecedeConfiguration(t *testing.T) {
	bootstrapCommandTestEnvironment(t)
	missing := filepath.Join(t.TempDir(), "must-not-load-private.toml")
	err := rlsBootstrapEmptyCommand(context.Background(), []string{"-config", missing, "-production-shape", "v1"})
	if err == nil || err.Error() != "invalid bootstrap arguments" {
		t.Fatal("invalid shape did not refuse before loading configuration")
	}
	err = rlsBootstrapEmptyCommand(nil, []string{"-config", missing})
	if err == nil || err.Error() != "bootstrap context is required" {
		t.Fatal("nil context did not refuse before configuration")
	}
}

func TestRLSBootstrapEmptyConfigurationRefusesBeforeDatabase(t *testing.T) {
	bootstrapCommandTestEnvironment(t)
	const runtimeURL = "postgres://runtime:synthetic-private@127.0.0.1:1/never_connect"
	const ownerURL = "postgres://owner:synthetic-private@127.0.0.1:1/never_connect"
	const configRefusal = "bootstrap requires staging/test and distinct explicit database URLs"
	for _, tc := range []struct {
		name, environment, runtime, owner, role, want string
	}{
		{"production", "production", runtimeURL, ownerURL, "runtime", configRefusal},
		{"development", "development", runtimeURL, ownerURL, "runtime", configRefusal},
		{"missing-runtime-url", "test", "", ownerURL, "runtime", configRefusal},
		{"missing-owner-url", "test", runtimeURL, "", "runtime", configRefusal},
		{"blank-runtime-url", "test", "   ", ownerURL, "runtime", configRefusal},
		{"same-url", "staging", runtimeURL, runtimeURL, "runtime", configRefusal},
		{"space-role", "test", runtimeURL, ownerURL, "bad role", "invalid bootstrap runtime role"},
		{"hyphen-role", "test", runtimeURL, ownerURL, "bad-role", "invalid bootstrap runtime role"},
		{"digit-role", "test", runtimeURL, ownerURL, "1runtime", "invalid bootstrap runtime role"},
		{"unicode-role", "test", runtimeURL, ownerURL, "runtime\u00e9", "invalid bootstrap runtime role"},
		{"long-role", "test", runtimeURL, ownerURL, strings.Repeat("r", 64), "invalid bootstrap runtime role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "synthetic.toml")
			raw := fmt.Sprintf("[app]\nenvironment = %q\n[database]\nurl = %q\nmigration_url = %q\nruntime_role = %q\n", tc.environment, tc.runtime, tc.owner, tc.role)
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal("could not write owned synthetic configuration")
			}
			loaded, err := config.Load(path)
			if err != nil || loaded == nil || loaded.App.Environment != tc.environment ||
				loaded.Database.URL != tc.runtime || loaded.Database.MigrationURL != tc.owner || loaded.Database.RuntimeRole != tc.role {
				t.Fatal("test configuration did not reach the intended command guard")
			}
			err = rlsBootstrapEmptyCommand(context.Background(), []string{"-config", path})
			if err == nil || err.Error() != tc.want {
				t.Fatal("configuration did not produce its exact preconnection refusal")
			}
		})
	}
}

func TestRLSBootstrapEmptyConfigurationErrorsAreSanitized(t *testing.T) {
	bootstrapCommandTestEnvironment(t)
	for _, tc := range []struct {
		name, content string
		create        bool
	}{
		{"missing", "", false},
		{"invalid-toml", "[private-password\n", true},
		{"invalid-environment", "[app]\nenvironment = \"private-invalid-environment\"\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private-config-name.toml")
			if tc.create {
				if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
					t.Fatal("could not write owned invalid configuration")
				}
			}
			err := rlsBootstrapEmptyCommand(context.Background(), []string{"-config", path})
			if err == nil || err.Error() != "bootstrap configuration unavailable" {
				t.Fatal("configuration failure was not sanitized")
			}
		})
	}
}
