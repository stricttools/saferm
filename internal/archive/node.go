package archive

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"
)

// A node is a special file: a FIFO, a socket, or a character or block device.
// None of them has content saferm can archive -- reading a FIFO drains whatever
// a writer sends, a socket cannot be opened at all, and a device is read to its
// end -- so what the archive keeps of one is what it takes to make it again:
// its node type, its permission bits, and for a device its major and minor numbers.
//
// This file is the one codec for that description. A node archived on its own
// is kept as a `<uuid>.node` descriptor ([EncodeNode], [DecodeNode]); a node
// inside a tree is a tar member ([nodeTarHeader], [nodeFromTarHeader]), where
// FIFOs and devices have tar typeflags of their own and a socket, which has
// none, is a zero-length regular member carrying the [paxNodeType] record.

// Node is what the archive keeps of a special file.
type Node struct {
	NodeType NodeType
	Perm     fs.FileMode // permission bits, with setuid, setgid and sticky
	Major    uint32      // device numbers; zero for a FIFO or a socket
	Minor    uint32
}

// IsSpecialFileType reports whether a node type is one of the special files.
func IsSpecialFileType(k NodeType) bool {
	switch k {
	case NodeTypeFIFO, NodeTypeSocket, NodeTypeCharacterDevice, NodeTypeBlockDevice:
		return true
	case NodeTypeFile, NodeTypeDirectory, NodeTypeSymlink:
		return false
	}
	panic(fmt.Sprintf("archive.IsSpecialFileType: %s", unknownNodeType(k)))
}

// isDeviceType reports whether a special file's node type carries device numbers.
func isDeviceType(k NodeType) bool {
	return k == NodeTypeCharacterDevice || k == NodeTypeBlockDevice
}

// ErrNodeDescriptorMalformed is a `.node` descriptor or a tar member that does
// not describe a node in the one format [EncodeNode] writes.
var ErrNodeDescriptorMalformed = errors.New("malformed node descriptor")

// ErrNodeNeedsPrivilege is a device that could not be recreated because the
// process lacks the privilege mknod needs for one.
var ErrNodeNeedsPrivilege = errors.New("creating a device node needs privilege (CAP_MKNOD on Linux, root on macOS)")

// ErrNodeUnsupported is a special file this platform gives saferm no way to
// recreate.
var ErrNodeUnsupported = errors.New("this type of special file cannot be recreated")

// nodeDescriptorHeader is the first line of every descriptor, naming the
// format and its version.
const nodeDescriptorHeader = "saferm-node 1"

