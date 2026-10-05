package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stricttools/strictcli/go/strictcli"
	"github.com/stricttools/testisolation/go/hygiene"
)

// The cross-device branch of a file restore is unreachable from the outside: it
// fires only when the archive and the destination sit on different
// filesystems, and a test cannot arrange two filesystems inside a temporary
// directory. So it is driven the same way the archive package drives its link
// fallback -- through a package-level seam the test replaces, here making the
// rename fail with the errno the kernel returns for a cross-device rename.
//
// What it proves is the ROUTING, which is the part that lives here: the failed
// rename is recognized and handed to CopyOut rather than reported. CopyOut's
// own behaviour (copy first, consume the entry only afterwards, take a partial
// destination back on failure) is proven in the archive package's tests.
func TestUndelete_CrossDeviceRenameRoutesThroughCopyOut(t *testing.T) {
	hygiene.Isolate(t, hygiene.Preserve(hygiene.GoPath, hygiene.GoModCache, hygiene.GoCache))

	home := t.TempDir()
	t.Setenv("SAFERM_HOME", home)
	workDir := t.TempDir()

	file := filepath.Join(workDir, "moved.txt")
	if err := os.WriteFile(file, []byte("the archived content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := newApp().Test([]string{"delete", "--on-error", "abort", "--description", "cross-device seam", file})
	if res.ExitCode != 0 {
		t.Fatalf("delete failed (exit %d): %s", res.ExitCode, res.Stderr)
	}
	uuid := soleArchiveEntry(t, filepath.Join(home, "archive"))

	// Every rename out of the archive now fails the way one across a filesystem
	// boundary does.
	original := renameOut
	renameOut = func(fx *strictcli.Effects, entry, dest string) error {
		return &os.LinkError{Op: "rename", Old: entry, New: dest, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameOut = original })

	res = newApp().Test([]string{"undelete", uuid})
	if res.ExitCode != 0 {
		t.Fatalf("a cross-device restore must fall back to a copy, got exit %d: %s", res.ExitCode, res.Stderr)
	}

	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("the file was not restored: %v", err)
	}
	if string(got) != "the archived content\n" {
		t.Errorf("restored content mismatch: got %q", got)
	}
	if _, err := os.Lstat(filepath.Join(home, "archive", uuid)); !os.IsNotExist(err) {
		t.Errorf("the copy fallback must consume the archived entry, got: %v", err)
	}
}

// soleArchiveEntry names the one entry in an archive directory, which is how a
// test that cannot read the `delete` line's uuid (it goes to the process's own
// stdout, not through the context writers) finds the record it just made.
func soleArchiveEntry(t *testing.T, archiveDir string) string {
	t.Helper()

	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatalf("reading the archive directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one archive entry, got %d: %v", len(entries), entries)
	}
	return entries[0].Name()
}
