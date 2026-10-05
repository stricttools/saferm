package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stricttools/saferm/internal/archive"
	"github.com/stricttools/saferm/internal/db"
	"github.com/stricttools/strictcli/go/strictcli"
)

// say prints progress or summary chatter to stdout unless --quiet is in force.
//
// It draws the line the framework's --quiet flag means in saferm: it is for
// what a caller can lose without losing information -- the counted summaries,
// the per-item --verbose progress, "Nothing to purge.", the restore
// confirmation. It is NOT for the outputs that ARE the command (`list` and
// `info`, and the dry-run previews -- those go through [emit]), and it is not
// for stderr, which --quiet never touches. Routing every chatter line through
// one helper is what keeps that boundary from drifting one print at a time.
//
// --quiet dominates --verbose, matching the framework's own Context.Debug.
//
// It writes through the context writer rather than to the process's stdout, so
// machine mode carries the line in the envelope's diagnostics instead of
// printing it beside the envelope. In human mode the writer IS stdout and the
// bytes are what they always were.
func say(ctx *strictcli.Context, format string, a ...interface{}) {
	ctx.Info(oneLine(format, a...))
}

// emit prints output that IS the command's answer: `list`'s and `info`'s
// tables, `purge`'s listing of what it is about to destroy, its dry-run table.
// Unlike [say] it is never suppressed -- --quiet silences chatter, never the
// thing the caller asked for.
//
// Machine mode is the one branch: there the envelope is the sole stdout
// document, so the text rides it as a diagnostic instead of being printed
// beside it. That is an explicit mode the caller selected with --json, not a
// degradation -- in human mode the bytes are exactly what they were.
//
// Callers build a whole table in one string and emit it once, so machine mode
// carries one diagnostic per answer rather than one per row.
func emit(ctx *strictcli.Context, format string, a ...interface{}) {
	if ctx.JSON() {
		ctx.Info(oneLine(format, a...))
		return
	}
	fmt.Printf(format, a...)
}

// oneLine formats a message and strips the trailing newline saferm's formats
// carry, because the context writer supplies its own.
func oneLine(format string, a ...interface{}) string {
	return strings.TrimSuffix(fmt.Sprintf(format, a...), "\n")
}

// ensureDirectories creates the base dir, archive dir, and db dir (parent of
// dbPath) if they don't exist.
//
// The creation is minted on the effects handle rather than performed directly,
// so a dry run declares it in the would-do log and makes nothing. It used to be
// a raw MkdirAll: the first-ever `saferm --dry-run delete` created saferm's
// whole state directory on its way to promising it would touch nothing.
//
// Only the three mutating commands call this. `list` and `info` are read_only,
// and the effects handle refuses a mutation from a read_only command outright
// -- they open the database where it already is and error where it is not.
func ensureDirectories(fx *strictcli.Effects, baseDir, archiveDir, dbPath string) error {
	dirs := []string{
		baseDir,
		archiveDir,
		filepath.Dir(dbPath),
	}
	for _, dir := range dirs {
		if _, err := fx.Mkdir(dir, strictcli.Resource("saferm-state:"+dir)); err != nil {
			return err
		}
	}
	return nil
}

// dbExit picks the exit code a database-layer failure deserves.
//
// Contention that outlived the retry budget is not a database failure in the
// same sense as the rest: nothing is wrong with the archive, another process
// simply held the write lock the whole time, and the caller's correct response
// is to run the command again rather than to investigate. It gets its own exit
// code so a script can tell the two apart without reading English.
func dbExit(err error) int {
	if db.IsContentionExhausted(err) {
		return ExitContention
	}
	return ExitDatabase
}

// retryNotifier reports each database contention retry on stderr under
// --verbose, and reports nothing otherwise.
//
// stderr rather than stdout, and so outside say(): for `list` and `info` stdout
// IS the command's output, and a retry notice interleaved into a table would
// corrupt the thing the caller asked for. A retry is a diagnostic about the
// wait, which is what stderr is for -- and --quiet, which never touches stderr,
// leaves it alone.
func retryNotifier(ctx *strictcli.Context) db.RetryNotifier {
	if !ctx.Verbose() {
		return nil
	}
	return func(attempt, maxAttempts int, delay time.Duration, err error) {
		fmt.Fprintf(os.Stderr, "database is locked by another process (attempt %d/%d); retrying in %s\n",
			attempt, maxAttempts, delay)
	}
}

