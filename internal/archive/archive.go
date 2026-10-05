package archive

import (
	"archive/tar"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Sentinel errors.
var (
	ErrFileNotFound      = errors.New("file not found")
	ErrRecursiveRequired = errors.New("target is a directory; recursive flag required")
	ErrHashMismatch      = errors.New("hash mismatch after copy")

	// ErrNotRegularFile is a read that was about to consume something other
	// than a regular file's bytes: a FIFO (whose read blocks until a writer
	// appears and then drains what it sends), a socket, or a device. Every path
	// that reads a file's content opens it through [openRegular], which is the
	// one place this is decided.
	ErrNotRegularFile = errors.New("not a regular file")

	// What a restore can find wrong with an archived copy before it touches
	// anything at the destination. See [VerifyEntry] for what each kind's hash
	// does and does not prove.
	ErrEntryMissing  = errors.New("the archived copy is not in the archive")
	ErrEntryCorrupt  = errors.New("the archived copy is not what the record says it is")
	ErrEntryDiverged = errors.New("the archived symlink entry does not name the target the record names")
	ErrUnverifiable  = errors.New("the record carries no hash, so the archived copy cannot be checked before the destination is destroyed")

	// The ways a source can stop being what was archived while the caller is
	// recording the deletion. See [RemoveSource] for why that window is wide
	// enough to matter and what each of these means for the record.
	ErrSourceReplaced         = errors.New("the source path no longer names the file that was archived")
	ErrArchiveEntryMissing    = errors.New("the archived copy is gone, so nothing holds the content the removal would destroy")
	ErrArchiveEntryReplaced   = errors.New("the archive entry is no longer the file that was archived")
	ErrSourceDiverged         = errors.New("the source changed after it was hashed, and the archive holds an independent copy of the older content")
	ErrArchivedContentChanged = errors.New("the source was written through while its archive entry was a link to it, so the recorded hash no longer describes the archived bytes")
	ErrDirectoryChanged       = errors.New("the tree changed after it was archived, so the archive does not hold everything the removal would destroy")
	ErrNotExecuted            = errors.New("the plan was never executed, so there is nothing to check the source against")

	// ErrUnsupportedTarMember is a member of a tree's tar that no case of the
	// extraction recreates.
	ErrUnsupportedTarMember = errors.New("the archived tree holds a member saferm cannot recreate")
)

// linkFile is os.Link, indirected so a test can make the link fail.
//
// The copy-and-verify fallback runs only when the kernel or the filesystem
// refuses a hard link -- a different device, protected_hardlinks, a filesystem
// with no links, an inode at its link limit -- and a test process can provoke
// none of those on a temp directory it owns. Without this seam the fallback is
// reachable only in production, which is the one place it must not be first
// exercised.
var linkFile = os.Link

// ArchiveResult holds the outcome of archiving a file or directory.
type ArchiveResult struct {
	UUID          string
	Kind          Kind
	Hash          string
	Size          int64
	SymlinkTarget string
}

// Plan is everything an archival can determine by reading: what the entry is,
// where it will land, and (for a symlink) what it points at. Building one
// mutates nothing, so a caller can render a plan as a preview and stop, or hand
// it to [Execute] and go through with it.
type Plan struct {
	Source        string
	ArchiveDir    string
	UUID          string
	Kind          Kind
	Dest          string
	SymlinkTarget string

	// What [Execute] saw, and what [RemoveSource] checks the path against
	// before it destroys anything. identity is the source's own os.FileInfo as
	// of the archival, so it carries the dev/ino pair the identity check needs
	// and the size and mtime the content check needs; hash is what was
	// recorded for a file; linked says whether the archive entry is the
	// source's own inode or an independent copy, which is what decides the
	// meaning of a content change.
	identity os.FileInfo
	hash     string
	linked   bool

	// node is what a special file's descriptor holds, read off the stat the
	// plan was built from and taken again by [Execute].
	node Node

	// What went into the tar, for a directory: every path the archiving walk
	// wrote, with the size and mtime it had when it was written. A tree's
	// identity is one inode and says nothing about its contents, so this is the
	// only thing that can tell [RemoveSource] whether the archive covers what
	// the recursive removal is about to destroy.
	members map[string]member
}

// member is one path as it went into a directory's tar: enough to notice, on
// the way back, that the tree no longer holds only what was archived.
type member struct {
	size    int64
	modTime time.Time
	kind    Kind
	node    Node // for a special file, what its tar member describes
}

// NewPlan inspects path and resolves where archiving it would put it. It
// performs no mutation.
func NewPlan(path string, archiveDir string, isRecursive bool) (*Plan, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrFileNotFound
		}
		return nil, err
	}

	if info.IsDir() && !isRecursive {
		return nil, ErrRecursiveRequired
	}

	kind, err := Classify(path, info.Mode())
	if err != nil {
		return nil, err
	}
	p := &Plan{Source: path, ArchiveDir: archiveDir, UUID: NewUUID(), Kind: kind}
	switch kind {
	case KindSymlink:
		target, err := os.Readlink(path)
		if err != nil {
			return nil, fmt.Errorf("reading symlink target: %w", err)
		}
		p.SymlinkTarget = target
	case KindFIFO, KindSocket, KindCharacterDevice, KindBlockDevice:
		p.node = nodeOf(kind, info)
	case KindFile, KindDirectory:
	default:
		return nil, unknownKind(kind)
	}
	p.Dest = EntryPath(archiveDir, p.UUID, p.Kind)
	return p, nil
}

