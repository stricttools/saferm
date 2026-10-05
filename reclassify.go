package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stricttools/strictcli/go/strictcli"
	"github.com/stricttools/saferm/internal/archive"
	"github.com/stricttools/saferm/internal/db"
)

// reclassifiedRecord is one record `reclassify-records` changed, or under
// --dry-run would change.
type reclassifiedRecord struct {
	ID   int64  `json:"id"`
	UUID string `json:"uuid"`
	Path string `json:"path"`
	From string `json:"from"`
	To   string `json:"to"`
}

// reclassifyPayload is the command's machine payload: every record it
// reclassified, oldest first. Under --dry-run it is every record it would.
type reclassifyPayload struct {
	Reclassified []reclassifiedRecord `json:"reclassified"`
}

var reclassifyPayloadSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"reclassified": map[string]interface{}{
			"type": "array",
			"items": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id":   map[string]interface{}{"type": "integer"},
					"uuid": map[string]interface{}{"type": "string"},
					"path": map[string]interface{}{"type": "string"},
					"from": map[string]interface{}{"type": "string", "enum": kindEnum()},
					"to":   map[string]interface{}{"type": "string", "enum": kindEnum()},
				},
				"required":             []interface{}{"id", "uuid", "path", "from", "to"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []interface{}{"reclassified"},
	"additionalProperties": false,
}

func registerReclassifyCmd(app *strictcli.App) {
	app.Command("reclassify-records",
		"Give every live record whose archive entry contradicts its kind the kind the entry really holds. "+
			"saferm versions that did not recognize symlinks or special files recorded them as files; "+
			"undelete refuses those records and info reports them as entry-corrupt until this runs. Preview it with --dry-run first",
		handleReclassify,
		strictcli.WithEffect(strictcli.EffectMutating),
		strictcli.PayloadSchema(reclassifyPayloadSchema),
	)
}

func handleReclassify(ctx *strictcli.Context, kwargs map[string]interface{}) strictcli.Outcome {
	archiveDir := kwargs["archive_dir"].(string)
	dbPath := kwargs["db_path"].(string)
	fx := ctx.Effects()
	dry := ctx.DryRun()

	payload := reclassifyPayload{Reclassified: []reclassifiedRecord{}}
	defer func() { ctx.Payload(payload) }()

	database, err := openArchiveDBIfPresent(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: opening database: %s\n", err)
		return strictcli.Exit(dbExit(err))
	}
	if database == nil {
		say(ctx, "No archive at %s; there is nothing to reclassify.\n", filepath.Dir(dbPath))
		return strictcli.Exit(ExitSuccess)
	}
	defer database.Close()

	records, err := database.QueryAll(false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: reading the archive's records: %s\n", err)
		return strictcli.Exit(dbExit(err))
	}

	// Every record is read before any is changed: a record no reclassification
	// resolves refuses the whole run, so the archive is never left half done
	// for a reason the run could have seen at the start.
	type planned struct {
		rec *db.DeletionRecord
		r   *archive.Reclassification
	}
	var plans []planned
	var refusals []string
	for i := len(records) - 1; i >= 0; i-- {
		rec := records[i]
		target := ""
		if rec.SymlinkTarget != nil {
			target = *rec.SymlinkTarget
		}
		r, err := archive.PlanReclassification(archiveDir, rec.UUID, rec.Kind, target)
		if err != nil {
			refusals = append(refusals, fmt.Sprintf("[%d] %s %s: %s", rec.ID, rec.UUID, rec.OriginalPath, err))
			continue
		}
		if r != nil {
			plans = append(plans, planned{rec, r})
		}
	}
	if len(refusals) > 0 {
		fmt.Fprintf(os.Stderr, "error: %d record(s) contradict their archive entries in a way no reclassification resolves, so nothing was changed:\n  %s\n",
			len(refusals), strings.Join(refusals, "\n  "))
		return strictcli.Exit(ExitArchive)
	}
	if len(plans) == 0 {
		say(ctx, "No live record contradicts its archive entry; there is nothing to reclassify.\n")
		return strictcli.Exit(ExitSuccess)
	}

	var listing strings.Builder
	verb := "reclassified"
	if dry {
		verb = "would reclassify"
	}
	for _, p := range plans {
		fmt.Fprintf(&listing, "%s: [%d] %s %s: recorded as a %s, the entry is a %s%s\n",
			verb, p.rec.ID, p.rec.UUID, p.rec.OriginalPath, p.r.From, p.r.To, describeSymlinkTarget(p.r))
		if err := reclassifyOne(fx, dry, database, p.rec, p.r); err != nil {
			emit(ctx, "%s", listing.String())
			fmt.Fprintf(os.Stderr, "error: reclassifying [%d] %s: %s\n", p.rec.ID, p.rec.UUID, err)
			if errors.Is(err, errDatabase) {
				return strictcli.Exit(dbExit(err))
			}
			return strictcli.Exit(ExitArchive)
		}
		payload.Reclassified = append(payload.Reclassified, reclassifiedRecord{
			ID: p.rec.ID, UUID: p.rec.UUID, Path: p.rec.OriginalPath, From: string(p.r.From), To: string(p.r.To),
		})
	}
	emit(ctx, "%s", listing.String())
	if !dry {
		say(ctx, "%d record(s) reclassified\n", len(plans))
	}
	return strictcli.Exit(ExitSuccess)
}

// errDatabase marks a reclassification that failed in the database rather
// than in the archive directory.
var errDatabase = errors.New("updating the record")

// describeSymlinkTarget names what a reclassified symlink points at.
func describeSymlinkTarget(r *archive.Reclassification) string {
	if r.To != archive.KindSymlink {
		return ""
	}
	return " to " + r.SymlinkTarget
}

// reclassifyOne rewrites one record's entry into its real kind's form and
// updates the record to match, in the order that keeps a failure safe: the
// new entry is written first, the record changed next (the new entry is taken
// back if that fails), and the old entry removed last.
//
// Under --dry-run the two archive-directory acts are declared on the effects
// handle and nothing is performed; the record update has no primitive there and
// is skipped, as purge's is.
func reclassifyOne(fx *strictcli.Effects, dry bool, database *db.DB, rec *db.DeletionRecord, r *archive.Reclassification) error {
	if dry {
		if _, err := fx.Write(r.NewEntry, r.Content, strictcli.Resource("saferm-entry:"+r.UUID)); err != nil {
			return err
		}
		_, err := fx.Remove(r.OldEntry, strictcli.Resource("saferm-entry:"+r.UUID))
		return err
	}

	if err := r.WriteEntry(); err != nil {
		return fmt.Errorf("writing %s: %w", r.NewEntry, err)
	}
	var target *string
	if r.To == archive.KindSymlink {
		target = &r.SymlinkTarget
	}
	if err := database.Reclassify(rec.ID, r.From, r.To, target, r.Hash, 0); err != nil {
		if derr := r.DiscardEntry(); derr != nil {
			return fmt.Errorf("%w: %w; and the new entry %s could not be taken back: %v", errDatabase, err, r.NewEntry, derr)
		}
		return fmt.Errorf("%w: %w; the record and its entry were left as they were", errDatabase, err)
	}
	if _, err := fx.Remove(r.OldEntry, strictcli.Resource("saferm-entry:"+r.UUID)); err != nil {
		return fmt.Errorf("the record is reclassified and its new entry %s written, but the old entry %s could not be removed: %w", r.NewEntry, r.OldEntry, err)
	}
	return nil
}
