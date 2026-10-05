//go:build linux || darwin

package archive

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// makeNode creates the special file n describes at path, with n's mode bits
// exactly (the umask does not apply to them). path must not exist.
//
// A device needs privilege to create -- CAP_MKNOD on Linux, root on macOS --
// and without it this is an [ErrNodeNeedsPrivilege] hard error: nothing else
// can stand in for a device node.
func makeNode(path string, n Node) error {
	perm := unixPerm(n.Perm)
	var err error
	switch n.Kind {
	case KindFIFO:
		err = unix.Mkfifo(path, perm)
	case KindSocket:
		if !socketNodesSupported {
			return fmt.Errorf("recreating the socket %s: %w on %s", path, ErrNodeUnsupported, runtime.GOOS)
		}
		err = unix.Mknod(path, unix.S_IFSOCK|perm, 0)
	case KindCharacterDevice:
		err = unix.Mknod(path, unix.S_IFCHR|perm, int(unix.Mkdev(n.Major, n.Minor)))
	case KindBlockDevice:
		err = unix.Mknod(path, unix.S_IFBLK|perm, int(unix.Mkdev(n.Major, n.Minor)))
	case KindFile, KindDirectory, KindSymlink:
		return fmt.Errorf("%s is not a special-file kind", n.Kind)
	default:
		return unknownKind(n.Kind)
	}
	if err != nil {
		if isDeviceKind(n.Kind) && (errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)) {
			return fmt.Errorf("recreating the %s %s (%d,%d): %w (%v)", n.Kind, path, n.Major, n.Minor, ErrNodeNeedsPrivilege, err)
		}
		return fmt.Errorf("recreating the %s %s: %w", n.Kind, path, err)
	}
	if err := os.Chmod(path, n.Perm); err != nil {
		os.Remove(path)
		return fmt.Errorf("setting the mode of the recreated %s %s: %w", n.Kind, path, err)
	}
	return nil
}
