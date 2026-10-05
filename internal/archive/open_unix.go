//go:build !windows

package archive

import (
	"os"
	"syscall"
)

// openReadFlags are the flags every content read opens with. O_NONBLOCK makes
// the open of a FIFO return at once instead of waiting for a writer, so the
// fstat in [openRegular] gets to refuse it; O_NOFOLLOW refuses a path that was
// swapped for a symlink, which would otherwise read whatever the link names.
const openReadFlags = os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW
