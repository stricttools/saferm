package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/stricttools/saferm/internal/db"
	"github.com/smm-h/strictcli/go/strictcli"
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

// listPayloadSchema declares `list`'s payload: the rows, as an array. An empty
// archive answers with an empty array rather than null, so a consumer never has
// to special-case "nothing has ever been deleted here".
var listPayloadSchema = map[string]interface{}{
	"type": "array",
	"items": map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"id":         map[string]interface{}{"type": "integer"},
			"uuid":       map[string]interface{}{"type": "string"},
			"path":       map[string]interface{}{"type": "string"},
			"size":       map[string]interface{}{"type": "integer"},
			"kind":       map[string]interface{}{"type": "string", "enum": []interface{}{kindFile, kindDirectory, kindSymlink}},
			"deleted_at": map[string]interface{}{"type": "string"},
			"status":     map[string]interface{}{"type": "string", "enum": []interface{}{statusArchived, statusRestored, statusPurged}},
		},
		"required":             []interface{}{"id", "uuid", "path", "size", "kind", "deleted_at", "status"},
		"additionalProperties": false,
	},
}

func registerListCmd(app *strictcli.App) {
	app.Command("list", "Show all items currently held in the saferm archive", handleList,
		strictcli.WithEffect(strictcli.EffectReadOnly),
		strictcli.PayloadSchema(listPayloadSchema),
		strictcli.WithFlags(
			// Optional, not Default(""): the handler asked `!= ""` to find out
			// whether a filter had been supplied at all, which is an absence
			// sentinel. list is read_only so the mutating-default ban does not
			// reach it; the declaration changes because it was never a default.
			strictcli.StringFlag("path", "Filter results to original paths matching the given glob pattern (* spans directory separators, so /home/m/* reaches any depth); omitted, every path is listed", strictcli.Optional()),
			strictcli.BoolFlag("all", "Include items that have already been restored or purged", strictcli.Default(false)),
		),
	)
}

func handleList(ctx *strictcli.Context, kwargs map[string]interface{}) strictcli.Outcome {
	pathGlob := optStr(kwargs["path"], "")
	includeAll := kwargs["all"].(bool)

	dbPath := kwargs["db_path"].(string)

	database, err := openArchiveDBIfPresent(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: opening database: %s\n", err)
		return strictcli.Exit(dbExit(err))
	}
	// No database file means nothing has ever been deleted on this machine,
	// which is a list of length zero, not a failure.
	if database == nil {
		ctx.Payload([]listRow{})
		emit(ctx, "No archived items found.\n")
		return strictcli.Exit(ExitSuccess)
	}
	defer database.Close()

	records, err := database.QueryAll(includeAll)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: querying database: %s\n", err)
		return strictcli.Exit(dbExit(err))
	}

	// Filter by glob if specified
	if pathGlob != "" {
		var filtered []*db.DeletionRecord
		for _, rec := range records {
			matched, err := matchArchivePath(pathGlob, rec.OriginalPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: invalid glob pattern %q: %s\n", pathGlob, err)
				return strictcli.Exit(ExitUsage)
			}
			if matched {
				filtered = append(filtered, rec)
			}
		}
		records = filtered
	}

	// The payload is the filtered set, whatever its size: an empty selection is
	// an empty array, never null.
	ctx.Payload(listRows(records))

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
		typeIndicator := ""
		if rec.SymlinkTarget != nil {
			typeIndicator = " [sym]"
		} else if rec.IsDirectory {
			typeIndicator = " [dir]"
		}

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
	emit(ctx, "%s", table.String())

	return strictcli.Exit(ExitSuccess)
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
