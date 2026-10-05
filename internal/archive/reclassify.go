package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

// Reclassification is a record whose archive entry contradicts the record's
// kind, and what the entry really holds. saferm versions that did not
// recognize symlinks or special files recorded them as files: a symlink was
// renamed into the archive as `<uuid>` and a FIFO was hard-linked there after
// its content was drained. Both entries are still on disk; what is wrong is the
// record, and the entry's file name, which the kind decides.
//
// A reclassification rewrites the entry into the form its real kind uses -- the
// `<uuid>.symlink` target file, or the `<uuid>.node` descriptor -- and the
// record is then updated to that kind, with the hash and size a deletion of
// that kind records. The old entry is removed last, so a failure partway leaves
// the record's own entry in place.
type Reclassification struct {
	UUID          string
	From          NodeType
	To            NodeType
	OldEntry      string
	NewEntry      string
	SymlinkTarget string // what a reclassified symlink points at
	Content       []byte // the new entry's bytes
	Hash          string // the record's new hash: empty for a symlink, the descriptor's for a node
}

// ErrNotReclassifiable is an entry that contradicts its record in a way no
// reclassification resolves: anything but a symlink or a special file standing
// where a file record expects a regular file.
var ErrNotReclassifiable = errors.New("the archive entry contradicts its record and is not a symlink or special file recorded as a file")

// PlanReclassification reads a live record's entry and returns the
// reclassification it needs, nil when the entry agrees with the kind or is not
// there at all (a missing entry is `info`'s entry-missing, which nothing here
// can fix), or [ErrNotReclassifiable]. It changes nothing.
func PlanReclassification(archiveDir string, uuid string, kind NodeType, symlinkTarget string) (*Reclassification, error) {
	p := NewRestorePlan(uuid, archiveDir, "", kind, symlinkTarget)
	err := EntryPresent(p)
	switch {
	case err == nil, errors.Is(err, ErrEntryMissing):
		return nil, nil
	case !errors.Is(err, ErrEntryCorrupt):
		return nil, err
	}

	info, lerr := os.Lstat(p.Entry)
	if lerr != nil || kind != NodeTypeFile {
		return nil, fmt.Errorf("%s: %w: %v", p.Entry, ErrNotReclassifiable, err)
	}
	actual, cerr := Classify(p.Entry, info.Mode())
	if cerr != nil {
		return nil, fmt.Errorf("%s: %w: %v", p.Entry, ErrNotReclassifiable, cerr)
	}
	r := &Reclassification{UUID: uuid, From: kind, To: actual, OldEntry: p.Entry, NewEntry: EntryPath(archiveDir, uuid, actual)}
	switch actual {
	case NodeTypeSymlink:
		target, err := os.Readlink(p.Entry)
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
		return nil, fmt.Errorf("%s: %w: it is %s", p.Entry, ErrNotReclassifiable, articled(describeMode(info.Mode())))
	default:
		return nil, unknownNodeType(actual)
	}
	return r, nil
}

// WriteEntry writes the reclassified entry. It must not exist yet.
func (r *Reclassification) WriteEntry() error {
	return writeNew(r.NewEntry, r.Content)
}

// DiscardEntry takes back what [Reclassification.WriteEntry] wrote, when the
// record could not be updated to match it.
func (r *Reclassification) DiscardEntry() error {
	return os.Remove(r.NewEntry)
}
