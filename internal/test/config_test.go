package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

func TestConfig_DefaultValues(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	safermHome := filepath.Join(homeDir, ".saferm")

	stdout, stderr, code := runSaferm(t, homeDir, "config", "show", "--plain")
	if code != 0 {
		t.Fatalf("config show --plain failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify default values and source attribution.
	// InfraRoot-resolved defaults show as (source: infra) with the resolved path;
	// config show uses a separate resolution path, so it displays the marker.
	// Check that the infra root is shown and env-patterns remain as default.
	checks := []struct {
		label    string
		contains string
	}{
		{"SAFERM_HOME root displayed", "SAFERM_HOME (root) = " + safermHome},
		{"exclude_env_patterns default", `exclude_env_patterns = ["(?i)token", "(?i)secret", "(?i)password", "(?i)key", "(?i)credential"]  (source: default)`},
	}
	for _, check := range checks {
		if !strings.Contains(stdout, check.contains) {
			t.Errorf("%s: expected output to contain %q, got:\n%s", check.label, check.contains, stdout)
		}
	}
}

func TestConfig_TOMLOverride(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	safermHome := filepath.Join(homeDir, ".saferm")

	// Write a config.toml with custom values.
	configContent := `archive_dir = "/custom/archive"
db_path = "/custom/db.sqlite"
`
	configPath := filepath.Join(safermHome, "config.toml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("writing config.toml: %v", err)
	}

	stdout, stderr, code := runSaferm(t, homeDir, "config", "show", "--plain")
	if code != 0 {
		t.Fatalf("config show --plain failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify overridden values show source: config.
	checks := []struct {
		label    string
		contains string
	}{
		{"archive_dir from config", "archive_dir = /custom/archive  (source: config)"},
		{"db_path from config", "db_path = /custom/db.sqlite  (source: config)"},
		// exclude_env_patterns should still show default since not overridden.
		{"exclude_env_patterns still default", "exclude_env_patterns ="},
	}
	for _, check := range checks {
		if !strings.Contains(stdout, check.contains) {
			t.Errorf("%s: expected output to contain %q, got:\n%s", check.label, check.contains, stdout)
		}
	}

	// Verify exclude_env_patterns is still from default.
	if !strings.Contains(stdout, "exclude_env_patterns") || !strings.Contains(stdout, "(source: default)") {
		t.Errorf("exclude_env_patterns should still be from default, got:\n%s", stdout)
	}
}

func TestConfig_CLIOverride(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)

	// Run delete with CLI flag overrides and -f (force, to skip nonexistent file error).
	// This confirms CLI flags are accepted without error.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort",
		"--archive-dir", "/tmp/saferm-test-cli-override-archive",
		"--db-path", "/tmp/saferm-test-cli-override-db",
		"--description", "testing CLI override",
		"-f",
		"nonexistent-file-for-cli-override-test",
	)
	if code != 0 {
		t.Fatalf("delete with CLI overrides failed (exit %d): stderr=%q", code, stderr)
	}
}

func TestConfig_SAFERM_HOME(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	safermHome := filepath.Join(homeDir, ".saferm")

	stdout, stderr, code := runSaferm(t, homeDir, "config", "path")
	if code != 0 {
		t.Fatalf("config path failed (exit %d): stderr=%q", code, stderr)
	}

	expected := filepath.Join(safermHome, "config.toml")
	got := strings.TrimSpace(stdout)
	if got != expected {
		t.Fatalf("config path: got %q, want %q", got, expected)
	}
}
