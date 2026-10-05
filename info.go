package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/stricttools/saferm/internal/db"
	"github.com/stricttools/saferm/internal/meta"
	"github.com/smm-h/strictcli/go/strictcli"
)

// The closed set of words the machine payload's `status` takes. They are the
// same four states [recordStatus] describes in prose, named rather than
// narrated: a consumer branches on a word, and "the archived copy is gone
// though nothing restored or purged it" is a sentence.
//
// statusRestoredThenPurged exists because both lifecycle columns can be set at
// once -- a record restored and later purged -- and collapsing that into either
// half would hide the other.
const (
	statusRestorable         = "restorable"
	statusRestoredThenPurged = "restored-then-purged"
	statusEntryMissing       = "entry-missing"
)

// infoPayload is `info`'s machine payload: the record, including the three
// things the printed page states in prose or not at all -- the derived status,
// the group identifier, and the origin.
//
// Every nullable column is a pointer and none of them is omitted when absent:
// the key is always there and null is the answer, because the difference
// between "no tool claimed this deletion" and "a tool named the empty string"
// is the whole content of the origin columns.
//
// The captured metadata blob is deliberately NOT here. It is an open-ended
// document -- every environment variable, the whole resolved ancestry chain --
// and declaring it in the closed subset would mean either lying about its shape
// or freezing it. `info`'s printed page remains the way to read it.
type infoPayload struct {
	ID            int64   `json:"id"`
	UUID          string  `json:"uuid"`
	OriginalPath  string  `json:"original_path"`
	OriginalName  string  `json:"original_name"`
	Size          int64   `json:"size"`
	Hash          string  `json:"hash"`
	Kind          string  `json:"kind"`
	SymlinkTarget *string `json:"symlink_target"`
	DeletedAt     string  `json:"deleted_at"`
	Status        string  `json:"status"`
	Description   string  `json:"description"`
	Command       string  `json:"command"`
	RestoredAt    *string `json:"restored_at"`
	RestoredTo    *string `json:"restored_to"`
	PurgedAt      *string `json:"purged_at"`
	OriginName    *string `json:"origin_name"`
	OriginVersion *string `json:"origin_version"`
	GroupID       *string `json:"group_id"`
}

// infoPayloadSchema declares the payload above over the framework's closed
// subset. A nullable member is a two-element type list, which is how the subset
// spells "a string or nothing".
var infoPayloadSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"id":             map[string]interface{}{"type": "integer"},
		"uuid":           map[string]interface{}{"type": "string"},
		"original_path":  map[string]interface{}{"type": "string"},
		"original_name":  map[string]interface{}{"type": "string"},
		"size":           map[string]interface{}{"type": "integer"},
		"hash":           map[string]interface{}{"type": "string"},
		"kind":           map[string]interface{}{"type": "string", "enum": kindEnum()},
		"symlink_target": map[string]interface{}{"type": []interface{}{"string", "null"}},
		"deleted_at":     map[string]interface{}{"type": "string"},
		"status": map[string]interface{}{"type": "string", "enum": []interface{}{
			statusRestorable, statusRestored, statusPurged, statusRestoredThenPurged, statusEntryMissing,
		}},
		"description":    map[string]interface{}{"type": "string"},
		"command":        map[string]interface{}{"type": "string"},
		"restored_at":    map[string]interface{}{"type": []interface{}{"string", "null"}},
		"restored_to":    map[string]interface{}{"type": []interface{}{"string", "null"}},
		"purged_at":      map[string]interface{}{"type": []interface{}{"string", "null"}},
		"origin_name":    map[string]interface{}{"type": []interface{}{"string", "null"}},
		"origin_version": map[string]interface{}{"type": []interface{}{"string", "null"}},
		"group_id":       map[string]interface{}{"type": []interface{}{"string", "null"}},
	},
	"required": []interface{}{
		"id", "uuid", "original_path", "original_name", "size", "hash", "kind", "symlink_target",
		"deleted_at", "status", "description", "command", "restored_at", "restored_to", "purged_at",
		"origin_name", "origin_version", "group_id",
	},
	"additionalProperties": false,
}

