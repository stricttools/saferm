//go:build unix

package test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// allocated is what the filesystem says a file occupies: its allocated
// blocks, not its length.
func allocated(t *testing.T, path string) (int64, uint64) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return st.Blocks * 512, uint64(st.Nlink)
}

// compressibleTree makes a directory holding one megabyte of zeros, which
// its .tar.zst archive entry holds in a few hundred bytes.
func compressibleTree(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, "zeros")
	testutil.CreateTempFile(t, dir, "zeros.bin", strings.Repeat("\x00", 1<<20))
	return dir
}

// humanBytes reads back one of saferm's humanSize strings as a byte count.
func humanBytes(t *testing.T, s string) float64 {
	t.Helper()
	m := regexp.MustCompile(`^([0-9.]+) (B|KB|MB|GB|TB)$`).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("not a size: %q", s)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("not a size: %q", s)
	}
	return n * map[string]float64{"B": 1, "KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40}[m[2]]
}

// purge's dry run says how much disk the purge frees, and that is what the
// selected entries occupy -- not the size each record captured at delete
// time, which for a compressed tree is many times larger.
func TestPurge_DryRunSizeIsWhatTheEntriesOccupy(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	tree := compressibleTree(t, workDir)
	if _, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "purge size test", tree); code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}

	stdout, stderr, code := runSaferm(t, homeDir, "--dry-run", "purge", "--all")
	if code != 0 {
		t.Fatalf("purge dry run failed (exit %d): %q", code, stderr)
	}
	m := regexp.MustCompile(`Would purge 1 item\(s\), freeing ~([0-9.]+ [KMGT]?B)`).FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("no freeing figure in:\n%s", stdout)
	}
	if got := humanBytes(t, m[1]); got >= 64<<10 {
		t.Errorf("freeing ~%s: the tree's entry is a compressed sliver of its 1 MB, the figure must say so", m[1])
	}
}

// An entry still linked from outside the archive frees nothing, and the dry
// run says so instead of counting it.
func TestPurge_DryRunDoesNotCountASharedEntry(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	shared := testutil.CreateTempFile(t, workDir, "shared.txt", strings.Repeat("s", 200000))
	if err := os.Link(shared, filepath.Join(workDir, "other-name.txt")); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "shared purge test", shared); code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	if _, links := allocated(t, filepath.Join(workDir, "other-name.txt")); links < 2 {
		t.Skipf("the archive entry was copied rather than linked (links=%d); nothing is shared", links)
	}

	stdout, stderr, code := runSaferm(t, homeDir, "--dry-run", "purge", "--all")
	if code != 0 {
		t.Fatalf("purge dry run failed (exit %d): %q", code, stderr)
	}
	if !strings.Contains(stdout, "Would purge 1 item(s), freeing ~0 B") {
		t.Errorf("a shared entry frees nothing:\n%s", stdout)
	}
	if !strings.Contains(stdout, "also linked from outside the archive") {
		t.Errorf("the dry run should say why the shared entry frees nothing:\n%s", stdout)
	}
}
