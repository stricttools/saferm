package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// The two failure reports an archival can end on once its record is committed
// or its record is refused. Neither is reachable without holding the window
// open: see startDeleteInWindow in delete_window_test.go.

// A source that disappears inside the window is the plain failure of the second
// half: the record is committed and truthful, and the archived copy is now the
// only copy there is. Its identifiers are what the caller needs, so they are in
// the message.
func TestDelete_ASourceThatVanishesDuringTheInsertIsReportedWithItsRecord(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedArchive(t, home, work, "seed.txt")

	target := testutil.CreateTempFile(t, work, "vanishing.txt", "content\n")
	inFlight := startDeleteInWindow(t, home, "delete", "--on-error", "abort", "--description", "vanished mid-archival", target)

	if err := os.Remove(target); err != nil {
		t.Fatalf("removing the source inside the window: %v", err)
	}
	out := inFlight.finish()

	if out.code == 0 {
		t.Fatalf("a delete that could not remove its source must not exit 0; stderr=%s", out.stderr)
	}
	if !recordIdentifiers.MatchString(out.stderr) {
		t.Errorf("the failure must name the record holding the archived copy, got: %s", out.stderr)
	}
	if !strings.Contains(out.stderr, "holds the archived copy") {
		t.Errorf("the failure must say the archive still has the content, got: %s", out.stderr)
	}

	// And the record really does resolve to the content, which is the whole
	// point of naming it: the file is recoverable from the identifiers printed.
	if _, stderr, code := runSaferm(t, home, "undelete", target); code != 0 {
		t.Fatalf("the archived copy the failure pointed at could not be restored: %s", stderr)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("undelete did not restore the file: %v", err)
	}
	if string(content) != "content\n" {
		t.Errorf("the restored content is %q", content)
	}
}

// The last way to end up with an archive entry no command can resolve: the
// insert fails, the entry is discarded to take the archival back whole, and the
// discard ITSELF fails. Nothing can clean that up afterwards -- the entry has no
// row and no command enumerates the archive directory -- so it is reported by
// name, loudly, rather than swallowed behind the insert failure that caused it.
//
// Both injections are needed to reach it: a trigger that refuses every insert,
// and a held write lock that stretches the window wide enough to make the
// archive directory unwritable while the archival is inside it.
func TestDelete_AFailingDiscardIsReportedByName(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedAndRefuse(t, home, work)

	archiveDir := filepath.Join(home, ".saferm", "archive")
	target := testutil.CreateTempFile(t, work, "undiscardable.txt", "content\n")
	inFlight := startDeleteInWindow(t, home, "delete", "--on-error", "abort", "--description", "discard fails", target)

	// Removing a directory entry needs write permission on the directory, so
	// this is what makes DiscardBlob's os.Remove fail on an entry it just
	// created.
	if err := os.Chmod(archiveDir, 0500); err != nil {
		t.Fatalf("making the archive directory unwritable: %v", err)
	}
	t.Cleanup(func() { os.Chmod(archiveDir, 0700) })
	out := inFlight.finish()

	if out.code == 0 {
		t.Fatalf("a refused insert must fail the delete; got exit 0")
	}
	entryPath := filepath.Join(archiveDir, inFlight.entry)
	if !strings.Contains(out.stderr, entryPath) {
		t.Errorf("the failed discard must name the entry it could not remove (%s), got: %s", entryPath, out.stderr)
	}
	if !strings.Contains(out.stderr, "orphaned copy no saferm command can name") {
		t.Errorf("the failed discard must say what the leftover entry is, got: %s", out.stderr)
	}

	// The source is untouched, as it is for every failed insert.
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the source was consumed by a delete that recorded nothing: %v", err)
	}
	// And the entry really is still there, which is what the message claims.
	if _, err := os.Stat(entryPath); err != nil {
		t.Errorf("the entry the message called orphaned is not there: %v", err)
	}
}
