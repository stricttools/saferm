//go:build unix

package main

import (
	"io/fs"
	"syscall"
)

// fileDisk reads what a file occupies as the filesystem accounts for it: its
// allocated blocks rather than its length, how many names link its inode, and
// the inode's identity, so a file reached twice is counted once.
func fileDisk(fi fs.FileInfo) fileDiskUse {
	st := fi.Sys().(*syscall.Stat_t)
	return fileDiskUse{
		Bytes: int64(st.Blocks) * 512,
		Links: uint64(st.Nlink),
		ID:    fileID{dev: uint64(st.Dev), ino: uint64(st.Ino)},
		HasID: true,
	}
}
