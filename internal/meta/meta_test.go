package meta

import (
	"os"
	"strings"
	"testing"

	"github.com/stricttools/testisolation/go/hygiene"
)

// isolate binds testisolation's environment floor. It matters more here than
// anywhere else in saferm: this package's whole job is to capture the process
// environment into deletion metadata, so a test running with the developer's
// real credentials exported is a test that could put them somewhere.
func isolate(t *testing.T) {
	t.Helper()
	hygiene.Isolate(t)
}

func TestCollectEnv_FiltersPatterns(t *testing.T) {
	isolate(t)
	// Set known env vars that should be filtered.
	t.Setenv("SAFERM_TEST_SECRET_KEY", "should-be-filtered")
	t.Setenv("SAFERM_TEST_API_TOKEN", "should-be-filtered")
	t.Setenv("SAFERM_TEST_PASSWORD_DB", "should-be-filtered")
	t.Setenv("SAFERM_TEST_NORMAL_VAR", "should-be-kept")

	patterns := []string{
		"(?i)secret",
		"(?i)token",
		"(?i)password",
	}

	result, err := collectEnv(patterns)
	if err != nil {
		t.Fatalf("collectEnv errored on valid patterns: %v", err)
	}

	if _, ok := result["SAFERM_TEST_SECRET_KEY"]; ok {
		t.Error("SECRET_KEY should be filtered out")
	}
	if _, ok := result["SAFERM_TEST_API_TOKEN"]; ok {
		t.Error("API_TOKEN should be filtered out")
	}
	if _, ok := result["SAFERM_TEST_PASSWORD_DB"]; ok {
		t.Error("PASSWORD_DB should be filtered out")
	}
	if v, ok := result["SAFERM_TEST_NORMAL_VAR"]; !ok || v != "should-be-kept" {
		t.Errorf("NORMAL_VAR should be kept with value 'should-be-kept', got %q", v)
	}
}

func TestCollectEnv_NoPatterns(t *testing.T) {
	isolate(t)
	t.Setenv("SAFERM_TEST_ANYTHING", "captured")

	result, err := collectEnv(nil)
	if err != nil {
		t.Fatalf("collectEnv errored with no patterns: %v", err)
	}

	if v, ok := result["SAFERM_TEST_ANYTHING"]; !ok || v != "captured" {
		t.Errorf("with no patterns, all vars should be captured; SAFERM_TEST_ANYTHING = %q", v)
	}

	// Also check that PATH is present (always set in any environment).
	if _, ok := result["PATH"]; !ok {
		t.Error("PATH should be present when no patterns are used")
	}
}

// A pattern that does not compile is the caller asking for a redaction saferm
// cannot perform. It used to be dropped from the compiled set and everything
// continued, so a typo turned a redaction off in silence.
func TestCollectEnv_UncompilablePatternIsAnError(t *testing.T) {
	isolate(t)
	t.Setenv("SAFERM_TEST_API_KEY", "must-not-leak")

	badPatterns := []struct {
		name    string
		pattern string
	}{
		// RE2 has no lookahead; this exact pattern was in saferm's own docs.
		{"lookahead", "(?i)key(?!BOARD)"},
		{"unterminated class", "[unterminated"},
		{"dangling repeat", "*key"},
	}

	for _, bp := range badPatterns {
		t.Run(bp.name, func(t *testing.T) {
			result, err := collectEnv([]string{bp.pattern})
			if err == nil {
				t.Fatalf("collectEnv(%q) should error; it returned %d variables", bp.pattern, len(result))
			}
			if result != nil {
				t.Errorf("collectEnv(%q) should return no environment alongside its error", bp.pattern)
			}
			if !strings.Contains(err.Error(), bp.pattern) {
				t.Errorf("the error should name the offending pattern, got: %v", err)
			}
		})
	}
}