// permBits are the mode bits a node keeps.
const permBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// unixPerm renders a node's mode bits the way chmod and mknod spell them.
func unixPerm(m fs.FileMode) uint32 {
	bits := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// permFromUnix is [unixPerm] reversed.
func permFromUnix(bits uint32) fs.FileMode {
	m := fs.FileMode(bits & 0o777)
	if bits&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if bits&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if bits&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

// EncodeNode writes a node's descriptor: a header line, then one line each for
// the node type, the mode in octal, and the major and minor device numbers.
func EncodeNode(n Node) []byte {
	return []byte(fmt.Sprintf("%s\nnode_type %s\nmode %04o\nmajor %d\nminor %d\n",
		nodeDescriptorHeader, n.NodeType, unixPerm(n.Perm), n.Major, n.Minor))
}

// DecodeNode reads a descriptor [EncodeNode] wrote, and refuses anything else:
// a missing or extra line, a node type that is not a special file, a mode with bits a
// node does not keep, or device numbers on a FIFO or a socket.
func DecodeNode(data []byte) (Node, error) {
	bad := func(format string, a ...any) (Node, error) {
		return Node{}, fmt.Errorf("%w: %s", ErrNodeDescriptorMalformed, fmt.Sprintf(format, a...))
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) != 6 || lines[5] != "" {
		return bad("want 5 newline-terminated lines")
	}
	if lines[0] != nodeDescriptorHeader {
		return bad("first line is %q, want %q", lines[0], nodeDescriptorHeader)
	}
	field := func(line, name string) (string, bool) {
		value, ok := strings.CutPrefix(line, name+" ")
		return value, ok && value != ""
	}
	typeWord, ok := field(lines[1], "node_type")
	if !ok {
		return bad("second line is %q, want the node type", lines[1])
	}
	nodeType, err := ParseNodeType(typeWord)
	if err != nil || !IsSpecialFileType(nodeType) {
		return bad("%q is not a special-file node type", typeWord)
	}
	modeWord, ok := field(lines[2], "mode")
	if !ok {
		return bad("third line is %q, want the mode", lines[2])
	}
	mode, err := strconv.ParseUint(modeWord, 8, 32)
	if err != nil || mode&^0o7777 != 0 {
		return bad("mode %q is not an octal permission", modeWord)
	}
	var nums [2]uint32
	for i, name := range []string{"major", "minor"} {
		word, ok := field(lines[3+i], name)
		if !ok {
			return bad("line %d is %q, want the %s number", 4+i, lines[3+i], name)
		}
		v, err := strconv.ParseUint(word, 10, 32)
		if err != nil {
			return bad("%s number %q", name, word)
		}
		nums[i] = uint32(v)
	}
	n := Node{NodeType: nodeType, Perm: permFromUnix(uint32(mode)), Major: nums[0], Minor: nums[1]}
	if !isDeviceType(nodeType) && (n.Major != 0 || n.Minor != 0) {
		return bad("a %s carries no device numbers", nodeType)
	}
	if !bytes.Equal(EncodeNode(n), data) {
		return bad("not in the canonical form")
	}
	return n, nil
}

// paxNodeType is the PAX record that marks a tar member as a special file tar
// has no typeflag for. Its value is the node's type word; only a socket is ever
// written this way.
const paxNodeType = "SAFERM.nodetype"

// nodeTarHeader is the tar member for a node inside a tree.
func nodeTarHeader(n Node, name string, modTime time.Time) (*tar.Header, error) {
	h := &tar.Header{Name: name, Mode: int64(unixPerm(n.Perm)), ModTime: modTime}
	switch n.NodeType {
	case NodeTypeFIFO:
		h.Typeflag = tar.TypeFifo
	case NodeTypeCharacterDevice:
		h.Typeflag = tar.TypeChar
		h.Devmajor, h.Devminor = int64(n.Major), int64(n.Minor)
	case NodeTypeBlockDevice:
		h.Typeflag = tar.TypeBlock
		h.Devmajor, h.Devminor = int64(n.Major), int64(n.Minor)
	case NodeTypeSocket:
		h.Typeflag = tar.TypeReg
		h.Format = tar.FormatPAX
		h.PAXRecords = map[string]string{paxNodeType: string(NodeTypeSocket)}
	case NodeTypeFile, NodeTypeDirectory, NodeTypeSymlink:
		return nil, fmt.Errorf("%s is not a special-file node type", n.NodeType)
	default:
		return nil, unknownNodeType(n.NodeType)
	}
	return h, nil
}

// nodeFromTarHeader reads the node a tar member describes. ok is false for a
// member that is not a node -- a directory, a symlink, or a regular file
// without the [paxNodeType] record.
func nodeFromTarHeader(h *tar.Header) (n Node, ok bool, err error) {
	perm := permFromUnix(uint32(h.Mode) & 0o7777)
	switch h.Typeflag {
	case tar.TypeFifo:
		return Node{NodeType: NodeTypeFIFO, Perm: perm}, true, nil
	case tar.TypeChar, tar.TypeBlock:
		nodeType := NodeTypeCharacterDevice
		if h.Typeflag == tar.TypeBlock {
			nodeType = NodeTypeBlockDevice
		}
		if h.Devmajor < 0 || h.Devmajor > 1<<32-1 || h.Devminor < 0 || h.Devminor > 1<<32-1 {
			return Node{}, false, fmt.Errorf("%w: %s: device numbers %d,%d", ErrNodeDescriptorMalformed, h.Name, h.Devmajor, h.Devminor)
		}
		return Node{NodeType: nodeType, Perm: perm, Major: uint32(h.Devmajor), Minor: uint32(h.Devminor)}, true, nil
	case tar.TypeReg:
		word, marked := h.PAXRecords[paxNodeType]
		if !marked {
			return Node{}, false, nil
		}
		if word != string(NodeTypeSocket) || h.Size != 0 {
			return Node{}, false, fmt.Errorf("%w: %s: %s=%q on a member of %d bytes", ErrNodeDescriptorMalformed, h.Name, paxNodeType, word, h.Size)
		}
		return Node{NodeType: NodeTypeSocket, Perm: perm}, true, nil
	}
	return Node{}, false, nil
}

// nodeOf describes the special file info stats, as its node type says it is.
func nodeOf(nodeType NodeType, info fs.FileInfo) Node {
	n := Node{NodeType: nodeType, Perm: info.Mode() & permBits}
	if isDeviceType(nodeType) {
		n.Major, n.Minor = deviceNumbers(info)
	}
	return n
}
