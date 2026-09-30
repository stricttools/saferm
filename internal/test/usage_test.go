//go:build unix

package test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// usageGroup is one row of `usage`'s age or directory breakdown.
type usageGroup struct {
	Age           string `json:"age"`
	Directory     string `json:"directory"`
	Records       int    `json:"records"`
	OriginalBytes int64  `json:"original_bytes"`
	DiskBytes     int64  `json:"disk_bytes"`
}

// usageDoc is `usage`'s payload as a consumer parses it.
type usageDoc struct {
	ArchiveDir            string       `json:"archive_dir"`
	DatabasePath          string       `json:"database_path"`
	ArchiveDiskBytes      int64        `json:"archive_disk_bytes"`
	SharedDiskBytes       int64        `json:"shared_disk_bytes"`
	UnreferencedDiskBytes int64        `json:"unreferenced_disk_bytes"`
	DatabaseDiskBytes     int64        `json:"database_disk_bytes"`
	TotalDiskBytes        int64        `json:"total_disk_bytes"`
	Records               int          `json:"records"`
	MissingEntries        int          `json:"missing_entries"`
	ByAge                 []usageGroup `json:"by_age"`
	ByDirectory           []usageGroup `json:"by_directory"`
	OtherDirectories      struct {
		Directories   int   `json:"directories"`
		Records       int   `json:"records"`
		OriginalBytes int64 `json:"original_bytes"`
		DiskBytes     int64 `json:"disk_bytes"`
	} `json:"other_directories"`
}

func usageOf(t *testing.T, homeDir string, args ...string) usageDoc {
	t.Helper()
	env, stderr, code := runSafermJSON(t, homeDir, append([]string{"usage"}, args...)...)
	if code != 0 {
		t.Fatalf("usage failed (exit %d): %q", code, stderr)
	}
	var doc usageDoc
	if err := json.Unmarshal(env.Payload, &doc); err != nil {
		t.Fatalf("usage's payload does not parse (%v): %s", err, env.Payload)
	}
	return doc
}

// archiveTotal sums the allocated size of every file in the archive
// directory, counting each inode once.
func archiveTotal(t *testing.T, archiveDir string) int64 {
	t.Helper()
	seen := map[uint64]bool{}
	var total int64
	err := filepath.WalkDir(archiveDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st := fi.Sys().(*syscall.Stat_t)
		if seen[st.Ino] {
			return nil
		}
		seen[st.Ino] = true
		total += st.Blocks * 512
		return nil
	})
	if err != nil {
		t.Fatalf("walking the archive: %v", err)
	}
	return total
}