// Execute writes the archive entry a [Plan] describes, and LEAVES THE SOURCE
// WHERE IT IS.
//
// Archiving is deliberately split from removing the original, because between
// the two there is a third party: the caller's database, which is what makes an
// archived entry findable at all. Doing it in one step meant an archive that
// succeeded and a record that failed produced a blob nobody could name, a
// source path that was gone, and no way back -- and the live archive really did
// collect orphaned blobs that way. So the order is Execute, record, then
// [RemoveSource]; a record that fails calls [DiscardBlob] instead and the
// caller's file has never been touched.
//
// For a regular file the entry is a hard link to the original, which is why
// leaving the source in place costs nothing: the content is not copied and both
// names point at the same inode until RemoveSource drops one of them.
func Execute(p *Plan) (*ArchiveResult, error) {
	if err := os.MkdirAll(p.ArchiveDir, 0700); err != nil {
		return nil, fmt.Errorf("creating archive dir: %w", err)
	}
	switch p.Kind {
	case KindFile:
		return archiveFile(p)
	case KindDirectory:
		return archiveDirectory(p)
	case KindSymlink:
		return archiveSymlink(p)
	case KindFIFO, KindSocket, KindCharacterDevice, KindBlockDevice:
		return archiveNode(p)
	}
	return nil, unknownKind(p.Kind)
}

// NodeDescriptor is the descriptor [Execute] writes for a special file, as of
// the plan. It reads nothing, so a dry run can show the real bytes.
func (p *Plan) NodeDescriptor() []byte {
	return EncodeNode(p.node)
}

// RemoveSource removes the original an executed [Plan] archived. It is the
// second half of an archival and runs only once the entry is recorded.
//
// It removes by identity, not by name, and only once it has seen the archived
// copy. Between [Execute] and this call sits the caller's database insert, and
// that is not an instant: a contended SQLite write retries for tens of seconds,
// and both the source path and the archive entry are live filesystem paths the
// whole time. Three things can happen in there, and removing whatever the name
// happens to resolve to gets all three wrong:
//
//   - The path can be REPLACED -- renamed over, or removed and recreated. The
//     archive holds the original; the name now leads somewhere else, and
//     removing it would destroy a file nothing archived.
//   - A regular file's archive entry is a hard link, so a write THROUGH the
//     path mutates the archived bytes. The recorded hash then describes content
//     that no longer exists anywhere, and removing the source would leave that
//     record standing over a blob it does not match.
//   - The ENTRY can go, or stop being the archived thing. The row is inserted
//     before this runs, so a concurrent purge can select it and destroy its
//     blob perfectly legitimately; removing the source afterwards leaves no
//     copy of the content anywhere at all.
//
// So both sides are re-checked first and the removal is refused on any
// mismatch, with [ErrSourceReplaced], [ErrSourceDiverged],
// [ErrArchivedContentChanged], [ErrArchiveEntryMissing] or
// [ErrArchiveEntryReplaced] naming which one the caller is holding. Nothing is
// undone here: refusing to remove is the whole of the remedy this half can
// apply, and what to do about the record is the recording caller's decision.
func RemoveSource(p *Plan) error {
	if err := verifySource(p); err != nil {
		return err
	}
	switch p.Kind {
	case KindDirectory:
		return os.RemoveAll(p.Source)
	case KindFile, KindSymlink, KindFIFO, KindSocket, KindCharacterDevice, KindBlockDevice:
		return os.Remove(p.Source)
	}
	return unknownKind(p.Kind)
}

// verifySource reports whether the source is still the thing [Execute]
// archived AND the archived copy is still there to hold it.
//
// Both halves are checked for every kind, because the removal is irreversible
// for every kind and either half alone lets it through wrongly:
//
//   - The SOURCE's identity, by dev/ino -- necessary for every kind and
//     sufficient for none, because the filesystem reuses the inode number of an
//     unlinked path. A directory is checked against the member list the
//     archiving walk recorded, a symlink against the target that was archived
//     (see [verifySymlinkUnchanged]); a regular file is also checked for
//     content, and cheaply -- the size and mtime as of the hash against the
//     current stat, and a re-hash only when those differ, rather than
//     re-reading every archived file on every delete. A write that restores
//     both size and mtime is not detected; that is the one hole left open
//     deliberately, and it costs a full re-read to close.
//   - The ENTRY's existence. The record is inserted before this runs, which is
//     what makes the archived copy findable -- and findable by a concurrent
//     `saferm purge --all` too, which will legitimately select that row and
//     destroy its blob while this archival is still waiting on its own insert.
//     Nothing else on the machine holds a copied file's bytes, a tree's
//     .tar.zst or a symlink's recorded target, so removing the source with the
//     entry gone destroys the content outright.
//
// The entry check is an Lstat, not a Stat, and the difference is the whole
// point for the linked case: a Stat follows a symlink, so an entry replaced by
// a link back at the source would satisfy os.SameFile and let the removal
// proceed -- destroying the source and leaving the "archive entry" dangling.
func verifySource(p *Plan) error {
	if p.identity == nil {
		return ErrNotExecuted
	}

	cur, err := os.Lstat(p.Source)
	if err != nil {
		return fmt.Errorf("re-checking %s before removing it: %w", p.Source, err)
	}
	if !os.SameFile(cur, p.identity) {
		return fmt.Errorf("%s: %w", p.Source, ErrSourceReplaced)
	}

	entry, err := os.Lstat(p.Dest)
	if err != nil {
		return fmt.Errorf("%s: %w (%v)", p.Dest, ErrArchiveEntryMissing, err)
	}

	switch p.Kind {
	case KindDirectory:
		return verifyTreeUnchanged(p)
	case KindSymlink:
		return verifySymlinkUnchanged(p)
	case KindFile:
		return verifyFileUnchanged(p, cur, entry)
	case KindFIFO, KindSocket, KindCharacterDevice, KindBlockDevice:
		return verifyNodeUnchanged(p, cur)
	}
	return unknownKind(p.Kind)
}

