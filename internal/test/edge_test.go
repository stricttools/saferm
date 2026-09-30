package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

func TestDelete_NonexistentFile(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	nonexistent := filepath.Join(workDir, "does-not-exist.txt")

	// Without -f: should fail.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "nonexist", nonexistent)
	if code == 0 {
		t.Fatal("delete of nonexistent file without -f should fail")
	}
	if !strings.Contains(stderr, "not found") && !strings.Contains(stderr, "no such file") &&
		!strings.Contains(stderr, "archiving") {
		t.Logf("stderr: %q (exit %d) -- acceptable error for nonexistent file", stderr, code)
	}

	// With -f: should succeed (no-op).
	_, stderr, code = runSaferm(t, homeDir, "delete", "--on-error", "abort", "-f", "--description", "nonexist", nonexistent)
	if code != 0 {
		t.Fatalf("delete -f of nonexistent file should succeed, got exit %d: stderr=%q", code, stderr)
	}
}

func TestDelete_FileWithSpaces(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	content := "file with spaces in the name"
	filePath := testutil.CreateTempFile(t, workDir, "my file with spaces.txt", content)

	// Delete.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "spaces test", filePath)
	if code != 0 {
		t.Fatalf("delete file with spaces failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify gone.
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatal("file with spaces should be gone")
	}

	// Get ID and undelete.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete file with spaces failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify content restored.
	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if string(got) != content {
		t.Fatalf("content mismatch: got %q, want %q", string(got), content)
	}
}

func TestDelete_FileWithUnicode(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	content := "Unicode filename content"
	filePath := testutil.CreateTempFile(t, workDir, "日本語.txt", content)

	// Delete.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "unicode test", filePath)
	if code != 0 {
		t.Fatalf("delete unicode file failed (exit %d): stderr=%q", code, stderr)
	}

	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatal("unicode file should be gone")
	}

	// Get ID and undelete.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete unicode file failed (exit %d): stderr=%q", code, stderr)
	}

	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading restored unicode file: %v", err)
	}
	if string(got) != content {
		t.Fatalf("content mismatch: got %q, want %q", string(got), content)
	}
}

func TestDelete_EmptyFile(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "empty.txt", "")

	// Delete.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "empty file test", filePath)
	if code != 0 {
		t.Fatalf("delete empty file failed (exit %d): stderr=%q", code, stderr)
	}

	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatal("empty file should be gone")
	}

	// Get ID and undelete.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete empty file failed (exit %d): stderr=%q", code, stderr)
	}

	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading restored empty file: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty file, got %d bytes", len(got))
	}
}

func TestDelete_Symlink(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	// Create a target file and a symlink to it.
	targetContent := "symlink target content"
	targetPath := testutil.CreateTempFile(t, workDir, "target.txt", targetContent)
	symlinkPath := filepath.Join(workDir, "link.txt")
	if err := os.Symlink(targetPath, symlinkPath); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}

	// Delete the symlink (it's inside a directory, so we delete it as a file within a dir).
	// Actually delete the symlink as a single file -- saferm should archive the link itself.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "symlink test", symlinkPath)
	if code != 0 {
		t.Fatalf("delete symlink failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify symlink is gone but target still exists.
	if _, err := os.Lstat(symlinkPath); !os.IsNotExist(err) {
		t.Fatal("symlink should be gone")
	}
	if _, err := os.Stat(targetPath); err != nil {
		t.Fatal("target file should still exist")
	}

	// Verify target content is untouched.
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if string(got) != targetContent {
		t.Fatalf("target content changed: got %q", string(got))
	}

	// Undelete the symlink and verify it's restored as a symlink.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete symlink failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify restored path is a symlink (not a regular file).
	info, err := os.Lstat(symlinkPath)
	if err != nil {
		t.Fatalf("lstat restored symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("restored path should be a symlink")
	}

	// Verify symlink points to the correct target.
	linkTarget, err := os.Readlink(symlinkPath)
	if err != nil {
		t.Fatalf("readlink restored symlink: %v", err)
	}
	if linkTarget != targetPath {
		t.Fatalf("symlink target mismatch: got %q, want %q", linkTarget, targetPath)
	}
}
