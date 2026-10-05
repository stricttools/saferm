//go:build linux || darwin

package archive

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNodeDescriptor_RoundTripsEverySpecialFileType(t *testing.T) {
	for _, n := range []Node{
		{NodeType: NodeTypeFIFO, Perm: 0o640},
		{NodeType: NodeTypeSocket, Perm: 0o755 | fs.ModeSticky},
		{NodeType: NodeTypeCharacterDevice, Perm: 0o666, Major: 1, Minor: 3},
		{NodeType: NodeTypeBlockDevice, Perm: 0o660 | fs.ModeSetgid, Major: 259, Minor: 1 << 20},
	} {
		got, err := DecodeNode(EncodeNode(n))
		if err != nil {
			t.Fatalf("%s: %v", n.NodeType, err)
		}
		if got != n {
			t.Errorf("%s: decoded %+v, want %+v", n.NodeType, got, n)
		}
	}
}

func TestNodeDescriptor_RefusesAnythingElse(t *testing.T) {
	valid := string(EncodeNode(Node{NodeType: NodeTypeFIFO, Perm: 0o644}))
	for label, data := range map[string]string{
		"empty":                  "",
		"no trailing newline":    strings.TrimSuffix(valid, "\n"),
		"extra line":             valid + "extra\n",
		"wrong header":           strings.Replace(valid, "saferm-node 1", "saferm-node 2", 1),
		"not a special file":     strings.Replace(valid, "node_type fifo", "node_type file", 1),
		"unknown node type":      strings.Replace(valid, "node_type fifo", "node_type door", 1),
		"mode not octal":         strings.Replace(valid, "mode 0644", "mode 0698", 1),
		"mode beyond permission": strings.Replace(valid, "mode 0644", "mode 10644", 1),
		"fifo with a device":     strings.Replace(valid, "major 0", "major 8", 1),
		"non-canonical mode":     strings.Replace(valid, "mode 0644", "mode 644", 1),
	} {
		if _, err := DecodeNode([]byte(data)); !errors.Is(err, ErrNodeDescriptorMalformed) {
			t.Errorf("%s: got %v, want ErrNodeDescriptorMalformed", label, err)
		}
	}
}

func TestClassify_RefusesAnIrregularFileByName(t *testing.T) {
	_, err := Classify("/some/door", fs.ModeIrregular|0o644)
	var unsupported *UnsupportedFileError
	if !errors.As(err, &unsupported) {
		t.Fatalf("got %v, want an UnsupportedFileError", err)
	}
	if !strings.Contains(err.Error(), "an irregular file") || !strings.Contains(err.Error(), "/some/door") {
		t.Errorf("the error does not name the path and its type: %v", err)
	}
}

