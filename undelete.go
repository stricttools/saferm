package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stricttools/saferm/internal/archive"
	gitutil "github.com/stricttools/saferm/internal/git"
	"github.com/smm-h/strictcli/go/strictcli"
)

// The two answers `undelete` accepts for a destination that is already
// occupied, spelled once.
const (
	onConflictOverwrite = "overwrite"
	onConflictAbort     = "abort"
)

// undeletePayload is `undelete`'s machine payload: which record was restored,
// and where the content actually went.
//
// `restored_to` is the whole point of it. The human line says "Restored <from>
// to <to>" only when the two differ, and a consumer that named an alternate
// destination should not have to parse that sentence to learn where its content
// is. `overwrote` says whether something was standing at the destination and
// was destroyed, which is the one fact nothing on the filesystem records
// afterwards.
//
// It is supplied in both modes: under --dry-run it names where the content
// WOULD go, and the envelope's own dry_run flag is what tells the two apart.
type undeletePayload struct {
	ID           int64  `json:"id"`
	UUID         string `json:"uuid"`
	OriginalPath string `json:"original_path"`
	RestoredTo   string `json:"restored_to"`
	Kind         string `json:"kind"`
	Overwrote    bool   `json:"overwrote"`
}

// undeletePayloadSchema declares the payload above over the framework's closed
// subset. `kind` is an enum because the three archived shapes are the three
// saferm has and a fourth would be a new feature, not a new value.
var undeletePayloadSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"id":            map[string]interface{}{"type": "integer"},
		"uuid":          map[string]interface{}{"type": "string"},
		"original_path": map[string]interface{}{"type": "string"},
		"restored_to":   map[string]interface{}{"type": "string"},
		"kind":          map[string]interface{}{"type": "string", "enum": []interface{}{kindFile, kindDirectory, kindSymlink}},
		"overwrote":     map[string]interface{}{"type": "boolean"},
	},
	"required":             []interface{}{"id", "uuid", "original_path", "restored_to", "kind", "overwrote"},
	"additionalProperties": false,
}

func registerUndeleteCmd(app *strictcli.App) {
	app.Command("undelete", "Restore a previously archived file back to its original path", handleUndelete,
		strictcli.WithEffect(strictcli.EffectMutating),
		strictcli.PayloadSchema(undeletePayloadSchema),
		strictcli.WithGrants(strictcli.Grant{
			Name:   "git-index",
			Reason: "a restored file is staged so the working tree and the index agree again",
			Kind:   strictcli.ProcMutate,
		}),
		strictcli.WithFlags(
			// No default, and required exactly when the situation arises. A
			// destination that is already occupied has two defensible answers
			// -- replace what is there, or refuse and change nothing -- and they
			// suit opposite callers, so saferm refuses to choose one silently.
			// There is deliberately no third mode that keeps both copies: a
			// restore consumes the archived copy, and parking a second one
			// beside the destination is the workflow saferm exists to prevent.
			strictcli.StringFlag("on-conflict",
				"What to do when something already exists at the restoration destination. Required only when the destination is occupied; an absent destination, or the emptied original directory of an archived tree, needs no answer. There is no default",
				strictcli.Optional(),
				strictcli.Choices(
					strictcli.Ch(onConflictOverwrite, "check the archived copy against the record, then replace what is standing at the destination"),
					strictcli.Ch(onConflictAbort, "refuse the restore and change nothing at the destination"),
				)),
			// The delete side's switch, mirrored: a caller that hands paths to
			// saferm programmatically may want no index side effects at all,
			// and it must be able to say so on both halves of the round trip.
			strictcli.BoolFlag("update-git-index", "Run git add to stage the restored path in the git index; omitted, the index is updated", strictcli.Optional()),
			strictcli.StringFlag("destination",
				"Restore to this path instead of the record's original one; omitted, the record's own path is used. Where the content actually went is written to the record, so `info` names it afterwards",
				strictcli.Optional()),
		),
		strictcli.WithArgs(
			strictcli.NewArg("target", "Record UUID, numeric database ID, or original file path of the item to restore ("+identifierOrderHelp+", anything else is a path)",
				strictcli.ArgRequired()),
		),
	)
}

