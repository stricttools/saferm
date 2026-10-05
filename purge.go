package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stricttools/saferm/internal/db"
	"github.com/stricttools/strictcli/go/strictcli"
)

func registerPurgeCmd(app *strictcli.App) {
	app.Command("purge", "Permanently destroy archived items and free disk space", handlePurge,
		strictcli.WithEffect(strictcli.EffectMutating),
		// The one saferm command with no way back. `delete` moves a file into
		// the archive and `undelete` brings it back; `purge` destroys the
		// archived content, and after it nothing in the tool can recover the
		// file. That is the whole reason saferm exists, inverted -- so it is
		// worth interrupting someone for.
		strictcli.WithConsequential(),
		strictcli.WithGrants(strictcli.Grant{
			Name:   "purge",
			Reason: "purging destroys the archived content permanently; undelete cannot bring it back",
			Kind:   strictcli.FileWrite,
		}),
		// Nothing here carries a value default: purge is `mutating`, and the
		// framework's mutating-default ban forbids a declaration whose absence
		// resolves to a value nobody typed. The two strings used to declare
		// Default("") and the handler tested `!= ""` to find out whether anyone
		// had asked -- an absence sentinel, which Optional() now says outright.
		strictcli.WithFlags(
			strictcli.StringFlag("older-than", "Purge items older than duration (e.g., 30d, 24h, 1w); omitted, age selects nothing", strictcli.Optional()),
			strictcli.StringFlag("larger-than", "Only purge items larger than this size (e.g. 100MB, 1GB); omitted, size filters nothing", strictcli.Optional()),
			strictcli.BoolFlag("all", "Select all archived items for permanent destruction; omitted, nothing is selected by this flag", strictcli.Optional()),
		),
		strictcli.WithArgs(
			strictcli.NewArg("targets", "Record UUIDs or numeric database IDs of specific items to permanently destroy ("+identifierOrderHelp+")",
				strictcli.Variadic(), strictcli.ArgOptional()),
		),
		// The selection rule, declared once. It used to be a hand guard in the
		// handler printing "specify record UUIDs or numeric IDs, --older-than,
		// --larger-than, or --all" and returning ExitUsage; the framework now
		// refuses the empty selection at parse time, renders the rule in --help,
		// and publishes it in the dumped schema. `--all` elects on `true` alone,
		// so `--no-all` selects nothing and is told so.
		strictcli.WithConstraints(
			strictcli.AtLeastOne("purge-selection",
				strictcli.Member("targets", strictcli.WhenNonEmpty()),
				strictcli.Member("older-than"),
				strictcli.Member("larger-than"),
				strictcli.Member("all", strictcli.WhenTrue()),
			),
		),
	)
}