func TestCollect_UncompilablePatternIsAnError(t *testing.T) {
	isolate(t)

	m, err := Collect([]string{"(?i)key(?!BOARD)"}, nil)
	if err == nil {
		t.Fatalf("Collect should refuse an uncompilable exclude pattern; got metadata with %d env vars", len(m.Env))
	}
	if m != nil {
		t.Errorf("Collect should return no metadata alongside its error")
	}
}

func TestCollectGitContext(t *testing.T) {
	isolate(t)
	// saferm is itself a git repo, so this should return non-empty values
	// when run from the project directory.
	branch, head, root := collectGitContext()

	if branch == "" {
		t.Error("git branch should not be empty when running in a git repo")
	}
	if head == "" {
		t.Error("git HEAD should not be empty when running in a git repo")
	}
	if len(head) < 7 {
		t.Errorf("git HEAD should be a full SHA, got %q", head)
	}
	if root == "" {
		t.Error("git root should not be empty when running in a git repo")
	}
}

func TestCollectGitContext_NotARepo(t *testing.T) {
	isolate(t)
	// Run git commands from a directory that is not a git repo.
	dir := t.TempDir()
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	t.Cleanup(func() { os.Chdir(origDir) })

	branch, head, root := collectGitContext()

	if branch != "" {
		t.Errorf("branch should be empty outside a repo, got %q", branch)
	}
	if head != "" {
		t.Errorf("HEAD should be empty outside a repo, got %q", head)
	}
	if root != "" {
		t.Errorf("root should be empty outside a repo, got %q", root)
	}
}

func TestCollectParentProcess(t *testing.T) {
	isolate(t)
	ppid, cmdline := collectParentProcess()

	if ppid <= 0 {
		t.Errorf("PPID should be > 0, got %d", ppid)
	}
	if cmdline == "" {
		t.Error("parent cmdline should not be empty when running from go test")
	}
}

func TestCollect_Integration(t *testing.T) {
	isolate(t)
	defaultPatterns := []string{
		"(?i)token",
		"(?i)secret",
		"(?i)password",
		"(?i)key",
		"(?i)credential",
	}

	m, err := Collect(defaultPatterns, nil)
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}

	if m.Env == nil || len(m.Env) == 0 {
		t.Error("Env should not be empty")
	}
	// Verify default patterns filter sensitive vars.
	t.Setenv("SAFERM_INTEGRATION_SECRET", "hidden")
	m2, _ := Collect(defaultPatterns, nil)
	if _, ok := m2.Env["SAFERM_INTEGRATION_SECRET"]; ok {
		t.Error("SAFERM_INTEGRATION_SECRET should be filtered by default patterns")
	}

	if m.GitBranch == "" {
		t.Error("GitBranch should not be empty in saferm repo")
	}
	if m.GitHEAD == "" {
		t.Error("GitHEAD should not be empty in saferm repo")
	}
	if m.GitRoot == "" {
		t.Error("GitRoot should not be empty in saferm repo")
	}
	if m.PPID <= 0 {
		t.Errorf("PPID should be > 0, got %d", m.PPID)
	}
	if m.ParentCmd == "" {
		t.Error("ParentCmd should not be empty")
	}
	if m.Custom != nil {
		t.Error("Custom should be nil when no custom meta is passed")
	}
}

func TestCollect_CustomMeta(t *testing.T) {
	isolate(t)
	custom := map[string]string{
		"reason": "cleanup",
		"ticket": "PROJ-123",
	}

	m, err := Collect(nil, custom)
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}

	if m.Custom == nil {
		t.Fatal("Custom should not be nil when custom meta is provided")
	}
	if len(m.Custom) != 2 {
		t.Fatalf("Custom length = %d, want 2", len(m.Custom))
	}
	if m.Custom["reason"] != "cleanup" {
		t.Errorf("Custom[reason] = %q, want 'cleanup'", m.Custom["reason"])
	}
	if m.Custom["ticket"] != "PROJ-123" {
		t.Errorf("Custom[ticket] = %q, want 'PROJ-123'", m.Custom["ticket"])
	}
}
