package test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stricttools/saferm/internal/testutil"
	"github.com/stricttools/saferm/internal/trace"
)

var safermBinary string

func TestMain(m *testing.M) {
	// Build the saferm binary into a temp location.
	tmpDir, err := os.MkdirTemp("", "saferm-test-bin-*")
	if err != nil {
		panic("creating temp dir for binary: " + err.Error())
	}

	safermBinary = filepath.Join(tmpDir, "saferm-test")
	if runtime.GOOS == "windows" {
		safermBinary += ".exe"
	}

	// Find the project root (two levels up from internal/test/).
	_, thisFile, _, _ := runtime.Caller(0)
	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))

	cmd := exec.Command("go", "build", "-o", safermBinary, ".")
	cmd.Dir = projectRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("building saferm binary: " + err.Error())
	}

	exitCode := m.Run()

	os.RemoveAll(tmpDir)
	os.Exit(exitCode)
}

// runSaferm executes the saferm binary with the given args in an isolated
// environment. SAFERM_HOME is set to homeDir/.saferm/ so that all saferm
// data (archive, db) lives under the test-local directory without
// interfering with the real HOME.
// It adds nothing to argv. strictcli's confirm protocol prompts only for
// commands that declare themselves `consequential`, and in saferm that is
// `purge` alone -- `delete` and `undelete` are recoverable and run bare. A test
// that exercises `purge` passes --approve-consequential itself, which keeps the
// approval visible at the call site instead of hidden in a helper.
func runSaferm(t *testing.T, homeDir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return runSafermNoConsent(t, homeDir, args...)
}

// runSafermNoConsent executes the saferm binary with exactly the given args and
// nothing added. Use it when the test is ABOUT consent.
func runSafermNoConsent(t *testing.T, homeDir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return runSafermEnv(t, homeDir, nil, args...)
}

// runSafermTraced runs saferm the way a traced caller's child sees it: HOME
// points at the test's own home, so the trace store the test wrote is the store
// saferm reads, and STRICTCLI_TRACE_PARENT carries the parent entry's
// identifier. The value is passed through verbatim, malformed ones included.
func runSafermTraced(t *testing.T, homeDir, parentID string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return runSafermEnv(t, homeDir, []string{
		"HOME=" + homeDir,
		trace.ParentEnv + "=" + parentID,
	}, args...)
}

// runSafermEnv is runSafermNoConsent with extra environment entries appended.
// Later entries win, so an entry here overrides the inherited one.
func runSafermEnv(t *testing.T, homeDir string, extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return runSafermWithin(t, runLimit, homeDir, extraEnv, args...)
}

// runLimit bounds every saferm run a test makes. A run that outlives it is
// killed and fails its test: a saferm that blocks -- on a FIFO it should never
// have opened, say -- must fail the suite, not hang it.
const runLimit = 2 * time.Minute

// runSafermWithin is runSafermEnv with the bound stated: the run is killed, and
// the test failed, if it has not exited within limit.
func runSafermWithin(t *testing.T, limit time.Duration, homeDir string, extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, safermBinary, args...)

	// Set SAFERM_HOME so saferm's BaseDir() uses the test directory directly.
	// Strip ALL SAFERM_-prefixed vars from the inherited env first: a developer's
	// SAFERM_ARCHIVE_DIR (or any other SAFERM_* override) would otherwise leak in
	// and trigger nondeterministic env+config conflicts under conflict-mode "error".
	// STRICTCLI_-prefixed vars go the same way: a suite run from a traced session
	// inherits STRICTCLI_TRACE_PARENT, which would give every "untraced" run an
	// ancestry it never had. A test that wants one appends it through extraEnv,
	// which is applied after the strip.
	safermHome := filepath.Join(homeDir, ".saferm")
	env := filterEnv(filterEnv(os.Environ(), "SAFERM_"), "STRICTCLI_")
	env = append(env, "SAFERM_HOME="+safermHome)
	env = append(env, extraEnv...)
	cmd.Env = env

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()
	if ctx.Err() != nil {
		t.Fatalf("saferm %s did not exit within %s and was killed\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), limit, stdout, stderr)
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("running saferm: %v", err)
		}
	}

	return stdout, stderr, exitCode
}

// filterEnv returns env without any entries whose variable name starts with
// keyPrefix. Matching is on the variable name (the part before '='), so passing
// "SAFERM_" strips SAFERM_HOME, SAFERM_ARCHIVE_DIR, and every other SAFERM_* var.
func filterEnv(env []string, keyPrefix string) []string {
	var result []string
	for _, e := range env {
		name := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			name = e[:i]
		}
		if !strings.HasPrefix(name, keyPrefix) {
			result = append(result, e)
		}
	}
	return result
}

