package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// EntryRepair is a record whose archive entry contradicts the node type the
// record was written with, and the entry form that resolves it. saferm
// versions from before the node_type column did not recognize symlinks or
// special files: a symlink was renamed into the archive as `<uuid>` and a FIFO
// was hard-linked there after its content was drained, both under a file
// record. The entry is still on disk; what is wrong is the record, and the
// entry's file name, which the node type decides.
//
// A repair writes the entry in the form its real node type uses -- the
// `<uuid>.symlink` target file, or the `<uuid>.node` descriptor -- and the
// record then takes that node type, with the hash and size a deletion of that
// node type records. The database migration that adds the node_type column
// makes these repairs: it plans every one before it changes anything, and
// orders the writes so a failure leaves the archive as it found it.
type EntryRepair struct {
	UUID          string
	NodeType      NodeType // what the entry really holds
	OldEntry      string
	NewEntry      string
	SymlinkTarget string // what a symlink points at; empty for a special file
	Content       []byte // the new entry's bytes
	Hash          string // empty for a symlink, the descriptor's SHA-256 for a special file
}

// ErrEntryUnrepairable is an entry that contradicts its record in a way no
// repair resolves: anything but a symlink or a special file standing where a
// file record expects a regular file.
var ErrEntryUnrepairable = errors.New("the archive entry contradicts its record and is not a symlink or special file recorded as a file")

// ErrRepairEntryExists is a repair's new entry already on disk with content
// other than the repair writes.
var ErrRepairEntryExists = errors.New("the repaired entry's path is already taken by other content")

// PlanEntryRepair reads, without following it, the entry a record of the given
// node type names, and returns the repair it needs: nil when the entry is a
// regular file, which every node type's entry is, or is not there at all (an
// archival that meets a changed source discards its entry on purpose, so a
// missing entry is a state saferm writes itself). A symlink or a special file
// under a file record is a repair; any other contradiction is
// [ErrEntryUnrepairable], and a stat that fails for any reason but absence is
// returned as it is, because it cannot say what the entry holds. It changes
// nothing.
func PlanEntryRepair(archiveDir string, uuid string, recorded NodeType) (*EntryRepair, error) {
	oldEntry := EntryPath(archiveDir, uuid, recorded)
	info, err := os.Lstat(oldEntry)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode().IsRegular() {
		return nil, nil
	}
	actual, err := Classify(oldEntry, info.Mode())
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %v", oldEntry, ErrEntryUnrepairable, err)
	}
	if recorded != NodeTypeFile {
		return nil, fmt.Errorf("%s: %w: the record names a %s and the entry is %s",
			oldEntry, ErrEntryUnrepairable, recorded, articled(describeMode(info.Mode())))
	}
	r := &EntryRepair{UUID: uuid, NodeType: actual, OldEntry: oldEntry, NewEntry: EntryPath(archiveDir, uuid, actual)}
	switch actual {
	case NodeTypeSymlink:
		target, err := os.Readlink(oldEntry)
		if err != nil {
			return nil, err
		}
		r.SymlinkTarget = target
		r.Content = []byte(target)
	case NodeTypeFIFO, NodeTypeSocket, NodeTypeCharacterDevice, NodeTypeBlockDevice:
		r.Content = EncodeNode(nodeOf(actual, info))
		sum := sha256.Sum256(r.Content)
		r.Hash = hex.EncodeToString(sum[:])
	case NodeTypeFile, NodeTypeDirectory:
		return nil, fmt.Errorf("%s: %w: it is %s", oldEntry, ErrEntryUnrepairable, articled(describeMode(info.Mode())))
	default:
		return nil, unknownNodeType(actual)
	}
	return r, nil
}

// WriteEntry writes the repaired entry and flushes it to disk. A regular file
// already at that path holding these bytes is accepted as written: it is what
// a repair interrupted before its record changed leaves behind, and the
// repair it belongs to is this one. Anything else there is
// [ErrRepairEntryExists].
func (r *EntryRepair) WriteEntry() error {
	err := writeNewSynced(r.NewEntry, r.Content)
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	f, _, oerr := openRegular(r.NewEntry)
	if oerr != nil {
		return fmt.Errorf("%s: %w: %v", r.NewEntry, ErrRepairEntryExists, oerr)
	}
	defer f.Close()
	have, rerr := io.ReadAll(io.LimitReader(f, int64(len(r.Content))+1))
	if rerr != nil {
		return fmt.Errorf("reading %s: %w", r.NewEntry, rerr)
	}
	if !bytes.Equal(have, r.Content) {
		return fmt.Errorf("%s: %w", r.NewEntry, ErrRepairEntryExists)
	}
	return f.Sync()
}

// DiscardEntry takes back what [EntryRepair.WriteEntry] wrote, when the record
// could not be changed to match it.
func (r *EntryRepair) DiscardEntry() error {
	return os.Remove(r.NewEntry)
}

// writeNewSynced is [writeNew] followed by a flush of the file to disk.
func writeNewSynced(path string, data []byte) error {
	if err := writeNew(path, data); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// SyncDir flushes a directory's entries -- the names created and removed in
// it -- to disk.
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