// verifyNodeUnchanged reports whether a special file is still the node its
// descriptor describes. The dev/ino check ahead of it can be fooled by a reused
// inode number, as [verifySymlinkUnchanged] explains, so the kind, the mode and
// the device numbers are compared too: they are everything the archive holds.
func verifyNodeUnchanged(p *Plan, cur os.FileInfo) error {
	kind, err := Classify(p.Source, cur.Mode())
	if err != nil || kind != p.Kind || nodeOf(kind, cur) != p.node {
		return fmt.Errorf("%s: %w", p.Source, ErrSourceReplaced)
	}
	return nil
}

// verifyFileUnchanged reports whether a regular file still holds the bytes
// that were hashed, and, while its entry is a link to it, whether the entry is
// still that same inode.
func verifyFileUnchanged(p *Plan, cur os.FileInfo, entry os.FileInfo) error {
	if p.linked {
		// The exact form of the check, available only while the entry and the
		// source are one inode: whatever else moved, these two must still be
		// the same file, or the archive is not holding what is about to be
		// destroyed.
		if !os.SameFile(cur, entry) {
			return fmt.Errorf("the archive entry %s is no longer the same file as %s: %w", p.Dest, p.Source, ErrArchiveEntryReplaced)
		}
	}

	if cur.Size() == p.identity.Size() && cur.ModTime().Equal(p.identity.ModTime()) {
		return nil
	}
	hash, err := hashFile(p.Source)
	if err != nil {
		return fmt.Errorf("re-hashing %s: %w", p.Source, err)
	}
	if hash == p.hash {
		// Touched, but the bytes are the ones that were recorded.
		return nil
	}
	if p.linked {
		return fmt.Errorf("%s: %w", p.Source, ErrArchivedContentChanged)
	}
	return fmt.Errorf("%s: %w", p.Source, ErrSourceDiverged)
}

// verifySymlinkUnchanged reports whether the link still names the target that
// was archived.
//
// The identity check ahead of it does not settle this, because a dev/ino pair
// is not a durable name for a path. A symlink's target cannot be rewritten in
// place -- replacing one means unlinking it and creating another -- and the
// filesystem is free to hand the freed inode number straight back to the
// replacement. ext4 routinely does, so os.SameFile reports the new link as the
// archived one and the removal destroys a link nothing holds a copy of. A tmpfs
// never reuses an inode number, which is why the hole is invisible on a
// developer's machine and reachable on every CI runner.
//
// A symlink has no content, no hash and no member list, so the recorded target
// is the only thing left that can tell the two apart. It is what the archive
// actually holds: the .symlink metadata file beside the entry carries the same
// string, and a restore recreates the link from it.
func verifySymlinkUnchanged(p *Plan) error {
	target, err := os.Readlink(p.Source)
	if err != nil {
		return fmt.Errorf("re-reading %s before removing it: %w", p.Source, err)
	}
	if target != p.SymlinkTarget {
		return fmt.Errorf("%s: %w", p.Source, ErrSourceReplaced)
	}
	return nil
}

