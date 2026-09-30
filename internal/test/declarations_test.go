package test

import (
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// What every flag and every positional argument says about its own absence is a
// declaration now, and `purge`'s selection rule is a declaration too. These
// tests hold the parts of that a caller can see from outside the binary: the
// refusals the framework gives instead of the guards saferm used to hand-write,
// and the fallbacks the optional switches name in their own help.

// `purge` with nothing selected used to be refused by a guard inside the
// handler, printing saferm's own sentence and returning ExitUsage. The rule is
// now the declared at-least-one constraint, so the parser refuses the command
// line before dispatch, names the constraint, and exits 1.
func TestPurge_EmptySelectionIsRefusedByTheDeclaration(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)

	_, stderr, code := runSaferm(t, homeDir, "--approve-consequential", "purge")
	if code != 1 {
		t.Errorf("an unselected purge is a parse error and exits 1, got %d: %q", code, stderr)
	}
	if !strings.Contains(stderr, `constraint "purge-selection"`) {
		t.Errorf("the refusal must name the constraint a reader can find in --help, got: %q", stderr)
	}
	for _, member := range []string{"targets", "--older-than", "--larger-than", "--all"} {
		if !strings.Contains(stderr, member) {
			t.Errorf("the refusal must name %s among the selections, got: %q", member, stderr)
		}
	}
	// The guard that used to print this is gone with the declaration that
	// replaced it; leaving both would mean two sentences for one refusal.
	if strings.Contains(stderr, "specify record UUIDs or numeric IDs") {
		t.Errorf("the hand-written selection guard is deleted, got: %q", stderr)
	}
}

// `--all` elects on `true` alone, so declining it selects nothing rather than
// selecting everything -- and the framework says which of the two happened.
func TestPurge_DecliningAllSelectsNothing(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)

	_, stderr, code := runSaferm(t, homeDir, "--approve-consequential", "purge", "--no-all")
	if code != 1 {
		t.Errorf("--no-all selects nothing, so the selection is still empty and exits 1, got %d: %q", code, stderr)
	}
	if !strings.Contains(stderr, "--no-all declines an option") {
		t.Errorf("the refusal must say that declining is not choosing, got: %q", stderr)
	}
}

// The two selection strings declare Optional(), not Default(""), so an empty
// string is a value the invocation supplied rather than an absence. It engages
// the selection rule and is then refused by the duration parser, instead of
// silently meaning "no age filter" as the sentinel did.
func TestPurge_EmptyOlderThanIsASuppliedValue(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)

	_, stderr, code := runSaferm(t, homeDir, "--approve-consequential", "purge", "--older-than", "")
	if code == 1 {
		t.Fatalf("an empty --older-than engages the selection rule, so it is not an empty selection: %q", stderr)
	}
	if code != 2 {
		t.Errorf("an unparseable duration is a usage error (exit 2), got %d: %q", code, stderr)
	}
	if !strings.Contains(stderr, "duration") {
		t.Errorf("the refusal must come from the duration parser, got: %q", stderr)
	}
}

// `delete`'s switches carry no value default -- the framework forbids one on a
// mutating command -- so each names its fallback in its help and behaves on
// absence exactly as it did when the fallback was a declared default.
func TestDelete_OptionalSwitchesKeepTheirFallbacks(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	// --recursive absent: a directory is still refused.
	dir := t.TempDir()
	testutil.CreateTempFile(t, dir, "inside.txt", "x\n")
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "no -r", dir)
	if code == 0 {
		t.Errorf("without --recursive a directory is refused, got exit 0: %q", stderr)
	}

	// --ignore-missing absent: a missing path is still an error.
	missing := workDir + "/nothing-here.txt"
	_, stderr, code = runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "no -f", missing)
	if code == 0 {
		t.Errorf("without --ignore-missing a missing path is an error, got exit 0: %q", stderr)
	}

	// --ignore-missing present: still skipped, and the run still succeeds.
	_, stderr, code = runSaferm(t, homeDir, "delete", "--on-error", "abort", "-f", "--description", "with -f", missing)
	if code != 0 {
		t.Errorf("--ignore-missing still skips a missing path, got exit %d: %q", code, stderr)
	}
}

// Every declaration renders exactly one presence part, and `purge`'s rule
// renders in a block of its own. A caller reading --help can see which flags
// must be typed, which may be omitted, and what the selection rule is -- none of
// which the help said before.
func TestHelp_RendersPresenceAndTheSelectionConstraint(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)

	deleteHelp, _, code := runSaferm(t, homeDir, "delete", "--help")
	if code != 0 {
		t.Fatalf("delete --help failed (exit %d)", code)
	}
	for _, want := range []string{
		"--description",
		"[required]",
		"[optional]",
		"files...",
	} {
		if !strings.Contains(deleteHelp, want) {
			t.Errorf("delete --help must show %q, got:\n%s", want, deleteHelp)
		}
	}
	// The two --on-error values carry their own help records, so a reader is
	// told what each one does rather than only that it exists.
	for _, want := range []string{"stop at the first path", "archive the remaining paths"} {
		if !strings.Contains(deleteHelp, want) {
			t.Errorf("delete --help must describe each --on-error value, missing %q", want)
		}
	}

	purgeHelp, _, code := runSaferm(t, homeDir, "purge", "--help")
	if code != 0 {
		t.Fatalf("purge --help failed (exit %d)", code)
	}
	if !strings.Contains(purgeHelp, "Constraints:") {
		t.Errorf("purge --help must render the constraint block, got:\n%s", purgeHelp)
	}
	if !strings.Contains(purgeHelp, "purge-selection") {
		t.Errorf("purge --help must name the constraint its violation prints, got:\n%s", purgeHelp)
	}

	undeleteHelp, _, code := runSaferm(t, homeDir, "undelete", "--help")
	if code != 0 {
		t.Fatalf("undelete --help failed (exit %d)", code)
	}
	// --on-conflict's two values carry help records too.
	for _, want := range []string{"replace what is standing", "refuse the restore"} {
		if !strings.Contains(undeleteHelp, want) {
			t.Errorf("undelete --help must describe each --on-conflict value, missing %q", want)
		}
	}
}
