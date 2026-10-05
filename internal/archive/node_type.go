package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// NodeType names what an archived record holds. Its value is the word every
// surface uses for it: the database's `node_type` column, the `node_type` member of the
// machine payloads, and the Type line `info` prints. [NodeTypes] is the one list of
// them, and everything that enumerates node types -- the column's CHECK constraint,
// the payload enums -- is generated from it.
type NodeType string

const (
	NodeTypeFile      NodeType = "file"
	NodeTypeDirectory NodeType = "directory"
	NodeTypeSymlink   NodeType = "symlink"

	// The special files (see [Node]).
	NodeTypeFIFO            NodeType = "fifo"
	NodeTypeSocket          NodeType = "socket"
	NodeTypeCharacterDevice NodeType = "character-device"
	NodeTypeBlockDevice     NodeType = "block-device"
)

// nodeTypes is every NodeType, in the order the surfaces list them.
var nodeTypes = []NodeType{NodeTypeFile, NodeTypeDirectory, NodeTypeSymlink, NodeTypeFIFO, NodeTypeSocket, NodeTypeCharacterDevice, NodeTypeBlockDevice}

// NodeTypes returns every NodeType. It is a copy, so a caller cannot change the set.
func NodeTypes() []NodeType {
	return append([]NodeType(nil), nodeTypes...)
}

// ErrUnknownNodeType is a node type word that names none of [NodeTypes]: a database written
// by a newer saferm, or one edited by hand.
var ErrUnknownNodeType = errors.New("unknown node type")

// ParseNodeType returns the NodeType a word names, and refuses any word that is not
// one of [NodeTypes].
func ParseNodeType(s string) (NodeType, error) {
	for _, k := range nodeTypes {
		if string(k) == s {
			return k, nil
		}
	}
	return "", fmt.Errorf("%w %q", ErrUnknownNodeType, s)
}

// EntryPath is where the archive holds a record's content: `<uuid>` for a
// file, `<uuid>.tar.zst` for a tree, `<uuid>.symlink` for a symlink, and
// `<uuid>.node` for a special file's descriptor (see [Node]).
//
// It panics on a NodeType outside [NodeTypes]: every NodeType reaches here from a constant
// or through [ParseNodeType], so an unknown one is a bug in saferm, not an input.
func EntryPath(archiveDir string, uuid string, k NodeType) string {
	path := filepath.Join(archiveDir, uuid)
	switch k {
	case NodeTypeFile:
		return path
	case NodeTypeDirectory:
		return path + ".tar.zst"
	case NodeTypeSymlink:
		return path + ".symlink"
	case NodeTypeFIFO, NodeTypeSocket, NodeTypeCharacterDevice, NodeTypeBlockDevice:
		return path + ".node"
	}
	panic(fmt.Sprintf("archive.EntryPath: %s", unknownNodeType(k)))
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

// Classify names the node type a mode describes, and refuses a type saferm
// has no node type for, with an [UnsupportedFileError].
func Classify(path string, mode fs.FileMode) (NodeType, error) {
	switch {
	case mode.IsRegular():
		return NodeTypeFile, nil
	case mode.IsDir():
		return NodeTypeDirectory, nil
	case mode&fs.ModeSymlink != 0:
		return NodeTypeSymlink, nil
	case mode&fs.ModeNamedPipe != 0:
		return NodeTypeFIFO, nil
	case mode&fs.ModeSocket != 0:
		return NodeTypeSocket, nil
	case mode&fs.ModeCharDevice != 0:
		return NodeTypeCharacterDevice, nil
	case mode&fs.ModeDevice != 0:
		return NodeTypeBlockDevice, nil
	}
	return "", &UnsupportedFileError{Path: path, Mode: mode}
}

// unknownNodeType is the message every node type dispatch's default case carries.
func unknownNodeType(k NodeType) error {
	return fmt.Errorf("%w %q: no case handles it", ErrUnknownNodeType, string(k))
}
