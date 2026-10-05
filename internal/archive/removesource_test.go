package archive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The window between Execute and RemoveSource.
//
// Those two calls are deliberately separate so the caller can record the
// deletion in between, and that recording is a SQLite write: on a contended
// archive it retries for tens of seconds before it either succeeds or gives up.
// The source path is live for all of it, and removing it afterwards by name
// alone gets two things wrong.
//
// The path can be REPLACED -- renamed over, or removed and recreated -- and
// os.Remove would then destroy a file nothing ever archived while the archive
// holds the original. And a regular file's archive entry is a hard link, so a
// write THROUGH the path rewrites the archived bytes: the hash that was
// recorded describes content that no longer exists, and removing the source
// would leave that record standing over a blob it does not match, silently and
// permanently.
//
// These tests reproduce both without any timing: the window is opened by hand,
// which is what the two halves being separate calls makes possible.

// linkedFilePlan archives a regular file as a hard link and returns the plan
// with the window open: the entry exists, the source is still there.
func linkedFilePlan(t *testing.T, dir, name, content string) *Plan {
	t.Helper()

	src := filepath.Join(dir, name)
	if err := os.WriteFile(src, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(src, filepath.Join(dir, "archive"), false)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !plan.linked {
		t.Fatalf("the file was copied, not linked; this test is about the linked case")
	}
	return plan
}

// A file removed and recreated at the same path is a different inode. The
// archive holds the original; the name leads somewhere else.
func TestRemoveSource_RefusesAReplacedFile(t *testing.T) {
	dir := t.TempDir()
	plan := linkedFilePlan(t, dir, "target.txt", "the archived bytes")

	if err := os.Remove(plan.Source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan.Source, []byte("a completely different file"), 0644); err != nil {
		t.Fatal(err)
	}

	err := RemoveSource(plan)
	if !errors.Is(err, ErrSourceReplaced) {
		t.Fatalf("expected ErrSourceReplaced, got %v", err)
	}

	got, readErr := os.ReadFile(plan.Source)
	if readErr != nil {
		t.Fatalf("the replacement was destroyed by a removal that went by name: %v", readErr)
	}
	if string(got) != "a completely different file" {
		t.Errorf("the replacement's content changed: %q", got)
	}
	archived, err := os.ReadFile(plan.Dest)
	if err != nil {
		t.Fatalf("reading the archive entry: %v", err)
	}
	if string(archived) != "the archived bytes" {
		t.Errorf("the archive entry no longer holds the original: %q", archived)
	}
}

// A rename over the path is the same replacement by another route, and it is
// the one an editor's atomic save performs.
func TestRemoveSource_RefusesAFileRenamedOver(t *testing.T) {
	dir := t.TempDir()
	plan := linkedFilePlan(t, dir, "target.txt", "the archived bytes")

	other := filepath.Join(dir, "other.txt")
	if err := os.WriteFile(other, []byte("saved over the top"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, plan.Source); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); !errors.Is(err, ErrSourceReplaced) {
		t.Fatalf("expected ErrSourceReplaced, got %v", err)
	}
	if got, err := os.ReadFile(plan.Source); err != nil || string(got) != "saved over the top" {
		t.Fatalf("the renamed-in file was destroyed: content=%q err=%v", got, err)
	}
}

// A write through the path reaches the archived bytes, because they are the
// same inode. The record's hash is stale the moment it happens.
func TestRemoveSource_RefusesALinkedFileWrittenThrough(t *testing.T) {
	dir := t.TempDir()
	plan := linkedFilePlan(t, dir, "target.txt", "the archived bytes")

	if err := os.WriteFile(plan.Source, []byte("rewritten while the insert was in flight"), 0644); err != nil {
		t.Fatal(err)
	}

	err := RemoveSource(plan)
	if !errors.Is(err, ErrArchivedContentChanged) {
		t.Fatalf("expected ErrArchivedContentChanged, got %v", err)
	}
	if _, err := os.Lstat(plan.Source); err != nil {
		t.Errorf("the source was removed despite the mismatch: %v", err)
	}
}

// Same length, so only the mtime distinguishes it. Chtimes sets a plainly
// different one rather than relying on the clock's granularity between two
// writes in the same microsecond.
func TestRemoveSource_RefusesASameSizedRewrite(t *testing.T) {
	dir := t.TempDir()
	plan := linkedFilePlan(t, dir, "target.txt", "aaaaaaaa")

	if err := os.WriteFile(plan.Source, []byte("bbbbbbbb"), 0644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(plan.Source, later, later); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); !errors.Is(err, ErrArchivedContentChanged) {
		t.Fatalf("expected ErrArchivedContentChanged, got %v", err)
	}
}

// The other half of the content check: a changed mtime over identical bytes is
// not a reason to refuse, or every `touch` during a slow insert would abort a
// delete. The re-hash is what tells the two apart.
func TestRemoveSource_AcceptsATouchThatLeftTheContentAlone(t *testing.T) {
	dir := t.TempDir()
	plan := linkedFilePlan(t, dir, "target.txt", "unchanged bytes")

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(plan.Source, later, later); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); err != nil {
		t.Fatalf("a touched but unmodified file must still be removable: %v", err)
	}
	if _, err := os.Lstat(plan.Source); !os.IsNotExist(err) {
		t.Errorf("the source survived a successful RemoveSource (err=%v)", err)
	}
}

