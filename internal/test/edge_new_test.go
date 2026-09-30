package test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

func TestDelete_ReadOnlyFile(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	// Create a file and make it read-only.
	filePath := filepath.Join(workDir, "readonly.txt")
	if err := os.WriteFile(filePath, []byte("read only content"), 0644); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	if err := os.Chmod(filePath, 0444); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// saferm should still archive it (parent dir is writable, so rename works).
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "readonly test", filePath)
	if code != 0 {
		t.Fatalf("delete read-only file failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify file is gone.
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatal("read-only file should be gone after delete")
	}

	// Undelete and verify content.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete failed (exit %d): stderr=%q", code, stderr)
	}

	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if string(got) != "read only content" {
		t.Fatalf("content mismatch: got %q", string(got))
	}
}

func TestDelete_LargeFile(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	// Create a 10MB file with random content.
	filePath := filepath.Join(workDir, "largefile.bin")
	data := make([]byte, 10*1024*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("generating random data: %v", err)
	}
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		t.Fatalf("writing large file: %v", err)
	}

	// Compute hash before deletion.
	originalHash := sha256.Sum256(data)
	originalHashHex := hex.EncodeToString(originalHash[:])

	// Delete.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "large file test", filePath)
	if code != 0 {
		t.Fatalf("delete large file failed (exit %d): stderr=%q", code, stderr)
	}

	// Get ID.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	// Verify info shows the correct hash.
	stdout, _, code = runSaferm(t, homeDir, "info", id)
	if code != 0 {
		t.Fatalf("info failed (exit %d)", code)
	}
	if !strings.Contains(stdout, originalHashHex) {
		t.Fatalf("info should show correct hash %s:\n%s", originalHashHex, stdout)
	}

	// Undelete and verify hash matches.
	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete large file failed (exit %d): stderr=%q", code, stderr)
	}

	restoredData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	restoredHash := sha256.Sum256(restoredData)
	restoredHashHex := hex.EncodeToString(restoredHash[:])

	if restoredHashHex != originalHashHex {
		t.Fatalf("hash mismatch after restore: got %s, want %s", restoredHashHex, originalHashHex)
	}
}

func TestDelete_DeeplyNestedDir(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	// Create 5 levels of nesting.
	deepDir := filepath.Join(workDir, "level1", "level2", "level3", "level4", "level5")
	if err := os.MkdirAll(deepDir, 0755); err != nil {
		t.Fatalf("creating deep dir: %v", err)
	}

	// Create files at various levels.
	files := map[string]string{
		filepath.Join(workDir, "level1", "f1.txt"):                                         "level 1",
		filepath.Join(workDir, "level1", "level2", "f2.txt"):                               "level 2",
		filepath.Join(workDir, "level1", "level2", "level3", "f3.txt"):                     "level 3",
		filepath.Join(workDir, "level1", "level2", "level3", "level4", "f4.txt"):           "level 4",
		filepath.Join(workDir, "level1", "level2", "level3", "level4", "level5", "f5.txt"): "level 5",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	// Delete the top-level dir.
	topDir := filepath.Join(workDir, "level1")
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "deep dir test", topDir)
	if code != 0 {
		t.Fatalf("delete deep dir failed (exit %d): stderr=%q", code, stderr)
	}

	// Get ID and undelete.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete deep dir failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify all files restored with correct content.
	for path, expectedContent := range files {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading %s: %v", path, err)
			continue
		}
		if string(got) != expectedContent {
			t.Errorf("%s: got %q, want %q", path, string(got), expectedContent)
		}
	}
}

func TestDelete_EmptyDirectory(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	emptyDir := filepath.Join(workDir, "emptydir")
	if err := os.MkdirAll(emptyDir, 0755); err != nil {
		t.Fatalf("creating empty dir: %v", err)
	}

	// Delete empty directory.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "empty dir test", emptyDir)
	if code != 0 {
		t.Fatalf("delete empty dir failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify gone.
	if _, err := os.Stat(emptyDir); !os.IsNotExist(err) {
		t.Fatal("empty dir should be gone after delete")
	}

	// Get ID and undelete.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete empty dir failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify directory is restored.
	info, err := os.Stat(emptyDir)
	if err != nil {
		t.Fatalf("restored dir stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("restored path should be a directory")
	}
}

func TestDelete_DirWithSymlinks(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	// Create a directory with a symlink inside.
	dirPath := filepath.Join(workDir, "linkdir")
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		t.Fatalf("creating dir: %v", err)
	}

	// Create a real file and a symlink to it.
	realFile := filepath.Join(dirPath, "real.txt")
	if err := os.WriteFile(realFile, []byte("real content"), 0644); err != nil {
		t.Fatalf("writing real file: %v", err)
	}
	linkFile := filepath.Join(dirPath, "link.txt")
	if err := os.Symlink("real.txt", linkFile); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}

	// Delete the directory.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "symlink dir test", dirPath)
	if code != 0 {
		t.Fatalf("delete dir with symlinks failed (exit %d): stderr=%q", code, stderr)
	}

	// Get ID and undelete.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete dir with symlinks failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify real file restored.
	got, err := os.ReadFile(realFile)
	if err != nil {
		t.Fatalf("reading real file: %v", err)
	}
	if string(got) != "real content" {
		t.Fatalf("real file content mismatch: got %q", string(got))
	}

	// Verify symlink preserved.
	linkTarget, err := os.Readlink(linkFile)
	if err != nil {
		t.Fatalf("reading symlink: %v", err)
	}
	if linkTarget != "real.txt" {
		t.Fatalf("symlink target mismatch: got %q, want %q", linkTarget, "real.txt")
	}
}

func TestDelete_SpecialCharsInPath(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	// Filename with parens, brackets, ampersand.
	specialName := "file (1) [copy] & backup.txt"
	content := "special chars content"
	filePath := testutil.CreateTempFile(t, workDir, specialName, content)

	// Delete.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "special chars test", filePath)
	if code != 0 {
		t.Fatalf("delete special chars file failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify gone.
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatal("special chars file should be gone")
	}

	// Get ID and undelete.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	_, stderr, code = runSaferm(t, homeDir, "undelete", id)
	if code != 0 {
		t.Fatalf("undelete special chars file failed (exit %d): stderr=%q", code, stderr)
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

func TestPurge_VerifyArchiveDeleted(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	filePath := testutil.CreateTempFile(t, workDir, "purge-archive.txt", "to be purged from disk")

	// Delete.
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "purge archive test", filePath)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}

	// Get the UUID from info.
	stdout, _, _ := runSaferm(t, homeDir, "list")
	id := parseFirstID(t, stdout)

	stdout, _, code = runSaferm(t, homeDir, "info", id)
	if code != 0 {
		t.Fatalf("info failed (exit %d)", code)
	}

	// Parse UUID from info output.
	var uuid string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "UUID:") {
			uuid = strings.TrimSpace(strings.TrimPrefix(line, "UUID:"))
			break
		}
	}
	if uuid == "" {
		t.Fatalf("could not parse UUID from info output:\n%s", stdout)
	}

	// Verify archive file exists before purge.
	archivePath := filepath.Join(homeDir, ".saferm", "archive", uuid)
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("archive file should exist before purge: %v", err)
	}

	// Purge.
	_, stderr, code = runSaferm(t, homeDir, "--approve-consequential", "purge", id)
	if code != 0 {
		t.Fatalf("purge failed (exit %d): stderr=%q", code, stderr)
	}

	// Verify archive file is gone from disk.
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatal("archive file should not exist after purge")
	}
}