// verifyTreeUnchanged reports whether the tree still holds only what the tar
// holds. A directory's identity check is its top-level inode and covers none of
// this: a file written INTO the tree after the tar was closed is in no archive
// anywhere, and os.RemoveAll destroys it along with everything else.
//
// So the tree is walked once more against the member list the archiving walk
// recorded, and these findings refuse the removal: a path the tar does not
// have at all; a path whose type of file is no longer the one that went into
// the tar (a FIFO swapped for a regular file is a file nothing archived); a
// regular file whose size or mtime no longer matches what went into it -- the
// same cheap comparison the single-file case makes before it re-hashes, minus
// the re-hash, because re-reading a whole tree to catch a same-size same-mtime
// rewrite would cost a second full pass over it; and a special file whose mode
// or device numbers are no longer the ones its member describes.
//
// A path the tar has and the tree no longer does is NOT a refusal: the archive
// then holds more than the tree, which is the state a completed archival aims
// for anyway.
//
// The residual window is real and deliberate: a write landing between this walk
// and the os.RemoveAll that follows it is not seen. That gap is microseconds
// wide, against the tens of seconds the database insert can hold open, and
// closing it would need something the filesystem does not offer.
func verifyTreeUnchanged(p *Plan) error {
	var unarchived []string
	err := filepath.WalkDir(p.Source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		recorded, ok := p.members[path]
		if !ok {
			unarchived = append(unarchived, path)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		kind, err := Classify(path, info.Mode())
		if err != nil || kind != recorded.kind {
			unarchived = append(unarchived, path)
			return nil
		}
		switch kind {
		case KindFile:
			if info.Size() != recorded.size || !info.ModTime().Equal(recorded.modTime) {
				unarchived = append(unarchived, path)
			}
		case KindFIFO, KindSocket, KindCharacterDevice, KindBlockDevice:
			if nodeOf(kind, info) != recorded.node {
				unarchived = append(unarchived, path)
			}
		case KindDirectory, KindSymlink:
		default:
			return unknownKind(kind)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("re-walking %s before removing it: %w", p.Source, err)
	}
	if len(unarchived) > 0 {
		return fmt.Errorf("%s: %w: %s", p.Source, ErrDirectoryChanged, NamePaths(unarchived))
	}
	return nil
}

// NamePaths renders the paths a refusal is about. All of them up to a handful,
// because the caller has to go and look at them, and a count after that so a
// tree that changed wholesale does not print itself into the terminal.
func NamePaths(paths []string) string {
	const shown = 5
	if len(paths) <= shown {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s (and %d more)", strings.Join(paths[:shown], ", "), len(paths)-shown)
}

// DiscardBlob removes the archive entry [Execute] wrote, undoing it. The source
// is untouched by both calls, so a discarded archival leaves the filesystem
// exactly as it was.
func DiscardBlob(p *Plan) error {
	return os.Remove(p.Dest)
}

func archiveSymlink(p *Plan) (*ArchiveResult, error) {
	target, err := os.Readlink(p.Source)
	if err != nil {
		return nil, fmt.Errorf("reading symlink target: %w", err)
	}

	info, err := os.Lstat(p.Source)
	if err != nil {
		return nil, err
	}

	// Write the target path to a .symlink metadata file for defense-in-depth
	// recovery if the database is lost. The link itself stays until the caller
	// has recorded the entry; see [Execute].
	metaPath := filepath.Join(p.ArchiveDir, p.UUID+".symlink")
	if err := os.WriteFile(metaPath, []byte(target), 0600); err != nil {
		return nil, fmt.Errorf("writing symlink metadata: %w", err)
	}

	// What the archive holds, which is what RemoveSource checks the link
	// against -- the plan was built from a read taken before this one.
	p.SymlinkTarget = target
	p.identity = info
	return &ArchiveResult{UUID: p.UUID, Kind: KindSymlink, Hash: "", Size: 0, SymlinkTarget: target}, nil
}

// archiveNode writes a special file's descriptor. Nothing is read from the node
// and nothing is linked to it: the descriptor is the whole of the archived copy.
// Its hash is the record's, so the restore can check the descriptor the way it
// checks a file's bytes.
func archiveNode(p *Plan) (*ArchiveResult, error) {
	info, err := os.Lstat(p.Source)
	if err != nil {
		return nil, err
	}
	kind, err := Classify(p.Source, info.Mode())
	if err != nil {
		return nil, err
	}
	if kind != p.Kind {
		return nil, fmt.Errorf("%s: %w: it was planned as a %s and is now a %s", p.Source, ErrSourceReplaced, p.Kind, kind)
	}
	p.node = nodeOf(kind, info)
	descriptor := EncodeNode(p.node)
	if err := writeNew(p.Dest, descriptor); err != nil {
		return nil, fmt.Errorf("writing node descriptor: %w", err)
	}
	sum := sha256.Sum256(descriptor)
	p.identity = info
	p.hash = hex.EncodeToString(sum[:])
	return &ArchiveResult{UUID: p.UUID, Kind: p.Kind, Hash: p.hash, Size: 0}, nil
}

// writeNew writes data to a file that must not exist yet, and removes what it
// wrote if the write fails partway.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func archiveFile(p *Plan) (*ArchiveResult, error) {
	hash, err := hashFile(p.Source)
	if err != nil {
		return nil, fmt.Errorf("hashing file: %w", err)
	}

	// Taken after the hash, so the size and mtime it carries are the ones that
	// went with the bytes that were read. [verifySource] compares against them.
	info, err := os.Lstat(p.Source)
	if err != nil {
		return nil, err
	}
	size := info.Size()

	dst := filepath.Join(p.ArchiveDir, p.UUID)

	// A hard link, not a rename: the archive entry and the original are the
	// same inode until the caller has recorded the deletion and calls
	// [RemoveSource]. It costs one directory entry and no content copy, and it
	// is what lets a failed record undo itself with the source untouched.
	linked := true
	if err := linkFile(p.Source, dst); err != nil {
		if !linkRefused(err) {
			return nil, fmt.Errorf("linking file into archive: %w", err)
		}
		// The filesystem or the kernel's policy will not link this file --
		// a different device, a filesystem without hard links, or Linux's
		// protected_hardlinks refusing a file the caller does not own. Copying
		// and verifying the copy against the hash reaches the same state at the
		// cost of reading the file twice, which is what the cross-device path
		// has always done.
		if err := copyAndVerify(p.Source, dst, hash); err != nil {
			return nil, err
		}
		linked = false
	}

	p.identity = info
	p.hash = hash
	p.linked = linked
	return &ArchiveResult{UUID: p.UUID, Kind: KindFile, Hash: hash, Size: size}, nil
}

func archiveDirectory(p *Plan) (*ArchiveResult, error) {
	path, archiveDir, uuid := p.Source, p.ArchiveDir, p.UUID

	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}

	// Walk tree to compute total size.
	var totalSize int64
	err = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			totalSize += info.Size()
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking directory: %w", err)
	}

	dst := filepath.Join(archiveDir, uuid+".tar.zst")

	members, err := createTarZst(path, dst)
	if err != nil {
		// Clean up partial archive on failure.
		os.Remove(dst)
		return nil, fmt.Errorf("creating tar.zst: %w", err)
	}

	hash, err := hashFile(dst)
	if err != nil {
		os.Remove(dst)
		return nil, fmt.Errorf("hashing archive: %w", err)
	}

	// The tree itself stays until the caller has recorded the entry; see
	// [Execute]. It used to go here, which is why a failing record left a
	// compressed tree nobody could name and no directory to go back to.
	p.identity = info
	p.members = members
	return &ArchiveResult{UUID: uuid, Kind: KindDirectory, Hash: hash, Size: totalSize}, nil
}

