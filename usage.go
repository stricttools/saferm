package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stricttools/saferm/internal/db"
	"github.com/stricttools/strictcli/go/strictcli"
)

// usageGroup is one row of a breakdown: how many records still in the archive
// fall in it, what they measured when they were deleted, and what their
// archive entries occupy on disk now.
type usageGroup struct {
	Records       int   `json:"records"`
	OriginalBytes int64 `json:"original_bytes"`
	DiskBytes     int64 `json:"disk_bytes"`
}

func (g *usageGroup) add(rec *db.DeletionRecord, diskBytes int64) {
	g.Records++
	g.OriginalBytes += rec.Size
	g.DiskBytes += diskBytes
}

type usageAgeRow struct {
	Age string `json:"age"`
	usageGroup
}

type usageDirectoryRow struct {
	Directory string `json:"directory"`
	usageGroup
}

type usageOtherDirectories struct {
	Directories int `json:"directories"`
	usageGroup
}

// usagePayload is `usage`'s machine payload: the report's figures in bytes,
// where the printed report rounds them.
type usagePayload struct {
	ArchiveDir            string                `json:"archive_dir"`
	DatabasePath          string                `json:"database_path"`
	ArchiveDiskBytes      int64                 `json:"archive_disk_bytes"`
	SharedDiskBytes       int64                 `json:"shared_disk_bytes"`
	UnreferencedDiskBytes int64                 `json:"unreferenced_disk_bytes"`
	DatabaseDiskBytes     int64                 `json:"database_disk_bytes"`
	TotalDiskBytes        int64                 `json:"total_disk_bytes"`
	Records               int                   `json:"records"`
	MissingEntries        int                   `json:"missing_entries"`
	ByAge                 []usageAgeRow         `json:"by_age"`
	ByDirectory           []usageDirectoryRow   `json:"by_directory"`
	OtherDirectories      usageOtherDirectories `json:"other_directories"`
}

var usageIntegers = map[string]interface{}{
	"records":        map[string]interface{}{"type": "integer"},
	"original_bytes": map[string]interface{}{"type": "integer"},
	"disk_bytes":     map[string]interface{}{"type": "integer"},
}

// usageGroupSchema declares a breakdown row: the three figures, plus one
// string member naming the row when name is not empty.
func usageGroupSchema(name string) map[string]interface{} {
	props := map[string]interface{}{}
	required := []interface{}{}
	for k, v := range usageIntegers {
		props[k] = v
	}
	if name != "" {
		props[name] = map[string]interface{}{"type": "string"}
		required = append(required, name)
	}
	required = append(required, "records", "original_bytes", "disk_bytes")
	return map[string]interface{}{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func usagePayloadSchema() map[string]interface{} {
	other := usageGroupSchema("")
	other["properties"].(map[string]interface{})["directories"] = map[string]interface{}{"type": "integer"}
	other["required"] = append(other["required"].([]interface{}), "directories")
	integer := map[string]interface{}{"type": "integer"}
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"archive_dir":             map[string]interface{}{"type": "string"},
			"database_path":           map[string]interface{}{"type": "string"},
			"archive_disk_bytes":      integer,
			"shared_disk_bytes":       integer,
			"unreferenced_disk_bytes": integer,
			"database_disk_bytes":     integer,
			"total_disk_bytes":        integer,
			"records":                 integer,
			"missing_entries":         integer,
			"by_age":                  map[string]interface{}{"type": "array", "items": usageGroupSchema("age")},
			"by_directory":            map[string]interface{}{"type": "array", "items": usageGroupSchema("directory")},
			"other_directories":       other,
		},
		"required": []interface{}{
			"archive_dir", "database_path", "archive_disk_bytes", "shared_disk_bytes",
			"unreferenced_disk_bytes", "database_disk_bytes", "total_disk_bytes", "records",
			"missing_entries", "by_age", "by_directory", "other_directories",
		},
		"additionalProperties": false,
	}
}

// usageAgeBuckets are the age breakdown's rows, youngest first. A record
// belongs to the first bucket whose bound its age is under; the last bucket
// has no bound.
var usageAgeBuckets = []struct {
	label string
	under time.Duration
}{
	{"under 1 day", 24 * time.Hour},
	{"1 to 7 days", 7 * 24 * time.Hour},
	{"7 to 30 days", 30 * 24 * time.Hour},
	{"30 to 90 days", 90 * 24 * time.Hour},
	{"90 days or more", 0},
}

