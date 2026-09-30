//go:build windows

package main

import "io/fs"

// fileDisk on Windows reports a file's length: the allocated size and the
// link count are not in what os.Lstat returns there, so every file counts as
// its own single name.
func fileDisk(fi fs.FileInfo) fileDiskUse {
	return fileDiskUse{Bytes: fi.Size(), Links: 1}
}