// RestorePlan is everything a restore can determine by reading: which archive
// entry holds the record's content, what shape that entry is, and where it is
// going. Building one mutates nothing.
//
// A restore is split the way an archival is, and for the same reason: the acts
// that consume the archived copy must be separable from the acts that write the
// destination, so that a failure can always leave the copy where it is. Every
// primitive below is one of those halves, and none of them decides anything --
// what to do about a destination that already exists, and whether the entry is
// verified first, are the caller's decisions.
type RestorePlan struct {
	UUID          string
	ArchiveDir    string
	Dest          string
	Kind          Kind
	SymlinkTarget string

	// Entry is the archive file holding the content: the bare uuid for a
	// regular file, uuid.tar.zst for a tree, uuid.symlink for a link,
	// uuid.node for a special file (see [EntryPath]).
	Entry string
}

// NewRestorePlan resolves where a record's content lives and where it is going.
// The kind is read off the record, not off the archive: a record knows what it
// archived and what a symlink pointed at, and both facts must be available
// before anything is read from disk.
func NewRestorePlan(uuid string, archiveDir string, dest string, kind Kind, symlinkTarget string) *RestorePlan {
	return &RestorePlan{
		UUID:          uuid,
		ArchiveDir:    archiveDir,
		Dest:          dest,
		Kind:          kind,
		SymlinkTarget: symlinkTarget,
		Entry:         EntryPath(archiveDir, uuid, kind),
	}
}

// EntryPresent reports whether the archived copy is there to be restored at
// all, and is the shape the record's kind says it is: every kind's entry is a
// regular file, and a special file's is a descriptor of that kind. It is a stat
// and, for a special file, a read of its few-byte descriptor: every restore
// makes this check, including the ones that deliberately do no verification,
// because an absent or mismatched entry is worth naming as such rather than
// surfacing as a failed rename of a UUID.
func EntryPresent(p *RestorePlan) error {
	info, err := os.Lstat(p.Entry)
	if err != nil {
		return fmt.Errorf("%s: %w (%v)", p.Entry, ErrEntryMissing, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: %w: the record names a %s, whose entry is a regular file, and the entry is %s",
			p.Entry, ErrEntryCorrupt, p.Kind, articled(describeMode(info.Mode())))
	}
	switch p.Kind {
	case KindFile, KindDirectory, KindSymlink:
		return nil
	case KindFIFO, KindSocket, KindCharacterDevice, KindBlockDevice:
		_, err := readNode(p)
		return err
	}
	return unknownKind(p.Kind)
}

// VerifyEntry checks the archived copy against what the record says about it,
// reading only -- so a caller can refuse a destructive restore BEFORE the
// destination is touched.
//
// The recorded hash means a different thing per kind, and this is the only
// place that states each honestly:
//
//   - KindFile: recordedHash is the SHA-256 of the archived file's CONTENT, and
//     the entry is that file. The check is exact: a byte that rotted in the
//     archive is found here.
//   - KindDirectory: recordedHash is the SHA-256 of the .tar.zst CONTAINER, not
//     of any member and not of the tree. So this proves the container arrived
//     intact -- which is what a corrupt or truncated archive fails -- and says
//     nothing about individual members beyond that. There is no per-member
//     digest anywhere in the archive, so no check here can promise one.
//   - KindSymlink: nothing was hashed at all. A symlink has no content; its
//     entry is the recorded target written out, and recordedHash is empty by
//     construction. The check is therefore an equality: the entry must still
//     name the target the record names. A hash comparison here would fail
//     spuriously on every symlink ever archived.
//   - A special file (KindFIFO, KindSocket, KindCharacterDevice,
//     KindBlockDevice): recordedHash is the SHA-256 of the `.node` DESCRIPTOR,
//     which is the whole of what the archive holds of the node, so the check is
//     as exact as a file's. [EntryPresent] has already decoded the descriptor
//     and matched its kind against the record's.
//
// A file, a tree or a special file whose record carries no hash cannot be verified at all, and
// that is [ErrUnverifiable] rather than a pass: the caller asked to destroy a
// destination on the strength of a check that cannot be made.
func VerifyEntry(p *RestorePlan, recordedHash string) error {
	if err := EntryPresent(p); err != nil {
		return err
	}

	switch p.Kind {
	case KindSymlink:
		recorded, err := os.ReadFile(p.Entry)
		if err != nil {
			return fmt.Errorf("reading the archived symlink entry %s: %w", p.Entry, err)
		}
		if string(recorded) != p.SymlinkTarget {
			return fmt.Errorf("%s: %w: the entry names %q and the record names %q",
				p.Entry, ErrEntryDiverged, string(recorded), p.SymlinkTarget)
		}
		return nil
	case KindFile, KindDirectory, KindFIFO, KindSocket, KindCharacterDevice, KindBlockDevice:
		return verifyEntryHash(p, recordedHash)
	}
	return unknownKind(p.Kind)
}