func handlePurge(ctx *strictcli.Context, kwargs map[string]interface{}) strictcli.Outcome {
	// Absence is absence, not "". Whether anyone asked for an age or a size
	// selection is the two-result type assertion, so `--older-than ""` is a
	// supplied value that fails in parseDuration rather than a silent no-op.
	olderThan, hasOlderThan := kwargs["older_than"].(string)
	largerThan, hasLargerThan := kwargs["larger_than"].(string)
	// `all` is read by the declared constraint alone: it selects every record
	// still in the archive, which is what the final branch below already does
	// for --larger-than on its own, so the handler has nothing left to ask it.
	dryRun := ctx.DryRun()
	targets := optStrSlice(kwargs["targets"])
	verbose := ctx.Verbose()
	fx := ctx.Effects()

	hasTargets := len(targets) > 0

	// The at-least-one selection rule is the declaration's, enforced by the
	// parser before dispatch: reaching this line means something was selected.
	// --larger-than alone is valid (acts like --all --larger-than).

	// Shape before archive: a path among the targets is refused before anything
	// is opened or selected, on any machine.
	for _, target := range targets {
		if code := requireIdentifierShape(target); code != ExitSuccess {
			return strictcli.Exit(code)
		}
	}

	archiveDir := kwargs["archive_dir"].(string)
	dbPath := kwargs["db_path"].(string)

	if err := ensureDirectories(fx, filepath.Dir(archiveDir), archiveDir, dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: creating directories: %s\n", err)
		return strictcli.Exit(ExitGeneral)
	}

	database, err := openArchiveDB(ctx, dbPath, archiveDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: opening database: %s\n", err)
		return strictcli.Exit(dbExit(err))
	}
	// nil means the dry run found no archive at all, which is the emptiest
	// possible selection.
	if database == nil {
		say(ctx, "Nothing to purge.\n")
		return strictcli.Exit(ExitSuccess)
	}
	defer database.Close()

	var records []*db.DeletionRecord

	if hasTargets {
		for _, target := range targets {
			// The shared resolver keeps the identifier order identical across
			// the verbs, and keeps a missing record distinguishable from a
			// locked database: reporting contention as "no record" would tell a
			// caller its record is gone when it is only busy.
			rec, code := resolveRecord(database, target, false)
			if code != ExitSuccess {
				return strictcli.Exit(code)
			}
			records = append(records, rec)
		}
	} else if hasOlderThan {
		dur, err := parseDuration(olderThan)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			return strictcli.Exit(ExitUsage)
		}
		before := time.Now().Add(-dur)
		records, err = database.QueryOlderThan(before)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: querying database: %s\n", err)
			return strictcli.Exit(dbExit(err))
		}
	} else {
		// --all, or --larger-than alone, which selects from the same set: every
		// record still in the archive. A restored record's content is back
		// where it came from and a purged record's is already destroyed, so
		// neither has anything left to purge, and counting them made the dry
		// run promise more than the purge frees.
		records, err = database.QueryAll(false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: querying database: %s\n", err)
			return strictcli.Exit(dbExit(err))
		}
	}

	// Apply --larger-than filter if set.
	if hasLargerThan {
		threshold, err := parseSize(largerThan)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			return strictcli.Exit(ExitUsage)
		}
		var filtered []*db.DeletionRecord
		for _, rec := range records {
			if rec.Size >= threshold {
				filtered = append(filtered, rec)
			}
		}
		records = filtered
	}

	if len(records) == 0 {
		say(ctx, "Nothing to purge.\n")
		return strictcli.Exit(ExitSuccess)
	}

	if dryRun {
		// Display what would be purged in tabular format.
		var table strings.Builder
		fmt.Fprintf(&table, "%-6s %-40s %-10s %-16s\n", "ID", "Path", "Size", "Age")
		fmt.Fprintf(&table, "%-6s %-40s %-10s %-16s\n", "------", "----------------------------------------", "----------", "----------------")
		// The Size column is what each record captured at delete time; what
		// the purge frees is what the selected entries occupy on disk, which
		// for a compressed tree is a fraction of it, and for a file entry
		// still linked from outside the archive is nothing at all.
		var freeing int64
		shared := 0
		for _, rec := range records {
			path := rec.OriginalPath
			if len(path) > 40 {
				path = "..." + path[len(path)-37:]
			}
			fmt.Fprintf(&table, "%-6d %-40s %-10s %-16s\n",
				rec.ID, path, humanSize(rec.Size), humanAge(rec.DeletedAt))
			use, present, err := entryDiskUse(archiveDir, rec)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: reading the archive entry of [%d] %s: %s\n", rec.ID, rec.OriginalPath, err)
				return strictcli.Exit(ExitArchive)
			}
			if !present {
				continue
			}
			if use.Shared() {
				shared++
			}
			freeing += use.Freed()
		}
		fmt.Fprintf(&table, "\nWould purge %d item(s), freeing ~%s\n", len(records), humanSize(freeing))
		if shared > 0 {
			fmt.Fprintf(&table, "(%d of them also linked from outside the archive: purging frees nothing for those until the other links go)\n", shared)
		}
		emit(ctx, "%s", table.String())
		// Fall through: the loop below mints each archive-file removal on the
		// effects handle, which records it under --dry-run instead of doing it,
		// so the would-do log names every file that would be destroyed.
	}

	// Consent is the framework's, once, at the `consequential` gate in front of
	// dispatch: reaching this line means it was given. saferm used to raise a
	// second prompt of its own here, which meant one operation asked twice and
	// the second ask was unanswerable in the non-interactive case that matters
	// most -- an approved `saferm --approve-consequential purge --all` read EOF
	// and aborted.
	//
	// What the prompt was actually for -- naming every record about to be
	// destroyed -- outlives it, and prints unconditionally: after consent,
	// before the first removal, whether or not anyone is watching. It is the
	// record of an irreversible act, not prompt chrome, so --quiet does not
	// suppress it. Under --dry-run the table above has already listed the same
	// records, so this does not repeat it.
	if !dryRun {
		var listing strings.Builder
		fmt.Fprintf(&listing, "Permanently deleting %d item(s):\n", len(records))
		for _, rec := range records {
			fmt.Fprintf(&listing, "  [%d] %s (%s, %s)\n",
				rec.ID, rec.OriginalPath, humanSize(rec.Size), humanAge(rec.DeletedAt))
		}
		emit(ctx, "%s", listing.String())
	}

	purged := 0
	for _, rec := range records {
		// Skip already-purged items.
		if rec.PurgedAt != nil {
			continue
		}

		// Remove the archive file
		archivePath := archiveEntryPath(archiveDir, rec)

		// A row can legitimately name nothing: an archival that meets a changed
		// source inside its window commits its record and discards its entry on
		// purpose, and `info` reports that state. Purging it is how the row is
		// cleared, so it is not an error -- but it destroys nothing, and saying
		// so is the difference between "one item purged" meaning content was
		// destroyed and it meaning a row was tidied up.
		if rec.RestoredAt == nil && archiveEntryIsGone(archiveDir, rec) {
			fmt.Fprintf(os.Stderr, "note: [%d] %s: the archived copy was already gone; there is nothing to destroy for this row\n",
				rec.ID, rec.OriginalPath)
		}

		// Minted on the handle: purging is the irreversible half of saferm, so
		// the destruction is declared (and, under --dry-run, only declared).
		if _, err := fx.Remove(archivePath,
			strictcli.UseGrant("purge"),
			strictcli.Resource("saferm-entry:"+rec.UUID),
		); err != nil {
			fmt.Fprintf(os.Stderr, "warning: removing archive file %s: %s\n", archivePath, err)
		}

		// Mark the record as purged (preserves metadata). The database is
		// SQLite and no member of the effects handle's closed method set can
		// describe a row update, so this mutation stays outside the handle and
		// is skipped in dry mode.
		if !dryRun {
			if err := database.MarkPurged(rec.ID); err != nil {
				fmt.Fprintf(os.Stderr, "error: marking record %d as purged: %s\n", rec.ID, err)
				continue
			}
		}

		if verbose && !dryRun {
			say(ctx, "purged: [%d] %s\n", rec.ID, rec.OriginalPath)
		}
		purged++
	}

	if !verbose && !dryRun {
		say(ctx, "%d item(s) purged\n", purged)
	}

	return strictcli.Exit(ExitSuccess)
}