// The archive entry itself can be replaced, and then it is not the file that is
// about to be destroyed either. It is its own error: the source is still what
// it was, so saying "the source path no longer names the file that was
// archived" would name the wrong half of the pair -- and the record does NOT
// hold the archived content here, which is what the caller has to be told.
func TestRemoveSource_RefusesWhenTheArchiveEntryIsNoLongerTheSource(t *testing.T) {
	dir := t.TempDir()
	plan := linkedFilePlan(t, dir, "target.txt", "the archived bytes")

	if err := os.Remove(plan.Dest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan.Dest, []byte("not the original"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); !errors.Is(err, ErrArchiveEntryReplaced) {
		t.Fatalf("expected ErrArchiveEntryReplaced, got %v", err)
	}
	if _, err := os.Lstat(plan.Source); err != nil {
		t.Errorf("the source was removed with no archived copy of it: %v", err)
	}
}

// When the link was refused and the entry is an independent copy, a write
// through the path does not reach the archived bytes: the record stays true and
// the blob stays right, but the path now holds newer content that nothing
// archived, so it still must not be removed.
func TestRemoveSource_RefusesACopiedSourceThatDiverged(t *testing.T) {
	dir := t.TempDir()
	refuseLinks(t, syscall.EXDEV)

	src := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(src, []byte("the archived bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(src, filepath.Join(dir, "archive"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if plan.linked {
		t.Fatalf("the file was linked; this test is about the copy fallback")
	}

	if err := os.WriteFile(src, []byte("newer content the archive never saw"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); !errors.Is(err, ErrSourceDiverged) {
		t.Fatalf("expected ErrSourceDiverged, got %v", err)
	}
	if _, err := os.Lstat(src); err != nil {
		t.Errorf("the source was removed although the archive holds older content: %v", err)
	}
	archived, err := os.ReadFile(plan.Dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(archived) != "the archived bytes" {
		t.Errorf("the copy was disturbed: %q", archived)
	}
}

// A directory has no content the archive shares -- the .tar.zst is a snapshot --
// so identity is the whole question for it: the tree that is recursively
// removed must be the tree that was compressed.
func TestRemoveSource_RefusesAReplacedDirectory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "f.txt"), []byte("archived"), 0644); err != nil {
		t.Fatal(err)
	}

	plan, err := NewPlan(src, filepath.Join(dir, "archive"), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	survivor := filepath.Join(src, "unrelated.txt")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(survivor, []byte("belongs to whoever made this one"), 0644); err != nil {
		t.Fatal(err)
	}

	// Which refusal this is depends on how the filesystem hands out inode
	// numbers, and both are correct. A filesystem that gives the recreated
	// directory a fresh inode fails the identity check outright; one that hands
	// back the inode the removal just freed -- ext4 routinely does -- gets past
	// it, and the tree walk refuses on the unarchived file instead. What the
	// removal must never do is proceed.
	err = RemoveSource(plan)
	if !errors.Is(err, ErrSourceReplaced) && !errors.Is(err, ErrDirectoryChanged) {
		t.Fatalf("expected ErrSourceReplaced or ErrDirectoryChanged, got %v", err)
	}
	if _, err := os.Stat(survivor); err != nil {
		t.Fatalf("the recursive removal destroyed a tree nothing archived: %v", err)
	}
}

// A dev/ino pair is not a durable name for a path: the filesystem is free to
// hand the inode number of an unlinked file straight back to the next one
// created in its place, and ext4 does it routinely. The identity check then
// reports a replacement as the archived thing, so whatever stands behind it has
// to carry the refusal on its own.
//
// Both tests below stand in for the reuse rather than waiting for a filesystem
// that performs it -- a tmpfs never reuses an inode number, so on a developer's
// machine these paths are otherwise unreachable while a CI runner's ext4 /tmp
// takes them every time.
func TestRemoveSource_RefusesAReplacedDirectoryOnAReusedInode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "f.txt"), []byte("archived"), 0644); err != nil {
		t.Fatal(err)
	}

	plan, err := NewPlan(src, filepath.Join(dir, "archive"), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	survivor := filepath.Join(src, "unrelated.txt")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(survivor, []byte("belongs to whoever made this one"), 0644); err != nil {
		t.Fatal(err)
	}

	replaced, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	plan.identity = replaced

	if err := RemoveSource(plan); !errors.Is(err, ErrDirectoryChanged) {
		t.Fatalf("expected ErrDirectoryChanged, got %v", err)
	}
	if _, err := os.Stat(survivor); err != nil {
		t.Fatalf("the recursive removal destroyed a tree nothing archived: %v", err)
	}
}