// RestoreNode recreates the special file the plan's descriptor describes at
// the plan's destination. Like [RestoreSymlink] it does NOT consume the entry,
// so a node that cannot be made -- a device without the privilege mknod needs
// -- leaves the descriptor where it is.
func RestoreNode(p *RestorePlan) error {
	n, err := readNode(p)
	if err != nil {
		return err
	}
	return makeNode(p.Dest, n)
}

// readNode reads and decodes a node's descriptor, and refuses one that does
// not describe the kind the record names.
func readNode(p *RestorePlan) (Node, error) {
	f, _, err := openRegular(p.Entry)
	if err != nil {
		return Node{}, fmt.Errorf("%s: %w (%v)", p.Entry, ErrEntryCorrupt, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxNodeDescriptor+1))
	if err != nil {
		return Node{}, fmt.Errorf("reading the node descriptor %s: %w", p.Entry, err)
	}
	n, err := DecodeNode(data)
	if err != nil {
		return Node{}, fmt.Errorf("%s: %w: %v", p.Entry, ErrEntryCorrupt, err)
	}
	if n.Kind != p.Kind {
		return Node{}, fmt.Errorf("%s: %w: the descriptor describes a %s and the record names a %s", p.Entry, ErrEntryCorrupt, n.Kind, p.Kind)
	}
	return n, nil
}

// maxNodeDescriptor bounds the read of a descriptor: [EncodeNode] never writes
// more than a few dozen bytes, so anything past this is not a descriptor.
const maxNodeDescriptor = 256

// verifyEntryHash checks an entry's bytes against the hash the record carries.
func verifyEntryHash(p *RestorePlan, recordedHash string) error {
	if recordedHash == "" {
		return fmt.Errorf("%s: %w", p.Entry, ErrUnverifiable)
	}
	hash, err := hashFile(p.Entry)
	if err != nil {
		return fmt.Errorf("hashing the archived copy %s: %w", p.Entry, err)
	}
	if hash != recordedHash {
		return fmt.Errorf("%s: %w: the entry hashes to %s and the record says %s",
			p.Entry, ErrEntryCorrupt, hash, recordedHash)
	}
	return nil
}

// RestoreSymlink recreates the link at the plan's destination. It does NOT
// consume the entry: the caller removes it once the link is there, so a failure
// leaves the recorded target on disk.
func RestoreSymlink(p *RestorePlan) error {
	if err := os.Symlink(p.SymlinkTarget, p.Dest); err != nil {
		return fmt.Errorf("recreating symlink: %w", err)
	}
	return nil
}

// ExtractTree extracts the tree held in the plan's entry into its destination
// and does NOT consume the entry, for the same reason as [RestoreSymlink]: an
// extraction that fails partway must leave the archived copy readable, so the
// restore can simply be run again.
//
// It returns every path it created, in creation order, whether it succeeded or
// not -- that list is what makes a partial extraction reportable and undoable.
func ExtractTree(p *RestorePlan) ([]string, error) {
	if err := EntryPresent(p); err != nil {
		return nil, err
	}
	created, err := extractTarZst(p.Entry, p.Dest)
	if err != nil {
		return created, fmt.Errorf("extracting archive: %w", err)
	}
	return created, nil
}

// RollbackExtraction removes the paths a failed extraction created, newest
// first, and returns the ones it could not remove.
//
// The destination of a restore holds nothing but archive-derived bytes: it was
// absent, or an empty directory, or removed outright by an overwrite that
// verified the archived copy first. So undoing a partial extraction destroys
// nothing that is not still in the archive -- the entry is consumed only after
// the extraction succeeds. Leaving the half tree instead would leave a
// destination that looks restored and is not, and a retry would then meet its
// own leftovers as a conflict.
//
// Directories go through os.Remove, not os.RemoveAll: reverse order empties
// them first, and one that is still not empty holds something this extraction
// did not write. That is exactly what must survive, so it is reported as stuck
// rather than destroyed.
func RollbackExtraction(created []string) []string {
	var stuck []string
	for i := len(created) - 1; i >= 0; i-- {
		if err := os.Remove(created[i]); err != nil && !errors.Is(err, os.ErrNotExist) {
			stuck = append(stuck, created[i])
		}
	}
	return stuck
}

