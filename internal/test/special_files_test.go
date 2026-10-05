//go:build !windows

package test

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net"
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

// specialLimit is how long a delete of a special file may take. Archiving one
// reads nothing, so it is instant; a run that reaches the limit was blocked on
// a FIFO it opened.
const specialLimit = 20 * time.Second

// makeFIFO creates a FIFO at path with mode perm, the umask notwithstanding.
func makeFIFO(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := syscall.Mkfifo(path, uint32(perm)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// makeSocket binds a unix socket at path and closes the listener, leaving the
// socket file behind with mode perm, which is what a crashed server leaves.
func makeSocket(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// archiveSpecial archives one path and returns the uuid `delete` reported for it.
func archiveSpecial(t *testing.T, home, path string) string {
	t.Helper()
	stdout, stderr, code := runSafermWithin(t, specialLimit, home, nil,
		"delete", "--on-error", "abort", "--description", "special file test", path)
	if code != 0 {
		t.Fatalf("delete %s: exit %d\nstderr: %s", path, code, stderr)
	}
	lines := parseArchivedLines(t, stdout)
	if len(lines) != 1 {
		t.Fatalf("delete reported %d archived lines:\n%s", len(lines), stdout)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s is still there after the delete (%v)", path, err)
	}
	return lines[0][1]
}

// assertInfoType checks the Type line `info` prints for a record.
func assertInfoType(t *testing.T, home, uuid, want string) {
	t.Helper()
	out, stderr, code := runSaferm(t, home, "info", uuid)
	if code != 0 {
		t.Fatalf("info %s: exit %d: %s", uuid, code, stderr)
	}
	if got := parseInfoField(t, out, "Type:"); got != want {
		t.Errorf("info Type = %q, want %q", got, want)
	}
	if got := parseInfoField(t, out, "Status:"); got != "restorable" {
		t.Errorf("info Status = %q, want restorable", got)
	}
}

// undeleteOne restores a record and returns the restored path's mode.
func undeleteOne(t *testing.T, home, uuid, path string) fs.FileMode {
	t.Helper()
	_, stderr, code := runSafermWithin(t, specialLimit, home, nil, "undelete", uuid)
	if code != 0 {
		t.Fatalf("undelete %s: exit %d\nstderr: %s", uuid, code, stderr)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("nothing at %s after the undelete: %v", path, err)
	}
	return info.Mode()
}

func TestDelete_AFIFOIsArchivedWithoutBeingReadAndRestoredAsAFIFO(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	fifo := filepath.Join(t.TempDir(), "pipe")
	makeFIFO(t, fifo, 0o640)

	uuid := archiveSpecial(t, home, fifo)
	assertInfoType(t, home, uuid, "fifo")

	mode := undeleteOne(t, home, uuid, fifo)
	if mode.Type() != fs.ModeNamedPipe || mode.Perm() != 0o640 {
		t.Errorf("restored mode %s, want a FIFO with 0640", mode)
	}
}

func TestDelete_ASocketIsArchivedAndRestoredAsASocket(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	sock := filepath.Join(t.TempDir(), "sk")
	makeSocket(t, sock, 0o600)

	uuid := archiveSpecial(t, home, sock)
	assertInfoType(t, home, uuid, "socket")

	mode := undeleteOne(t, home, uuid, sock)
	if mode.Type() != fs.ModeSocket || mode.Perm() != 0o600 {
		t.Errorf("restored mode %s, want a socket with 0600", mode)
	}
}

// The dry run of a FIFO's delete names what would really be written: the
// node's descriptor, with its real length. It used to promise a 0-byte write of
// an archive entry that was in fact a hard link to the FIFO.
func TestDelete_DryRunOfAFIFONamesTheDescriptor(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	fifo := filepath.Join(t.TempDir(), "pipe")
	makeFIFO(t, fifo, 0o644)

	stdout, stderr, code := runSafermWithin(t, specialLimit, home, nil,
		"--dry-run", "delete", "--on-error", "abort", "--description", "preview", fifo)
	if code != 0 {
		t.Fatalf("dry run: exit %d: %s", code, stderr)
	}
	log := wouldDoLog(stdout)
	if !strings.Contains(log, ".node (") || strings.Contains(log, ".node (0 bytes)") {
		t.Errorf("the would-do log does not name a non-empty .node descriptor:\n%s", log)
	}
	if _, err := os.Lstat(fifo); err != nil {
		t.Errorf("the dry run touched the FIFO: %v", err)
	}
}

// archiveDescriptor writes a special file's record and descriptor straight into
// the test archive, the way a delete of that node would have left them, and
// returns the record's uuid. A device cannot be created, and so cannot be
// deleted, by an unprivileged test.
func archiveDescriptor(t *testing.T, home, path string, n archive.Node) string {
	t.Helper()
	descriptor := archive.EncodeNode(n)
	sum := sha256.Sum256(descriptor)
	uuid := archive.NewUUID()
	archiveDir := filepath.Join(home, ".saferm", "archive")
	if err := os.WriteFile(archive.EntryPath(archiveDir, uuid, n.Kind), descriptor, 0o600); err != nil {
		t.Fatal(err)
	}
	d := openArchive(t, home)
	if _, err := d.Insert(&db.DeletionRecord{
		UUID: uuid, OriginalPath: path, OriginalName: filepath.Base(path), Size: 0,
		Hash: hex.EncodeToString(sum[:]), Kind: n.Kind, DeletedAt: time.Now(), Description: "a device",
	}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	return uuid
}

// Restoring a device needs the privilege mknod asks for. Without it the
// undelete is a hard error that keeps the descriptor, so the record stays
// restorable by a process that has the privilege.
func TestUndelete_ADeviceWithoutPrivilegeFailsAndKeepsTheEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mknod of a device succeeds")
	}
	home := testutil.SetupTestEnv(t)
	dest := filepath.Join(t.TempDir(), "null")
	uuid := archiveDescriptor(t, home, dest, archive.Node{Kind: archive.KindCharacterDevice, Perm: 0o666, Major: 1, Minor: 3})
	assertInfoType(t, home, uuid, "character-device")

	_, stderr, code := runSafermWithin(t, specialLimit, home, nil, "undelete", uuid)
	if code != 6 {
		t.Fatalf("undelete of a device without privilege: exit %d, want 6\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "needs privilege") || !strings.Contains(stderr, "still restorable") {
		t.Errorf("stderr does not say why and that the record is kept: %s", stderr)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Errorf("something was left at the destination: %v", err)
	}
	assertInfoType(t, home, uuid, "character-device")
}

// A tree holding a FIFO and a socket is archived without reading either, and
// the undelete that reports success brings both back.
func TestDelete_ATreeWithSpecialFilesRoundTrips(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	tree := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	makeFIFO(t, filepath.Join(tree, "pipe"), 0o640)
	makeSocket(t, filepath.Join(tree, "sk"), 0o700)

	stdout, stderr, code := runSafermWithin(t, specialLimit, home, nil,
		"delete", "-r", "--on-error", "abort", "--description", "special tree", tree)
	if code != 0 {
		t.Fatalf("delete: exit %d: %s", code, stderr)
	}
	uuid := parseArchivedLines(t, stdout)[0][1]
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatalf("the tree is still there: %v", err)
	}
	if _, stderr, code := runSafermWithin(t, specialLimit, home, nil, "undelete", uuid); code != 0 {
		t.Fatalf("undelete: exit %d: %s", code, stderr)
	}
	for name, want := range map[string]fs.FileMode{"pipe": fs.ModeNamedPipe | 0o640, "sk": fs.ModeSocket | 0o700} {
		info, err := os.Lstat(filepath.Join(tree, name))
		if err != nil {
			t.Errorf("%s was not restored: %v", name, err)
			continue
		}
		if info.Mode() != want {
			t.Errorf("%s restored as %s, want %s", name, info.Mode(), want)
		}
	}
}