func registerInfoCmd(app *strictcli.App) {
	app.Command("info", "Display full metadata and context for an archived deletion", handleInfo,
		strictcli.WithEffect(strictcli.EffectReadOnly),
		strictcli.PayloadSchema(infoPayloadSchema),
		strictcli.WithArgs(
			strictcli.NewArg("target", "Record UUID or numeric database ID of the archived item to inspect ("+identifierOrderHelp+")",
				strictcli.ArgRequired()),
		),
	)
}

// recordStatus states, in one line, whether the archived content is still
// there to restore.
//
// The two lifecycle columns answer it wherever they are set: a restore moves
// the blob out and stamps restored_at, a purge destroys it and stamps
// purged_at. Both can be set -- a record restored first and purged afterwards
// -- and then both are reported, because either alone would hide half of what
// happened.
//
// Where neither is set the columns are no longer the whole answer, and the
// archive directory is consulted for the one remaining case. An archival that
// meets a changed source inside its window commits its row and DISCARDS its
// entry on purpose -- a file written through while its archive entry was a
// hard link to it, a tree that grew during the insert -- so a row that names
// nothing is a state saferm produces itself, not a corruption. Answering
// "restorable" for it is the one answer that sends a caller into an undelete
// that cannot work.
func recordStatus(rec *db.DeletionRecord, archiveDir string) string {
	var parts []string
	if rec.RestoredAt != nil {
		parts = append(parts, "restored at "+rec.RestoredAt.Format(time.RFC3339))
	}
	if rec.PurgedAt != nil {
		parts = append(parts, "purged at "+rec.PurgedAt.Format(time.RFC3339))
	}
	if len(parts) == 0 {
		if archiveEntryIsGone(archiveDir, rec) {
			return "the archived copy is gone though nothing restored or purged it -- this row names nothing; purge it to clear it"
		}
		return "restorable"
	}
	return strings.Join(parts, ", ")
}

// recordMachineStatus is [recordStatus]'s answer as one of the closed status
// words. It reads exactly the same three inputs -- the two lifecycle columns
// and, where neither is set, one stat of the record's own archive entry -- so
// the printed line and the payload can only ever say the same thing in two
// vocabularies.
func recordMachineStatus(rec *db.DeletionRecord, archiveDir string) string {
	switch {
	case rec.RestoredAt != nil && rec.PurgedAt != nil:
		return statusRestoredThenPurged
	case rec.RestoredAt != nil:
		return statusRestored
	case rec.PurgedAt != nil:
		return statusPurged
	case archiveEntryIsGone(archiveDir, rec):
		return statusEntryMissing
	}
	return statusRestorable
}

// formatTime renders a nullable timestamp column the way the payload carries
// it: an RFC3339 string, or null.
func formatTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}

// infoPayloadOf renders a record as the machine payload carries it.
func infoPayloadOf(rec *db.DeletionRecord, archiveDir string) infoPayload {
	return infoPayload{
		ID:            rec.ID,
		UUID:          rec.UUID,
		OriginalPath:  rec.OriginalPath,
		OriginalName:  rec.OriginalName,
		Size:          rec.Size,
		Hash:          rec.Hash,
		Kind:          recordKind(rec),
		SymlinkTarget: rec.SymlinkTarget,
		DeletedAt:     rec.DeletedAt.Format(time.RFC3339),
		Status:        recordMachineStatus(rec, archiveDir),
		Description:   rec.Description,
		Command:       rec.Command,
		RestoredAt:    formatTime(rec.RestoredAt),
		RestoredTo:    rec.RestoredTo,
		PurgedAt:      formatTime(rec.PurgedAt),
		OriginName:    rec.OriginName,
		OriginVersion: rec.OriginVersion,
		GroupID:       rec.GroupID,
	}
}