func handleUndelete(ctx *strictcli.Context, kwargs map[string]interface{}) strictcli.Outcome {
	// Absent unless the caller answered: the flag is optional at parse time and
	// required by the destination's state, which nothing can know before the
	// record is resolved.
	onConflict := ""
	if v, ok := kwargs["on_conflict"].(string); ok {
		onConflict = v
	}
	updateGitIndex := optBool(kwargs["update_git_index"], true)
	destination := optStr(kwargs["destination"], "")
	target := kwargs["target"].(string)

	archiveDir := kwargs["archive_dir"].(string)
	dbPath := kwargs["db_path"].(string)

	if err := ensureDirectories(ctx.Effects(), filepath.Dir(archiveDir), archiveDir, dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: creating directories: %s\n", err)
		return strictcli.Exit(ExitGeneral)
	}

	database, err := openArchiveDB(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: opening database: %s\n", err)
		return strictcli.Exit(dbExit(err))
	}
	// nil means the dry run found no archive at all, so there is nothing to
	// restore from and no record to name.
	if database == nil {
		fmt.Fprintf(os.Stderr, "error: no archived record found for %q\n", target)
		return strictcli.Exit(ExitFileNotFound)
	}
	defer database.Close()

	// A uuid, a numeric id or an original path -- read in that fixed order by
	// the one resolver every identifier-taking verb shares.
	rec, code := resolveRecord(database, target, true)
	if code != ExitSuccess {
		return strictcli.Exit(code)
	}

	// Guard: cannot restore an already-restored item. A restore consumes the
	// archived entry -- it is moved out of the archive, not copied -- so a
	// second restore of the same record has nothing to read. Reported here, in
	// the record's own vocabulary, because both routes below answer badly: with
	// the destination gone the archive layer reports a raw failed stat of a
	// UUID, and with the destination still there the conflict path advertises
	// an overwrite, which could not have helped.
	if rec.RestoredAt != nil {
		restoredTo := rec.OriginalPath
		if rec.RestoredTo != nil {
			restoredTo = *rec.RestoredTo
		}
		fmt.Fprintf(os.Stderr, "error: record %d was already restored at %s to %s; the archived copy was consumed by that restore\n",
			rec.ID, rec.RestoredAt.Format(time.RFC3339), restoredTo)
		return strictcli.Exit(ExitArchive)
	}

	// Guard: cannot restore a purged item (archive content is gone).
	if rec.PurgedAt != nil {
		fmt.Fprintf(os.Stderr, "error: content for %d was purged on %s; metadata is preserved but the file cannot be restored\n",
			rec.ID, rec.PurgedAt.Format(time.RFC3339))
		return strictcli.Exit(ExitArchive)
	}

	// Where the content is going: the record's own path unless the caller named
	// somewhere else. It is resolved to an absolute path here, because it is
	// what gets written to the record -- a relative path in the row would mean
	// nothing to anyone reading it later from another directory.
	dest := rec.OriginalPath
	if destination != "" {
		abs, err := filepath.Abs(destination)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resolving destination %q: %s\n", destination, err)
			return strictcli.Exit(ExitUsage)
		}
		dest = abs
	}

	symlinkTarget := ""
	if rec.SymlinkTarget != nil {
		symlinkTarget = *rec.SymlinkTarget
	}

	plan := archive.NewRestorePlan(rec.UUID, archiveDir, dest, rec.IsDirectory, symlinkTarget)

	// A stat, not a read, and it runs in every mode: an entry that is not there
	// is worth saying so before anything else is decided, rather than surfacing
	// as a failed rename of a UUID halfway through.
	if err := archive.EntryPresent(plan); err != nil {
		fmt.Fprintf(os.Stderr, "error: restoring record %d: %s\n", rec.ID, err)
		return strictcli.Exit(ExitArchive)
	}

	occupied, err := destinationOccupied(dest, rec.IsDirectory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: reading the destination %s: %s\n", dest, err)
		return strictcli.Exit(ExitGeneral)
	}
	overwrite := false
	if occupied {
		switch onConflict {
		case onConflictOverwrite:
			overwrite = true
		case onConflictAbort:
			fmt.Fprintf(os.Stderr, "error: %s already exists and --on-conflict %s refused the restore; nothing was touched and the archived copy was kept\n",
				dest, onConflictAbort)
			return strictcli.Exit(ExitConflict)
		default:
			fmt.Fprintf(os.Stderr, "error: %s already exists; pass --on-conflict %s to replace it or --on-conflict %s to refuse\n",
				dest, onConflictOverwrite, onConflictAbort)
			return strictcli.Exit(ExitUsage)
		}
	}

	// Verification is proportional: it runs when, and only when, the restore is
	// about to destroy something. An overwrite reads the archived copy through
	// once BEFORE the destination is touched, because the alternative -- what
	// this used to do -- is to remove the destination and only then discover
	// that the archive holds nothing worth having. A restore into an empty or
	// absent destination gets no verify pass at all: a corrupt copy simply
	// fails the restore, which costs nothing and keeps the copy.
	if overwrite {
		if err := archive.VerifyEntry(plan, rec.Hash); err != nil {
			fmt.Fprintf(os.Stderr, "error: refusing to overwrite %s: %s; the destination was not touched and the archived copy was kept\n", dest, err)
			return strictcli.Exit(ExitArchive)
		}
	}

	if err := runRestore(ctx, plan, overwrite); err != nil {
		fmt.Fprintf(os.Stderr, "error: restoring %s: %s; the archived copy was kept, so record %d is still restorable\n",
			dest, err, rec.ID)
		return strictcli.Exit(ExitArchive)
	}

	// Supplied here, once, for both modes: the restore has happened (or, in dry
	// mode, has been recorded), and everything the payload names is settled.
	ctx.Payload(undeletePayload{
		ID:           rec.ID,
		UUID:         rec.UUID,
		OriginalPath: rec.OriginalPath,
		RestoredTo:   dest,
		Kind:         recordKind(rec),
		Overwrote:    overwrite,
	})

	if ctx.DryRun() {
		say(ctx, "Would restore %s\n", describeDestination(rec.OriginalPath, dest))
		return strictcli.Exit(ExitSuccess)
	}

	if err := database.MarkRestored(rec.ID, dest); err != nil {
		fmt.Fprintf(os.Stderr, "error: updating database: %s\n", err)
		return strictcli.Exit(dbExit(err))
	}

	// Stage the restored file in git if inside a git repo.
	destDir := filepath.Dir(dest)
	if updateGitIndex && gitutil.IsInGitRepo(destDir) {
		if err := gitutil.GitAdd(dest); err != nil {
			fmt.Fprintf(os.Stderr, "warning: git add failed for %s: %s\n", dest, err)
		}
	}

	say(ctx, "Restored %s\n", describeDestination(rec.OriginalPath, dest))
	return strictcli.Exit(ExitSuccess)
}

