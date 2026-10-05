package archive

import (
	"io/fs"
	"syscall"

	"golang.org/x/sys/unix"
)

// socketNodesSupported: Linux lets an unprivileged process mknod a socket.
const socketNodesSupported = true

// deviceNumbers reads a device's major and minor numbers off its stat.
func deviceNumbers(info fs.FileInfo) (major, minor uint32) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return unix.Major(st.Rdev), unix.Minor(st.Rdev)
}