func registerUsageCmd(app *strictcli.App) {
	app.Command("usage", "Report how much disk the archive takes, broken down by the age and the original directory of what it holds", handleUsage,
		strictcli.WithEffect(strictcli.EffectReadOnly),
		strictcli.PayloadSchema(usagePayloadSchema()),
		strictcli.WithFlags(
			strictcli.IntFlag("directory-depth", "How many leading components of each record's original directory name its group in the directory breakdown (1 groups /home/m/Projects/x under /home)", strictcli.Default(3)),
			strictcli.IntFlag("directory-limit", "How many of the directory groups using the most disk to list one by one; the rest are summed on one line, and 0 lists every group", strictcli.Default(20)),
		),
	)
}

// directoryGroup names the directory breakdown's group for an original path:
// the first depth components of the directory it was archived from.
func directoryGroup(originalPath string, depth int) string {
	dir := filepath.Dir(originalPath)
	parts := strings.Split(strings.Trim(dir, string(filepath.Separator)), string(filepath.Separator))
	if dir == string(filepath.Separator) || len(parts) == 0 || parts[0] == "" {
		return dir
	}
	if len(parts) > depth {
		parts = parts[:depth]
	}
	return string(filepath.Separator) + filepath.Join(parts...)
}

// archiveFile is one file found in the archive directory.
type archiveFile struct {
	use     fileDiskUse
	claimed bool
}

// scanArchiveDir stats every file under the archive directory. An archive
// directory that does not exist holds nothing.
func scanArchiveDir(archiveDir string) (map[string]*archiveFile, error) {
	files := map[string]*archiveFile{}
	err := filepath.WalkDir(archiveDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == archiveDir && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		files[path] = &archiveFile{use: fileDisk(fi)}
		return nil
	})
	return files, err
}

// databaseDisk is what the database occupies: the file itself and, in WAL
// mode, its write-ahead log and shared-memory index beside it.
func databaseDisk(dbPath string) (int64, error) {
	var total int64
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += fileDisk(fi).Bytes
	}
	return total, nil
}

func handleUsage(ctx *strictcli.Context, kwargs map[string]interface{}) strictcli.Outcome {
	depth := kwargs["directory_depth"].(int)
	limit := kwargs["directory_limit"].(int)
	if depth < 1 {
		fmt.Fprintf(os.Stderr, "error: --directory-depth must be at least 1, got %d\n", depth)
		return strictcli.Exit(ExitUsage)
	}
	if limit < 0 {
		fmt.Fprintf(os.Stderr, "error: --directory-limit must be 0 (every group) or more, got %d\n", limit)
		return strictcli.Exit(ExitUsage)
	}
	archiveDir := kwargs["archive_dir"].(string)
	dbPath := kwargs["db_path"].(string)

	var records []*db.DeletionRecord
	database, err := openArchiveDBIfPresent(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: opening database: %s\n", err)
		return strictcli.Exit(dbExit(err))
	}
	if database != nil {
		records, err = database.QueryAll(false)
		database.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: querying database: %s\n", err)
			return strictcli.Exit(dbExit(err))
		}
	}

	files, err := scanArchiveDir(archiveDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: reading the archive directory: %s\n", err)
		return strictcli.Exit(ExitArchive)
	}
	dbBytes, err := databaseDisk(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: reading the database file: %s\n", err)
		return strictcli.Exit(ExitDatabase)
	}

	report := usagePayload{
		ArchiveDir:        archiveDir,
		DatabasePath:      dbPath,
		DatabaseDiskBytes: dbBytes,
		Records:           len(records),
		ByDirectory:       []usageDirectoryRow{},
	}

	// The whole directory, each inode once: what the archive takes from the
	// disk, whether or not a live record names the file.
	seen := map[fileID]bool{}
	for _, f := range files {
		if f.use.HasID {
			if seen[f.use.ID] {
				continue
			}
			seen[f.use.ID] = true
		}
		report.ArchiveDiskBytes += f.use.Bytes
		if f.use.Shared() {
			report.SharedDiskBytes += f.use.Bytes
		}
	}

	ages := make([]usageGroup, len(usageAgeBuckets))
	byDir := map[string]*usageGroup{}
	now := time.Now()
	for _, rec := range records {
		var diskBytes int64
		if f, ok := files[archiveEntryPath(archiveDir, rec)]; ok {
			f.claimed = true
			diskBytes = f.use.Bytes
		} else {
			report.MissingEntries++
		}
		age := now.Sub(rec.DeletedAt)
		for i, b := range usageAgeBuckets {
			if b.under == 0 || age < b.under {
				ages[i].add(rec, diskBytes)
				break
			}
		}
		key := directoryGroup(rec.OriginalPath, depth)
		if byDir[key] == nil {
			byDir[key] = &usageGroup{}
		}
		byDir[key].add(rec, diskBytes)
	}
	for _, f := range files {
		if !f.claimed {
			report.UnreferencedDiskBytes += f.use.Bytes
		}
	}
	report.TotalDiskBytes = report.ArchiveDiskBytes + report.DatabaseDiskBytes

	for i, b := range usageAgeBuckets {
		report.ByAge = append(report.ByAge, usageAgeRow{Age: b.label, usageGroup: ages[i]})
	}
	var dirs []usageDirectoryRow
	for name, g := range byDir {
		dirs = append(dirs, usageDirectoryRow{Directory: name, usageGroup: *g})
	}
	sort.Slice(dirs, func(i, j int) bool {
		if dirs[i].DiskBytes != dirs[j].DiskBytes {
			return dirs[i].DiskBytes > dirs[j].DiskBytes
		}
		if dirs[i].Records != dirs[j].Records {
			return dirs[i].Records > dirs[j].Records
		}
		return dirs[i].Directory < dirs[j].Directory
	})
	if limit > 0 && len(dirs) > limit {
		for _, d := range dirs[limit:] {
			report.OtherDirectories.Directories++
			report.OtherDirectories.Records += d.Records
			report.OtherDirectories.OriginalBytes += d.OriginalBytes
			report.OtherDirectories.DiskBytes += d.DiskBytes
		}
		dirs = dirs[:limit]
	}
	report.ByDirectory = append(report.ByDirectory, dirs...)

	ctx.Payload(report)
	emit(ctx, "%s", renderUsage(report, depth))
	return strictcli.Exit(ExitSuccess)
}