// A symlink has no content and no member list, so the identity check was the
// whole of its verification -- and past a reused inode number that check says
// yes. The recorded target is the only thing left that tells the archived link
// apart from one written over it.
func TestRemoveSource_RefusesAReplacedSymlinkOnAReusedInode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "link")
	if err := os.Symlink("/etc/hostname", src); err != nil {
		t.Fatal(err)
	}

	plan, err := NewPlan(src, filepath.Join(dir, "archive"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", src); err != nil {
		t.Fatal(err)
	}

	replaced, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	plan.identity = replaced

	if err := RemoveSource(plan); !errors.Is(err, ErrSourceReplaced) {
		t.Fatalf("expected ErrSourceReplaced, got %v", err)
	}
	target, err := os.Readlink(src)
	if err != nil {
		t.Fatalf("the replacement symlink was destroyed: %v", err)
	}
	if target != "/etc/hosts" {
		t.Errorf("the surviving symlink points at %q", target)
	}
}

func TestRemoveSource_RefusesAReplacedSymlink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "link")
	if err := os.Symlink("/etc/hostname", src); err != nil {
		t.Fatal(err)
	}

	plan, err := NewPlan(src, filepath.Join(dir, "archive"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", src); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); !errors.Is(err, ErrSourceReplaced) {
		t.Fatalf("expected ErrSourceReplaced, got %v", err)
	}
	target, err := os.Readlink(src)
	if err != nil {
		t.Fatalf("the replacement symlink was destroyed: %v", err)
	}
	if target != "/etc/hosts" {
		t.Errorf("the surviving symlink points at %q", target)
	}
}

// The other half of the same question, and the one the identity check alone
// never asked: the ARCHIVED COPY has to still be there. The row is inserted
// before the source is removed, so a concurrent `saferm purge --all` can
// legitimately select that row and destroy its blob while the archival is
// still inside its window. Removing the source afterwards would leave no copy
// of the content anywhere -- the one outcome saferm exists to make impossible.
// Every node type is checked, because every node type can have its entry removed.

// directoryPlan archives a small tree and returns the plan with the window
// open: the .tar.zst exists, the tree is still there.
func directoryPlan(t *testing.T, dir, name string) *Plan {
	t.Helper()

	src := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "f.txt"), []byte("archived"), 0644); err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(src, filepath.Join(dir, "archive"), true)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return plan
}

// symlinkPlan archives a symlink and returns the plan with the window open.
func symlinkPlan(t *testing.T, dir, name, target string) *Plan {
	t.Helper()

	src := filepath.Join(dir, name)
	if err := os.Symlink(target, src); err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(src, filepath.Join(dir, "archive"), false)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return plan
}

// The linked file's entry: the identity check covered this one already, but it
// belongs beside the other three so all four node types are pinned in one place.
func TestRemoveSource_RefusesWhenALinkedEntryIsGone(t *testing.T) {
	dir := t.TempDir()
	plan := linkedFilePlan(t, dir, "target.txt", "the archived bytes")

	if err := os.Remove(plan.Dest); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); !errors.Is(err, ErrArchiveEntryMissing) {
		t.Fatalf("expected ErrArchiveEntryMissing, got %v", err)
	}
	if _, err := os.Lstat(plan.Source); err != nil {
		t.Errorf("the source was destroyed with no archived copy of it: %v", err)
	}
}

