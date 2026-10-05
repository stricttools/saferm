package archive

import (
	"errors"
	"fmt"
	"path/filepath"
)

// Kind names what an archived record holds. Its value is the word every
// surface uses for it: the database's `kind` column, the `kind` member of the
// machine payloads, and the Type line `info` prints. [Kinds] is the one list of
// them, and everything that enumerates kinds -- the column's CHECK constraint,
// the payload enums -- is generated from it.
type Kind string

const (
	KindFile      Kind = "file"
	KindDirectory Kind = "directory"
	KindSymlink   Kind = "symlink"
)

// kinds is every Kind, in the order the surfaces list them.
var kinds = []Kind{KindFile, KindDirectory, KindSymlink}

// Kinds returns every Kind. It is a copy, so a caller cannot change the set.
func Kinds() []Kind {
	return append([]Kind(nil), kinds...)
}

// ErrUnknownKind is a kind word that names none of [Kinds]: a database written
// by a newer saferm, or one edited by hand.
var ErrUnknownKind = errors.New("unknown archive kind")

// ParseKind returns the Kind a word names, and refuses any word that is not
// one of [Kinds].
func ParseKind(s string) (Kind, error) {
	for _, k := range kinds {
		if string(k) == s {
			return k, nil
		}
	}
	return "", fmt.Errorf("%w %q", ErrUnknownKind, s)
}

// EntryPath is where the archive holds a record's content: `<uuid>` for a
// file, `<uuid>.tar.zst` for a tree, `<uuid>.symlink` for a symlink.
//
// It panics on a Kind outside [Kinds]: every Kind reaches here from a constant
// or through [ParseKind], so an unknown one is a bug in saferm, not an input.
func EntryPath(archiveDir string, uuid string, k Kind) string {
	path := filepath.Join(archiveDir, uuid)
	switch k {
	case KindFile:
		return path
	case KindDirectory:
		return path + ".tar.zst"
	case KindSymlink:
		return path + ".symlink"
	}
	panic(fmt.Sprintf("archive.EntryPath: %s", unknownKind(k)))
}

// unknownKind is the message every kind dispatch's default case carries.
func unknownKind(k Kind) error {
	return fmt.Errorf("%w %q: no case handles it", ErrUnknownKind, string(k))
}