// parseFirstID extracts the first numeric ID from saferm list output.
// The list format is: "ID     Path ..."
func parseFirstID(t *testing.T, listOutput string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(listOutput), "\n")
	// Skip the header lines (first two lines are header + separator).
	re := regexp.MustCompile(`^\s*(\d+)\s+`)
	for _, line := range lines {
		if m := re.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	t.Fatalf("no ID found in list output:\n%s", listOutput)
	return ""
}

// parseAllIDs extracts all numeric IDs from saferm list output.
func parseAllIDs(t *testing.T, listOutput string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(listOutput), "\n")
	re := regexp.MustCompile(`^\s*(\d+)\s+`)
	var ids []string
	for _, line := range lines {
		if m := re.FindStringSubmatch(line); m != nil {
			ids = append(ids, m[1])
		}
	}
	return ids
}

func TestDeleteAndUndelete_Roundtrip(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	content := "hello, saferm roundtrip test!"
	filePath := testutil.CreateTempFile(t, workDir, "roundtrip.txt", content)

	// Delete the file.
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "roundtrip test", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}

	// Verify file is gone.
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatal("file should not exist after delete")
	}

	// List and verify the item appears.
	stdout, stderr, code = runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d): stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "roundtrip.txt") {
		t.Fatalf("list output should contain the filename:\n%s", stdout)
	}

	id := parseFirstID(t, stdout)

	// Undelete.
	stdout, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete failed (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}

	// Verify file is back with same content.
	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if string(got) != content {
		t.Fatalf("content mismatch: got %q, want %q", string(got), content)
	}
}

func TestDeleteDirectory_Roundtrip(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	dirPath := testutil.CreateTempDir(t, workDir, "mydir")

	// Delete the directory.
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "dir roundtrip", dirPath)
	if code != 0 {
		t.Fatalf("delete dir failed (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}

	// Verify dir is gone.
	if _, err := os.Stat(dirPath); !os.IsNotExist(err) {
		t.Fatal("directory should not exist after delete")
	}

	// List and get ID.
	stdout, _, code = runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d)", code)
	}
	id := parseFirstID(t, stdout)

	// Undelete.
	stdout, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete dir failed (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}

	// Verify all files are restored with correct content.
	for _, tc := range []struct {
		relPath string
		content string
	}{
		{"file1.txt", "content of file1"},
		{"file2.txt", "content of file2"},
		{"subdir/nested.txt", "nested content"},
	} {
		fp := filepath.Join(dirPath, tc.relPath)
		got, err := os.ReadFile(fp)
		if err != nil {
			t.Errorf("reading %s: %v", tc.relPath, err)
			continue
		}
		if string(got) != tc.content {
			t.Errorf("%s: got %q, want %q", tc.relPath, string(got), tc.content)
		}
	}
}

func TestDelete_RequiresDescription(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "nodesc.txt", "content")

	_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", filePath)
	if code == 0 {
		t.Fatal("delete without --description should fail")
	}
}

func TestDelete_RequiresRecursiveForDir(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	dirPath := testutil.CreateTempDir(t, workDir, "norecursive")

	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "test", dirPath)
	if code == 0 {
		t.Fatal("delete directory without -r should fail")
	}
	if !strings.Contains(stderr, "directory") || !strings.Contains(stderr, "-r") {
		t.Fatalf("expected error about -r flag, got: %q", stderr)
	}
}

func TestUndelete_ConflictError(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "conflict.txt", "original")

	// Delete.
	_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "conflict test", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d)", code)
	}

	// Recreate the file at the same path.
	testutil.CreateTempFile(t, workDir, "conflict.txt", "replacement")

	// Get ID.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	// Undelete with no conflict mode should fail.
	_, stderr, code := runSaferm(t, homeDir, "undelete", id)
	if code == 0 {
		t.Fatal("undelete should fail when file exists at destination")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Fatalf("expected conflict error, got: %q", stderr)
	}

	// Verify the replacement file is still intact.
	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading replacement file: %v", err)
	}
	if string(got) != "replacement" {
		t.Fatalf("replacement file was modified: got %q", string(got))
	}
}