// CopyOut is the cross-device half of a file restore: the archived copy is
// copied to dst and consumed only once the copy is complete.
//
// A rename cannot cross a filesystem boundary, and the archive and the
// destination are not always on one. The order is what keeps the failure safe:
// a copy that fails leaves the entry untouched and takes its own partial
// destination back, so nothing is left looking restored and the restore can be
// run again.
func CopyOut(src string, dst string) error {
	if err := copyFile(src, dst); err != nil {
		// Archive-derived bytes only, and incomplete ones: the content they
		// came from is still in the entry, which is not removed below.
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

// IsCrossDeviceError reports whether a failed rename means "these two paths are
// on different filesystems", which is what sends a file restore through
// [CopyOut].
func IsCrossDeviceError(err error) bool {
	return isCrossDevice(err)
}

// NewUUID returns a UUID v4 string using crypto/rand.
//
// It names an archive entry, and it is also what mints the group identifier a
// delete invocation stamps on every record it writes: both are opaque handles
// minted with no coordination between processes, so they are the same thing.
func NewUUID() string {
	var uuid [16]byte
	if _, err := io.ReadFull(rand.Reader, uuid[:]); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	// Set version 4.
	uuid[6] = (uuid[6] & 0x0f) | 0x40
	// Set variant bits.
	uuid[8] = (uuid[8] & 0x3f) | 0x80

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16])
}

// openRegular opens path for reading its content, and refuses anything that is
// not a regular file at the moment it is opened.
//
// The check is on the opened descriptor, not on an earlier stat of the path:
// the path can be swapped between a stat and an open, and a FIFO swapped in
// there would block the read until a writer appeared and then hand the archival
// whatever that writer sent. The open itself cannot block (see openReadFlags),
// so the fstat always gets to run.
func openRegular(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, openReadFlags, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%s: %w: it is a %s", path, ErrNotRegularFile, describeMode(info.Mode()))
	}
	return f, info, nil
}

// describeMode names the type of file a mode describes, for an error message.
func describeMode(m fs.FileMode) string {
	switch {
	case m.IsRegular():
		return "regular file"
	case m.IsDir():
		return "directory"
	case m&fs.ModeSymlink != 0:
		return "symlink"
	case m&fs.ModeNamedPipe != 0:
		return "FIFO"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeCharDevice != 0:
		return "character device"
	case m&fs.ModeDevice != 0:
		return "block device"
	}
	return "irregular file"
}

// hashFile computes the SHA-256 hex digest of a regular file by streaming.
func hashFile(path string) (string, error) {
	f, _, err := openRegular(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFile copies src to dst, preserving permissions.
func copyFile(src, dst string) error {
	srcFile, srcInfo, err := openRegular(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}
	return dstFile.Close()
}

// copyAndVerify copies a file and verifies the SHA-256 hash matches.
func copyAndVerify(src, dst string, expectedHash string) error {
	if err := copyFile(src, dst); err != nil {
		return err
	}

	hash, err := hashFile(dst)
	if err != nil {
		os.Remove(dst)
		return err
	}

	if hash != expectedHash {
		os.Remove(dst)
		return ErrHashMismatch
	}
	return nil
}

// createTarZst creates a .tar.zst archive of srcDir at dstPath, and returns
// what it put in it: every walked path, with the size and mtime it had as it
// was written. That list is the only description of a tree's contents the
// archival produces, and [verifyTreeUnchanged] is what reads it.
func createTarZst(srcDir string, dstPath string) (map[string]member, error) {
	members := make(map[string]member)

	outFile, err := os.Create(dstPath)
	if err != nil {
		return nil, err
	}
	defer outFile.Close()

	zstWriter, err := zstd.NewWriter(outFile)
	if err != nil {
		return nil, err
	}
	defer zstWriter.Close()

	tarWriter := tar.NewWriter(zstWriter)
	defer tarWriter.Close()

	baseDir := filepath.Base(srcDir)

	err = filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Compute relative path within the archive.
		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		// Prefix with the base directory name.
		archivePath := filepath.Join(baseDir, relPath)
		if archivePath == baseDir+"/"+"." {
			archivePath = baseDir
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		kind, err := Classify(path, info.Mode())
		if err != nil {
			return err
		}

		// Recorded before the content is written, not after: a file that is
		// rewritten WHILE it is being copied gets a member whose size and mtime
		// are the pre-copy ones, so the check on the way back refuses. Recording
		// afterwards would instead make the refusal disappear for exactly the
		// case that needs it.
		m := member{size: info.Size(), modTime: info.ModTime(), kind: kind}

		switch kind {
		case KindSymlink:
			members[path] = m
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header := &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     archivePath,
				Linkname: linkTarget,
				Mode:     int64(info.Mode()),
				ModTime:  info.ModTime(),
			}
			return tarWriter.WriteHeader(header)

		case KindFIFO, KindSocket, KindCharacterDevice, KindBlockDevice:
			m.node = nodeOf(kind, info)
			members[path] = m
			header, err := nodeTarHeader(m.node, archivePath, info.ModTime())
			if err != nil {
				return err
			}
			return tarWriter.WriteHeader(header)

		case KindDirectory:
			members[path] = m
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name = archivePath
			return tarWriter.WriteHeader(header)

		case KindFile:
			members[path] = m

		default:
			return unknownKind(kind)
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = archivePath

		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}

		f, _, err := openRegular(path)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(tarWriter, f)
		return err
	})
	if err != nil {
		return nil, err
	}

	// Close in reverse order.
	if err := tarWriter.Close(); err != nil {
		return nil, err
	}
	if err := zstWriter.Close(); err != nil {
		return nil, err
	}
	if err := outFile.Close(); err != nil {
		return nil, err
	}
	return members, nil
}

