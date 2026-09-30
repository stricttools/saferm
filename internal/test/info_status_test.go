package test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// A record and its archived content have separate fates: a restore consumes
// the blob, a purge destroys it, and in both cases the record survives as
// metadata with nothing left to restore. `info` used to print the two
// timestamps and leave the reader to work out what they implied. The status
// line states it, and is derived from the columns that already record it --
// there is no separate detection pass over the archive directory.

var statusLine = regexp.MustCompile(`(?m)^Status:\s+(.*)$`)

func infoStatus(t *testing.T, home, target string) string {
	t.Helper()
	stdout, stderr, code := runSaferm(t, home, "info", target)
	if code != 0 {
		t.Fatalf("info %s failed (%d): %s", target, code, stderr)
	}
	m := statusLine.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("info %s printed no status line:\n%s", target, stdout)
	}
	return strings.TrimSpace(m[1])
}

func TestInfo_StatusIsRestorableThenRestored(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "status.txt", "content\n")

	_, uuid := deleteOne(t, home, target)

	if got := infoStatus(t, home, uuid); got != "restorable" {
		t.Errorf("a freshly archived record must be restorable, got %q", got)
	}

	if _, stderr, code := runSaferm(t, home, "undelete", uuid); code != 0 {
		t.Fatalf("undelete failed (%d): %s", code, stderr)
	}

	got := infoStatus(t, home, uuid)
	if !strings.HasPrefix(got, "restored at ") {
		t.Errorf("a consumed record must report when it was restored, got %q", got)
	}
}

func TestInfo_StatusReportsPurged(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "purged-status.txt", "content\n")

	_, uuid := deleteOne(t, home, target)

	if _, stderr, code := runSaferm(t, home, "--approve-consequential", "purge", uuid); code != 0 {
		t.Fatalf("purge failed (%d): %s", code, stderr)
	}

	got := infoStatus(t, home, uuid)
	if !strings.HasPrefix(got, "purged at ") {
		t.Errorf("a purged record must report when it was purged, got %q", got)
	}
}

// Restored and then purged is a real state: the restore consumed the blob and
// the purge stamped the record afterwards. Both facts are reported, in the
// order they happened.
func TestInfo_StatusReportsRestoredAndPurged(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "both.txt", "content\n")

	_, uuid := deleteOne(t, home, target)

	if _, stderr, code := runSaferm(t, home, "undelete", uuid); code != 0 {
		t.Fatalf("undelete failed (%d): %s", code, stderr)
	}
	if _, stderr, code := runSaferm(t, home, "--approve-consequential", "purge", uuid); code != 0 {
		t.Fatalf("purge failed (%d): %s", code, stderr)
	}

	got := infoStatus(t, home, uuid)
	if !strings.Contains(got, "restored at ") || !strings.Contains(got, "purged at ") {
		t.Errorf("a restored-then-purged record must report both, got %q", got)
	}
}

// The premise the two columns rested on -- that every blob absence is explained
// by restored_at or purged_at -- stopped holding when the mid-archival
// recoveries started leaving rows on purpose: a source written through while
// its entry was a hard link to it, or a tree that grew during the insert, has
// its entry discarded and its row committed, so the row names nothing and
// neither column says why. `info` used to answer "restorable" for those, which
// is the one answer that gets a caller to try an undelete that cannot work.
func TestInfo_StatusReportsAnArchivedCopyThatIsGone(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "residue.txt", "content\n")

	_, uuid := deleteOne(t, home, target)

	// The same state a discarded archival leaves: the row is there, the entry
	// is not, and nothing was restored or purged.
	if err := os.Remove(filepath.Join(home, ".saferm", "archive", uuid)); err != nil {
		t.Fatalf("removing the archived copy: %v", err)
	}

	got := infoStatus(t, home, uuid)
	if got == "restorable" {
		t.Fatalf("a row whose archived copy is gone must not be reported as restorable")
	}
	if !strings.Contains(got, "names nothing") {
		t.Errorf("the status must say the row names no archived copy, got %q", got)
	}
	if !strings.Contains(got, "purge") {
		t.Errorf("the status must say what clears the row, got %q", got)
	}
}

// And purging such a row is not an error: there is simply nothing to destroy,
// which is said once, on stderr, and the row is stamped as purged like any
// other.
func TestPurge_SaysWhenTheArchivedCopyWasAlreadyGone(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "already-gone.txt", "content\n")

	_, uuid := deleteOne(t, home, target)
	if err := os.Remove(filepath.Join(home, ".saferm", "archive", uuid)); err != nil {
		t.Fatalf("removing the archived copy: %v", err)
	}

	_, stderr, code := runSaferm(t, home, "--approve-consequential", "purge", uuid)
	if code != 0 {
		t.Fatalf("purging a row whose copy is already gone must succeed, got %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "already gone") {
		t.Errorf("the purge must say the archived copy was already gone, got: %q", stderr)
	}

	if got := infoStatus(t, home, uuid); !strings.HasPrefix(got, "purged at ") {
		t.Errorf("the row must still be stamped as purged, got %q", got)
	}
}
