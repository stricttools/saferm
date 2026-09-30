package test

import (
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// createFileOfSize creates a file at dir/name with exactly the given number of bytes.
func createFileOfSize(t *testing.T, dir, name string, sizeBytes int) string {
	t.Helper()
	content := strings.Repeat("x", sizeBytes)
	return testutil.CreateTempFile(t, dir, name, content)
}

func TestPurge_LargerThan(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	// Create files of known sizes: 500B, 1500B (>1KB), 3000B (>1KB).
	smallFile := createFileOfSize(t, workDir, "small.txt", 500)
	medFile := createFileOfSize(t, workDir, "medium.txt", 1500)
	largeFile := createFileOfSize(t, workDir, "large.txt", 3000)

	// Archive all three.
	for _, f := range []string{smallFile, medFile, largeFile} {
		_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "size test", f)
		if code != 0 {
			t.Fatalf("delete %s failed (exit %d): stderr=%q", f, code, stderr)
		}
	}

	// Verify all three are archived.
	stdout, _, code := runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d)", code)
	}
	ids := parseAllIDs(t, stdout)
	if len(ids) != 3 {
		t.Fatalf("expected 3 archived items, got %d:\n%s", len(ids), stdout)
	}

	// Dry run with --larger-than 1KB — should show only medium and large.
	stdout, stderr, code := runSaferm(t, homeDir, "purge", "--larger-than", "1KB", "--dry-run")
	if code != 0 {
		t.Fatalf("purge --larger-than --dry-run failed (exit %d): stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "small.txt") {
		t.Errorf("dry-run output should NOT contain small.txt:\n%s", stdout)
	}
	if !strings.Contains(stdout, "medium.txt") {
		t.Errorf("dry-run output should contain medium.txt:\n%s", stdout)
	}
	if !strings.Contains(stdout, "large.txt") {
		t.Errorf("dry-run output should contain large.txt:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Would purge 2 item(s)") {
		t.Errorf("dry-run output should say 'Would purge 2 item(s)':\n%s", stdout)
	}

	// Actual purge with --larger-than 1KB -f — should only purge medium and large.
	stdout, stderr, code = runSaferm(t, homeDir, "--approve-consequential", "purge", "--larger-than", "1KB")
	if code != 0 {
		t.Fatalf("purge --larger-than -f failed (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}

	// Verify only small.txt remains.
	stdout, _, code = runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d)", code)
	}
	ids = parseAllIDs(t, stdout)
	if len(ids) != 1 {
		t.Fatalf("expected 1 remaining item after purge, got %d:\n%s", len(ids), stdout)
	}
	if !strings.Contains(stdout, "small.txt") {
		t.Fatalf("remaining item should be small.txt:\n%s", stdout)
	}
}

func TestPurge_PreservesMetadata(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "preserve.txt", "metadata preservation test")

	// Delete the file.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "preserve metadata test", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}

	// Get ID from list.
	stdout, _, code := runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d)", code)
	}
	id := parseFirstID(t, stdout)

	// Purge the item.
	_, stderr, code = runSaferm(t, homeDir, "--approve-consequential", "purge", id)
	if code != 0 {
		t.Fatalf("purge failed (exit %d): stderr=%q", code, stderr)
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

	// list --all should show purged item with "purged" status.
	stdout, _, code = runSaferm(t, homeDir, "list", "--all")
	if code != 0 {
		t.Fatalf("list --all failed (exit %d)", code)
	}
	ids = parseAllIDs(t, stdout)
	if len(ids) != 1 {
		t.Fatalf("expected 1 item in list --all after purge, got %d:\n%s", len(ids), stdout)
	}
	if !strings.Contains(stdout, "purged") {
		t.Errorf("list --all output should contain 'purged' status:\n%s", stdout)
	}

	// info should return full metadata for purged item.
	stdout, _, code = runSaferm(t, homeDir, "info", id)
	if code != 0 {
		t.Fatalf("info should succeed for purged item (exit %d)", code)
	}
	if !strings.Contains(stdout, "Purged At:") {
		t.Errorf("info output should contain 'Purged At:':\n%s", stdout)
	}
	if !strings.Contains(stdout, "preserve metadata test") {
		t.Errorf("info output should contain original description:\n%s", stdout)
	}
	if !strings.Contains(stdout, "preserve.txt") {
		t.Errorf("info output should contain original filename:\n%s", stdout)
	}

	// undelete on purged item should error with clear message.
	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code != 6 { // ExitArchive
		t.Fatalf("undelete purged item: expected exit code 6, got %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "purged") {
		t.Errorf("undelete error should mention 'purged': stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "metadata is preserved") {
		t.Errorf("undelete error should mention 'metadata is preserved': stderr=%q", stderr)
	}
}

func TestPurge_AlreadyPurgedIsNoop(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "double-purge.txt", "double purge test")

	// Delete.
	_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "double purge test", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d)", code)
	}

	// Get ID.
	stdout, _, code := runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d)", code)
	}
	id := parseFirstID(t, stdout)

	// First purge.
	_, stderr, code := runSaferm(t, homeDir, "--approve-consequential", "purge", id)
	if code != 0 {
		t.Fatalf("first purge failed (exit %d): stderr=%q", code, stderr)
	}

	// Second purge of same ID should be a no-op (no error).
	stdout, stderr, code = runSaferm(t, homeDir, "--approve-consequential", "purge", id)
	if code != 0 {
		t.Fatalf("second purge of already-purged item should not error (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}
	// The item should be skipped (already purged), so 0 items purged.
	if !strings.Contains(stdout, "0 item(s) purged") {
		t.Errorf("second purge should report '0 item(s) purged': stdout=%q", stdout)
	}
}

func TestPurge_DryRun(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "dryrun.txt", "dry run test content")

	// Archive the file.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "dry run test", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}

	// Purge with --all --dry-run.
	stdout, stderr, code := runSaferm(t, homeDir, "purge", "--all", "--dry-run")
	if code != 0 {
		t.Fatalf("purge --all --dry-run failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify output shows the file.
	if !strings.Contains(stdout, "dryrun.txt") {
		t.Errorf("dry-run output should contain dryrun.txt:\n%s", stdout)
	}

	// Verify "Would purge" summary.
	if !strings.Contains(stdout, "Would purge 1 item(s)") {
		t.Errorf("dry-run output should say 'Would purge 1 item(s)':\n%s", stdout)
	}

	// Verify the file is NOT actually purged — it should still be in the list.
	stdout, _, code = runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d)", code)
	}
	ids := parseAllIDs(t, stdout)
	if len(ids) != 1 {
		t.Fatalf("expected 1 item still in archive after dry-run, got %d:\n%s", len(ids), stdout)
	}
	if !strings.Contains(stdout, "dryrun.txt") {
		t.Fatalf("dryrun.txt should still be in archive:\n%s", stdout)
	}
}