// describeDestination names where the content went, and where it came from
// when those are not the same place: a restore to an alternate destination that
// only printed the destination would read as if the record had always been
// there.
func describeDestination(originalPath string, dest string) string {
	if dest == originalPath {
		return dest
	}
	return fmt.Sprintf("%s to %s", originalPath, dest)
}

// destinationOccupied reports whether something is standing where the restore
// wants to go, which is what makes the conflict mode required.
//
// The one exception is the empty-destination rule: an EMPTY directory where a
// tree was archived is that tree's own place, emptied, and extracting into it
// replaces nothing. Requiring an answer there would make the commonest
// directory restore -- the tree is gone, its directory is not -- need a flag to
// say "replace nothing".
//
// The rule is for trees only. An empty directory standing where a FILE was
// archived is still occupied: a file cannot be renamed over a directory, and
// removing that directory is a decision the caller has to state.
func destinationOccupied(dest string, isDirectory bool) (bool, error) {
	info, err := os.Lstat(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !isDirectory || !info.IsDir() {
		return true, nil
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// restoreStep is one mutation a restore performs, described once for both
// modes.
//
// The effects handle's closed method set can perform some of a restore itself
// -- removing what is at the destination, making the parent directory, moving a
// file out of the archive, dropping the consumed entry -- and can only DESCRIBE
// the rest, because recreating a symlink and extracting a tar+zstd tree have no
// primitive on the handle. Both kinds of step live in one list, built once from
// one plan, so a change to what a restore does changes what a preview says by
// construction. The real path used to do all of it behind the handle's back,
// with only the dry branch minting anything, which is exactly how a preview
// drifts from the thing it previews.
type restoreStep struct {
	// seam is the effects-handle call for this step. When act is nil the handle
	// performs the step itself, so this runs in both modes; when act is
	// non-nil the handle can only describe the step, so this runs in dry mode
	// only and act performs it for real.
	seam func(*strictcli.Effects) error
	act  func() error
}

// runRestore walks the step list a plan implies: recording it in dry mode,
// performing it otherwise.
func runRestore(ctx *strictcli.Context, p *archive.RestorePlan, overwrite bool) error {
	fx := ctx.Effects()
	dry := ctx.DryRun()
	for _, step := range restoreSteps(p, overwrite) {
		if step.act == nil || dry {
			if err := step.seam(fx); err != nil {
				return err
			}
		}
		if step.act != nil && !dry {
			if err := step.act(); err != nil {
				return err
			}
		}
	}
	return nil
}

// restoreSteps is the whole of what a restore does, in the order it does it.
//
// The ordering carries one invariant: THE ARCHIVED COPY IS CONSUMED LAST. A
// file's move out of the archive is itself the consumption and cannot fail
// halfway; every other kind writes the destination first and drops the entry
// only once that has worked. So any failure -- a refused symlink, a truncated
// tar, a copy that ran out of space -- leaves the entry where it is and the
// restore can simply be run again.
func restoreSteps(p *archive.RestorePlan, overwrite bool) []restoreStep {
	var steps []restoreStep

	if overwrite {
		steps = append(steps, restoreStep{seam: func(fx *strictcli.Effects) error {
			_, err := fx.Remove(p.Dest, strictcli.Resource("path:"+p.Dest))
			return err
		}})
	}

	parent := filepath.Dir(p.Dest)
	steps = append(steps, restoreStep{seam: func(fx *strictcli.Effects) error {
		_, err := fx.Mkdir(parent, strictcli.Resource("path:"+parent))
		return err
	}})

	switch p.Kind {
	case archive.KindFile:
		// The move IS the consumption: a rename either happened or did not, and
		// the cross-device fallback copies before it removes.
		steps = append(steps, restoreStep{seam: func(fx *strictcli.Effects) error {
			err := renameOut(fx, p.Entry, p.Dest)
			if err != nil && archive.IsCrossDeviceError(err) {
				return archive.CopyOut(p.Entry, p.Dest)
			}
			return err
		}})

	case archive.KindSymlink:
		// A symlink's entry IS its target path written out, which is what the
		// preview says; the real act is a symlink call the handle has no
		// primitive for.
		steps = append(steps,
			restoreStep{
				seam: func(fx *strictcli.Effects) error {
					_, err := fx.Write(p.Dest, []byte(p.SymlinkTarget), strictcli.Resource("path:"+p.Dest))
					return err
				},
				act: func() error { return archive.RestoreSymlink(p) },
			},
			consumeEntryStep(p),
		)

	case archive.KindDirectory:
		// The tree appears at the destination; its size is not knowable before
		// the extraction runs, so the write is declared with no content.
		steps = append(steps,
			restoreStep{
				seam: func(fx *strictcli.Effects) error {
					_, err := fx.Write(p.Dest, []byte(nil), strictcli.Resource("path:"+p.Dest))
					return err
				},
				act: func() error { return extractTree(p) },
			},
			consumeEntryStep(p),
		)
	}

	return steps
}

// renameOut moves a file's archive entry to the destination through the effects
// handle, and is the whole of what the handle does for that step.
//
// It is indirected for the same reason the archive package indirects os.Link:
// the branch that follows it -- the cross-device fallback -- fires only when
// the archive and the destination are on different filesystems, which no test
// can arrange inside a temporary directory. A test replaces this to make the
// rename fail with EXDEV and drives the fallback that way.
var renameOut = func(fx *strictcli.Effects, entry, dest string) error {
	_, err := fx.Rename(entry, dest, strictcli.Resource("path:"+dest))
	return err
}

// consumeEntryStep drops the archived copy once the destination holds it. It is
// always the last step of the kinds that do not consume the entry by moving it.
func consumeEntryStep(p *archive.RestorePlan) restoreStep {
	return restoreStep{seam: func(fx *strictcli.Effects) error {
		_, err := fx.Remove(p.Entry, strictcli.Resource("saferm-entry:"+p.UUID))
		return err
	}}
}

// extractTree extracts a directory's archive entry and undoes itself if the
// extraction fails partway.
func extractTree(p *archive.RestorePlan) error {
	created, err := archive.ExtractTree(p)
	if err == nil {
		return nil
	}
	if len(created) == 0 {
		return err
	}
	return &partialExtraction{err: err, extracted: created, stuck: archive.RollbackExtraction(created)}
}

// partialExtraction is an extraction that wrote part of a tree and then failed.
//
// The half tree is taken back rather than left: the destination of a restore
// holds nothing but bytes this extraction just wrote there -- it was absent, or
// an empty directory, or removed by a verified overwrite -- and the content is
// still in the archive, because the entry is consumed only after a successful
// extraction. A destination left half-full would look restored, and a retry
// would meet its own leftovers as a conflict. What the extraction managed to
// write is named anyway, because "nothing is there now" is only half the truth
// the caller needs.
type partialExtraction struct {
	err       error
	extracted []string
	stuck     []string
}

func (e *partialExtraction) Error() string {
	msg := fmt.Sprintf("%s; %d path(s) had been extracted (%s)",
		e.err, len(e.extracted), archive.NamePaths(e.extracted))
	if len(e.stuck) > 0 {
		return fmt.Sprintf("%s, and %d of them could not be removed again (%s)",
			msg, len(e.stuck), archive.NamePaths(e.stuck))
	}
	return msg + " and were removed again"
}

func (e *partialExtraction) Unwrap() error { return e.err }