// openArchiveDB opens the archive database, or reports that no archive exists.
//
// Under --dry-run ensureDirectories only records the directories it would
// create, so on a machine with no archive yet there is nothing for SQLite to
// open -- and creating the file to answer a preview would be exactly the write
// the preview promises not to make. The caller gets a nil *db.DB meaning "no
// archive yet" and must say so in its own vocabulary. Outside dry mode the
// directories have just been created, so the open always proceeds and the
// result is never nil.
func openArchiveDB(ctx *strictcli.Context, dbPath string) (*db.DB, error) {
	if ctx.DryRun() {
		return openArchiveDBIfPresent(ctx, dbPath)
	}
	return db.Open(dbPath, retryNotifier(ctx))
}

// openArchiveDBIfPresent opens the archive database, or returns nil when there
// is no database file.
//
// It is what the read_only commands use. `list` and `info` never create
// saferm's state directory -- they cannot, the effects handle refuses a
// mutation from a read_only command -- so on a machine that has never deleted
// anything they meet a file that is not there. SQLite's "unable to open
// database file" is the wrong answer to "what have I deleted?"; the caller
// turns the nil into "nothing", which is the true one.
func openArchiveDBIfPresent(ctx *strictcli.Context, dbPath string) (*db.DB, error) {
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return db.Open(dbPath, retryNotifier(ctx))
}

// recordKind names what a record archived, in the word every surface uses:
// the record's own `kind` column, which is the one authority for it.
func recordKind(rec *db.DeletionRecord) string {
	return string(rec.Kind)
}

// kindEnum is the closed set the machine payloads' `kind` member declares,
// generated from [archive.Kinds] so a new kind reaches every schema at once.
func kindEnum() []interface{} {
	enum := make([]interface{}, 0, len(archive.Kinds()))
	for _, k := range archive.Kinds() {
		enum = append(enum, string(k))
	}
	return enum
}

// archiveEntryPath is where a record's archived content lives (see
// [archive.EntryPath]). Spelled once, because `purge` destroys that path and
// `info` reports whether it is still there, and the two answering differently
// would be worse than either being wrong.
func archiveEntryPath(archiveDir string, rec *db.DeletionRecord) string {
	return archive.EntryPath(archiveDir, rec.UUID, rec.Kind)
}

// archiveEntryIsGone reports whether a record's archived content is not on
// disk. Any other stat failure -- a permission problem, an unreadable mount --
// is not an absence, and is deliberately not reported as one.
func archiveEntryIsGone(archiveDir string, rec *db.DeletionRecord) bool {
	_, err := os.Lstat(archiveEntryPath(archiveDir, rec))
	return errors.Is(err, os.ErrNotExist)
}

// pathSeparatorPlaceholder stands in for "/" while a pattern and a path are
// handed to filepath.Match. NUL is the one byte a POSIX path can never contain
// (the kernel interface terminates paths with it), so substituting it can never
// collide with real path content.
const pathSeparatorPlaceholder = "\x00"

// matchArchivePath reports whether an archived original path matches the
// user's --path glob.
//
// The syntax is filepath.Match's, with one deliberate difference: `*`, `?` and
// character classes match the path separator too, so a pattern names a subtree
// rather than exactly one directory level. saferm stores absolute original
// paths, which are always several levels deep, so the stock semantics made
// `--path "/home/*"` mean "the immediate children of /home and nothing else" --
// a filter that could not reach a single realistically archived path. Spanning
// separators is what the flag's own documented example always implied.
//
// Malformed patterns still come back as filepath.ErrBadPattern, so the caller
// keeps reporting them as usage errors rather than as an empty result.
func matchArchivePath(pattern, path string) (bool, error) {
	return filepath.Match(
		strings.ReplaceAll(pattern, "/", pathSeparatorPlaceholder),
		strings.ReplaceAll(path, "/", pathSeparatorPlaceholder),
	)
}

// humanSize formats a byte count as a human-readable string.
func humanSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	size := float64(bytes)
	for _, unit := range units {
		size /= 1024
		if size < 1024 {
			if size < 10 {
				return fmt.Sprintf("%.1f %s", size, unit)
			}
			return fmt.Sprintf("%.0f %s", size, unit)
		}
	}
	return fmt.Sprintf("%.0f TB", size)
}