// extractTarZst extracts a .tar.zst archive into dstDir and returns every path
// it created, in creation order.
//
// The list is returned on the failure path too, and it is the whole reason the
// extraction tracks what it writes: an archive that ends mid-stream leaves a
// destination holding part of a tree, and only the extraction knows which part.
// See [RollbackExtraction] for what is done with it.
func extractTarZst(srcPath string, dstDir string) ([]string, error) {
	var created []string

	inFile, err := os.Open(srcPath)
	if err != nil {
		return created, err
	}
	defer inFile.Close()

	zstReader, err := zstd.NewReader(inFile)
	if err != nil {
		return created, err
	}
	defer zstReader.Close()

	tarReader := tar.NewReader(zstReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return created, err
		}

		// Security: prevent path traversal.
		cleanName := filepath.Clean(header.Name)
		if strings.HasPrefix(cleanName, "/") || strings.HasPrefix(cleanName, "..") || strings.Contains(cleanName, "/../") {
			return created, fmt.Errorf("invalid tar entry path (path traversal): %s", header.Name)
		}

		// Strip the top-level directory from the path so contents extract
		// directly into dstDir.
		parts := strings.SplitN(filepath.ToSlash(cleanName), "/", 2)
		var targetRel string
		if len(parts) < 2 || parts[1] == "" {
			// This is the top-level directory entry itself; just ensure dstDir exists.
			if err := mkdirTracked(dstDir, 0755, &created); err != nil {
				return created, err
			}
			continue
		}
		targetRel = parts[1]

		target := filepath.Join(dstDir, targetRel)

		// Double-check the resolved path is inside dstDir.
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return created, err
		}
		absDst, err := filepath.Abs(dstDir)
		if err != nil {
			return created, err
		}
		if !strings.HasPrefix(absTarget, absDst+string(filepath.Separator)) && absTarget != absDst {
			return created, fmt.Errorf("invalid tar entry path (escapes destination): %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := mkdirTracked(target, os.FileMode(header.Mode), &created); err != nil {
				return created, err
			}
		case tar.TypeSymlink:
			if err := mkdirTracked(filepath.Dir(target), 0755, &created); err != nil {
				return created, err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return created, err
			}
			created = append(created, target)
		case tar.TypeFifo, tar.TypeChar, tar.TypeBlock:
			if err := extractNode(header, target, &created); err != nil {
				return created, err
			}
		case tar.TypeReg:
			if _, isNode, err := nodeFromTarHeader(header); err != nil {
				return created, err
			} else if isNode {
				if err := extractNode(header, target, &created); err != nil {
					return created, err
				}
				continue
			}
			if err := mkdirTracked(filepath.Dir(target), 0755, &created); err != nil {
				return created, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return created, err
			}
			created = append(created, target)
			if _, err := io.Copy(f, tarReader); err != nil {
				f.Close()
				return created, err
			}
			f.Close()
		default:
			// A member type the archiving walk never writes. Skipping it would
			// report a restore that left part of the tree behind.
			return created, fmt.Errorf("%w: %s has tar typeflag %q", ErrUnsupportedTarMember, header.Name, header.Typeflag)
		}
	}

	return created, nil
}

// extractNode recreates the special file a tar member describes, through the
// same codec and the same [makeNode] a top-level node's restore uses.
func extractNode(header *tar.Header, target string, created *[]string) error {
	n, isNode, err := nodeFromTarHeader(header)
	if err != nil {
		return err
	}
	if !isNode {
		return fmt.Errorf("%w: %s is not a special file", ErrUnsupportedTarMember, header.Name)
	}
	if err := mkdirTracked(filepath.Dir(target), 0755, created); err != nil {
		return err
	}
	if err := makeNode(target, n); err != nil {
		return err
	}
	*created = append(*created, target)
	return nil
}

// mkdirTracked is os.MkdirAll that appends every directory it actually creates
// to created, so an undo can tell the directories the extraction made from the
// ones it found. A destination the caller already owns -- the empty original
// location of a restored tree -- is found, not created, and so survives a
// rollback.
func mkdirTracked(path string, mode os.FileMode, created *[]string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", path)
		}
		return nil
	}
	if parent := filepath.Dir(path); parent != path {
		if err := mkdirTracked(parent, 0755, created); err != nil {
			return err
		}
	}
	if err := os.Mkdir(path, mode); err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	*created = append(*created, path)
	return nil
}

// isCrossDevice checks if an error is a cross-device link error (EXDEV).
func isCrossDevice(err error) bool {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		if sysErr, ok := linkErr.Err.(syscall.Errno); ok {
			return sysErr == syscall.EXDEV
		}
	}
	return false
}

// linkRefused reports whether a failed os.Link means "this file cannot be hard
// linked here", as opposed to a genuine failure.
//
// The four refusals, all of which the copy-and-verify path handles correctly:
// EXDEV (source and archive on different filesystems), EPERM (Linux's
// protected_hardlinks refusing a link to a file the caller neither owns nor can
// write, and filesystems that reject links outright), EOPNOTSUPP/ENOSYS (a
// filesystem with no hard links at all) and EMLINK (the inode is already at its
// link limit). Anything else -- a missing archive directory, a full disk, a
// read-only mount -- is reported as the failure it is, because copying would
// fail the same way and hide the reason.
func linkRefused(err error) bool {
	if isCrossDevice(err) {
		return true
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		if sysErr, ok := linkErr.Err.(syscall.Errno); ok {
			switch sysErr {
			case syscall.EPERM, syscall.EOPNOTSUPP, syscall.ENOSYS, syscall.EMLINK:
				return true
			}
		}
	}
	return false
}
