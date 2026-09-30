package test

import (
	"os"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// --quiet is framework-owned and, until now, saferm read it nowhere: every
// command printed its summaries and progress lines regardless. These tests pin
// the boundary the flag draws.
//
// Suppressed: the chatter a caller can lose without losing information -- the
// counted summaries, the per-item --verbose progress, "Nothing to purge.", the
// restore confirmation.
//
// Never suppressed: the outputs that ARE the command. `list` and `info` print
// tables that a quiet caller is asking for, not narrating over; `purge
// --dry-run` prints the table of what it would destroy; `purge` prints the
// per-record listing of what it is destroying; and errors and warnings go to
// stderr, which --quiet does not touch at all.

func seedArchive(t *testing.T, home, work, name string) string {
	t.Helper()
	target := testutil.CreateTempFile(t, work, name, "content\n")
	if _, stderr, code := runSaferm(t, home, "delete", "--on-error", "abort", "--description", "seed", target); code != 0 {
		t.Fatalf("seeding delete failed (%d): %s", code, stderr)
	}
	return target
}

func TestQuietSuppressesDeleteSummary(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "quiet.txt", "content\n")

	stdout, stderr, code := runSaferm(t, home, "--quiet", "delete", "--on-error", "abort", "--description", "quiet delete", target)
	if code != 0 {
		t.Fatalf("quiet delete failed (%d): %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("--quiet delete must print nothing on stdout, got: %q", stdout)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("--quiet delete did not archive the file (err=%v)", err)
	}
}

// --quiet dominates --verbose, the same way it does in the framework's own
// Context.Debug. Asking for both is asking for silence.
func TestQuietDominatesVerbose(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "loud.txt", "content\n")

	stdout, stderr, code := runSaferm(t, home,
		"--quiet", "--verbose", "delete", "--on-error", "abort", "--description", "quiet beats verbose", target)
	if code != 0 {
		t.Fatalf("quiet+verbose delete failed (%d): %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("--quiet must dominate --verbose, got stdout: %q", stdout)
	}
}

func TestQuietSuppressesUndeleteConfirmation(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := seedArchive(t, home, work, "restore.txt")

	stdout, stderr, code := runSaferm(t, home, "--quiet", "undelete", "1")
	if code != 0 {
		t.Fatalf("quiet undelete failed (%d): %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("--quiet undelete must print nothing on stdout, got: %q", stdout)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("--quiet undelete did not restore the file: %v", err)
	}
}

func TestQuietSuppressesNothingToPurge(t *testing.T) {
	home := testutil.SetupTestEnv(t)

	stdout, stderr, code := runSaferm(t, home, "--quiet", "--approve-consequential", "purge", "--all")
	if code != 0 {
		t.Fatalf("quiet purge of an empty archive failed (%d): %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("--quiet must suppress \"Nothing to purge.\", got: %q", stdout)
	}
}

// The purge summary goes; the per-record listing of what is being destroyed
// stays. The listing is the audit record of an irreversible act, not chatter.
func TestQuietSuppressesPurgeSummaryButNotTheListing(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := seedArchive(t, home, work, "doomed.txt")

	stdout, stderr, code := runSaferm(t, home, "--quiet", "--approve-consequential", "purge", "--all")
	if code != 0 {
		t.Fatalf("quiet purge failed (%d): %s", code, stderr)
	}
	if strings.Contains(stdout, "item(s) purged") {
		t.Errorf("--quiet must suppress the purge summary, got: %q", stdout)
	}
	if !strings.Contains(stdout, target) {
		t.Errorf("--quiet must NOT suppress the listing of what is destroyed, got: %q", stdout)
	}
}

func TestQuietNeverSuppressesListOutput(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedArchive(t, home, work, "listed.txt")

	// The table truncates long paths from the left, so assert on the basename.
	stdout, stderr, code := runSaferm(t, home, "--quiet", "list")
	if code != 0 {
		t.Fatalf("quiet list failed (%d): %s", code, stderr)
	}
	if !strings.Contains(stdout, "listed.txt") {
		t.Errorf("--quiet list must still print the table, got: %q", stdout)
	}

	// The empty-archive line is list's answer, not narration.
	emptyHome := testutil.SetupTestEnv(t)
	stdout, _, code = runSaferm(t, emptyHome, "--quiet", "list")
	if code != 0 {
		t.Fatalf("quiet list on an empty archive failed (%d)", code)
	}
	if !strings.Contains(stdout, "No archived items found.") {
		t.Errorf("--quiet list must still answer on an empty archive, got: %q", stdout)
	}
}

func TestQuietNeverSuppressesInfoOutput(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedArchive(t, home, work, "inspected.txt")

	stdout, stderr, code := runSaferm(t, home, "--quiet", "info", "1")
	if code != 0 {
		t.Fatalf("quiet info failed (%d): %s", code, stderr)
	}
	if !strings.Contains(stdout, "Original Path:") {
		t.Errorf("--quiet info must still print the record, got: %q", stdout)
	}
}

func TestQuietNeverSuppressesTheDryRunPreview(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := seedArchive(t, home, work, "previewed.txt")

	stdout, stderr, code := runSaferm(t, home, "--quiet", "--dry-run", "purge", "--all")
	if code != 0 {
		t.Fatalf("quiet dry-run purge failed (%d): %s", code, stderr)
	}
	if !strings.Contains(stdout, "Would purge 1 item(s)") {
		t.Errorf("--quiet must not suppress the dry-run table, got: %q", stdout)
	}
	if !strings.Contains(stdout, "previewed.txt") {
		t.Errorf("the dry-run table must still name the record, got: %q", stdout)
	}
	if wouldDoLog(stdout) == "" {
		t.Errorf("--quiet must not suppress the framework would-do log, got: %q", stdout)
	}
	if _, err := os.Stat(target); err == nil {
		t.Errorf("a dry-run purge restored nothing, but %s exists", target)
	}
}
