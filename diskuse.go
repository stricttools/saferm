package main

import (
	"errors"
	"os"

	"github.com/stricttools/saferm/internal/db"
)

// fileID identifies an inode, so a file reached under two names is counted
// once.
type fileID struct {
	dev uint64
	ino uint64
}

// fileDiskUse is what one file occupies on disk (see fileDisk).
type fileDiskUse struct {
	Bytes int64
	Links uint64
	ID    fileID
	HasID bool
}

// Shared reports whether another name outside this one links the file: a
// file entry archived with os.Link whose inode also has a name elsewhere.
// Removing the archive's name frees nothing until the last name goes.
func (u fileDiskUse) Shared() bool {
	return u.Links > 1
}

// Freed is what removing this name gives back to the filesystem.
func (u fileDiskUse) Freed() int64 {
	if u.Shared() {
		return 0
	}
	return u.Bytes
}

// entryDiskUse reads what a record's archive entry occupies on disk. present
// is false when the entry is not there; any other stat failure is an error,
// never an absence.
func entryDiskUse(archiveDir string, rec *db.DeletionRecord) (use fileDiskUse, present bool, err error) {
	fi, err := os.Lstat(archiveEntryPath(archiveDir, rec))
	if errors.Is(err, os.ErrNotExist) {
		return fileDiskUse{}, false, nil
	}
	if err != nil {
		return fileDiskUse{}, false, err
	}
	return fileDisk(fi), true, nil
}