func TestUndelete_OverwriteById(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "forcetest.txt", "original content")

	// Delete.
	_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "force test", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d)", code)
	}

	// Recreate the file.
	testutil.CreateTempFile(t, workDir, "forcetest.txt", "should be overwritten")

	// Get ID.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	// Undelete with --on-conflict overwrite should succeed.
	stdout, stderr, code := runSaferm(t, homeDir, "undelete", "--on-conflict", "overwrite", id)
	if code != 0 {
		t.Fatalf("undelete --on-conflict overwrite failed (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}

	// Verify original content is restored.
	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if string(got) != "original content" {
		t.Fatalf("content mismatch: got %q, want %q", string(got), "original content")
	}
}

func TestPurge_ById(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "purge-me.txt", "to be purged")

	// Delete.
	_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "purge test", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d)", code)
	}

	// Get ID.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	// Purge by ID (with -f to skip confirmation).
	stdout, stderr, code := runSaferm(t, homeDir, "--approve-consequential", "purge", id)
	if code != 0 {
		t.Fatalf("purge failed (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}

	// Info should still succeed (metadata is preserved after purge).
	stdout, stderr, code = runSaferm(t, homeDir, "info", id)
	if code != 0 {
		t.Fatalf("info should succeed after purge (metadata preserved) (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Purged At:") {
		t.Errorf("info output should contain 'Purged At:' after purge:\n%s", stdout)
	}

	// Default list should hide purged items.
	stdout, _, code = runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d)", code)
	}
	ids := parseAllIDs(t, stdout)
	if len(ids) != 0 {
		t.Errorf("expected 0 items in default list after purge, got %d:\n%s", len(ids), stdout)
	}

	// Verify archive files are gone.
	safermDir := filepath.Join(homeDir, ".saferm", "archive")
	entries, err := os.ReadDir(safermDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading archive dir: %v", err)
	}
	for _, entry := range entries {
		t.Errorf("archive dir should be empty, found: %s", entry.Name())
	}
}

func TestList_PathFilter(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	// Create files with different path patterns.
	fileA := testutil.CreateTempFile(t, workDir, "alpha.txt", "a")
	fileB := testutil.CreateTempFile(t, workDir, "beta.txt", "b")
	fileC := testutil.CreateTempFile(t, workDir, "gamma.txt", "c")

	// Delete all three.
	for _, f := range []string{fileA, fileB, fileC} {
		_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "filter test", f)
		if code != 0 {
			t.Fatalf("delete %s failed", f)
		}
	}

	// Filter by actual glob pattern: * matches any non-separator chars in the filename.
	pattern := filepath.Join(workDir, "*eta*")
	stdout, stderr, code := runSaferm(t, homeDir, "list", "--path", pattern)
	if code != 0 {
		t.Fatalf("list --path failed (exit %d): stderr=%q", code, stderr)
	}

	ids := parseAllIDs(t, stdout)
	if len(ids) != 1 {
		t.Fatalf("expected 1 match, got %d: %s", len(ids), stdout)
	}

	if !strings.Contains(stdout, "beta.txt") {
		t.Fatalf("expected beta.txt in filtered output:\n%s", stdout)
	}
}

func TestInfo_ShowsMetadata(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "info-test.txt", "metadata test")

	// Delete with custom metadata and command.
	_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort",
		"--description", "info meta test",
		"--command", "rm info-test.txt",
		"--meta", "foo=bar",
		"--meta", "env=test",
		filePath,
	)
	if code != 0 {
		t.Fatalf("delete failed (exit %d)", code)
	}

	// Get ID.
	listOut, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, listOut)

	// Run info.
	stdout, stderr, code := runSaferm(t, homeDir, "info", id)
	if code != 0 {
		t.Fatalf("info failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify output contains expected metadata.
	checks := []struct {
		label   string
		content string
	}{
		{"description", "info meta test"},
		{"command", "rm info-test.txt"},
		{"custom meta foo", "foo = bar"},
		{"custom meta env", "env = test"},
		{"original path", filePath},
	}
	for _, check := range checks {
		if !strings.Contains(stdout, check.content) {
			t.Errorf("info output missing %s (%q):\n%s", check.label, check.content, stdout)
		}
	}

	// Verify it shows a hash (SHA-256).
	if !strings.Contains(stdout, "Hash:") {
		t.Errorf("info output missing hash:\n%s", stdout)
	}

	// Verify description line.
	if !strings.Contains(stdout, fmt.Sprintf("Description:   %s", "info meta test")) {
		t.Errorf("info output missing description line:\n%s", stdout)
	}
}
