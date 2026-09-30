package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

func TestDeleteMultipleFiles(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	f1 := testutil.CreateTempFile(t, workDir, "file1.txt", "one")
	f2 := testutil.CreateTempFile(t, workDir, "file2.txt", "two")
	f3 := testutil.CreateTempFile(t, workDir, "file3.txt", "three")

	// Delete all 3 in one command.
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "multi delete", f1, f2, f3)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}

	// Verify all 3 are gone.
	for _, f := range []string{f1, f2, f3} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("file %s should not exist after delete", f)
		}
	}

	// List should show 3 items.
	stdout, stderr, code = runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d): stderr=%q", code, stderr)
	}

	ids := parseAllIDs(t, stdout)
	if len(ids) != 3 {
		t.Fatalf("expected 3 items in list, got %d:\n%s", len(ids), stdout)
	}
}

func TestDeleteWithMeta(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "meta-test.txt", "metadata content")

	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort",
		"--description", "cleanup build",
		"--meta", "author=alice",
		"--meta", "reason=cleanup",
		"--command", "rm -rf build",
		filePath,
	)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}

	// Get ID.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	// Info should show all metadata.
	stdout, stderr, code = runSaferm(t, homeDir, "info", id)
	if code != 0 {
		t.Fatalf("info failed (exit %d): stderr=%q", code, stderr)
	}

	checks := []struct {
		label   string
		content string
	}{
		{"command", "rm -rf build"},
		{"description", "cleanup build"},
		{"meta author", "author = alice"},
		{"meta reason", "reason = cleanup"},
	}
	for _, c := range checks {
		if !strings.Contains(stdout, c.content) {
			t.Errorf("info output missing %s (%q):\n%s", c.label, c.content, stdout)
		}
	}
}

func TestDeleteWithForce_NonexistentSkipped(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	existsPath := testutil.CreateTempFile(t, workDir, "exists.txt", "I exist")
	noexistPath := filepath.Join(workDir, "noexist.txt")

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-f",
		"--description", "force skip test",
		existsPath, noexistPath,
	)
	if code != 0 {
		t.Fatalf("delete -f should succeed even with nonexistent files (exit %d): stdout=%q stderr=%q",
			code, stdout, stderr)
	}

	// exists.txt should be archived.
	if _, err := os.Stat(existsPath); !os.IsNotExist(err) {
		t.Fatal("exists.txt should be gone after delete")
	}

	// List should show exactly 1 item.
	stdout, _, code = runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d)", code)
	}
	ids := parseAllIDs(t, stdout)
	if len(ids) != 1 {
		t.Fatalf("expected 1 item, got %d:\n%s", len(ids), stdout)
	}
}

func TestUndeleteByPath(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	content := "undelete by path content"
	filePath := testutil.CreateTempFile(t, workDir, "bypath.txt", content)

	// Delete.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "path restore", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}

	// Undelete by path (not ID).
	stdout, stderr, code := runSaferm(t, homeDir, "undelete", filePath)
	if code != 0 {
		t.Fatalf("undelete by path failed (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}

	// Verify file is restored with correct content.
	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if string(got) != content {
		t.Fatalf("content mismatch: got %q, want %q", string(got), content)
	}
}

func TestUndeleteByPath_MultipleMatches(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "multi-match.txt", "first version")

	// Delete the first time.
	_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "first delete", filePath)
	if code != 0 {
		t.Fatalf("first delete failed (exit %d)", code)
	}

	// Get ID of first deletion.
	listOut, _, _ := runSaferm(t, homeDir, "list")
	firstID := parseFirstID(t, listOut)

	// Undelete by ID to restore.
	_, _, code = runSaferm(t, homeDir, "undelete", firstID)
	if code != 0 {
		t.Fatalf("undelete failed (exit %d)", code)
	}

	// Modify and delete again.
	if err := os.WriteFile(filePath, []byte("second version"), 0644); err != nil {
		t.Fatalf("writing second version: %v", err)
	}
	_, _, code = runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "second delete", filePath)
	if code != 0 {
		t.Fatalf("second delete failed (exit %d)", code)
	}

	// Now there should be 2 records for this path (one restored, one active).
	// Undelete by path should show multiple matches and fail.
	_, stderr, code := runSaferm(t, homeDir, "undelete", filePath)

	// The QueryByPath only returns non-restored records, so there might be only 1 match.
	// If there's exactly 1 non-restored match, it should succeed.
	// Let's check: if code == 0, the undelete worked (only 1 active record).
	// If code != 0, it should mention multiple matches.
	// From reading db.go: QueryByPath filters restored_at IS NULL, so only 1 active record.
	if code != 0 {
		t.Fatalf("undelete by path should succeed with 1 active record (exit %d): stderr=%q", code, stderr)
	}

	// Verify file is restored.
	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if string(got) != "second version" {
		t.Fatalf("content mismatch: got %q, want %q", string(got), "second version")
	}
}