// humanAge formats a time as a relative duration from now.
func humanAge(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		mins := int(d.Minutes())
		if mins == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	}
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1 day ago"
	}
	if days < 30 {
		return fmt.Sprintf("%d days ago", days)
	}
	months := days / 30
	if months == 1 {
		return "1 month ago"
	}
	if months < 12 {
		return fmt.Sprintf("%d months ago", months)
	}
	years := months / 12
	if years == 1 {
		return "1 year ago"
	}
	return fmt.Sprintf("%d years ago", years)
}

// parseSize parses a human-readable size string into bytes.
// Supported suffixes (case-insensitive): B, KB (1024), MB (1024^2), GB (1024^3), TB (1024^4).
// Suffix is mandatory — bare numbers are rejected.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}

	upper := strings.ToUpper(s)

	// Find where the numeric part ends and the suffix begins.
	suffixStart := -1
	for i, ch := range upper {
		if ch < '0' || ch > '9' {
			suffixStart = i
			break
		}
	}

	if suffixStart <= 0 {
		// Either no digits at all, or no suffix (bare number).
		if suffixStart == 0 {
			return 0, fmt.Errorf("invalid size %q: no numeric part", s)
		}
		return 0, fmt.Errorf("invalid size %q: suffix is mandatory (use B, KB, MB, GB, or TB)", s)
	}

	numStr := s[:suffixStart]
	suffix := upper[suffixStart:]

	n, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: number part %q is not a valid integer", s, numStr)
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid size %q: must be positive", s)
	}

	var multiplier int64
	switch suffix {
	case "B":
		multiplier = 1
	case "KB":
		multiplier = 1024
	case "MB":
		multiplier = 1024 * 1024
	case "GB":
		multiplier = 1024 * 1024 * 1024
	case "TB":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("invalid size %q: unknown suffix %q (use B, KB, MB, GB, or TB)", s, suffix)
	}

	result := n * multiplier
	// Check for overflow: if dividing back doesn't give the original number, it overflowed.
	if result/multiplier != n {
		return 0, fmt.Errorf("invalid size %q: value overflows int64", s)
	}

	return result, nil
}

// parseDuration parses a human-friendly duration string.
// Supported suffixes: h (hours), d (days), w (weeks), m (months of 30 days).
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration string")
	}

	suffix := s[len(s)-1:]
	numStr := s[:len(s)-1]

	n, err := strconv.Atoi(numStr)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: number part %q is not an integer", s, numStr)
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid duration %q: must be positive", s)
	}

	switch suffix {
	case "h":
		return time.Duration(n) * time.Hour, nil
	case "d":
		return time.Duration(n) * 24 * time.Hour, nil
	case "w":
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	case "m":
		return time.Duration(n) * 30 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("invalid duration %q: unknown suffix %q (use h, d, w, or m)", s, suffix)
	}
}

// optBool resolves an optional flag's absence to the fallback its own help text
// declares.
//
// strictcli's mutating-default ban (contract §27.1) forbids Default() on any
// flag or positional arg of a command declaring effect="mutating": absence must
// never resolve to a value the invocation did not state, because on a mutating
// command a value the framework picked is a value the framework writes. saferm's
// opt-in and opt-out switches on delete, undelete and purge therefore declare
// Optional() and name their fallback in their own help, and this function is the
// ONLY place where absence becomes that fallback -- so no code further down ever
// receives a nil it would misread as the zero value.
func optBool(v interface{}, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return v.(bool)
}

// optStr is optBool's string twin: absence becomes the declared fallback, and a
// supplied value -- the empty string included -- is delivered as itself. The
// empty string is a value here, not a sentinel for absence; callers that need to
// know whether anything was supplied read the two-result type assertion instead.
func optStr(v interface{}, fallback string) string {
	if v == nil {
		return fallback
	}
	return v.(string)
}

// optStrSlice converts an optional repeatable flag's or variadic arg's value to
// []string, delivering absence as an empty slice.
func optStrSlice(v interface{}) []string {
	if v == nil {
		return nil
	}
	raw := v.([]interface{})
	out := make([]string, len(raw))
	for i, elem := range raw {
		out[i] = elem.(string)
	}
	return out
}
