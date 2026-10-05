//go:build !windows

package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stricttools/saferm/internal/archive"
	"github.com/stricttools/saferm/internal/db"
	"github.com/stricttools/saferm/internal/testutil"
)

// recordFileOverEntry writes a record of kind file whose archive entry is
// whatever makeEntry puts at <uuid>: the shape a FIFO deleted by a saferm that
// read it, or a symlink deleted before symlinks were recognized, left behind.
func recordFileOverEntry(t *testing.T, home string, makeEntry func(path string) error) string {
	t.Helper()
	uuid := archive.NewUUID()
	entry := filepath.Join(home, ".saferm", "archive", uuid)
	if err := makeEntry(entry); err != nil {
		t.Fatal(err)
	}
	d := openArchive(t, home)
	if _, err := d.Insert(&db.DeletionRecord{
		UUID: uuid, OriginalPath: filepath.Join(t.TempDir(), "orig"), OriginalName: "orig",
		Hash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		Kind: archive.KindFile, DeletedAt: time.Now(), Description: "contradicted",
	}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	return uuid
}

// An entry whose type contradicts the record's kind is reported as corrupt by
// info, never as restorable, and undelete refuses it without touching it.
func TestInfoAndUndelete_ReportAnEntryThatContradictsItsKindAsCorrupt(t *testing.T) {
	for label, makeEntry := range map[string]func(string) error{
		"a FIFO":    func(p string) error { return syscall.Mkfifo(p, 0o644) },
		"a symlink": func(p string) error { return os.Symlink("../../scripts/hooks/pre-push", p) },
	} {
		t.Run(label, func(t *testing.T) {
			home := testutil.SetupTestEnv(t)
			uuid := recordFileOverEntry(t, home, makeEntry)

			out, stderr, code := runSaferm(t, home, "info", uuid)
			if code != 0 {
				t.Fatalf("info: exit %d: %s", code, stderr)
			}
			status := parseInfoField(t, out, "Status:")
			if status == "restorable" || !strings.Contains(status, "not what this record says") {
				t.Errorf("info Status = %q, want the corrupt-entry line", status)
			}

			env, _, code := runSafermJSON(t, home, "info", uuid)
			if code != 0 {
				t.Fatalf("info --json: exit %d", code)
			}
			var payload struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Status != "entry-corrupt" {
				t.Errorf("payload status = %q, want entry-corrupt", payload.Status)
			}

			_, stderr, code = runSafermWithin(t, specialLimit, home, nil, "undelete", uuid)
			if code != 6 || !strings.Contains(stderr, "not what the record says") {
				t.Errorf("undelete: exit %d, stderr %q; want exit 6 naming the corrupt entry", code, stderr)
			}
			if _, err := os.Lstat(filepath.Join(home, ".saferm", "archive", uuid)); err != nil {
				t.Errorf("the refused entry was touched: %v", err)
			}
		})
	}
}

// reclassify-records turns a FIFO and a symlink that older saferm versions
// recorded as files into records of their real kinds, after a dry run that
// changes nothing, and both are then restorable as what they were.
func TestReclassifyRecords_MakesContradictedRecordsRestorable(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	archiveDir := filepath.Join(home, ".saferm", "archive")
	fifoUUID := recordFileOverEntry(t, home, func(p string) error { return syscall.Mkfifo(p, 0o640) })
	linkUUID := recordFileOverEntry(t, home, func(p string) error { return os.Symlink("../../scripts/hooks/pre-push", p) })
	healthy := filepath.Join(t.TempDir(), "healthy.txt")
	if err := os.WriteFile(healthy, []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	healthyUUID := archiveSpecial(t, home, healthy)

	stdout, stderr, code := runSafermWithin(t, specialLimit, home, nil, "--dry-run", "reclassify-records")
	if code != 0 {
		t.Fatalf("dry run: exit %d: %s", code, stderr)
	}
	for _, want := range []string{
		"would reclassify: [", fifoUUID, "recorded as a file, the entry is a fifo",
		linkUUID, "the entry is a symlink to ../../scripts/hooks/pre-push",
		fifoUUID + ".node (", linkUUID + ".symlink (",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the dry run does not say %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, healthyUUID) {
		t.Errorf("the dry run names the healthy record:\n%s", stdout)
	}
	if info, err := os.Lstat(filepath.Join(archiveDir, fifoUUID)); err != nil || info.Mode().Type() != os.ModeNamedPipe {
		t.Fatalf("the dry run changed the FIFO entry: %v", err)
	}
	out, _, _ := runSaferm(t, home, "info", fifoUUID)
	if parseInfoField(t, out, "Type:") != "file" {
		t.Fatalf("the dry run changed the record:\n%s", out)
	}

	stdout, stderr, code = runSafermWithin(t, specialLimit, home, nil, "reclassify-records")
	if code != 0 {
		t.Fatalf("reclassify-records: exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "2 record(s) reclassified") {
		t.Errorf("no summary:\n%s", stdout)
	}
	assertInfoType(t, home, fifoUUID, "fifo")
	assertInfoType(t, home, linkUUID, "symlink")
	assertInfoType(t, home, healthyUUID, "file")
	for _, old := range []string{fifoUUID, linkUUID} {
		if _, err := os.Lstat(filepath.Join(archiveDir, old)); !os.IsNotExist(err) {
			t.Errorf("the old entry %s is still there: %v", old, err)
		}
	}

	stdout, _, code = runSafermWithin(t, specialLimit, home, nil, "reclassify-records")
	if code != 0 || !strings.Contains(stdout, "nothing to reclassify") {
		t.Errorf("a second run: exit %d:\n%s", code, stdout)
	}

	linkDest := filepath.Join(t.TempDir(), "hook")
	if _, stderr, code := runSafermWithin(t, specialLimit, home, nil, "undelete", "--destination", linkDest, linkUUID); code != 0 {
		t.Fatalf("undelete of the reclassified symlink: exit %d: %s", code, stderr)
	}
	if target, err := os.Readlink(linkDest); err != nil || target != "../../scripts/hooks/pre-push" {
		t.Errorf("restored link reads %q (%v)", target, err)
	}
	fifoDest := filepath.Join(t.TempDir(), "pipe")
	if _, stderr, code := runSafermWithin(t, specialLimit, home, nil, "undelete", "--destination", fifoDest, fifoUUID); code != 0 {
		t.Fatalf("undelete of the reclassified FIFO: exit %d: %s", code, stderr)
	}
	if info, err := os.Lstat(fifoDest); err != nil || info.Mode() != os.ModeNamedPipe|0o640 {
		t.Errorf("restored FIFO: %v (%v)", info, err)
	}
}

// A contradiction no reclassification resolves refuses the whole run before
// anything changes, naming the record.
func TestReclassifyRecords_RefusesWhatItCannotResolveAndChangesNothing(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	archiveDir := filepath.Join(home, ".saferm", "archive")
	fifoUUID := recordFileOverEntry(t, home, func(p string) error { return syscall.Mkfifo(p, 0o640) })
	dirUUID := recordFileOverEntry(t, home, func(p string) error { return os.Mkdir(p, 0o755) })

	_, stderr, code := runSafermWithin(t, specialLimit, home, nil, "reclassify-records")
	if code != 6 || !strings.Contains(stderr, dirUUID) || !strings.Contains(stderr, "nothing was changed") {
		t.Fatalf("exit %d, stderr %q; want exit 6 naming %s", code, stderr, dirUUID)
	}
	if info, err := os.Lstat(filepath.Join(archiveDir, fifoUUID)); err != nil || info.Mode().Type() != os.ModeNamedPipe {
		t.Errorf("the resolvable record's entry was changed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(archiveDir, fifoUUID+".node")); !os.IsNotExist(err) {
		t.Errorf("a descriptor was written: %v", err)
	}
}
