package archive

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// archiveNow is [Execute] followed by [RemoveSource] with nothing recorded in
// between, which is what these tests need and what no caller of the package
// wants: saferm's whole reason for splitting the two is to put the database
// insert between them. The package used to export this as Archive, kept alive
// by these tests alone.
func archiveNow(path string, archiveDir string, isRecursive bool) (*ArchiveResult, error) {
	p, err := NewPlan(path, archiveDir, isRecursive)
	if err != nil {
		return nil, err
	}
	result, err := Execute(p)
	if err != nil {
		return nil, err
	}
	if err := RemoveSource(p); err != nil {
		return nil, fmt.Errorf("removing original after archiving: %w", err)
	}
	return result, nil
}

func TestArchive_File(t *testing.T) {
	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	srcFile := filepath.Join(tmpDir, "hello.txt")

	content := []byte("hello, saferm!")
	if err := os.WriteFile(srcFile, content, 0644); err != nil {
		t.Fatal(err)
	}

	// Compute expected hash.
	h := sha256.Sum256(content)
	expectedHash := hex.EncodeToString(h[:])

	result, err := archiveNow(srcFile, archiveDir, false)
	if err != nil {
		t.Fatal(err)
	}

	// Original should be gone.
	if _, err := os.Stat(srcFile); !os.IsNotExist(err) {
		t.Error("original file still exists after archive")
	}

	// Archive entry should exist.
	archivedPath := filepath.Join(archiveDir, result.UUID)
	if _, err := os.Stat(archivedPath); err != nil {
		t.Errorf("archived file not found: %v", err)
	}

	// Verify hash.
	if result.Hash != expectedHash {
		t.Errorf("hash mismatch: got %s, want %s", result.Hash, expectedHash)
	}

	// Verify size.
	if result.Size != int64(len(content)) {
		t.Errorf("size mismatch: got %d, want %d", result.Size, len(content))
	}
}

func TestArchive_FileRestore(t *testing.T) {
	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	srcFile := filepath.Join(tmpDir, "hello.txt")
	restorePath := filepath.Join(tmpDir, "restored.txt")

	content := []byte("restore me please")
	if err := os.WriteFile(srcFile, content, 0644); err != nil {
		t.Fatal(err)
	}

	result, err := archiveNow(srcFile, archiveDir, false)
	if err != nil {
		t.Fatal(err)
	}

	if err := restoreNow(result.UUID, archiveDir, restorePath, KindFile, ""); err != nil {
		t.Fatal(err)
	}

	restored, err := os.ReadFile(restorePath)
	if err != nil {
		t.Fatal(err)
	}

	if string(restored) != string(content) {
		t.Errorf("restored content mismatch: got %q, want %q", restored, content)
	}
}

func TestArchive_Directory(t *testing.T) {
	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	srcDir := filepath.Join(tmpDir, "mydir")

	// Create directory structure.
	if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("aaa"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "sub", "b.txt"), []byte("bbb"), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := archiveNow(srcDir, archiveDir, true)
	if err != nil {
		t.Fatal(err)
	}

	// Original dir should be gone.
	if _, err := os.Stat(srcDir); !os.IsNotExist(err) {
		t.Error("original directory still exists after archive")
	}

	// .tar.zst should exist.
	archivedPath := filepath.Join(archiveDir, result.UUID+".tar.zst")
	if _, err := os.Stat(archivedPath); err != nil {
		t.Errorf("archived tar.zst not found: %v", err)
	}

	// Size should be the sum of file sizes (3 + 3 = 6).
	if result.Size != 6 {
		t.Errorf("size mismatch: got %d, want 6", result.Size)
	}
}

