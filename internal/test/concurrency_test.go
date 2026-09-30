package test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

func TestConcurrentDeletes(t *testing.T) {
	homeDir := t.TempDir()
	workDir := t.TempDir()

	const numFiles = 20

	// Create all temp files.
	files := make([]string, numFiles)
	for i := range numFiles {
		name := fmt.Sprintf("concurrent_%03d.txt", i)
		content := fmt.Sprintf("content of file %d", i)
		files[i] = testutil.CreateTempFile(t, workDir, name, content)
	}

	// Launch parallel deletes. Nothing here retries: saferm retries database
	// contention itself, and these tests are one of the two places that used to
	// hand-roll a retry loop around the binary to compensate for it not doing so.
	var wg sync.WaitGroup
	results := make([]struct {
		stdout   string
		stderr   string
		exitCode int
	}, numFiles)

	wg.Add(numFiles)
	for i := range numFiles {
		go func(idx int) {
			defer wg.Done()
			desc := fmt.Sprintf("concurrent %d", idx)
			stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort",
				"--description", desc, files[idx])
			results[idx].stdout = stdout
			results[idx].stderr = stderr
			results[idx].exitCode = code
		}(i)
	}
	wg.Wait()

	// Verify all deletes succeeded.
	for i, r := range results {
		if r.exitCode != 0 {
			t.Errorf("delete %d failed (exit %d): stderr=%q", i, r.exitCode, r.stderr)
		}
	}

	// Verify list shows all items.
	stdout, stderr, code := runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d): stderr=%q", code, stderr)
	}

	ids := parseAllIDs(t, stdout)
	if len(ids) != numFiles {
		t.Errorf("expected %d items in list, got %d:\n%s", numFiles, len(ids), stdout)
	}

	// Verify all records are individually queryable via info.
	for _, id := range ids {
		_, stderr, code := runSaferm(t, homeDir, "info", id)
		if code != 0 {
			t.Errorf("info %s failed (exit %d): stderr=%q", id, code, stderr)
		}
	}

	// Verify list output contains all concurrent file names.
	for i := range numFiles {
		name := fmt.Sprintf("concurrent_%03d.txt", i)
		if !strings.Contains(stdout, name) {
			t.Errorf("list output missing %s", name)
		}
	}
}

func TestConcurrentDeleteAndUndelete(t *testing.T) {
	homeDir := t.TempDir()
	workDir := t.TempDir()

	const numDeleteFiles = 10
	const numUndeleteFiles = 5

	// Pre-create files that will be deleted by undelete goroutines (they need
	// existing records to restore). We delete them first sequentially.
	undeleteIDs := make([]string, numUndeleteFiles)
	for i := range numUndeleteFiles {
		name := fmt.Sprintf("pre_del_%03d.txt", i)
		content := fmt.Sprintf("pre-deleted content %d", i)
		path := testutil.CreateTempFile(t, workDir, name, content)

		_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort",
			"--description", fmt.Sprintf("pre-delete %d", i), path)
		if code != 0 {
			t.Fatalf("pre-delete %d failed (exit %d): stderr=%q", i, code, stderr)
		}
	}

	// Get the IDs of the pre-deleted files.
	stdout, _, code := runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d)", code)
	}
	ids := parseAllIDs(t, stdout)
	if len(ids) < numUndeleteFiles {
		t.Fatalf("expected at least %d items, got %d", numUndeleteFiles, len(ids))
	}
	// The list is ordered newest-first, so the first numUndeleteFiles IDs
	// correspond to the pre-deleted files.
	copy(undeleteIDs, ids[:numUndeleteFiles])

	// Create files for the concurrent delete goroutines.
	deleteFiles := make([]string, numDeleteFiles)
	for i := range numDeleteFiles {
		name := fmt.Sprintf("conc_del_%03d.txt", i)
		content := fmt.Sprintf("concurrent delete content %d", i)
		deleteFiles[i] = testutil.CreateTempFile(t, workDir, name, content)
	}

	// Launch concurrent deletes and undeletes.
	var wg sync.WaitGroup

	deleteResults := make([]struct {
		exitCode int
		stderr   string
	}, numDeleteFiles)

	undeleteResults := make([]struct {
		exitCode int
		stderr   string
	}, numUndeleteFiles)

	// Delete goroutines.
	wg.Add(numDeleteFiles)
	for i := range numDeleteFiles {
		go func(idx int) {
			defer wg.Done()
			desc := fmt.Sprintf("concurrent del %d", idx)
			_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort",
				"--description", desc, deleteFiles[idx])
			deleteResults[idx].exitCode = code
			deleteResults[idx].stderr = stderr
		}(i)
	}

	// Undelete goroutines.
	wg.Add(numUndeleteFiles)
	for i := range numUndeleteFiles {
		go func(idx int) {
			defer wg.Done()
			_, stderr, code := runSaferm(t, homeDir, "undelete", undeleteIDs[idx])
			undeleteResults[idx].exitCode = code
			undeleteResults[idx].stderr = stderr
		}(i)
	}

	wg.Wait()

	// Verify all deletes succeeded.
	for i, r := range deleteResults {
		if r.exitCode != 0 {
			t.Errorf("delete %d failed (exit %d): stderr=%q", i, r.exitCode, r.stderr)
		}
	}

	// Verify all undeletes succeeded.
	for i, r := range undeleteResults {
		if r.exitCode != 0 {
			t.Errorf("undelete %d failed (exit %d): stderr=%q", i, r.exitCode, r.stderr)
		}
	}

	// Verify DB integrity: list --all should show all records (pre-deleted + newly deleted).
	stdout, stderr, code := runSaferm(t, homeDir, "list", "--all")
	if code != 0 {
		t.Fatalf("list --all failed (exit %d): stderr=%q", code, stderr)
	}

	allIDs := parseAllIDs(t, stdout)
	expectedTotal := numDeleteFiles + numUndeleteFiles
	if len(allIDs) != expectedTotal {
		t.Errorf("expected %d total records, got %d:\n%s", expectedTotal, len(allIDs), stdout)
	}

	// Verify each record is individually queryable (no corruption).
	for _, id := range allIDs {
		_, stderr, code := runSaferm(t, homeDir, "info", id)
		if code != 0 {
			t.Errorf("info %s failed (exit %d): stderr=%q", id, code, stderr)
		}
	}
}
