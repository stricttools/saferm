package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// saferm's three mutating commands create their own state directory (the
// SAFERM_HOME base, the archive and the database's parent) before doing
// anything else. That creation used to be a raw MkdirAll, outside the effects
// handle: a preview that promises to touch nothing made three directories on
// its way to saying so, and never mentioned them. They are now minted on the
// handle like every other mutation -- declared in the would-do log, performed
// only in a real run.
//
// freshHome returns a home directory with no .saferm inside it, so these tests
// exercise the first-ever run. testutil.SetupTestEnv deliberately pre-creates
// the tree and cannot see this.
func freshHome(t *testing.T) string {
	t.Helper()
	testutil.Isolate(t)
	return t.TempDir()
}

func stateDir(home string) string { return filepath.Join(home, ".saferm") }

func TestDryRunDeleteCreatesNoStateDirectory(t *testing.T) {
	home := freshHome(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "doomed.txt", "content\n")

	stdout, stderr, code := runSaferm(t, home, "--dry-run", "delete", "--on-error", "abort", "--description", "preview", target)
	if code != 0 {
		t.Fatalf("first-ever dry-run delete failed (%d): %s", code, stderr)
	}
	if _, err := os.Stat(stateDir(home)); !os.IsNotExist(err) {
		t.Errorf("a dry run created %s (err=%v)", stateDir(home), err)
	}
	log := wouldDoLog(stdout)
	if !strings.Contains(log, "mkdir: "+stateDir(home)) {
		t.Errorf("the would-do log must declare the state-directory creation, got: %s", log)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the dry run archived %s for real: %v", target, err)
	}
}

func TestDryRunPurgeCreatesNoStateDirectory(t *testing.T) {
	home := freshHome(t)

	stdout, stderr, code := runSaferm(t, home, "--dry-run", "purge", "--all")
	if code != 0 {
		t.Fatalf("first-ever dry-run purge failed (%d): %s stdout=%s", code, stderr, stdout)
	}
	if _, err := os.Stat(stateDir(home)); !os.IsNotExist(err) {
		t.Errorf("a dry run created %s (err=%v)", stateDir(home), err)
	}
}

func TestDryRunUndeleteCreatesNoStateDirectory(t *testing.T) {
	home := freshHome(t)

	_, _, code := runSaferm(t, home, "--dry-run", "undelete", "1")
	if code == 0 {
		t.Errorf("undeleting from an archive that does not exist must fail, got exit 0")
	}
	if _, err := os.Stat(stateDir(home)); !os.IsNotExist(err) {
		t.Errorf("a dry run created %s (err=%v)", stateDir(home), err)
	}
}

// The read-only commands never create the tree at all -- they cannot, the
// effects handle refuses a mutation from a read_only command -- so on a fresh
// machine they meet a database file that is not there. SQLite's "unable to open
// database file" is the wrong answer to "what have I deleted?"; the right one
// is "nothing".
func TestListOnAFreshMachineReportsAnEmptyArchive(t *testing.T) {
	home := freshHome(t)

	stdout, stderr, code := runSaferm(t, home, "list")
	if code != 0 {
		t.Fatalf("list on a fresh machine must succeed, got %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "No archived items found.") {
		t.Errorf("list must report an empty archive, got: %q / %q", stdout, stderr)
	}
	if _, err := os.Stat(stateDir(home)); !os.IsNotExist(err) {
		t.Errorf("a read-only list created %s (err=%v)", stateDir(home), err)
	}
}

func TestInfoOnAFreshMachineReportsNoSuchRecord(t *testing.T) {
	home := freshHome(t)

	_, stderr, code := runSaferm(t, home, "info", "1")
	if code != 3 { // ExitFileNotFound
		t.Fatalf("info on a fresh machine must exit 3, got %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "no record with ID 1") {
		t.Errorf("info must say the record does not exist, got: %q", stderr)
	}
	if _, err := os.Stat(stateDir(home)); !os.IsNotExist(err) {
		t.Errorf("a read-only info created %s (err=%v)", stateDir(home), err)
	}
}

// The real run still creates the tree -- the point is that it is declared, not
// that it stopped happening.
func TestRealDeleteCreatesTheStateDirectory(t *testing.T) {
	home := freshHome(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "doomed.txt", "content\n")

	if _, stderr, code := runSaferm(t, home, "delete", "--on-error", "abort", "--description", "for real", target); code != 0 {
		t.Fatalf("first-ever delete failed (%d): %s", code, stderr)
	}
	for _, dir := range []string{
		stateDir(home),
		filepath.Join(stateDir(home), "archive"),
		filepath.Join(stateDir(home), "db"),
	} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("a real delete did not create %s: %v", dir, err)
		}
	}
	stdout, _, _ := runSaferm(t, home, "list")
	if !strings.Contains(stdout, "doomed.txt") {
		t.Errorf("the real delete did not archive the file; list: %s", stdout)
	}
}