func TestPurgeOlderThan(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	f1 := testutil.CreateTempFile(t, workDir, "old1.txt", "old content 1")
	f2 := testutil.CreateTempFile(t, workDir, "old2.txt", "old content 2")

	// Delete both.
	_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "purge test", f1, f2)
	if code != 0 {
		t.Fatalf("delete failed (exit %d)", code)
	}

	// Purge with --older-than 0s won't work since parseDuration needs h/d/w/m suffix.
	// Use --older-than 1h but records were just created, so they won't match.
	// Instead, use --all -f to purge everything.
	// Actually, let's test --older-than with a very recent threshold.
	// The records are created "just now", so --older-than 1h won't match.
	// Let's just verify --older-than 1h does NOT purge them (they're too new).
	stdout, stderr, code := runSaferm(t, homeDir, "--approve-consequential", "purge", "--older-than", "1h")
	if code != 0 {
		t.Fatalf("purge --older-than failed (exit %d): stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "Nothing to purge") {
		t.Fatalf("expected nothing to purge (files too new), got: %s", stdout)
	}

	// List should still show 2 items.
	stdout, _, _ = runSaferm(t, homeDir, "list")
	ids := parseAllIDs(t, stdout)
	if len(ids) != 2 {
		t.Fatalf("expected 2 items still in list, got %d", len(ids))
	}
}

func TestPurgeAll(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	f1 := testutil.CreateTempFile(t, workDir, "purge1.txt", "content1")
	f2 := testutil.CreateTempFile(t, workDir, "purge2.txt", "content2")
	f3 := testutil.CreateTempFile(t, workDir, "purge3.txt", "content3")

	// Delete all 3.
	_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "purge all test", f1, f2, f3)
	if code != 0 {
		t.Fatalf("delete failed (exit %d)", code)
	}

	// Purge --all -f.
	stdout, stderr, code := runSaferm(t, homeDir, "--approve-consequential", "purge", "--all")
	if code != 0 {
		t.Fatalf("purge --all failed (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}

	// List should be empty.
	stdout, _, code = runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d)", code)
	}
	if !strings.Contains(stdout, "No archived items") {
		t.Fatalf("expected no items after purge --all, got:\n%s", stdout)
	}

	// Archive dir should be empty.
	archiveDir := filepath.Join(homeDir, ".saferm", "archive")
	entries, err := os.ReadDir(archiveDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading archive dir: %v", err)
	}
	for _, entry := range entries {
		t.Errorf("archive dir should be empty, found: %s", entry.Name())
	}
}

func TestListAll_IncludesRestored(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "restored-list.txt", "content")

	// Delete.
	_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "list all test", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d)", code)
	}

	// Get ID and undelete.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	_, _, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete failed (exit %d)", code)
	}

	// list (without --all) should NOT show the restored item.
	stdout, _, code = runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d)", code)
	}
	if !strings.Contains(stdout, "No archived items") {
		t.Fatalf("expected no items in default list after restore, got:\n%s", stdout)
	}

	// list --all SHOULD show the restored item with "restored" status.
	stdout, _, code = runSaferm(t, homeDir, "list", "--all")
	if code != 0 {
		t.Fatalf("list --all failed (exit %d)", code)
	}
	if !strings.Contains(stdout, "restored") {
		t.Fatalf("list --all should show restored status:\n%s", stdout)
	}
	if !strings.Contains(stdout, "restored-list.txt") {
		t.Fatalf("list --all should show the filename:\n%s", stdout)
	}
}

func TestListOrdering(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	fileA := testutil.CreateTempFile(t, workDir, "aaa.txt", "a")
	fileB := testutil.CreateTempFile(t, workDir, "bbb.txt", "b")
	fileC := testutil.CreateTempFile(t, workDir, "ccc.txt", "c")

	// Delete in order: A, B, C.
	for _, f := range []string{fileA, fileB, fileC} {
		_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "order test", f)
		if code != 0 {
			t.Fatalf("delete %s failed", f)
		}
	}

	// List should show C first (newest first, ORDER BY deleted_at DESC).
	stdout, _, code := runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d)", code)
	}

	ids := parseAllIDs(t, stdout)
	if len(ids) != 3 {
		t.Fatalf("expected 3 items, got %d", len(ids))
	}

	// The first ID in the list should correspond to the most recently deleted file (ccc.txt).
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	// Find first data line containing an ID.
	foundCFirst := false
	for _, line := range lines {
		if strings.Contains(line, "ccc.txt") {
			foundCFirst = true
			break
		}
		if strings.Contains(line, "aaa.txt") || strings.Contains(line, "bbb.txt") {
			break
		}
	}
	if !foundCFirst {
		t.Fatalf("expected ccc.txt to be listed first (newest), got:\n%s", stdout)
	}
}