// The copy fallback's entry is an independent file, and nothing else on the
// machine holds those bytes once it is gone.
func TestRemoveSource_RefusesWhenACopiedEntryIsGone(t *testing.T) {
	dir := t.TempDir()
	refuseLinks(t, syscall.EXDEV)

	src := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(src, []byte("the archived bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(src, filepath.Join(dir, "archive"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if plan.linked {
		t.Fatalf("the file was linked; this test is about the copy fallback")
	}

	if err := os.Remove(plan.Dest); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); !errors.Is(err, ErrArchiveEntryMissing) {
		t.Fatalf("expected ErrArchiveEntryMissing, got %v", err)
	}
	got, readErr := os.ReadFile(src)
	if readErr != nil {
		t.Fatalf("the source was destroyed although the archive no longer holds it: %v", readErr)
	}
	if string(got) != "the archived bytes" {
		t.Errorf("the source's content changed: %q", got)
	}
}

// The .tar.zst is the only copy of a tree, and RemoveAll is the least
// recoverable removal saferm performs.
func TestRemoveSource_RefusesWhenADirectoryEntryIsGone(t *testing.T) {
	dir := t.TempDir()
	plan := directoryPlan(t, dir, "tree")

	if err := os.Remove(plan.Dest); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); !errors.Is(err, ErrArchiveEntryMissing) {
		t.Fatalf("expected ErrArchiveEntryMissing, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(plan.Source, "nested", "f.txt")); err != nil {
		t.Errorf("the tree was destroyed with no archived copy of it: %v", err)
	}
}

// A symlink's entry is the .symlink metadata file, and it is the only record of
// the target outside the database.
func TestRemoveSource_RefusesWhenASymlinkEntryIsGone(t *testing.T) {
	dir := t.TempDir()
	plan := symlinkPlan(t, dir, "link", "/etc/hostname")

	if err := os.Remove(plan.Dest); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); !errors.Is(err, ErrArchiveEntryMissing) {
		t.Fatalf("expected ErrArchiveEntryMissing, got %v", err)
	}
	if _, err := os.Lstat(plan.Source); err != nil {
		t.Errorf("the symlink was destroyed with no archived copy of it: %v", err)
	}
}

// The entry check is an Lstat, not a Stat, and the difference is a real hole:
// a Stat follows a symlink, so an entry replaced by a link back at the source
// would satisfy SameFile and let the removal proceed -- destroying the source
// and leaving the "archive entry" a dangling link.
func TestRemoveSource_RefusesAnEntryReplacedByALinkBackAtTheSource(t *testing.T) {
	dir := t.TempDir()
	plan := linkedFilePlan(t, dir, "target.txt", "the archived bytes")

	if err := os.Remove(plan.Dest); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(plan.Source, plan.Dest); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); !errors.Is(err, ErrArchiveEntryReplaced) {
		t.Fatalf("expected ErrArchiveEntryReplaced, got %v", err)
	}
	if _, err := os.Lstat(plan.Source); err != nil {
		t.Errorf("the source was destroyed although the entry is only a link to it: %v", err)
	}
}

// A directory's identity is the top-level inode, and that alone says nothing
// about what is INSIDE the tree. The .tar.zst is a snapshot taken at Execute;
// anything written into the tree afterwards is in no archive at all, and
// RemoveAll destroys it. So the tree is re-walked against what actually went
// into the tar, and any file the archive does not hold refuses the removal.

