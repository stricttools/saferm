//go:build linux || darwin

package archive

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// treeWithFIFO makes a tree holding a regular file and a FIFO with mode 0640.
func treeWithFIFO(t *testing.T, parent string) string {
	t.Helper()
	tree := filepath.Join(parent, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(tree, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fifo, 0o640); err != nil {
		t.Fatal(err)
	}
	return tree
}

// archiveTree archives a tree within a bound, so a member read that blocks
// fails the test instead of hanging it.
func archiveTree(t *testing.T, tree, archiveDir string) *ArchiveResult {
	t.Helper()
	var result *ArchiveResult
	if err := withTimeout(t, 5*time.Second, func() error {
		var err error
		result, err = archiveNow(tree, archiveDir, true)
		return err
	}); err != nil {
		t.Fatalf("archiving %s: %v", tree, err)
	}
	return result
}

func TestTree_AFIFOInsideIsRestoredAsAFIFO(t *testing.T) {
	tmp := t.TempDir()
	tree := treeWithFIFO(t, tmp)
	archiveDir := filepath.Join(tmp, "archive")
	result := archiveTree(t, tree, archiveDir)

	if err := restoreNow(result.UUID, archiveDir, tree, KindDirectory, ""); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	info, err := os.Lstat(filepath.Join(tree, "pipe"))
	if err != nil {
		t.Fatalf("the FIFO was not restored: %v", err)
	}
	if info.Mode().Type() != fs.ModeNamedPipe || info.Mode().Perm() != 0o640 {
		t.Errorf("restored %s, want a FIFO with 0640", info.Mode())
	}
}

func TestTree_ASocketInsideIsArchivedAndRestored(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a socket node is recreated on Linux only")
	}
	tmp := t.TempDir()
	tree := filepath.Join(tmp, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(tree, "sk")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	archiveDir := filepath.Join(tmp, "archive")
	result := archiveTree(t, tree, archiveDir)

	if err := restoreNow(result.UUID, archiveDir, tree, KindDirectory, ""); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	info, err := os.Lstat(sock)
	if err != nil {
		t.Fatalf("the socket was not restored: %v", err)
	}
	if info.Mode().Type() != fs.ModeSocket {
		t.Errorf("restored %s, want a socket", info.Mode())
	}
}

// A member that was a FIFO when the tar was written and is a regular file by
// the time the tree is removed is not what the archive holds: removing the
// tree would destroy a file nothing archived.
func TestRemoveSource_RefusesATreeWhoseMemberChangedType(t *testing.T) {
	tmp := t.TempDir()
	tree := treeWithFIFO(t, tmp)
	p, err := NewPlan(tree, filepath.Join(tmp, "archive"), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := withTimeout(t, 5*time.Second, func() error { _, err := Execute(p); return err }); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(tree, "pipe")
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fifo, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSource(p); !errors.Is(err, ErrDirectoryChanged) {
		t.Fatalf("got %v, want ErrDirectoryChanged", err)
	}
	if _, err := os.Lstat(fifo); err != nil {
		t.Errorf("the unarchived file was destroyed: %v", err)
	}
}

// writeTarZst writes a .tar.zst holding a top directory and the given members.
func writeTarZst(t *testing.T, path, top string, members ...*tar.Header) {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, h := range members {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	zw, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A member whose typeflag no case handles -- a hard link here -- fails the
// extraction instead of being skipped while the restore reports success.
func TestExtractTree_RefusesAMemberTypeItCannotRecreate(t *testing.T) {
	tmp := t.TempDir()
	uuid := NewUUID()
	writeTarZst(t, filepath.Join(tmp, uuid+".tar.zst"), "top",
		&tar.Header{Typeflag: tar.TypeLink, Name: "top/hard", Linkname: "top/other", Mode: 0o644})
	p := NewRestorePlan(uuid, tmp, filepath.Join(tmp, "dest"), KindDirectory, "")
	if _, err := ExtractTree(p); !errors.Is(err, ErrUnsupportedTarMember) {
		t.Fatalf("got %v, want ErrUnsupportedTarMember", err)
	}
}

// A device member without the privilege mknod needs fails the extraction, and
// the paths it had created are reported so the caller can take them back.
func TestExtractTree_ADeviceMemberWithoutPrivilegeFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mknod of a device succeeds")
	}
	tmp := t.TempDir()
	uuid := NewUUID()
	writeTarZst(t, filepath.Join(tmp, uuid+".tar.zst"), "top",
		&tar.Header{Typeflag: tar.TypeChar, Name: "top/null", Mode: 0o666, Devmajor: 1, Devminor: 3})
	dest := filepath.Join(tmp, "dest")
	p := NewRestorePlan(uuid, tmp, dest, KindDirectory, "")
	created, err := ExtractTree(p)
	if !errors.Is(err, ErrNodeNeedsPrivilege) {
		t.Fatalf("got %v, want ErrNodeNeedsPrivilege", err)
	}
	if len(created) == 0 || created[0] != dest {
		t.Errorf("created %v, want the destination directory it made first", created)
	}
}

// Every kind has a tar member the extraction has a case for: a kind added to
// the list without one fails here rather than being skipped on restore.
func TestExtractTree_EveryKindHasATarMemberCase(t *testing.T) {
	for _, k := range Kinds() {
		var h *tar.Header
		switch k {
		case KindFile:
			h = &tar.Header{Typeflag: tar.TypeReg, Name: "top/m", Mode: 0o644}
		case KindDirectory:
			h = &tar.Header{Typeflag: tar.TypeDir, Name: "top/m", Mode: 0o755}
		case KindSymlink:
			h = &tar.Header{Typeflag: tar.TypeSymlink, Name: "top/m", Linkname: "x"}
		default:
			var err error
			h, err = nodeTarHeader(Node{Kind: k, Perm: 0o600}, "top/m", time.Now())
			if err != nil {
				t.Fatalf("%s: %v", k, err)
			}
		}
		tmp := t.TempDir()
		uuid := NewUUID()
		writeTarZst(t, filepath.Join(tmp, uuid+".tar.zst"), "top", h)
		_, err := ExtractTree(NewRestorePlan(uuid, tmp, filepath.Join(tmp, "dest"), KindDirectory, ""))
		if errors.Is(err, ErrUnsupportedTarMember) || errors.Is(err, ErrNodeDescriptorMalformed) {
			t.Errorf("%s: the extraction has no case for its member: %v", k, err)
		}
	}
}
