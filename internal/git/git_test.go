package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stricttools/testisolation/go/hygiene"
)

// initGitRepo creates a bare git repo in dir with an initial commit.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	// testisolation's floor: no test here may read the developer's git config,
	// identity or credentials, and none may reach the network.
	hygiene.Isolate(t)
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s\n%s", args, err, out)
		}
	}
	// Create an initial commit so HEAD exists.
	placeholder := filepath.Join(dir, ".gitkeep")
	if err := os.WriteFile(placeholder, nil, 0644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", ".gitkeep")
	run(t, dir, "git", "commit", "-m", "init")
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %s\n%s", name, args, err, out)
	}
}

func TestIsInGitRepo(t *testing.T) {
	// A temp dir with a git repo should return true.
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	if !IsInGitRepo(repoDir) {
		t.Error("IsInGitRepo should return true inside a git repo")
	}

	// A plain temp dir should return false.
	plainDir := t.TempDir()
	if IsInGitRepo(plainDir) {
		t.Error("IsInGitRepo should return false outside a git repo")
	}
}

func TestIsGitTracked(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	// Create and track a file.
	tracked := filepath.Join(repoDir, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	run(t, repoDir, "git", "add", "tracked.txt")
	run(t, repoDir, "git", "commit", "-m", "add tracked")

	if !IsGitTracked(tracked) {
		t.Error("IsGitTracked should return true for a tracked file")
	}

	// An untracked file should return false.
	untracked := filepath.Join(repoDir, "untracked.txt")
	if err := os.WriteFile(untracked, []byte("world"), 0644); err != nil {
		t.Fatal(err)
	}
	if IsGitTracked(untracked) {
		t.Error("IsGitTracked should return false for an untracked file")
	}
}

func TestGitRmCached(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	// Create, add, and commit a file.
	filePath := filepath.Join(repoDir, "to-remove.txt")
	if err := os.WriteFile(filePath, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	run(t, repoDir, "git", "add", "to-remove.txt")
	run(t, repoDir, "git", "commit", "-m", "add file")

	// git rm --cached should unstage the file.
	if err := GitRmCached(filePath, false); err != nil {
		t.Fatalf("GitRmCached failed: %v", err)
	}

	// The file should still exist on disk.
	if _, err := os.Stat(filePath); err != nil {
		t.Errorf("file should still exist on disk after git rm --cached: %v", err)
	}

	// The file should no longer be tracked.
	if IsGitTracked(filePath) {
		t.Error("file should not be tracked after git rm --cached")
	}
}

func TestGitRmCached_Recursive(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	// Create a directory with files, add and commit.
	subdir := filepath.Join(repoDir, "mydir")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	f1 := filepath.Join(subdir, "a.txt")
	f2 := filepath.Join(subdir, "b.txt")
	os.WriteFile(f1, []byte("a"), 0644)
	os.WriteFile(f2, []byte("b"), 0644)
	run(t, repoDir, "git", "add", "mydir")
	run(t, repoDir, "git", "commit", "-m", "add dir")

	// git rm -r --cached should unstage the whole directory.
	if err := GitRmCached(subdir, true); err != nil {
		t.Fatalf("GitRmCached recursive failed: %v", err)
	}

	// Both files should still exist on disk.
	for _, f := range []string{f1, f2} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("file %s should still exist on disk: %v", f, err)
		}
	}

	// Neither should be tracked.
	if IsGitTracked(f1) {
		t.Error("a.txt should not be tracked after recursive git rm --cached")
	}
	if IsGitTracked(f2) {
		t.Error("b.txt should not be tracked after recursive git rm --cached")
	}
}

func TestGitAdd(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	// Create a file without adding it.
	filePath := filepath.Join(repoDir, "new-file.txt")
	if err := os.WriteFile(filePath, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}

	if IsGitTracked(filePath) {
		t.Fatal("file should not be tracked before git add")
	}

	if err := GitAdd(filePath); err != nil {
		t.Fatalf("GitAdd failed: %v", err)
	}

	// After git add, the file should show up in ls-files.
	if !IsGitTracked(filePath) {
		t.Error("file should be tracked after git add")
	}
}