// A machine that has never deleted anything has an archive that takes no
// space, and asking must not create saferm's state directory.
func TestUsage_NeedsNoArchive(t *testing.T) {
	homeDir := t.TempDir()
	testutil.Isolate(t)

	stdout, stderr, code := runSaferm(t, homeDir, "usage")
	if code != 0 {
		t.Fatalf("usage failed on a machine with no archive (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(homeDir, ".saferm")); !os.IsNotExist(err) {
		t.Fatalf("usage created saferm's state directory (stat err: %v)", err)
	}
	doc := usageOf(t, homeDir)
	if doc.TotalDiskBytes != 0 || doc.Records != 0 {
		t.Errorf("an absent archive uses nothing, got total %d over %d records", doc.TotalDiskBytes, doc.Records)
	}
	if len(doc.ByAge) == 0 {
		t.Errorf("the age breakdown lists its buckets even when they are empty")
	}
}

// The figures are what the archive's entries occupy on disk, not the sizes
// the records captured at delete time: a compressible tree archived as
// .tar.zst takes a sliver of its original size.
func TestUsage_ReportsTheEntriesDiskUse(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()
	archiveDir := filepath.Join(homeDir, ".saferm", "archive")

	tree := compressibleTree(t, workDir)
	file := testutil.CreateTempFile(t, workDir, "plain.txt", strings.Repeat("p", 10000))
	restored := testutil.CreateTempFile(t, workDir, "restored.txt", strings.Repeat("r", 10000))
	for _, args := range [][]string{{"-r", tree}, {file}, {restored}} {
		all := append([]string{"delete", "--on-error", "abort", "--description", "usage test"}, args...)
		if _, stderr, code := runSaferm(t, homeDir, all...); code != 0 {
			t.Fatalf("delete %v failed (exit %d): stderr=%q", args, code, stderr)
		}
	}
	stdout, _, _ := runSaferm(t, homeDir, "list")
	if _, stderr, code := runSaferm(t, homeDir, "undelete", parseAllIDs(t, stdout)[0]); code != 0 {
		t.Fatalf("undelete failed (exit %d): stderr=%q", code, stderr)
	}

	doc := usageOf(t, homeDir, "--directory-depth", "64")

	if want := archiveTotal(t, archiveDir); doc.ArchiveDiskBytes != want {
		t.Errorf("archive_disk_bytes = %d, want %d (the archive directory's allocated size)", doc.ArchiveDiskBytes, want)
	}
	if doc.Records != 2 {
		t.Errorf("records = %d, want 2 (the restored record is not in the archive)", doc.Records)
	}
	if doc.TotalDiskBytes != doc.ArchiveDiskBytes+doc.DatabaseDiskBytes {
		t.Errorf("total_disk_bytes = %d, want archive %d + database %d", doc.TotalDiskBytes, doc.ArchiveDiskBytes, doc.DatabaseDiskBytes)
	}
	if doc.DatabaseDiskBytes <= 0 {
		t.Errorf("database_disk_bytes = %d, want the database file's size", doc.DatabaseDiskBytes)
	}

	var entries int64
	for _, e := range mustReadDir(t, archiveDir) {
		b, _ := allocated(t, filepath.Join(archiveDir, e.Name()))
		entries += b
	}
	var group *usageGroup
	for i := range doc.ByDirectory {
		if doc.ByDirectory[i].Directory == workDir {
			group = &doc.ByDirectory[i]
		}
	}
	if group == nil {
		t.Fatalf("no directory group for %s in %+v", workDir, doc.ByDirectory)
	}
	if group.Records != 2 || group.DiskBytes != entries {
		t.Errorf("group %s = %d records, %d bytes on disk; want 2 records, %d bytes", workDir, group.Records, group.DiskBytes, entries)
	}
	if group.OriginalBytes < 1<<20 || group.DiskBytes >= group.OriginalBytes/4 {
		t.Errorf("group %s: original %d, on disk %d -- the compressed tree should take far less than it held",
			workDir, group.OriginalBytes, group.DiskBytes)
	}
	if doc.ByAge[0].Records != 2 || doc.ByAge[0].DiskBytes != entries {
		t.Errorf("youngest age bucket = %+v, want 2 records and %d bytes", doc.ByAge[0], entries)
	}

	human, stderr, code := runSaferm(t, homeDir, "usage")
	if code != 0 {
		t.Fatalf("usage failed (exit %d): %q", code, stderr)
	}
	for _, want := range []string{"Archive", "Database", "Total", "By age", "By original directory"} {
		if !strings.Contains(human, want) {
			t.Errorf("usage's report lacks %q:\n%s", want, human)
		}
	}
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	return entries
}

// A file entry still linked from somewhere outside the archive frees nothing
// when it is purged, and a file in the archive directory no record names is
// disk the archive takes that no purge selection reaches. Both are reported.
func TestUsage_ReportsSharedAndUnreferencedEntries(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()
	archiveDir := filepath.Join(homeDir, ".saferm", "archive")

	shared := testutil.CreateTempFile(t, workDir, "shared.txt", strings.Repeat("s", 20000))
	if err := os.Link(shared, filepath.Join(workDir, "other-name.txt")); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "shared test", shared); code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	stray := testutil.CreateTempFile(t, archiveDir, "stray-file", strings.Repeat("x", 30000))

	doc := usageOf(t, homeDir)
	sharedBytes, links := allocated(t, filepath.Join(workDir, "other-name.txt"))
	if links < 2 {
		t.Skipf("the archive entry was copied rather than linked (links=%d); nothing is shared", links)
	}
	if doc.SharedDiskBytes != sharedBytes {
		t.Errorf("shared_disk_bytes = %d, want %d", doc.SharedDiskBytes, sharedBytes)
	}
	strayBytes, _ := allocated(t, stray)
	if doc.UnreferencedDiskBytes != strayBytes {
		t.Errorf("unreferenced_disk_bytes = %d, want %d", doc.UnreferencedDiskBytes, strayBytes)
	}
}

// The directory breakdown lists the groups using the most disk and sums the
// rest on one line; the two flags shaping it refuse values that mean nothing.
func TestUsage_DirectoryLimitAndDepth(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	for _, name := range []string{"a/one.txt", "b/two.txt", "c/three.txt"} {
		f := testutil.CreateTempFile(t, workDir, name, strings.Repeat("z", 5000))
		if _, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "usage limit test", f); code != 0 {
			t.Fatalf("delete %s failed (exit %d): stderr=%q", f, code, stderr)
		}
	}

	doc := usageOf(t, homeDir, "--directory-depth", "64", "--directory-limit", "1")
	if len(doc.ByDirectory) != 1 || doc.OtherDirectories.Directories != 2 || doc.OtherDirectories.Records != 2 {
		t.Errorf("limit 1: by_directory %+v, other %+v; want one listed group and two summed", doc.ByDirectory, doc.OtherDirectories)
	}
	doc = usageOf(t, homeDir, "--directory-depth", "64", "--directory-limit", "0")
	if len(doc.ByDirectory) != 3 || doc.OtherDirectories.Directories != 0 {
		t.Errorf("limit 0: by_directory %+v, other %+v; want every group listed", doc.ByDirectory, doc.OtherDirectories)
	}

	for _, args := range [][]string{{"--directory-depth", "0"}, {"--directory-limit", "-1"}} {
		_, stderr, code := runSaferm(t, homeDir, append([]string{"usage"}, args...)...)
		if code != 2 {
			t.Errorf("usage %v: exit %d, want 2 (stderr=%q)", args, code, stderr)
		}
	}
}