// A device is archived as a descriptor without being opened: /dev/null is the
// one device every test machine has, and reading it would return nothing, so
// what this checks is the descriptor -- its node type and device numbers -- and that
// the source is untouched (Execute never removes it).
func TestNode_ADeviceIsArchivedAsADescriptor(t *testing.T) {
	archiveDir := filepath.Join(t.TempDir(), "archive")
	p, err := NewPlan("/dev/null", archiveDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.NodeType != NodeTypeCharacterDevice || !strings.HasSuffix(p.Dest, ".node") {
		t.Fatalf("plan node type %q, entry %s", p.NodeType, p.Dest)
	}
	result, err := Execute(p)
	if err != nil {
		t.Fatal(err)
	}
	if result.NodeType != NodeTypeCharacterDevice || result.Size != 0 || result.Hash == "" {
		t.Errorf("result %+v", result)
	}
	data, err := os.ReadFile(p.Dest)
	if err != nil {
		t.Fatal(err)
	}
	n, err := DecodeNode(data)
	if err != nil {
		t.Fatal(err)
	}
	major, minor := deviceNumbers(mustLstat(t, "/dev/null"))
	if n.NodeType != NodeTypeCharacterDevice || n.Major != major || n.Minor != minor {
		t.Errorf("descriptor %+v, want a character device %d,%d", n, major, minor)
	}
	if _, err := os.Lstat("/dev/null"); err != nil {
		t.Fatalf("Execute touched /dev/null: %v", err)
	}
}

// Without the privilege mknod needs, restoring a device is a hard error that
// keeps the descriptor, so the record stays restorable by a process that has it.
func TestRestoreNode_ADeviceWithoutPrivilegeIsAHardErrorThatKeepsTheEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mknod of a device succeeds")
	}
	tmp := t.TempDir()
	archiveDir := filepath.Join(tmp, "archive")
	p, err := NewPlan("/dev/null", archiveDir, false)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Execute(p)
	if err != nil {
		t.Fatal(err)
	}
	rp := NewRestorePlan(result.UUID, archiveDir, filepath.Join(tmp, "null"), NodeTypeCharacterDevice, "")
	if err := VerifyEntry(rp, result.Hash); err != nil {
		t.Fatalf("the descriptor does not verify: %v", err)
	}
	if err := RestoreNode(rp); !errors.Is(err, ErrNodeNeedsPrivilege) {
		t.Fatalf("got %v, want ErrNodeNeedsPrivilege", err)
	}
	if _, err := os.Lstat(rp.Entry); err != nil {
		t.Errorf("the descriptor is gone after a failed restore: %v", err)
	}
	if _, err := os.Lstat(rp.Dest); !os.IsNotExist(err) {
		t.Errorf("something was left at the destination: %v", err)
	}
}

func TestNode_AFIFORoundTripsThroughArchiveAndRestore(t *testing.T) {
	tmp := t.TempDir()
	fifo := filepath.Join(tmp, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fifo, 0o751); err != nil {
		t.Fatal(err)
	}
	archiveDir := filepath.Join(tmp, "archive")
	var result *ArchiveResult
	if err := withTimeout(t, 5*time.Second, func() error {
		var err error
		result, err = archiveNow(fifo, archiveDir, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rp := NewRestorePlan(result.UUID, archiveDir, fifo, NodeTypeFIFO, "")
	if err := VerifyEntry(rp, result.Hash); err != nil {
		t.Fatal(err)
	}
	if err := RestoreNode(rp); err != nil {
		t.Fatal(err)
	}
	info := mustLstat(t, fifo)
	if info.Mode().Type() != fs.ModeNamedPipe || info.Mode().Perm() != 0o751 {
		t.Errorf("restored %s, want a FIFO with 0751", info.Mode())
	}
}

// A FIFO swapped for another one with a different mode between the archival
// and the removal is not what the descriptor holds, so it is not removed, even
// where the filesystem hands the new node the old inode number.
func TestRemoveSource_RefusesANodeThatChangedAfterItWasArchived(t *testing.T) {
	tmp := t.TempDir()
	fifo := filepath.Join(tmp, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := NewPlan(fifo, filepath.Join(tmp, "archive"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSource(p); !errors.Is(err, ErrSourceReplaced) {
		t.Fatalf("got %v, want ErrSourceReplaced", err)
	}
	if _, err := os.Lstat(fifo); err != nil {
		t.Errorf("the changed FIFO was removed: %v", err)
	}
}

// A descriptor of another node type than the record names is a corrupt entry, and
// EntryPresent -- which every restore runs -- says so before anything is made.
func TestEntryPresent_RefusesADescriptorOfAnotherNodeType(t *testing.T) {
	archiveDir := t.TempDir()
	uuid := NewUUID()
	entry := EntryPath(archiveDir, uuid, NodeTypeSocket)
	if err := os.WriteFile(entry, EncodeNode(Node{NodeType: NodeTypeFIFO, Perm: 0o644}), 0o600); err != nil {
		t.Fatal(err)
	}
	rp := NewRestorePlan(uuid, archiveDir, filepath.Join(archiveDir, "dest"), NodeTypeSocket, "")
	if err := EntryPresent(rp); !errors.Is(err, ErrEntryCorrupt) {
		t.Fatalf("got %v, want ErrEntryCorrupt", err)
	}
}

func mustLstat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