// renderUsage prints the report as `usage` shows it.
func renderUsage(r usagePayload, depth int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Archive:   %s on disk in %s (%d records still in the archive)\n", humanSize(r.ArchiveDiskBytes), r.ArchiveDir, r.Records)
	if r.SharedDiskBytes > 0 {
		fmt.Fprintf(&b, "  of which %s is in file entries also linked from outside the archive; purging those frees nothing until the other links go\n", humanSize(r.SharedDiskBytes))
	}
	if r.UnreferencedDiskBytes > 0 {
		fmt.Fprintf(&b, "  of which %s is in files no record still in the archive names\n", humanSize(r.UnreferencedDiskBytes))
	}
	if r.MissingEntries > 0 {
		fmt.Fprintf(&b, "  %d record(s) name an archived copy that is gone\n", r.MissingEntries)
	}
	fmt.Fprintf(&b, "Database:  %s on disk in %s\n", humanSize(r.DatabaseDiskBytes), r.DatabasePath)
	fmt.Fprintf(&b, "Total:     %s\n", humanSize(r.TotalDiskBytes))

	row := func(name string, g usageGroup) {
		fmt.Fprintf(&b, "%-40s %8d %10s %10s\n", name, g.Records, humanSize(g.OriginalBytes), humanSize(g.DiskBytes))
	}
	header := func(title string) {
		fmt.Fprintf(&b, "\n%-40s %8s %10s %10s\n", title, "Records", "Original", "On disk")
		fmt.Fprintf(&b, "%-40s %8s %10s %10s\n", strings.Repeat("-", 40), "--------", "----------", "----------")
	}

	header("By age")
	for _, a := range r.ByAge {
		row(a.Age, a.usageGroup)
	}
	header(fmt.Sprintf("By original directory (depth %d)", depth))
	for _, d := range r.ByDirectory {
		name := d.Directory
		if len(name) > 40 {
			name = "..." + name[len(name)-37:]
		}
		row(name, d.usageGroup)
	}
	if r.OtherDirectories.Directories > 0 {
		row(fmt.Sprintf("%d other directories", r.OtherDirectories.Directories), r.OtherDirectories.usageGroup)
	}
	return b.String()
}
