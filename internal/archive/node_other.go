//go:build !linux && !darwin

package archive

import (
	"fmt"
	"io/fs"
	"runtime"
)

// deviceNumbers has nothing to read where saferm does not read device numbers.
func deviceNumbers(info fs.FileInfo) (major, minor uint32) {
	return 0, 0
}

// makeNode refuses: saferm recreates special files only on Linux and macOS.
func makeNode(path string, n Node) error {
	return fmt.Errorf("recreating the %s %s: %w on %s", n.Kind, path, ErrNodeUnsupported, runtime.GOOS)
}
