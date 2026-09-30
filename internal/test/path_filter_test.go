package test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// saferm records absolute original paths, so every archived path is several
// directory levels deep. A filter whose `*` stops at a separator can therefore
// only ever name the immediate children of a directory -- which is why the
// documented `--path "/home/*"` matched nothing anyone had actually archived.
// `*` spans separators.

func TestList_PathFilter_MatchesNestedPaths(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	nested := testutil.CreateTempFile(t, workDir, "a/b/c/buried.txt", "buried")
	shallow := testutil.CreateTempFile(t, workDir, "surface.txt", "surface")

	for _, f := range []string{nested, shallow} {
		if _, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "nested filter test", f); code != 0 {
			t.Fatalf("delete %s failed (exit %d): stderr=%q", f, code, stderr)
		}
	}

	// The shape of the documented example: a directory followed by a single
	// star. It must reach everything under that directory, at any depth.
	pattern := filepath.Join(workDir, "*")
	stdout, stderr, code := runSaferm(t, homeDir, "list", "--path", pattern)
	if code != 0 {
		t.Fatalf("list --path failed (exit %d): stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "buried.txt") {
		t.Fatalf("pattern %q should match the nested path, output was:\n%s", pattern, stdout)
	}
	if !strings.Contains(stdout, "surface.txt") {
		t.Fatalf("pattern %q should match the shallow path too, output was:\n%s", pattern, stdout)
	}
}

func TestList_PathFilter_MatchesAcrossInterveningDirectories(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	target := testutil.CreateTempFile(t, workDir, "one/two/build/artifact.o", "obj")
	other := testutil.CreateTempFile(t, workDir, "one/two/src/main.go", "src")

	for _, f := range []string{target, other} {
		if _, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "subtree filter test", f); code != 0 {
			t.Fatalf("delete %s failed (exit %d): stderr=%q", f, code, stderr)
		}
	}

	pattern := filepath.Join(workDir, "*", "build", "*")
	stdout, stderr, code := runSaferm(t, homeDir, "list", "--path", pattern)
	if code != 0 {
		t.Fatalf("list --path failed (exit %d): stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "artifact.o") {
		t.Fatalf("pattern %q should match the build artifact, output was:\n%s", pattern, stdout)
	}
	if strings.Contains(stdout, "main.go") {
		t.Fatalf("pattern %q must not match the source file, output was:\n%s", pattern, stdout)
	}
}

// A malformed glob is still a usage error, not a silent empty result.
func TestList_PathFilter_MalformedPatternIsUsageError(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	f := testutil.CreateTempFile(t, workDir, "x.txt", "x")
	if _, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "bad glob test", f); code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}

	_, stderr, code := runSaferm(t, homeDir, "list", "--path", "[")
	if code != 2 {
		t.Fatalf("expected exit 2 for a malformed glob, got %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stderr, "invalid glob pattern") {
		t.Fatalf("expected an invalid-glob message, got: %q", stderr)
	}
}

// The pattern is checked before anything is read, so a malformed one is a
// usage error whatever the archive holds -- including nothing at all, where
// the old filter never ran the matcher and answered "No archived items found."
func TestList_PathFilter_MalformedPatternIsUsageErrorOnAnEmptyArchive(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)

	for _, pattern := range []string{"[", "/home/*/[x"} {
		_, stderr, code := runSaferm(t, homeDir, "list", "--path", pattern)
		if code != 2 {
			t.Fatalf("pattern %q: expected exit 2 on an empty archive, got %d (stderr=%q)", pattern, code, stderr)
		}
		if !strings.Contains(stderr, "invalid glob pattern") {
			t.Fatalf("pattern %q: expected an invalid-glob message, got: %q", pattern, stderr)
		}
	}
}
