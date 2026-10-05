//go:build !windows

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stricttools/saferm/internal/archive"
	"github.com/stricttools/saferm/internal/testutil"
)

// A v3 archive as saferm versions from before the node_type column left it: a
// file record over a regular file, a directory record over its tarball, and a
// file record over each of a symlink and a FIFO, which those versions did not
// recognize. The record ids follow the order the rows are listed in.
type v3Archive struct {
	dbPath     string
	archiveDir string
}

const (
	repairFileUUID = "file-uuid"
	repairDirUUID  = "dir-uuid"
	repairLinkUUID = "link-uuid"
	repairFIFOUUID = "fifo-uuid"
	repairLinkDest = "../elsewhere"
)

// v3Entry is one row of a v3 archive and the entry it leaves at <uuid>, or at
// <uuid>.tar.zst for a directory record.
type v3Entry struct {
	uuid  string
	isDir int
	make  func(path string) error
}

func regularEntry(path string) error { return os.WriteFile(path, []byte("content"), 0o600) }

func fifoEntry(path string) error {
	if err := syscall.Mkfifo(path, 0o640); err != nil {
		return err
	}
	return os.Chmod(path, 0o640)
}

func newV3Archive(t *testing.T, extra ...v3Entry) v3Archive {
	t.Helper()
	a := v3Archive{dbPath: filepath.Join(t.TempDir(), "v3.db"), archiveDir: t.TempDir()}
	conn, err := sql.Open("sqlite", a.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(testutil.V3SchemaSQL); err != nil {
		t.Fatal(err)
	}
	rows := append([]v3Entry{
		{repairFileUUID, 0, regularEntry},
		{repairDirUUID, 1, regularEntry},
		{repairLinkUUID, 0, func(p string) error { return os.Symlink(repairLinkDest, p) }},
		{repairFIFOUUID, 0, fifoEntry},
	}, extra...)
	now := time.Now().Format(time.RFC3339)
	for _, row := range rows {
		entry := filepath.Join(a.archiveDir, row.uuid)
		if row.isDir != 0 {
			entry += ".tar.zst"
		}
		if err := row.make(entry); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(
			`INSERT INTO deletions (uuid, original_path, original_name, size, hash, is_directory, deleted_at, description)
			 VALUES (?, '/x/'||?, ?, 7, 'recorded-hash', ?, ?, 'd')`,
			row.uuid, row.uuid, row.uuid, row.isDir, now); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func (a v3Archive) entry(name string) string { return filepath.Join(a.archiveDir, name) }

// assertUnchanged checks that a refused or failed migration left the database
// at version 3 and every v3 entry where it was, with no repaired entry written.
func (a v3Archive) assertUnchanged(t *testing.T) {
	t.Helper()
	conn, err := sql.Open("sqlite", a.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var version int
	if err := conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Errorf("user_version is %d; a failed migration must leave it at 3", version)
	}
	if present, err := hasColumn(conn, "deletions", "node_type"); err != nil || present {
		t.Errorf("the node_type column exists after a failed migration (present=%v, err=%v)", present, err)
	}
	if info, err := os.Lstat(a.entry(repairLinkUUID)); err != nil || info.Mode().Type() != os.ModeSymlink {
		t.Errorf("the symlink entry was changed: %v", err)
	}
	if info, err := os.Lstat(a.entry(repairFIFOUUID)); err != nil || info.Mode().Type() != os.ModeNamedPipe {
		t.Errorf("the FIFO entry was changed: %v", err)
	}
	if _, err := os.Lstat(a.entry(repairLinkUUID + ".symlink")); !os.IsNotExist(err) {
		t.Errorf("a repaired symlink entry was left behind: %v", err)
	}
	if _, err := os.Lstat(a.entry(repairFIFOUUID + ".node")); !os.IsNotExist(err) {
		t.Errorf("a repaired FIFO descriptor was left behind: %v", err)
	}
}

// replaceSeam swaps a seam for the duration of t.
func replaceSeam[T any](t *testing.T, seam *T, with T) {
	t.Helper()
	original := *seam
	*seam = with
	t.Cleanup(func() { *seam = original })
}

// The migration gives a symlink and a FIFO recorded as files the node types
// their entries hold, writes each entry in that node type's form, removes the
// old one, and leaves the records that agree with their entries alone.
func TestMigration4_RepairsEveryRecordItsEntryContradicts(t *testing.T) {
	a := newV3Archive(t)
	d, err := Open(a.dbPath, a.archiveDir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	link, err := d.QueryByUUID(repairLinkUUID)
	if err != nil {
		t.Fatal(err)
	}
	if link.NodeType != archive.NodeTypeSymlink || link.SymlinkTarget == nil || *link.SymlinkTarget != repairLinkDest ||
		link.Hash != "" || link.Size != 0 {
		t.Errorf("symlink record: node type %q, target %v, hash %q, size %d", link.NodeType, link.SymlinkTarget, link.Hash, link.Size)
	}
	if got, err := os.ReadFile(a.entry(repairLinkUUID + ".symlink")); err != nil || string(got) != repairLinkDest {
		t.Errorf("symlink entry reads %q (%v)", got, err)
	}

	fifo, err := d.QueryByUUID(repairFIFOUUID)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := os.ReadFile(a.entry(repairFIFOUUID + ".node"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(descriptor)
	if fifo.NodeType != archive.NodeTypeFIFO || fifo.SymlinkTarget != nil || fifo.Hash != hex.EncodeToString(sum[:]) || fifo.Size != 0 {
		t.Errorf("FIFO record: node type %q, hash %q, size %d", fifo.NodeType, fifo.Hash, fifo.Size)
	}
	node, err := archive.DecodeNode(descriptor)
	if err != nil || node.NodeType != archive.NodeTypeFIFO || node.Perm != 0o640 {
		t.Errorf("descriptor decodes to %+v (%v)", node, err)
	}

	for _, old := range []string{repairLinkUUID, repairFIFOUUID} {
		if _, err := os.Lstat(a.entry(old)); !os.IsNotExist(err) {
			t.Errorf("the old entry %s is still there: %v", old, err)
		}
	}
	for uuid, want := range map[string]archive.NodeType{repairFileUUID: archive.NodeTypeFile, repairDirUUID: archive.NodeTypeDirectory} {
		rec, err := d.QueryByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		if rec.NodeType != want || rec.Hash != "recorded-hash" || rec.Size != 7 {
			t.Errorf("%s: node type %q, hash %q, size %d; an agreeing record must stay as it was", uuid, rec.NodeType, rec.Hash, rec.Size)
		}
	}
}

// A contradiction no repair resolves -- here a directory standing where a
// file record expects a regular file -- refuses the migration as a whole,
// naming the record, and nothing changes: not the resolvable records either.
func TestMigration4_RefusesAnUnresolvableContradictionAndChangesNothing(t *testing.T) {
	const dirAsFile = "dir-as-file-uuid"
	a := newV3Archive(t, v3Entry{dirAsFile, 0, func(p string) error { return os.Mkdir(p, 0o755) }})
	_, err := Open(a.dbPath, a.archiveDir, nil)
	if err == nil {
		t.Fatal("Open succeeded over an unresolvable contradiction")
	}
	for _, want := range []string{dirAsFile, "/x/" + dirAsFile, "nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), repairFIFOUUID) {
		t.Errorf("the refusal names a record a repair resolves: %v", err)
	}
	a.assertUnchanged(t)
}

// A repaired entry that cannot be written stops the migration before the
// database changes, and the entries already written are taken back. What was
// already at the path that failed is not this migration's and stays.
func TestMigration4_AFailedEntryWriteChangesNothing(t *testing.T) {
	a := newV3Archive(t)
	foreign := a.entry(repairFIFOUUID + ".node")
	if err := os.WriteFile(foreign, []byte("someone else's"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(a.dbPath, a.archiveDir, nil)
	if !errors.Is(err, archive.ErrRepairEntryExists) || !strings.Contains(err.Error(), repairFIFOUUID) {
		t.Fatalf("Open: %v; want the occupied path refused, naming the record", err)
	}
	if got, err := os.ReadFile(foreign); err != nil || string(got) != "someone else's" {
		t.Errorf("the occupying file was touched: %q (%v)", got, err)
	}
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	a.assertUnchanged(t)
}

// A commit that fails takes back every repaired entry the migration wrote.
func TestMigration4_AFailedCommitChangesNothing(t *testing.T) {
	a := newV3Archive(t)
	replaceSeam(t, &commitNodeTypeMigration, func(*sql.Conn) error { return errors.New("injected commit failure") })
	if _, err := Open(a.dbPath, a.archiveDir, nil); err == nil || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("Open: %v; want the commit failure", err)
	}
	a.assertUnchanged(t)
}

// An old entry that cannot be removed after the commit is reported: the
// migration is complete, and the leftover path, which no record names any
// more, is in the error. The next open finds the database migrated.
func TestMigration4_AnOldEntryLeftBehindIsReported(t *testing.T) {
	a := newV3Archive(t)
	replaceSeam(t, &removeOldEntry, func(path string) error {
		if strings.HasSuffix(path, repairFIFOUUID) {
			return errors.New("injected removal failure")
		}
		return os.Remove(path)
	})
	_, err := Open(a.dbPath, a.archiveDir, nil)
	if err == nil || !strings.Contains(err.Error(), a.entry(repairFIFOUUID)) || strings.Contains(err.Error(), repairLinkUUID) {
		t.Fatalf("Open: %v; want the one leftover entry named", err)
	}
	if _, err := os.Lstat(a.entry(repairLinkUUID)); !os.IsNotExist(err) {
		t.Errorf("the removable old entry is still there: %v", err)
	}
	d, err := Open(a.dbPath, a.archiveDir, nil)
	if err != nil {
		t.Fatalf("the second Open: %v", err)
	}
	defer d.Close()
	if rec, err := d.QueryByUUID(repairFIFOUUID); err != nil || rec.NodeType != archive.NodeTypeFIFO {
		t.Errorf("the FIFO record after the commit: %+v (%v)", rec, err)
	}
}

// A repaired entry an interrupted run already wrote, byte for byte what this
// run would write, is taken as written.
func TestMigration4_AcceptsAnEntryAnInterruptedRunWrote(t *testing.T) {
	a := newV3Archive(t)
	if err := os.WriteFile(a.entry(repairLinkUUID+".symlink"), []byte(repairLinkDest), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := Open(a.dbPath, a.archiveDir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if rec, err := d.QueryByUUID(repairLinkUUID); err != nil || rec.NodeType != archive.NodeTypeSymlink {
		t.Errorf("symlink record: %+v (%v)", rec, err)
	}
}

// A commit that reports a failure yet took effect leaves the repaired entries
// in place, because the records now name them, and removes the old ones.
func TestMigration4_ACommitThatTookEffectDespiteItsErrorKeepsTheRepairs(t *testing.T) {
	a := newV3Archive(t)
	replaceSeam(t, &commitNodeTypeMigration, func(c *sql.Conn) error {
		if _, err := c.ExecContext(context.Background(), "COMMIT"); err != nil {
			return err
		}
		return errors.New("injected error after the commit")
	})
	if _, err := Open(a.dbPath, a.archiveDir, nil); err == nil || !strings.Contains(err.Error(), "injected error after the commit") {
		t.Fatalf("Open: %v; want the reported commit error", err)
	}
	for _, kept := range []string{repairLinkUUID + ".symlink", repairFIFOUUID + ".node"} {
		if _, err := os.Lstat(a.entry(kept)); err != nil {
			t.Errorf("the repaired entry %s was taken back: %v", kept, err)
		}
	}
	for _, old := range []string{repairLinkUUID, repairFIFOUUID} {
		if _, err := os.Lstat(a.entry(old)); !os.IsNotExist(err) {
			t.Errorf("the old entry %s is still there: %v", old, err)
		}
	}
	d, err := Open(a.dbPath, a.archiveDir, nil)
	if err != nil {
		t.Fatalf("the second Open: %v", err)
	}
	defer d.Close()
	if rec, err := d.QueryByUUID(repairFIFOUUID); err != nil || rec.NodeType != archive.NodeTypeFIFO {
		t.Errorf("the FIFO record: %+v (%v)", rec, err)
	}
}
