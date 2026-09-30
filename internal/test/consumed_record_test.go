package test

import (
	"os"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// A restore consumes the archived blob: the entry is moved out of the archive
// and the row is stamped with restored_at. The record survives as metadata but
// has nothing left to restore. Asking to restore it again must say so, in the
// record's own vocabulary, before anything is attempted.
//
// Both tests below reach the same consumed record by two different routes and
// used to produce two different confusing answers: a raw archive-layer stat
// failure when the destination was gone, and an "--on-conflict overwrite" hint
// when it was still there -- a hint that could not have helped, since the blob
// the overwrite would have restored from no longer exists.

func TestUndelete_AlreadyRestored_DestinationGone_ReportsStatus(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "consumed.txt", "consumed content")

	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "consumed record test", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}

	stdout, stderr, code := runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d): stderr=%q", code, stderr)
	}
	id := parseFirstID(t, stdout)

	if _, stderr, code = runSaferm(t, homeDir, "undelete", id); code != 0 {
		t.Fatalf("first undelete failed (exit %d): stderr=%q", code, stderr)
	}

	// Take the restored file away again, so the second attempt reaches the
	// archive layer instead of stopping at a destination conflict.
	if err := os.Remove(filePath); err != nil {
		t.Fatalf("removing restored file: %v", err)
	}

	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code == 0 {
		t.Fatalf("restoring an already-restored record should fail; got exit 0, stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "already restored") {
		t.Fatalf("expected an already-restored status in stderr, got: %q", stderr)
	}
	if strings.Contains(stderr, "archive entry not found") {
		t.Fatalf("expected a record-status error, got a raw archive-layer failure: %q", stderr)
	}
}

func TestUndelete_AlreadyRestored_DestinationPresent_ReportsStatus(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "consumed-present.txt", "consumed content")

	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "consumed record test", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}

	stdout, stderr, code := runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d): stderr=%q", code, stderr)
	}
	id := parseFirstID(t, stdout)

	if _, stderr, code = runSaferm(t, homeDir, "undelete", id); code != 0 {
		t.Fatalf("first undelete failed (exit %d): stderr=%q", code, stderr)
	}

	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code == 0 {
		t.Fatalf("restoring an already-restored record should fail; got exit 0, stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "already restored") {
		t.Fatalf("expected an already-restored status in stderr, got: %q", stderr)
	}
	if strings.Contains(stderr, "--on-conflict") {
		t.Fatalf("a consumed record must not be advertised as overwritable: %q", stderr)
	}
}

// The purged guard is the same shape and already in place; this pins it so the
// two consumed states keep reporting alike.
func TestUndelete_Purged_ReportsStatus(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "purged.txt", "purged content")

	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "purged record test", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}

	stdout, stderr, code := runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d): stderr=%q", code, stderr)
	}
	id := parseFirstID(t, stdout)

	if _, stderr, code = runSaferm(t, homeDir, "--approve-consequential", "purge", id); code != 0 {
		t.Fatalf("purge failed (exit %d): stderr=%q", code, stderr)
	}

	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code == 0 {
		t.Fatalf("restoring a purged record should fail; got exit 0, stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "was purged") {
		t.Fatalf("expected a purged status in stderr, got: %q", stderr)
	}
}
