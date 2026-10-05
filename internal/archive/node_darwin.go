package archive

import (
	"io/fs"
	"syscall"

	"golang.org/x/sys/unix"
)

// socketNodesSupported: macOS's mknod makes no socket without root, so a
// socket node is never recreated there.
const socketNodesSupported = false

// deviceNumbers reads a device's major and minor numbers off its stat.
func deviceNumbers(info fs.FileInfo) (major, minor uint32) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	dev := uint64(uint32(st.Rdev))
	return unix.Major(dev), unix.Minor(dev)
}