// A file created inside the tree during the window is the audit's case: the tar
// was closed before it existed, and removing the tree would take it with it.
func TestRemoveSource_RefusesADirectoryThatGrewAfterItWasArchived(t *testing.T) {
	dir := t.TempDir()
	plan := directoryPlan(t, dir, "tree")

	newcomer := filepath.Join(plan.Source, "nested", "written-during-the-insert.txt")
	if err := os.WriteFile(newcomer, []byte("in no archive anywhere"), 0644); err != nil {
		t.Fatal(err)
	}

	err := RemoveSource(plan)
	if !errors.Is(err, ErrDirectoryChanged) {
		t.Fatalf("expected ErrDirectoryChanged, got %v", err)
	}
	if !strings.Contains(err.Error(), newcomer) {
		t.Errorf("the refusal must name the path the archive does not hold, got: %v", err)
	}
	if _, statErr := os.Stat(newcomer); statErr != nil {
		t.Errorf("the file that was in no archive was destroyed anyway: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(plan.Source, "nested", "f.txt")); statErr != nil {
		t.Errorf("the rest of the tree was destroyed: %v", statErr)
	}
}

// A whole subtree created during the window, which is the same case one level
// down: the walk reaches it, and neither the directory nor its file is in the
// tar.
func TestRemoveSource_RefusesADirectoryWithANewSubtree(t *testing.T) {
	dir := t.TempDir()
	plan := directoryPlan(t, dir, "tree")

	sub := filepath.Join(plan.Source, "late")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "g.txt"), []byte("also unarchived"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); !errors.Is(err, ErrDirectoryChanged) {
		t.Fatalf("expected ErrDirectoryChanged, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "g.txt")); err != nil {
		t.Errorf("the unarchived subtree was destroyed: %v", err)
	}
}

// A file rewritten in place during the window: the name is in the tar, the
// bytes in it are not. Size and mtime are what tell it apart, the same cheap
// check the regular-file case uses before it re-hashes.
func TestRemoveSource_RefusesADirectoryWhoseFileWasRewritten(t *testing.T) {
	dir := t.TempDir()
	plan := directoryPlan(t, dir, "tree")

	rewritten := filepath.Join(plan.Source, "nested", "f.txt")
	if err := os.WriteFile(rewritten, []byte("content the tar never saw"), 0644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(rewritten, later, later); err != nil {
		t.Fatal(err)
	}

	err := RemoveSource(plan)
	if !errors.Is(err, ErrDirectoryChanged) {
		t.Fatalf("expected ErrDirectoryChanged, got %v", err)
	}
	if !strings.Contains(err.Error(), rewritten) {
		t.Errorf("the refusal must name the file whose bytes are not in the archive, got: %v", err)
	}
	got, readErr := os.ReadFile(rewritten)
	if readErr != nil {
		t.Fatalf("the rewritten file was destroyed: %v", readErr)
	}
	if string(got) != "content the tar never saw" {
		t.Errorf("the rewritten file's content changed: %q", got)
	}
}

// The other direction is not a refusal: a file DELETED during the window means
// the archive holds more than the tree does, which is the state a completed
// archival is aiming for anyway. Refusing here would abort a delete because
// somebody else's cleanup got there first.
func TestRemoveSource_AcceptsADirectoryThatOnlyLostContent(t *testing.T) {
	dir := t.TempDir()
	plan := directoryPlan(t, dir, "tree")

	if err := os.Remove(filepath.Join(plan.Source, "nested", "f.txt")); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); err != nil {
		t.Fatalf("a tree the archive fully covers must still be removable: %v", err)
	}
	if _, err := os.Lstat(plan.Source); !os.IsNotExist(err) {
		t.Errorf("the tree survived a successful RemoveSource (err=%v)", err)
	}
}

// An untouched tree is removed, which is the whole point of the walk being a
// comparison rather than a timestamp: the archival itself does not make the
// tree look changed.
func TestRemoveSource_RemovesAnUnchangedDirectory(t *testing.T) {
	dir := t.TempDir()
	plan := directoryPlan(t, dir, "tree")

	if err := RemoveSource(plan); err != nil {
		t.Fatalf("an unchanged tree must be removable: %v", err)
	}
	if _, err := os.Lstat(plan.Source); !os.IsNotExist(err) {
		t.Errorf("the tree survived a successful RemoveSource (err=%v)", err)
	}
}

// A source that vanished is not a removal that quietly succeeded: it is
// reported, so the caller can say which record holds the only remaining copy.
func TestRemoveSource_ReportsAVanishedSource(t *testing.T) {
	dir := t.TempDir()
	plan := linkedFilePlan(t, dir, "target.txt", "content")

	if err := os.Remove(plan.Source); err != nil {
		t.Fatal(err)
	}

	err := RemoveSource(plan)
	if err == nil {
		t.Fatal("a source that is already gone must be reported, not passed over")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the error must say the path is gone, got: %v", err)
	}
}

// Fail closed: with nothing recorded there is nothing to check the path
// against, so the irreversible half refuses rather than trusting the name.
func TestRemoveSource_RefusesAPlanThatWasNeverExecuted(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(src, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(src, filepath.Join(dir, "archive"), false)
	if err != nil {
		t.Fatal(err)
	}

	if err := RemoveSource(plan); !errors.Is(err, ErrNotExecuted) {
		t.Fatalf("expected ErrNotExecuted, got %v", err)
	}
	if _, err := os.Lstat(src); err != nil {
		t.Errorf("an unexecuted plan removed the source anyway: %v", err)
	}
}
