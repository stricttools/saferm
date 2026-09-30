package test

import (
	"os"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// The env denylist is a redaction control: a pattern that does not compile used
// to be dropped and the deletion proceeded, so a typo turned redaction off for
// whatever that pattern covered and said nothing. There is no safe way to
// continue past it -- the caller asked for a redaction saferm cannot perform.

func TestDelete_UncompilableExcludePattern_IsHardError(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "denylist.txt", "content")

	// RE2 has no lookahead, so this is exactly the shape of typo that used to
	// disable a redaction silently.
	_, stderr, code := runSaferm(t, homeDir,
		"--exclude-env-patterns", "(?i)key(?!BOARD)",
		"delete", "--on-error", "abort", "--description", "uncompilable pattern test", filePath,
	)
	if code == 0 {
		t.Fatalf("an uncompilable exclude pattern must fail the command; got exit 0, stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "(?i)key(?!BOARD)") {
		t.Fatalf("the error must name the offending pattern, got: %q", stderr)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("nothing should have been archived; the file is gone: %v", err)
	}
}

func TestDelete_UncompilableExcludePattern_FailsUnderDryRunToo(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "denylist-dry.txt", "content")

	_, stderr, code := runSaferm(t, homeDir,
		"--dry-run",
		"--exclude-env-patterns", "[unterminated",
		"delete", "--on-error", "abort", "--description", "uncompilable pattern dry run", filePath,
	)
	if code == 0 {
		t.Fatalf("an uncompilable exclude pattern must fail the preview too; got exit 0, stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "[unterminated") {
		t.Fatalf("the error must name the offending pattern, got: %q", stderr)
	}
}

// The valid case still works, and still redacts.
func TestDelete_ValidExcludePattern_StillRedacts(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "redacted.txt", "content")

	_, stderr, code := runSaferm(t, homeDir,
		"--exclude-env-patterns", "(?i)key",
		"delete", "--on-error", "abort", "--description", "valid pattern test", filePath,
	)
	if code != 0 {
		t.Fatalf("a valid exclude pattern should succeed, got exit %d: stderr=%q", code, stderr)
	}
}
