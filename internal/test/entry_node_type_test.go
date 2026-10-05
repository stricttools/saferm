//go:build !windows

package test

import (
	"database/sql"
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

// recordFileOverEntry writes a record of node type file whose archive entry is
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
		Hash:     "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		NodeType: archive.NodeTypeFile, DeletedAt: time.Now(), Description: "contradicted",
	}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	return uuid
}

// An entry whose type contradicts the record's node type is reported as corrupt by
// info, never as restorable, and undelete refuses it without touching it.
func TestInfoAndUndelete_ReportAnEntryThatContradictsItsNodeTypeAsCorrupt(t *testing.T) {
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

// writeV3Archive writes a database at schema version 3 into home's archive,
// with one record per entry, each recorded as a file the way saferm versions
// from before the node_type column recorded a symlink or a FIFO; makeEntry
// puts the entry at <uuid>. It returns the uuids in that order.
func writeV3Archive(t *testing.T, home string, makeEntries ...func(path string) error) []string {
	t.Helper()
	conn, err := sql.Open("sqlite", filepath.Join(home, ".saferm", "db", "saferm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(testutil.V3SchemaSQL); err != nil {
		t.Fatal(err)
	}
	var uuids []string
	for _, makeEntry := range makeEntries {
		uuid := archive.NewUUID()
		if err := makeEntry(filepath.Join(home, ".saferm", "archive", uuid)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(
			`INSERT INTO deletions (uuid, original_path, original_name, size, hash, is_directory, deleted_at, description)
			 VALUES (?, ?, 'orig', 0, 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855', 0, ?, 'written by an older saferm')`,
			uuid, filepath.Join(t.TempDir(), "orig"), time.Now().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		uuids = append(uuids, uuid)
	}
	return uuids
}

// The first command that opens a version-3 database migrates it, read-only
// commands included, and the migration repairs the FIFO and the symlink older
// saferm versions recorded as files: both are then listed and restorable as
// what they were.
func TestMigration_RepairsRecordsOlderVersionsWroteAsFiles(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	archiveDir := filepath.Join(home, ".saferm", "archive")
	uuids := writeV3Archive(t, home,
		func(p string) error { return syscall.Mkfifo(p, 0o640) },
		func(p string) error { return os.Symlink("../../scripts/hooks/pre-push", p) },
		func(p string) error { return os.WriteFile(p, []byte("fine"), 0o600) },
	)
	fifoUUID, linkUUID, fileUUID := uuids[0], uuids[1], uuids[2]

	stdout, stderr, code := runSaferm(t, home, "list")
	if code != 0 {
		t.Fatalf("list: exit %d: %s", code, stderr)
	}
	for _, want := range []string{"[fifo]", "[sym]"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("list does not mark %s:\n%s", want, stdout)
		}
	}
	assertInfoType(t, home, fifoUUID, "fifo")
	assertInfoType(t, home, linkUUID, "symlink")
	assertInfoType(t, home, fileUUID, "file")
	for _, old := range []string{fifoUUID, linkUUID} {
		if _, err := os.Lstat(filepath.Join(archiveDir, old)); !os.IsNotExist(err) {
			t.Errorf("the old entry %s is still there: %v", old, err)
		}
	}

	linkDest := filepath.Join(t.TempDir(), "hook")
	if _, stderr, code := runSafermWithin(t, specialLimit, home, nil, "undelete", "--destination", linkDest, linkUUID); code != 0 {
		t.Fatalf("undelete of the repaired symlink: exit %d: %s", code, stderr)
	}
	if target, err := os.Readlink(linkDest); err != nil || target != "../../scripts/hooks/pre-push" {
		t.Errorf("restored link reads %q (%v)", target, err)
	}
	fifoDest := filepath.Join(t.TempDir(), "pipe")
	if _, stderr, code := runSafermWithin(t, specialLimit, home, nil, "undelete", "--destination", fifoDest, fifoUUID); code != 0 {
		t.Fatalf("undelete of the repaired FIFO: exit %d: %s", code, stderr)
	}
	if info, err := os.Lstat(fifoDest); err != nil || info.Mode() != os.ModeNamedPipe|0o640 {
		t.Errorf("restored FIFO: %v (%v)", info, err)
	}
}

// A contradiction no repair resolves refuses the migration, naming the
// record, and the database and the archive stay as they were.
func TestMigration_RefusesWhatItCannotRepairAndChangesNothing(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	archiveDir := filepath.Join(home, ".saferm", "archive")
	uuids := writeV3Archive(t, home,
		func(p string) error { return syscall.Mkfifo(p, 0o640) },
		func(p string) error { return os.Mkdir(p, 0o755) },
	)
	fifoUUID, dirUUID := uuids[0], uuids[1]

	_, stderr, code := runSaferm(t, home, "list")
	if code != exitDatabase || !strings.Contains(stderr, dirUUID) || !strings.Contains(stderr, "nothing was changed") {
		t.Fatalf("list: exit %d, stderr %q; want exit %d naming %s", code, stderr, exitDatabase, dirUUID)
	}
	if info, err := os.Lstat(filepath.Join(archiveDir, fifoUUID)); err != nil || info.Mode().Type() != os.ModeNamedPipe {
		t.Errorf("the repairable record's entry was changed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(archiveDir, fifoUUID+".node")); !os.IsNotExist(err) {
		t.Errorf("a descriptor was written: %v", err)
	}
	conn, err := sql.Open("sqlite", filepath.Join(home, ".saferm", "db", "saferm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var version int
	if err := conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
		t.Errorf("user_version is %d (%v); the refused migration must leave it at 3", version, err)
	}
}
