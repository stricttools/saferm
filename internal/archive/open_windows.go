//go:build windows

package archive

import "os"

// openReadFlags are the flags every content read opens with. Windows has no
// FIFO or device node a path can name and no O_NONBLOCK or O_NOFOLLOW, so the
// fstat in [openRegular] is the whole check there.
const openReadFlags = os.O_RDONLY