func handleInfo(ctx *strictcli.Context, kwargs map[string]interface{}) strictcli.Outcome {
	target := kwargs["target"].(string)

	// Shape before archive: a path is refused whether or not this machine has
	// ever deleted anything.
	if code := requireIdentifierShape(target); code != ExitSuccess {
		return strictcli.Exit(code)
	}

	archiveDir := kwargs["archive_dir"].(string)
	dbPath := kwargs["db_path"].(string)

	database, err := openArchiveDBIfPresent(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: opening database: %s\n", err)
		return strictcli.Exit(dbExit(err))
	}
	// No database file means nothing has ever been deleted on this machine, so
	// no identifier resolves -- the same answer as one that was never issued.
	if database == nil {
		reportNoSuchRecord(target)
		return strictcli.Exit(ExitFileNotFound)
	}
	defer database.Close()

	// Identifier only: `info` inspects a record, and a path can name several.
	rec, code := resolveRecord(database, target, false)
	if code != ExitSuccess {
		return strictcli.Exit(code)
	}

	fileType := recordKind(rec)

	ctx.Payload(infoPayloadOf(rec, archiveDir))

	// The whole record is built first and emitted once, so machine mode carries
	// it as a single diagnostic rather than one per field.
	var out strings.Builder
	fmt.Fprintf(&out, "ID:            %d\n", rec.ID)
	fmt.Fprintf(&out, "UUID:          %s\n", rec.UUID)
	fmt.Fprintf(&out, "Original Path: %s\n", rec.OriginalPath)
	fmt.Fprintf(&out, "Original Name: %s\n", rec.OriginalName)
	fmt.Fprintf(&out, "Size:          %s (%d bytes)\n", humanSize(rec.Size), rec.Size)
	fmt.Fprintf(&out, "Hash:          %s\n", rec.Hash)
	fmt.Fprintf(&out, "Type:          %s\n", fileType)
	if rec.SymlinkTarget != nil {
		fmt.Fprintf(&out, "Target:        %s\n", *rec.SymlinkTarget)
	}
	fmt.Fprintf(&out, "Deleted At:    %s\n", rec.DeletedAt.Format(time.RFC3339))
	fmt.Fprintf(&out, "Status:        %s\n", recordStatus(rec, archiveDir))
	fmt.Fprintf(&out, "Description:   %s\n", rec.Description)
	if rec.Command != "" {
		fmt.Fprintf(&out, "Command:       %s\n", rec.Command)
	}

	if rec.RestoredAt != nil {
		fmt.Fprintf(&out, "Restored At:   %s\n", rec.RestoredAt.Format(time.RFC3339))
	}
	if rec.RestoredTo != nil {
		fmt.Fprintf(&out, "Restored To:   %s\n", *rec.RestoredTo)
	}
	if rec.PurgedAt != nil {
		fmt.Fprintf(&out, "Purged At:     %s\n", rec.PurgedAt.Format(time.RFC3339))
	}

	// Parse and display metadata
	if rec.Metadata != "" {
		var m meta.Metadata
		if err := json.Unmarshal([]byte(rec.Metadata), &m); err == nil {
			fmt.Fprintln(&out, "\nMetadata:")
			if m.GitBranch != "" {
				fmt.Fprintf(&out, "  Git Branch:  %s\n", m.GitBranch)
			}
			if m.GitHEAD != "" {
				fmt.Fprintf(&out, "  Git HEAD:    %s\n", m.GitHEAD)
			}
			if m.GitRoot != "" {
				fmt.Fprintf(&out, "  Git Root:    %s\n", m.GitRoot)
			}
			if m.PPID != 0 {
				fmt.Fprintf(&out, "  Parent PID:  %d\n", m.PPID)
			}
			if m.ParentCmd != "" {
				fmt.Fprintf(&out, "  Parent Cmd:  %s\n", m.ParentCmd)
			}
			if len(m.Custom) > 0 {
				fmt.Fprintln(&out, "  Custom:")
				for k, v := range m.Custom {
					fmt.Fprintf(&out, "    %s = %s\n", k, v)
				}
			}
			if len(m.Env) > 0 {
				fmt.Fprintf(&out, "  Environment: %d variables captured\n", len(m.Env))
			}
		}
	}
	emit(ctx, "%s", out.String())

	return strictcli.Exit(ExitSuccess)
}
