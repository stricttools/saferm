package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
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

	// The special files (see [Node]).
	KindFIFO            Kind = "fifo"
	KindSocket          Kind = "socket"
	KindCharacterDevice Kind = "character-device"
	KindBlockDevice     Kind = "block-device"
)

// kinds is every Kind, in the order the surfaces list them.
var kinds = []Kind{KindFile, KindDirectory, KindSymlink, KindFIFO, KindSocket, KindCharacterDevice, KindBlockDevice}

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
// file, `<uuid>.tar.zst` for a tree, `<uuid>.symlink` for a symlink, and
// `<uuid>.node` for a special file's descriptor (see [Node]).
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
	case KindFIFO, KindSocket, KindCharacterDevice, KindBlockDevice:
		return path + ".node"
	}
	panic(fmt.Sprintf("archive.EntryPath: %s", unknownKind(k)))
}

// UnsupportedFileError is a path whose type of file saferm cannot archive:
// anything that is not a regular file, a directory, a symlink, or one of the
// special files a [Node] describes.
type UnsupportedFileError struct {
	Path string
	Mode fs.FileMode
}

func (e *UnsupportedFileError) Error() string {
	return fmt.Sprintf("%s is %s (mode %s), which saferm cannot archive", e.Path, articled(describeMode(e.Mode)), e.Mode.Type())
}

// articled puts the indefinite article in front of a noun phrase.
func articled(noun string) string {
	if strings.ContainsRune("aeiou", rune(noun[0])) {
		return "an " + noun
	}
	return "a " + noun
}

// Classify names the kind of file a mode describes, and refuses a type saferm
// has no kind for with an [UnsupportedFileError].
func Classify(path string, mode fs.FileMode) (Kind, error) {
	switch {
	case mode.IsRegular():
		return KindFile, nil
	case mode.IsDir():
		return KindDirectory, nil
	case mode&fs.ModeSymlink != 0:
		return KindSymlink, nil
	case mode&fs.ModeNamedPipe != 0:
		return KindFIFO, nil
	case mode&fs.ModeSocket != 0:
		return KindSocket, nil
	case mode&fs.ModeCharDevice != 0:
		return KindCharacterDevice, nil
	case mode&fs.ModeDevice != 0:
		return KindBlockDevice, nil
	}
	return "", &UnsupportedFileError{Path: path, Mode: mode}
}

// unknownKind is the message every kind dispatch's default case carries.
func unknownKind(k Kind) error {
	return fmt.Errorf("%w %q: no case handles it", ErrUnknownKind, string(k))
}
