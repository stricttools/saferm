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
// its kind, its permission bits, and for a device its major and minor numbers.
//
// This file is the one codec for that description. A node archived on its own
// is kept as a `<uuid>.node` descriptor ([EncodeNode], [DecodeNode]); a node
// inside a tree is a tar member ([nodeTarHeader], [nodeFromTarHeader]), where
// FIFOs and devices have tar typeflags of their own and a socket, which has
// none, is a zero-length regular member carrying the [paxNodeKind] record.

// Node is what the archive keeps of a special file.
type Node struct {
	Kind  Kind
	Perm  fs.FileMode // permission bits, with setuid, setgid and sticky
	Major uint32      // device numbers; zero for a FIFO or a socket
	Minor uint32
}

// IsNodeKind reports whether a kind is one of the special-file kinds.
func IsNodeKind(k Kind) bool {
	switch k {
	case KindFIFO, KindSocket, KindCharacterDevice, KindBlockDevice:
		return true
	case KindFile, KindDirectory, KindSymlink:
		return false
	}
	panic(fmt.Sprintf("archive.IsNodeKind: %s", unknownKind(k)))
}

// isDeviceKind reports whether a node kind carries device numbers.
func isDeviceKind(k Kind) bool {
	return k == KindCharacterDevice || k == KindBlockDevice
}

// ErrNodeDescriptorMalformed is a `.node` descriptor or a tar member that does
// not describe a node in the one format [EncodeNode] writes.
var ErrNodeDescriptorMalformed = errors.New("malformed node descriptor")

// ErrNodeNeedsPrivilege is a device that could not be recreated because the
// process lacks the privilege mknod needs for one.
var ErrNodeNeedsPrivilege = errors.New("creating a device node needs privilege (CAP_MKNOD on Linux, root on macOS)")

// ErrNodeUnsupported is a special file this platform gives saferm no way to
// recreate.
var ErrNodeUnsupported = errors.New("this kind of special file cannot be recreated")

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
// the kind, the mode in octal, and the major and minor device numbers.
func EncodeNode(n Node) []byte {
	return []byte(fmt.Sprintf("%s\nkind %s\nmode %04o\nmajor %d\nminor %d\n",
		nodeDescriptorHeader, n.Kind, unixPerm(n.Perm), n.Major, n.Minor))
}

// DecodeNode reads a descriptor [EncodeNode] wrote, and refuses anything else:
// a missing or extra line, a kind that is not a node kind, a mode with bits a
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
	kindWord, ok := field(lines[1], "kind")
	if !ok {
		return bad("second line is %q, want the kind", lines[1])
	}
	kind, err := ParseKind(kindWord)
	if err != nil || !IsNodeKind(kind) {
		return bad("%q is not a special-file kind", kindWord)
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
	n := Node{Kind: kind, Perm: permFromUnix(uint32(mode)), Major: nums[0], Minor: nums[1]}
	if !isDeviceKind(kind) && (n.Major != 0 || n.Minor != 0) {
		return bad("a %s carries no device numbers", kind)
	}
	if !bytes.Equal(EncodeNode(n), data) {
		return bad("not in the canonical form")
	}
	return n, nil
}

// paxNodeKind is the PAX record that marks a tar member as a special file tar
// has no typeflag for. Its value is the node's kind word; only a socket is ever
// written this way.
const paxNodeKind = "SAFERM.nodetype"

// nodeTarHeader is the tar member for a node inside a tree.
func nodeTarHeader(n Node, name string, modTime time.Time) (*tar.Header, error) {
	h := &tar.Header{Name: name, Mode: int64(unixPerm(n.Perm)), ModTime: modTime}
	switch n.Kind {
	case KindFIFO:
		h.Typeflag = tar.TypeFifo
	case KindCharacterDevice:
		h.Typeflag = tar.TypeChar
		h.Devmajor, h.Devminor = int64(n.Major), int64(n.Minor)
	case KindBlockDevice:
		h.Typeflag = tar.TypeBlock
		h.Devmajor, h.Devminor = int64(n.Major), int64(n.Minor)
	case KindSocket:
		h.Typeflag = tar.TypeReg
		h.Format = tar.FormatPAX
		h.PAXRecords = map[string]string{paxNodeKind: string(KindSocket)}
	case KindFile, KindDirectory, KindSymlink:
		return nil, fmt.Errorf("%s is not a special-file kind", n.Kind)
	default:
		return nil, unknownKind(n.Kind)
	}
	return h, nil
}

// nodeFromTarHeader reads the node a tar member describes. ok is false for a
// member that is not a node -- a directory, a symlink, or a regular file
// without the [paxNodeKind] record.
func nodeFromTarHeader(h *tar.Header) (n Node, ok bool, err error) {
	perm := permFromUnix(uint32(h.Mode) & 0o7777)
	switch h.Typeflag {
	case tar.TypeFifo:
		return Node{Kind: KindFIFO, Perm: perm}, true, nil
	case tar.TypeChar, tar.TypeBlock:
		kind := KindCharacterDevice
		if h.Typeflag == tar.TypeBlock {
			kind = KindBlockDevice
		}
		if h.Devmajor < 0 || h.Devmajor > 1<<32-1 || h.Devminor < 0 || h.Devminor > 1<<32-1 {
			return Node{}, false, fmt.Errorf("%w: %s: device numbers %d,%d", ErrNodeDescriptorMalformed, h.Name, h.Devmajor, h.Devminor)
		}
		return Node{Kind: kind, Perm: perm, Major: uint32(h.Devmajor), Minor: uint32(h.Devminor)}, true, nil
	case tar.TypeReg:
		word, marked := h.PAXRecords[paxNodeKind]
		if !marked {
			return Node{}, false, nil
		}
		if word != string(KindSocket) || h.Size != 0 {
			return Node{}, false, fmt.Errorf("%w: %s: %s=%q on a member of %d bytes", ErrNodeDescriptorMalformed, h.Name, paxNodeKind, word, h.Size)
		}
		return Node{Kind: KindSocket, Perm: perm}, true, nil
	}
	return Node{}, false, nil
}

// nodeOf describes the special file info stats, as its kind says it is.
func nodeOf(kind Kind, info fs.FileInfo) Node {
	n := Node{Kind: kind, Perm: info.Mode() & permBits}
	if isDeviceKind(kind) {
		n.Major, n.Minor = deviceNumbers(info)
	}
	return n
}