func TestArchive_DirectoryRestore(t *testing.T) {
	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	srcDir := filepath.Join(tmpDir, "mydir")
	restoreDir := filepath.Join(tmpDir, "restored")

	// Create directory structure.
	if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("file-a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "sub", "b.txt"), []byte("file-b"), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := archiveNow(srcDir, archiveDir, true)
	if err != nil {
		t.Fatal(err)
	}

	if err := restoreNow(result.UUID, archiveDir, restoreDir, KindDirectory, ""); err != nil {
		t.Fatal(err)
	}

	// Verify files restored correctly.
	content, err := os.ReadFile(filepath.Join(restoreDir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "file-a" {
		t.Errorf("a.txt content mismatch: got %q", content)
	}

	content, err = os.ReadFile(filepath.Join(restoreDir, "sub", "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "file-b" {
		t.Errorf("sub/b.txt content mismatch: got %q", content)
	}
}

func TestArchive_DirWithoutRecursive(t *testing.T) {
	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	srcDir := filepath.Join(tmpDir, "mydir")

	if err := os.Mkdir(srcDir, 0755); err != nil {
		t.Fatal(err)
	}

	_, err := archiveNow(srcDir, archiveDir, false)
	if err != ErrRecursiveRequired {
		t.Errorf("expected ErrRecursiveRequired, got: %v", err)
	}
}

func TestArchive_NonexistentFile(t *testing.T) {
	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")

	_, err := archiveNow(filepath.Join(tmpDir, "nonexistent"), archiveDir, false)
	if err != ErrFileNotFound {
		t.Errorf("expected ErrFileNotFound, got: %v", err)
	}
}

func TestArchive_Symlink(t *testing.T) {
	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	srcDir := filepath.Join(tmpDir, "mydir")
	restoreDir := filepath.Join(tmpDir, "restored")

	// Create a directory with a symlink inside.
	if err := os.Mkdir(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "target.txt"), []byte("target"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(srcDir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	result, err := archiveNow(srcDir, archiveDir, true)
	if err != nil {
		t.Fatal(err)
	}

	if err := restoreNow(result.UUID, archiveDir, restoreDir, KindDirectory, ""); err != nil {
		t.Fatal(err)
	}

	// Verify the symlink is still a symlink.
	linkPath := filepath.Join(restoreDir, "link.txt")
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("expected symlink, got regular file")
	}

	linkTarget, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	if linkTarget != "target.txt" {
		t.Errorf("symlink target mismatch: got %q, want %q", linkTarget, "target.txt")
	}
}

func TestHashFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.txt")

	content := []byte("known content for hashing")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	h := sha256.Sum256(content)
	expected := hex.EncodeToString(h[:])

	got, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if got != expected {
		t.Errorf("hash mismatch: got %s, want %s", got, expected)
	}
}

func TestGenerateUUID(t *testing.T) {
	uuidRegex := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		uuid := NewUUID()
		if !uuidRegex.MatchString(uuid) {
			t.Errorf("UUID %q does not match v4 format", uuid)
		}
		if seen[uuid] {
			t.Errorf("duplicate UUID generated: %s", uuid)
		}
		seen[uuid] = true
	}
}

func TestTarZst_PathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "malicious.tar.zst")
	extractDir := filepath.Join(tmpDir, "extract")

	// Create a tar.zst with a path traversal entry.
	var buf bytes.Buffer
	zstWriter, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tarWriter := tar.NewWriter(zstWriter)

	// Write a malicious entry that tries to escape.
	header := &tar.Header{
		Name:     "mydir/../../../etc/passwd",
		Mode:     0644,
		Size:     int64(len("pwned")),
		Typeflag: tar.TypeReg,
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte("pwned")); err != nil {
		t.Fatal(err)
	}
	tarWriter.Close()
	zstWriter.Close()

	if err := os.WriteFile(archivePath, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	_, err = extractTarZst(archivePath, extractDir)
	if err == nil {
		t.Fatal("expected path traversal error, got nil")
	}
	if !strings.Contains(err.Error(), "path traversal") && !strings.Contains(err.Error(), "escapes destination") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Execute writes the archive entry and stops there. The two halves of an
// archival are separate calls so that a caller can record the deletion in
// between -- and so a caller that fails to record it can take the whole thing
// back with DiscardBlob, having never touched the original.

func TestExecute_LeavesTheSourceInPlace(t *testing.T) {
	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")

	cases := []struct {
		name  string
		build func(t *testing.T) string
		rec   bool
	}{
		{
			name: "file",
			build: func(t *testing.T) string {
				p := filepath.Join(tmpDir, "file.txt")
				if err := os.WriteFile(p, []byte("content"), 0644); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "directory",
			build: func(t *testing.T) string {
				p := filepath.Join(tmpDir, "tree")
				if err := os.MkdirAll(filepath.Join(p, "nested"), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p, "nested", "f.txt"), []byte("x"), 0644); err != nil {
					t.Fatal(err)
				}
				return p
			},
			rec: true,
		},
		{
			name: "symlink",
			build: func(t *testing.T) string {
				p := filepath.Join(tmpDir, "link")
				if err := os.Symlink("/etc/hostname", p); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := c.build(t)

			plan, err := NewPlan(src, archiveDir, c.rec)
			if err != nil {
				t.Fatalf("NewPlan: %v", err)
			}
			if _, err := Execute(plan); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			if _, err := os.Lstat(src); err != nil {
				t.Errorf("Execute removed the source: %v", err)
			}
			if _, err := os.Lstat(plan.Dest); err != nil {
				t.Errorf("Execute wrote no archive entry: %v", err)
			}

			// DiscardBlob takes the archival back whole.
			if err := DiscardBlob(plan); err != nil {
				t.Fatalf("DiscardBlob: %v", err)
			}
			if _, err := os.Lstat(plan.Dest); !os.IsNotExist(err) {
				t.Errorf("DiscardBlob left the archive entry behind (err=%v)", err)
			}
			if _, err := os.Lstat(src); err != nil {
				t.Errorf("a discarded archival must leave the source exactly as it was: %v", err)
			}

			// And RemoveSource is the other half, once a caller has recorded it.
			if _, err := Execute(plan); err != nil {
				t.Fatalf("second Execute: %v", err)
			}
			if err := RemoveSource(plan); err != nil {
				t.Fatalf("RemoveSource: %v", err)
			}
			if _, err := os.Lstat(src); !os.IsNotExist(err) {
				t.Errorf("RemoveSource left the source behind (err=%v)", err)
			}
		})
	}
}

// A file's archive entry is a hard link to the original while both exist, so
// nothing is copied and the archived content is the original's own bytes.
func TestExecute_FileEntryIsTheSameInode(t *testing.T) {
	tmpDir := t.TempDir()
	archiveDir := filepath.Join(tmpDir, "archive")
	src := filepath.Join(tmpDir, "linked.txt")
	if err := os.WriteFile(src, []byte("same inode"), 0644); err != nil {
		t.Fatal(err)
	}

	plan, err := NewPlan(src, archiveDir, false)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	dstInfo, err := os.Stat(plan.Dest)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(srcInfo, dstInfo) {
		t.Error("the archive entry is a copy, not a link to the original")
	}

	if err := RemoveSource(plan); err != nil {
		t.Fatalf("RemoveSource: %v", err)
	}
	content, err := os.ReadFile(plan.Dest)
	if err != nil {
		t.Fatalf("reading the archived entry after the source went: %v", err)
	}
	if string(content) != "same inode" {
		t.Errorf("archived content is %q", content)
	}
}