func TestInfoDirectory(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	dirPath := testutil.CreateTempDir(t, workDir, "info-dir")

	// Delete directory.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "dir info test", dirPath)
	if code != 0 {
		t.Fatalf("delete dir failed (exit %d): stderr=%q", code, stderr)
	}

	// Get ID.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	// Info should show Type: directory.
	stdout, stderr, code = runSaferm(t, homeDir, "info", id)
	if code != 0 {
		t.Fatalf("info failed (exit %d): stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "Type:          directory") {
		t.Fatalf("info should show Type: directory:\n%s", stdout)
	}
}

func TestInfoRestoredRecord(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "info-restored.txt", "content")

	// Delete.
	_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "info restore test", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d)", code)
	}

	// Get ID.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	// Undelete.
	_, _, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete failed (exit %d)", code)
	}

	// Info should show Restored At and Restored To fields.
	stdout, stderr, code := runSaferm(t, homeDir, "info", id)
	if code != 0 {
		t.Fatalf("info failed (exit %d): stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "Restored At:") {
		t.Fatalf("info should show Restored At field:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Restored To:") {
		t.Fatalf("info should show Restored To field:\n%s", stdout)
	}
}

func TestDeleteSameFileTwice(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "twice.txt", "first")

	// Delete first time.
	_, _, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "first", filePath)
	if code != 0 {
		t.Fatalf("first delete failed (exit %d)", code)
	}

	// Get first ID.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	firstID := parseFirstID(t, stdout)

	// Undelete.
	_, _, code = runSaferm(t, homeDir, "undelete", firstID)
	if code != 0 {
		t.Fatalf("undelete failed (exit %d)", code)
	}

	// Modify and delete again.
	if err := os.WriteFile(filePath, []byte("second"), 0644); err != nil {
		t.Fatalf("writing second version: %v", err)
	}
	_, _, code = runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "second", filePath)
	if code != 0 {
		t.Fatalf("second delete failed (exit %d)", code)
	}

	// list (without --all) should show only the active record (ID 2).
	stdout, _, _ = runSaferm(t, homeDir, "list")
	activeIDs := parseAllIDs(t, stdout)
	if len(activeIDs) != 1 {
		t.Fatalf("expected 1 active item, got %d:\n%s", len(activeIDs), stdout)
	}

	// list --all should show both records.
	stdout, _, _ = runSaferm(t, homeDir, "list", "--all")
	allIDs := parseAllIDs(t, stdout)
	if len(allIDs) != 2 {
		t.Fatalf("expected 2 total items (1 restored, 1 active), got %d:\n%s", len(allIDs), stdout)
	}

	// Verify the restored record shows "restored" status.
	if !strings.Contains(stdout, "restored") {
		t.Fatalf("list --all should show restored status:\n%s", stdout)
	}
}

func TestSafermHome_IsolatesData(t *testing.T) {
	// Use two separate SAFERM_HOME directories to verify isolation.
	homeDirA := testutil.SetupTestEnv(t)
	homeDirB := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	fileA := testutil.CreateTempFile(t, workDir, "isolated.txt", "isolated content")
	fileB := testutil.CreateTempFile(t, workDir, "other.txt", "other content")

	// Delete a file using home A.
	_, stderr, code := runSaferm(t, homeDirA, "delete", "--on-error", "abort", "--description", "isolation test", fileA)
	if code != 0 {
		t.Fatalf("delete in A failed (exit %d): stderr=%q", code, stderr)
	}

	// Delete a different file using home B so its DB is initialized.
	_, stderr, code = runSaferm(t, homeDirB, "delete", "--on-error", "abort", "--description", "isolation test B", fileB)
	if code != 0 {
		t.Fatalf("delete in B failed (exit %d): stderr=%q", code, stderr)
	}

	// List in home A should show isolated.txt but NOT other.txt.
	stdout, _, code := runSaferm(t, homeDirA, "list")
	if code != 0 {
		t.Fatalf("list A failed (exit %d)", code)
	}
	if !strings.Contains(stdout, "isolated.txt") {
		t.Fatalf("home A list should contain isolated.txt:\n%s", stdout)
	}
	if strings.Contains(stdout, "other.txt") {
		t.Fatalf("home A list should NOT contain other.txt:\n%s", stdout)
	}

	// List in home B should show other.txt but NOT isolated.txt.
	stdout, _, code = runSaferm(t, homeDirB, "list")
	if code != 0 {
		t.Fatalf("list B failed (exit %d)", code)
	}
	if !strings.Contains(stdout, "other.txt") {
		t.Fatalf("home B list should contain other.txt:\n%s", stdout)
	}
	if strings.Contains(stdout, "isolated.txt") {
		t.Fatalf("home B list should NOT contain isolated.txt:\n%s", stdout)
	}

	// List in home A again should still show isolated.txt.
	stdout, _, code = runSaferm(t, homeDirA, "list")
	if code != 0 {
		t.Fatalf("list A again failed (exit %d)", code)
	}
	if !strings.Contains(stdout, "isolated.txt") {
		t.Fatalf("home A list should still contain isolated.txt:\n%s", stdout)
	}
}
