package test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"

	_ "modernc.org/sqlite"
)

// The blob-without-row window.
//
// Archiving and recording used to happen in that order with nothing between
// them but hope: the file was moved into the archive first, and the database
// row was inserted afterwards. An insert that failed left the blob on disk, the
// source gone and no record naming either -- the file was unreachable by every
// verb saferm has, and the live archive really did accumulate orphaned blobs
// this way.
//
// The order is now archive, record, and only then remove the source, so a
// failed insert can undo itself completely: the blob is discarded and the path
// is exactly where the caller left it. These tests inject the insert failure
// with a database trigger, which is the only way to fail an insert on a
// database that is otherwise healthy.

// refuseInserts installs a trigger that aborts every insert into deletions.
func refuseInserts(t *testing.T, home string) {
	t.Helper()
	dbPath := filepath.Join(home, ".saferm", "db", "saferm.db")
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening the archive database directly: %v", err)
	}
	defer conn.Close()
	_, err = conn.Exec(`CREATE TRIGGER refuse_inserts BEFORE INSERT ON deletions
		BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END;`)
	if err != nil {
		t.Fatalf("installing the insert-refusing trigger: %v", err)
	}
}

// archiveEntries lists the archive directory's contents.
func archiveEntries(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, ".saferm", "archive"))
	if err != nil {
		t.Fatalf("reading the archive directory: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// seedAndRefuse creates the archive (so the database file exists), then makes
// every later insert fail.
func seedAndRefuse(t *testing.T, home, work string) {
	t.Helper()
	seed := testutil.CreateTempFile(t, work, "seed.txt", "seed\n")
	if _, stderr, code := runSaferm(t, home, "delete", "--on-error", "abort", "--description", "seed", seed); code != 0 {
		t.Fatalf("seeding delete failed (%d): %s", code, stderr)
	}
	refuseInserts(t, home)
}

func TestDelete_InsertFailureLeavesTheFileInPlaceAndNoOrphanedBlob(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedAndRefuse(t, home, work)

	before := archiveEntries(t, home)
	target := testutil.CreateTempFile(t, work, "survivor.txt", "still here\n")

	_, stderr, code := runSaferm(t, home, "delete", "--on-error", "abort", "--description", "insert fails", target)
	if code == 0 {
		t.Fatalf("a refused insert must fail the delete; got exit 0")
	}
	if !strings.Contains(stderr, target) {
		t.Errorf("the failure must name the path it did not archive, got: %q", stderr)
	}

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the source file was consumed by a delete that recorded nothing: %v", err)
	}
	if string(content) != "still here\n" {
		t.Errorf("the source file was altered: %q", content)
	}

	if after := archiveEntries(t, home); len(after) != len(before) {
		t.Errorf("the archive gained an orphaned blob with no row: before=%v after=%v", before, after)
	}
}

func TestDelete_InsertFailureLeavesTheDirectoryInPlaceAndNoOrphanedBlob(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedAndRefuse(t, home, work)

	before := archiveEntries(t, home)
	dir := filepath.Join(work, "tree")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0755); err != nil {
		t.Fatalf("creating the directory: %v", err)
	}
	inner := filepath.Join(dir, "nested", "inner.txt")
	if err := os.WriteFile(inner, []byte("inner\n"), 0644); err != nil {
		t.Fatalf("writing the nested file: %v", err)
	}

	_, stderr, code := runSaferm(t, home, "delete", "--on-error", "abort", "-r", "--description", "insert fails", dir)
	if code == 0 {
		t.Fatalf("a refused insert must fail the delete; got exit 0")
	}
	_ = stderr

	if _, err := os.Stat(inner); err != nil {
		t.Fatalf("the directory tree was destroyed by a delete that recorded nothing: %v", err)
	}
	if after := archiveEntries(t, home); len(after) != len(before) {
		t.Errorf("the archive gained an orphaned blob with no row: before=%v after=%v", before, after)
	}
}

func TestDelete_InsertFailureLeavesTheSymlinkInPlaceAndNoOrphanedBlob(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedAndRefuse(t, home, work)

	before := archiveEntries(t, home)
	link := filepath.Join(work, "link")
	if err := os.Symlink("/etc/hostname", link); err != nil {
		t.Fatalf("creating the symlink: %v", err)
	}

	if _, _, code := runSaferm(t, home, "delete", "--on-error", "abort", "--description", "insert fails", link); code == 0 {
		t.Fatalf("a refused insert must fail the delete; got exit 0")
	}

	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the symlink was consumed by a delete that recorded nothing: %v", err)
	}
	if after := archiveEntries(t, home); len(after) != len(before) {
		t.Errorf("the archive gained an orphaned blob with no row: before=%v after=%v", before, after)
	}
}

// The batch's other paths are unaffected: a failing insert is one path's
// failure, and --on-error decides what happens to the rest. Continue mode
// therefore leaves every path in place here, because every insert fails.
func TestDelete_InsertFailureUnderContinueLeavesEveryPathInPlace(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedAndRefuse(t, home, work)

	before := archiveEntries(t, home)
	first := testutil.CreateTempFile(t, work, "a.txt", "a\n")
	second := testutil.CreateTempFile(t, work, "b.txt", "b\n")

	_, stderr, code := runSaferm(t, home, "delete", "--on-error", "continue",
		"--description", "insert fails for both", first, second)
	if code == 0 {
		t.Fatalf("both inserts failed; the command must exit non-zero")
	}
	if !strings.Contains(stderr, "2 of 2") {
		t.Errorf("continue mode must report both failures, got: %q", stderr)
	}
	for _, p := range []string{first, second} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was consumed by a delete that recorded nothing: %v", p, err)
		}
	}
	if after := archiveEntries(t, home); len(after) != len(before) {
		t.Errorf("the archive gained orphaned blobs: before=%v after=%v", before, after)
	}
}
