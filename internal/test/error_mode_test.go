package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// A multi-path delete has two defensible behaviours when one path fails, and
// picking one silently is the wrong answer for both audiences: a script wants
// the whole batch to stop, an interactive cleanup wants the rest to proceed.
// `--on-error` is therefore mandatory with no default, and the caller states
// which one it wants.
//
// Whichever mode is chosen, the identifiers of everything already archived are
// on stdout by the time the failure is reported -- the record's row was
// committed at the moment it was archived, and losing the printed id and uuid
// was what made a partial batch hard to reconstruct.

// threePathsWithABadMiddle lays out three targets where the second does not
// exist, and returns them in argument order.
func threePathsWithABadMiddle(t *testing.T, work string) (first, missing, third string) {
	t.Helper()
	first = testutil.CreateTempFile(t, work, "first.txt", "one\n")
	missing = filepath.Join(work, "does-not-exist.txt")
	third = testutil.CreateTempFile(t, work, "third.txt", "three\n")
	return first, missing, third
}

func TestDelete_ErrorModeIsMandatory(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "mandatory.txt", "content\n")

	_, stderr, code := runSaferm(t, home, "delete", "--description", "no error mode", target)
	if code == 0 {
		t.Fatalf("delete without --on-error must fail; got exit 0")
	}
	if !strings.Contains(stderr, "on-error") {
		t.Errorf("the refusal must name the missing flag, got: %q", stderr)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("a refused delete must not have touched the target: %v", err)
	}

	// The two valid values are one --help away, and named there.
	stdout, stderr, code := runSaferm(t, home, "delete", "--help")
	if code != 0 {
		t.Fatalf("delete --help failed (%d): %s", code, stderr)
	}
	if !strings.Contains(stdout, "abort") || !strings.Contains(stdout, "continue") {
		t.Errorf("delete --help must name both error modes:\n%s", stdout)
	}
}

func TestDelete_OnErrorAbort_StopsAtTheFirstFailure(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	first, missing, third := threePathsWithABadMiddle(t, work)

	stdout, stderr, code := runSaferm(t, home, "delete", "--on-error", "abort",
		"--description", "abort mode", first, missing, third)
	if code == 0 {
		t.Fatalf("a failing path must fail the command; stdout=%q", stdout)
	}
	if !strings.Contains(stderr, missing) {
		t.Errorf("the failing path must be named, got: %q", stderr)
	}

	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Errorf("the path before the failure should have been archived (err=%v)", err)
	}
	if _, err := os.Stat(third); err != nil {
		t.Errorf("abort mode must not archive anything after the failure: %v", err)
	}

	// The identifiers of what WAS archived survive the abort.
	lines := parseArchivedLines(t, stdout)
	if len(lines) != 1 {
		t.Fatalf("expected the archived path to be named with both identifiers, got %d lines in:\n%s", len(lines), stdout)
	}
	if lines[0][2] != first {
		t.Errorf("identifier line names %q, want %q", lines[0][2], first)
	}
	infoOut, stderr, code := runSaferm(t, home, "info", lines[0][1])
	if code != 0 {
		t.Fatalf("info by the uuid printed before the abort failed (%d): %s", code, stderr)
	}
	if !strings.Contains(infoOut, "ID:            "+lines[0][0]) {
		t.Errorf("the printed id and uuid name different records:\n%s", infoOut)
	}
}

func TestDelete_OnErrorContinue_ArchivesTheRestAndStillFails(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	first, missing, third := threePathsWithABadMiddle(t, work)

	stdout, stderr, code := runSaferm(t, home, "delete", "--on-error", "continue",
		"--description", "continue mode", first, missing, third)
	if code == 0 {
		t.Fatalf("continue mode must still exit non-zero when a path failed; stdout=%q", stdout)
	}
	if !strings.Contains(stderr, missing) {
		t.Errorf("the failing path must be named, got: %q", stderr)
	}

	for _, p := range []string{first, third} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("continue mode must archive %s despite the earlier failure (err=%v)", p, err)
		}
	}

	lines := parseArchivedLines(t, stdout)
	if len(lines) != 2 {
		t.Fatalf("expected both surviving paths named with identifiers, got %d lines in:\n%s", len(lines), stdout)
	}
	if lines[0][2] != first || lines[1][2] != third {
		t.Errorf("identifier lines name %v, want %s and %s", lines, first, third)
	}
	if !strings.Contains(stderr, "1 of 3") {
		t.Errorf("continue mode must report how many paths failed, got: %q", stderr)
	}
}
