package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/smm-h/strictcli/go/strictcli"
	"github.com/stricttools/saferm/internal/archive"
	"github.com/stricttools/saferm/internal/db"
)

// listRow is one row of `list`'s machine payload: the table's own columns, plus
// the two things the table cannot carry. The uuid is the handle that survives
// (the table has room only for the numeric id), and deleted_at is an absolute
// RFC3339 timestamp where the Age column is relative prose nothing can compute
// with.
type listRow struct {
	ID        int64  `json:"id"`
	UUID      string `json:"uuid"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Kind      string `json:"kind"`
	DeletedAt string `json:"deleted_at"`
	Status    string `json:"status"`
}

// listRowSchema declares one row of `list`'s payload.
var listRowSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"id":         map[string]interface{}{"type": "integer"},
		"uuid":       map[string]interface{}{"type": "string"},
		"path":       map[string]interface{}{"type": "string"},
		"size":       map[string]interface{}{"type": "integer"},
		"kind":       map[string]interface{}{"type": "string", "enum": kindEnum()},
		"deleted_at": map[string]interface{}{"type": "string"},
		"status":     map[string]interface{}{"type": "string", "enum": []interface{}{statusArchived, statusRestored, statusPurged}},
	},
	"required":             []interface{}{"id", "uuid", "path", "size", "kind", "deleted_at", "status"},
	"additionalProperties": false,
}

// listPayload is `list`'s machine payload: the rows shown, newest first, and
// the total the selection matched before --limit cut it, so a machine knows
// what the limit left out.
type listPayload struct {
	Total int       `json:"total"`
	Rows  []listRow `json:"rows"`
}

// listPayloadSchema declares `list`'s payload. An empty selection answers with
// an empty rows array rather than null, so a consumer never has to
// special-case "nothing has ever been deleted here".
var listPayloadSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"total": map[string]interface{}{"type": "integer"},
		"rows":  map[string]interface{}{"type": "array", "items": listRowSchema},
	},
	"required":             []interface{}{"total", "rows"},
	"additionalProperties": false,
}

// listDefaultLimit is how many entries a bare `list` shows. It is declared on
// the flag, so --help states it.
const listDefaultLimit = 50

func registerListCmd(app *strictcli.App) {
	app.Command("list", "Show the items held in the saferm archive, newest first", handleList,
		strictcli.WithEffect(strictcli.EffectReadOnly),
		strictcli.PayloadSchema(listPayloadSchema),
		strictcli.WithFlags(
			// Optional, not Default(""): the handler asked `!= ""` to find out
			// whether a filter had been supplied at all, which is an absence
			// sentinel. list is read_only so the mutating-default ban does not
			// reach it; the declaration changes because it was never a default.
			strictcli.StringFlag("path", "Filter results to original paths matching the given glob pattern (* spans directory separators, so /home/m/* reaches any depth); omitted, every path is listed", strictcli.Optional()),
			strictcli.BoolFlag("all", "Include items that have already been restored or purged", strictcli.Default(false)),
			strictcli.StringFlag("since", "List only entries deleted within this duration, in the syntax purge --older-than takes (e.g. 24h, 7d, 2w, 1m); omitted, entries of every age are listed", strictcli.Optional()),
			strictcli.IntFlag("limit", "Show only this many of the newest matching entries, and end with a line saying how many are hidden; 0 shows every matching entry", strictcli.Default(listDefaultLimit)),
		),
	)
}

func handleList(ctx *strictcli.Context, kwargs map[string]interface{}) strictcli.Outcome {
	pathGlob := optStr(kwargs["path"], "")
	includeAll := kwargs["all"].(bool)
	limit := kwargs["limit"].(int)
	// Absence is the two-result assertion, so `--since ""` is a supplied
	// value that parseDuration refuses rather than a silent "every age".
	sinceText, hasSince := kwargs["since"].(string)
	var since *time.Time
	if hasSince {
		dur, err := parseDuration(sinceText)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: --since: %s\n", err)
			return strictcli.Exit(ExitUsage)
		}
		from := time.Now().Add(-dur)
		since = &from
	}

	if limit < 0 {
		fmt.Fprintf(os.Stderr, "error: --limit must be 0 (every matching entry) or more, got %d\n", limit)
		return strictcli.Exit(ExitUsage)
	}

	// A malformed pattern is a usage error whatever the archive holds, so it
	// is refused before anything is opened or read.
	if pathGlob != "" {
		if err := validatePathPattern(pathGlob); err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid glob pattern %q: %s\n", pathGlob, err)
			return strictcli.Exit(ExitUsage)
		}
	}

	dbPath := kwargs["db_path"].(string)

	database, err := openArchiveDBIfPresent(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: opening database: %s\n", err)
		return strictcli.Exit(dbExit(err))
	}
	// No database file means nothing has ever been deleted on this machine,
	// which is a list of length zero, not a failure.
	if database == nil {
		ctx.Payload(listPayload{Total: 0, Rows: []listRow{}})
		emit(ctx, "No archived items found.\n")
		return strictcli.Exit(ExitSuccess)
	}
	defer database.Close()

	records, err := selectListRecords(database, pathGlob, includeAll, since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: querying database: %s\n", err)
		return strictcli.Exit(dbExit(err))
	}

	// The limit keeps the newest entries: records are newest first.
	total := len(records)
	if limit > 0 && total > limit {
		records = records[:limit]
	}

	// The payload is the set the table shows, whatever its size: an empty
	// selection is an empty array, never null.
	ctx.Payload(listPayload{Total: total, Rows: listRows(records)})

	if len(records) == 0 {
		emit(ctx, "No archived items found.\n")
		return strictcli.Exit(ExitSuccess)
	}

	// The whole table is built first and emitted once: in machine mode it rides
	// the envelope as a single diagnostic rather than one per row.
	var table strings.Builder
	fmt.Fprintf(&table, "%-6s %-40s %-10s %-16s %s\n", "ID", "Path", "Size", "Age", "Status")
	fmt.Fprintf(&table, "%-6s %-40s %-10s %-16s %s\n", "------", "----------------------------------------", "----------", "----------------", "--------")

	for _, rec := range records {
		path := rec.OriginalPath

		// Append type indicator for non-regular files.
		typeIndicator := kindIndicator(rec.Kind)

		if len(path)+len(typeIndicator) > 40 {
			maxPath := 40 - len(typeIndicator)
			path = "..." + path[len(path)-(maxPath-3):]
		}
		path += typeIndicator

		fmt.Fprintf(&table, "%-6d %-40s %-10s %-16s %s\n",
			rec.ID,
			path,
			humanSize(rec.Size),
			humanAge(rec.DeletedAt),
			listStatus(rec),
		)
	}
	// A limit that hid something says so, and says how to see the rest, as
	// the table's last line.
	if len(records) < total {
		fmt.Fprintf(&table, "showing %s of %s; pass --limit N, --since, or --path to see others\n",
			groupThousands(len(records)), groupThousands(total))
	}
	emit(ctx, "%s", table.String())

	return strictcli.Exit(ExitSuccess)
}

// groupThousands writes a count with a comma between each group of three
// digits.
func groupThousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// The three lifecycle words `list` shows in its Status column, spelled once and
// declared as the payload's own enum.
const (
	statusArchived = "archived"
	statusRestored = "restored"
	statusPurged   = "purged"
)

// listStatus is the lifecycle word `list` shows for a record, and the one the
// machine payload carries. One function, so the column and the payload can
// never disagree about what a row's state is.
func listStatus(rec *db.DeletionRecord) string {
	switch {
	case rec.PurgedAt != nil:
		return statusPurged
	case rec.RestoredAt != nil:
		return statusRestored
	}
	return statusArchived
}

// listRows renders the records as the machine payload carries them. It is built
// from the same slice the table is, after the same filtering, so the two
// answers can never describe different sets.
func listRows(records []*db.DeletionRecord) []listRow {
	rows := make([]listRow, 0, len(records))
	for _, rec := range records {
		rows = append(rows, listRow{
			ID:        rec.ID,
			UUID:      rec.UUID,
			Path:      rec.OriginalPath,
			Size:      rec.Size,
			Kind:      recordKind(rec),
			DeletedAt: rec.DeletedAt.Format(time.RFC3339),
			Status:    listStatus(rec),
		})
	}
	return rows
}

// kindIndicator is the marker the table appends to a path to say what kind of
// thing was archived there; a regular file carries none.
func kindIndicator(k archive.Kind) string {
	switch k {
	case archive.KindFile:
		return ""
	case archive.KindDirectory:
		return " [dir]"
	case archive.KindSymlink:
		return " [sym]"
	case archive.KindFIFO:
		return " [fifo]"
	case archive.KindSocket:
		return " [sock]"
	case archive.KindCharacterDevice:
		return " [chr]"
	case archive.KindBlockDevice:
		return " [blk]"
	}
	panic(fmt.Sprintf("kindIndicator: unknown archive kind %q", string(k)))
}
