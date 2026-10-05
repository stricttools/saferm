package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stricttools/saferm/internal/archive"
	"github.com/stricttools/saferm/internal/db"
	gitutil "github.com/stricttools/saferm/internal/git"
	"github.com/stricttools/saferm/internal/meta"
	"github.com/stricttools/strictcli/go/strictcli"
)

// The two error modes `delete` accepts, spelled once.
const (
	onErrorAbort    = "abort"
	onErrorContinue = "continue"
)

// archivedRecord is one record a `delete` invocation wrote, as the machine
// payload carries it: the identifier line's own content, in a shape a consumer
// does not have to parse out of prose.
type archivedRecord struct {
	ID   int64  `json:"id"`
	UUID string `json:"uuid"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// failedPath is one path a `delete` invocation could not archive, carrying the
// same message the run printed about it on stderr.
//
// It exists because the archived list alone cannot answer "what did not make
// it": under `--on-error continue` a consumer would have to diff its own
// argument list against `archived` to find the gaps, and the reason for each
// gap would live nowhere but in prose it would have to parse.
type failedPath struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// deletePayload is `delete`'s machine payload: the invocation's group
// identifier, every record it wrote, and every path it could not archive.
//
// A preview writes no records, so `archived` is empty there and no identifier
// is invented for a row that does not exist -- what a previewed delete WOULD do
// is the envelope's own `preview` member. `group_id` is minted for the
// invocation either way, and is the one thing on the payload that the human
// stream never names. Both lists are always present: a run that failed nothing
// answers with an empty `failed`, never with a missing member, so a consumer
// reads the same two arrays on every answer.
type deletePayload struct {
	GroupID  string           `json:"group_id"`
	Archived []archivedRecord `json:"archived"`
	Failed   []failedPath     `json:"failed"`
}

// deletePayloadSchema declares the payload above over the framework's closed
// subset. Sizes and identifiers are integers, and both stay far below the 2^53
// magnitude the envelope's number model permits; the uuid is a string because
// it always was one.
var deletePayloadSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"group_id": map[string]interface{}{"type": "string"},
		"archived": map[string]interface{}{
			"type": "array",
			"items": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id":   map[string]interface{}{"type": "integer"},
					"uuid": map[string]interface{}{"type": "string"},
					"path": map[string]interface{}{"type": "string"},
					"size": map[string]interface{}{"type": "integer"},
				},
				"required":             []interface{}{"id", "uuid", "path", "size"},
				"additionalProperties": false,
			},
		},
		"failed": map[string]interface{}{
			"type": "array",
			"items": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":  map[string]interface{}{"type": "string"},
					"error": map[string]interface{}{"type": "string"},
				},
				"required":             []interface{}{"path", "error"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []interface{}{"group_id", "archived", "failed"},
	"additionalProperties": false,
}

func registerDeleteCmd(app *strictcli.App) {
	app.Command("delete", "Move files to the saferm archive with metadata tracking", handleDelete,
		strictcli.WithEffect(strictcli.EffectMutating),
		strictcli.PayloadSchema(deletePayloadSchema),
		strictcli.WithGrants(strictcli.Grant{
			Name:   "git-index",
			Reason: "a tracked file that moved into the archive must leave the git index too, or the next commit resurrects it",
			Kind:   strictcli.ProcMutate,
		}),
		// Every switch below declares Optional() rather than a value default:
		// delete is `mutating`, and the framework's mutating-default ban forbids
		// a declaration whose absence resolves to a value the invocation never
		// stated. Each one names its fallback in its own help text, and
		// [optBool] / [optStr] are the only place absence becomes that fallback.
		// The command lines callers already write are unchanged.
		strictcli.WithFlags(
			strictcli.BoolFlag("recursive", "Allow recursive deletion of directories and all their contents; omitted, a directory is refused", strictcli.Short("r"), strictcli.Optional()),
			strictcli.BoolFlag("ignore-missing", "Silently skip files that do not exist instead of erroring; omitted, a missing path is an error", strictcli.Short("f"), strictcli.Optional()),
			strictcli.BoolFlag("interactive", "Prompt for confirmation before archiving each file; omitted, nothing is asked", strictcli.Short("i"), strictcli.Optional()),
			strictcli.StringFlag("description", "Mandatory explanation of why this deletion is happening", strictcli.Required()),
			strictcli.StringFlag("command", "Record the original rm command being replaced by saferm; omitted, no command is recorded", strictcli.Optional()),
			strictcli.StringFlag("meta", "Attach additional metadata as key=value pairs (repeatable); omitted, no custom metadata is attached", strictcli.Repeatable(), strictcli.Unique(false), strictcli.Optional()),
			strictcli.BoolFlag("update-git-index", "Run git rm --cached to stage removal in the git index; omitted, the index is updated", strictcli.Optional()),
			// Mandatory, with no default. A batch that meets a bad path has two
			// defensible answers and they suit opposite callers: a script wants
			// the batch to stop before it does more, an interactive cleanup
			// wants the remaining paths archived anyway. Choosing one silently
			// would be wrong for the other half of the callers, so saferm
			// refuses to choose.
			strictcli.StringFlag("on-error",
				"What to do when a path cannot be archived. Mandatory: there is no default",
				strictcli.Required(),
				strictcli.Choices(
					strictcli.Ch(onErrorAbort, "stop at the first path that cannot be archived, leaving the remaining paths untouched"),
					strictcli.Ch(onErrorContinue, "archive the remaining paths, report every failure, and exit with the first failure's code at the end"),
				)),
		),
		strictcli.WithArgs(
			strictcli.NewArg("files", "One or more files or directories to move into the archive", strictcli.Variadic(), strictcli.ArgRequired()),
		),
	)
}

func handleDelete(ctx *strictcli.Context, kwargs map[string]interface{}) strictcli.Outcome {
	// The optional switches resolve to the fallback their help declares; see
	// [optBool] for why none of them may carry a Default().
	recursive := optBool(kwargs["recursive"], false)
	ignoreMissing := optBool(kwargs["ignore_missing"], false)
	interactive := optBool(kwargs["interactive"], false)
	description := kwargs["description"].(string)
	command := optStr(kwargs["command"], "")
	updateGitIndex := optBool(kwargs["update_git_index"], true)
	onError := kwargs["on_error"].(string)
	metaValues := optStrSlice(kwargs["meta"])
	filesRaw := kwargs["files"].([]interface{})
	verbose := ctx.Verbose()
	dryRun := ctx.DryRun()
	fx := ctx.Effects()

	// Parse --meta key=value pairs
	customMeta := make(map[string]string)
	for _, s := range metaValues {
		key, value, ok := strings.Cut(s, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "error: --meta value %q must be in key=value format\n", s)
			return strictcli.Exit(ExitUsage)
		}
		customMeta[key] = value
	}

	archiveDir := kwargs["archive_dir"].(string)
	dbPath := kwargs["db_path"].(string)

	if err := ensureDirectories(fx, filepath.Dir(archiveDir), archiveDir, dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: creating directories: %s\n", err)
		return strictcli.Exit(ExitGeneral)
	}

	// nil means "no archive yet", which only a dry run can see. Nothing below
	// touches the database in dry mode, so there is nothing to say about it.
	database, err := openArchiveDB(ctx, dbPath, archiveDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: opening database: %s\n", err)
		return strictcli.Exit(dbExit(err))
	}
	if database != nil {
		defer database.Close()
	}

	// Extract exclude patterns from args
	rawPatterns := kwargs["exclude_env_patterns"].([]interface{})
	patterns := make([]string, len(rawPatterns))
	for i, p := range rawPatterns {
		patterns[i] = p.(string)
	}

	// Collect metadata
	metadata, err := meta.Collect(patterns, customMeta)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: collecting metadata: %s\n", err)
		return strictcli.Exit(ExitGeneral)
	}
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: serializing metadata: %s\n", err)
		return strictcli.Exit(ExitGeneral)
	}

	// Who ran this deletion, derived from the process trace store the metadata
	// capture already resolved. Both values are nil unless an entry resolved,
	// which is what "no tool claimed this" means -- there is no flag to state
	// an origin and nothing is inferred from any other variable.
	originName, originVersion := metadata.Trace.Origin()

	// One identifier for the whole invocation, minted unconditionally: every
	// record written below carries it, so a batch stays recoverable as a batch.
	groupID := archive.NewUUID()

	run := &deleteRun{
		ctx:            ctx,
		fx:             fx,
		database:       database,
		archiveDir:     archiveDir,
		originName:     originName,
		originVersion:  originVersion,
		groupID:        groupID,
		recursive:      recursive,
		ignoreMissing:  ignoreMissing,
		interactive:    interactive,
		updateGitIndex: updateGitIndex,
		verbose:        verbose,
		dryRun:         dryRun,
		description:    description,
		command:        command,
		metaJSON:       string(metaJSON),
		gitRoot:        metadata.GitRoot,
		scanner:        bufio.NewScanner(os.Stdin),
		written:        []archivedRecord{},
		failures:       []failedPath{},
	}

	// The machine payload is supplied on every way out from here, the abort
	// included: a batch that stopped partway still wrote the records above the
	// failure, and a consumer holding only the exit code would have to go
	// looking for them. It is supplied in both modes -- outside machine mode
	// nothing prints it.
	supplyPayload := func() {
		ctx.Payload(deletePayload{GroupID: groupID, Archived: run.written, Failed: run.failures})
	}

	archived := 0
	failed := 0
	firstFailure := ExitSuccess

	for _, fileRaw := range filesRaw {
		ok, code := run.archiveOne(fileRaw.(string))
		if ok {
			archived++
		}
		if code == ExitSuccess {
			continue
		}
		failed++
		if firstFailure == ExitSuccess {
			firstFailure = code
		}
		// abort: stop here. Everything archived so far is already committed and
		// its identifiers are already on stdout, so the caller loses nothing by
		// the exit -- which is exactly why this mode is safe to offer.
		if onError == onErrorAbort {
			supplyPayload()
			return strictcli.Exit(code)
		}
	}

	if archived > 0 && !verbose {
		if dryRun {
			say(ctx, "%d file(s) would be archived\n", archived)
		} else {
			say(ctx, "%d file(s) archived\n", archived)
		}
	}

	supplyPayload()

	if failed > 0 {
		// continue mode: every failure was reported as it happened, and the
		// count is repeated at the end because the per-path lines are far up
		// the stream by now. The exit code is the FIRST failure's, so a caller
		// reading only the code learns what went wrong first rather than last.
		fmt.Fprintf(os.Stderr, "error: %d of %d path(s) failed; --on-error %s archived the rest\n",
			failed, len(filesRaw), onErrorContinue)
		return strictcli.Exit(firstFailure)
	}

	return strictcli.Exit(ExitSuccess)
}

// deleteRun is everything one `delete` invocation established before it began
// walking its paths: the open archive, the flags, and the metadata blob every
// record it writes will carry.
type deleteRun struct {
	ctx            *strictcli.Context
	fx             *strictcli.Effects
	database       *db.DB
	archiveDir     string
	recursive      bool
	ignoreMissing  bool
	interactive    bool
	updateGitIndex bool
	verbose        bool
	dryRun         bool
	description    string
	command        string
	metaJSON       string
	gitRoot        string
	scanner        *bufio.Scanner

	// originName and originVersion name the tool that ran this invocation, as
	// resolved from the trace store; both nil when none did.
	originName    *string
	originVersion *string

	// written accumulates the records this invocation created, in the order it
	// created them. It is what the machine payload carries, and it is appended
	// to at exactly the point the identifier line is printed, so the two can
	// never disagree about what was archived.
	written []archivedRecord

	// failures accumulates the paths this invocation could not archive, in the
	// order it met them. Every entry is appended by fail(), which is also what
	// prints the message, so the payload and stderr can never disagree about
	// what failed or why.
	failures []failedPath

	// groupID is stamped on every record this invocation writes.
	groupID string
}

// fail reports why a path could not be archived and records it, in one call.
//
// Printing and recording are the same act on purpose: a failure path that
// printed without recording would leave the machine payload claiming a batch
// went cleanly, and the two lists a consumer reads would disagree with the
// stream a human reads. The message is stored without the `error: ` prefix and
// without its newline -- those belong to the stream, not to the fact.
//
// It returns archiveOne's own pair, so a failure site stays one line.
func (r *deleteRun) fail(path string, code int, format string, args ...interface{}) (archived bool, exitCode int) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	r.failures = append(r.failures, failedPath{Path: path, Error: msg})
	return false, code
}

// archiveOne archives a single path, reporting its own failures on stderr.
//
// It returns whether a record was created (a path the caller declined
// interactively, or skipped under --ignore-missing, creates none and is not a
// failure either) and the exit code the failure deserves, or ExitSuccess. The
// caller decides what a failure means for the rest of the batch -- that is
// --on-error's whole job -- so nothing here ends the command.
func (r *deleteRun) archiveOne(file string) (archived bool, code int) {
	absPath, err := filepath.Abs(file)
	if err != nil {
		if r.ignoreMissing {
			return false, ExitSuccess
		}
		// The absolute path is exactly what could not be computed, so the
		// failure is recorded under the path as the caller wrote it.
		return r.fail(file, ExitGeneral, "resolving path %q: %s", file, err)
	}

	if r.interactive {
		fmt.Fprintf(os.Stderr, "delete %s? [y/N] ", absPath)
		if !r.scanner.Scan() || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.scanner.Text())), "y") {
			return false, ExitSuccess
		}
	}

	// Plan first: everything up to the point of no return is reads, so it
	// runs identically in both modes and the preview is built from the
	// same facts the real archival uses.
	plan, err := archive.NewPlan(absPath, r.archiveDir, r.recursive)
	if err != nil {
		if r.ignoreMissing && (err == archive.ErrFileNotFound) {
			return false, ExitSuccess
		}
		if err == archive.ErrRecursiveRequired {
			return r.fail(absPath, ExitUsage, "%s is a directory; use -r to delete recursively", file)
		}
		return r.fail(absPath, ExitArchive, "archiving %s: %s", file, err)
	}

	if r.dryRun {
		if err := recordArchival(r.ctx, r.fx, plan); err != nil {
			return r.fail(absPath, ExitArchive, "recording archival of %s: %s", file, err)
		}
		if r.verbose {
			say(r.ctx, "would archive: %s\n", absPath)
		}
		return true, ExitSuccess
	}

	// The archive entry is written first and the source is left alone: the
	// removal below happens only once the record exists, so an insert that
	// fails can discard the entry and leave the path untouched. See
	// archive.Execute.
	result, err := archive.Execute(plan)
	if err != nil {
		return r.fail(absPath, ExitArchive, "archiving %s: %s", file, err)
	}

	rec := &db.DeletionRecord{
		UUID:          result.UUID,
		OriginalPath:  absPath,
		OriginalName:  filepath.Base(absPath),
		Size:          result.Size,
		Hash:          result.Hash,
		NodeType:      result.NodeType,
		DeletedAt:     time.Now(),
		Command:       r.command,
		Description:   r.description,
		Metadata:      r.metaJSON,
		OriginName:    r.originName,
		OriginVersion: r.originVersion,
		GroupID:       &r.groupID,
	}
	if result.NodeType == archive.NodeTypeSymlink {
		rec.SymlinkTarget = &result.SymlinkTarget
	}

	id, err := r.database.Insert(rec)
	if err != nil {
		archived, code := r.fail(absPath, dbExit(err), "inserting database record for %s: %s", absPath, err)
		// Nothing has happened to the caller's path yet, so the archival can be
		// taken back whole: drop the entry and say so. A discard that itself
		// fails is the one remaining way to leave a blob with no row, so it is
		// reported loudly and by name rather than swallowed.
		if derr := archive.DiscardBlob(plan); derr != nil {
			fmt.Fprintf(os.Stderr, "error: removing the unrecorded archive entry %s: %s; it is an orphaned copy no saferm command can name\n",
				plan.Dest, derr)
		} else {
			fmt.Fprintf(os.Stderr, "note: %s was left in place; nothing was archived\n", absPath)
		}
		return archived, code
	}

	// The record exists from here on, so the archived copy is findable by both
	// identifiers whatever happens next. Removing the source is the second half
	// of the archival: a failure here leaves a recorded deletion whose original
	// is still on disk, which is worth an error and is not silent data loss.
	if err := archive.RemoveSource(plan); err != nil {
		code, msg := describeUnremovedSource(absPath, plan, id, result.UUID, err)
		return r.fail(absPath, code, "%s", msg)
	}

	// Stage removal in git index if the file was tracked.
	if r.updateGitIndex && r.gitRoot != "" && gitutil.IsGitTracked(absPath) {
		if err := gitutil.GitRmCached(absPath, result.NodeType == archive.NodeTypeDirectory); err != nil {
			fmt.Fprintf(os.Stderr, "warning: git rm --cached failed for %s: %s\n", file, err)
		} else if r.verbose {
			say(r.ctx, "Staged removal in git: %s\n", file)
		}
	}

	// Both identifiers, one line per record, in every mode but --quiet.
	// The numeric id is the counter of this one database; the uuid names
	// the archived entry itself and is the handle that survives -- `info`,
	// `undelete` and `purge` all accept it. Printing them here is what
	// spares a caller from running `list` afterwards and guessing which row
	// was its own, and it is why an abort partway through a multi-path
	// delete still leaves the caller holding the identifiers of everything
	// that did get archived.
	say(r.ctx, "archived: [%d] %s %s (%s)\n", id, result.UUID, absPath, humanSize(result.Size))
	r.written = append(r.written, archivedRecord{ID: id, UUID: result.UUID, Path: absPath, Size: result.Size})

	return true, ExitSuccess
}

// describeUnremovedSource explains a [archive.RemoveSource] that refused or
// failed, returning the exit code for it and the message that states it. The
// caller prints and records that message in one act, which is what keeps the
// machine payload's failure list and stderr saying the same thing.
//
// The auxiliary lines -- a discard that itself failed -- are printed here and
// not returned: they are a second fact about the archive entry, not the reason
// this path could not be archived.
//
// The record is already committed by the time this runs, so every branch states
// two things: what the record holds, and what the path holds, because after
// this failure they are no longer the same thing and only saying one of them
// would be a half-truth.
//
// Two of the branches are about the ENTRY rather than the source: it can be
// destroyed by a concurrent purge that selected the row this delete had just
// inserted, or replaced by something saferm never archived. Neither removes
// anything -- there is nothing to take back in the first case and nothing
// saferm owns in the second -- and neither claims the record holds the archived
// content, because in both it does not.
//
// The ErrArchivedContentChanged branch is the only one that undoes anything. A file's archive
// entry is a hard link, so a write through the original path rewrites the
// archived bytes too: the row says one hash and the blob has another, and no
// re-reading fixes that, because the content the row describes is gone from the
// machine. Keeping the entry would leave a permanent lie in the archive AND a
// second name for a file the caller is still editing, so it is discarded, which
// costs nothing -- dropping one of two links to an inode leaves the caller's
// file exactly where it is, with the newer content it now has. What survives is
// a row naming no blob, which `list` still shows and `purge` can clear, and
// which is honest about the one thing that must not be got wrong: nothing was
// destroyed.
func describeUnremovedSource(absPath string, plan *archive.Plan, id int64, uuid string, err error) (int, string) {
	switch {
	case errors.Is(err, archive.ErrArchivedContentChanged):
		if derr := archive.DiscardBlob(plan); derr != nil {
			fmt.Fprintf(os.Stderr, "error: removing the archive entry %s, which no longer matches record [%d] %s: %s; it is now a second name for %s and a write to either changes both\n",
				plan.Dest, id, uuid, derr, absPath)
		}
		return ExitArchive, fmt.Sprintf("%s was written to while it was being archived: %s; it was left in place with its current content, the archived copy was discarded because record [%d] %s records the hash it had before the write, and that row now names nothing -- purge it and run the delete again",
			absPath, err, id, uuid)

	case errors.Is(err, archive.ErrDirectoryChanged):
		// The tree grew or was written into after the tar was closed, so the
		// archive does not cover what os.RemoveAll would destroy. Same remedy as
		// a file written through: the tree is left whole -- including the part
		// nothing archived -- and the incomplete archive is discarded rather
		// than left standing under a row that claims to hold the whole tree.
		if derr := archive.DiscardBlob(plan); derr != nil {
			fmt.Fprintf(os.Stderr, "error: removing the archive entry %s, which does not hold all of %s: %s; it is an incomplete copy under record [%d] %s\n",
				plan.Dest, absPath, derr, id, uuid)
		}
		return ExitArchive, fmt.Sprintf("%s changed while it was being archived: %s; the tree was left whole, the incomplete archive was discarded, and record [%d] %s now names nothing -- purge it and run the delete again",
			absPath, err, id, uuid)

	case errors.Is(err, archive.ErrSourceReplaced), errors.Is(err, archive.ErrSourceDiverged):
		return ExitArchive, fmt.Sprintf("not removing %s: %s; record [%d] %s holds the content that was archived, and %s now holds something else -- neither was destroyed",
			absPath, err, id, uuid, absPath)

	case errors.Is(err, archive.ErrArchiveEntryMissing):
		// The row was committed and then its blob went -- a concurrent purge
		// selecting the row saferm had just inserted does exactly this. There is
		// nothing to discard and nothing to undo: the row names nothing, and the
		// path the caller asked to delete is now the only copy of its content.
		return ExitArchive, fmt.Sprintf("not removing %s: %s; record [%d] %s was committed before the entry disappeared, so that row names nothing and %s is the only copy of its content left -- purge the row and run the delete again",
			absPath, err, id, uuid, absPath)

	case errors.Is(err, archive.ErrArchiveEntryReplaced):
		// The entry is still a file, but not the one that was archived, so
		// saferm did not put it there and does not remove it: discarding it
		// would destroy something whose origin is unknown. The row is left
		// standing over it and is worth nothing, which is what the message
		// says rather than claiming the record holds the archived content.
		return ExitArchive, fmt.Sprintf("not removing %s: %s; record [%d] %s names an archive entry that is no longer what was archived, so that row names nothing saferm can vouch for -- the entry was left alone because saferm did not put it there, and %s was not destroyed",
			absPath, err, id, uuid, absPath)

	default:
		return ExitArchive, fmt.Sprintf("removing %s after archiving it: %s; record [%d] %s holds the archived copy",
			absPath, err, id, uuid)
	}
}

// recordArchival mints the mutations an archival performs onto the effects
// handle, so that a dry run's would-do log names every path that would move or
// disappear.
//
// It runs in dry mode only. It is the ONE place in saferm where the record and
// the execution are separate calls, and it is deliberate: archiving is a
// compound operation -- hash, then a hard link with a copy-and-verify fallback
// where the link is refused, or tar + zstd of a whole tree, and then a separate
// removal of the original once the deletion is recorded -- and the effects
// handle's closed method set has no primitive for a streaming archive, a
// verified copy or a hard link. So the handle carries the description and
// archive.Execute carries the act; the dry-mode branch in the caller is what
// keeps the two from ever both happening.
//
// Every node type is described the same way, as the two things that actually happen:
// an archive entry appears, and the source goes. The file case used to mint
// `rename`, which was true when a file was archived by renaming it and has not
// been since. A rename says one atomic move whose destination then holds the
// only copy; the real sequence is a link, a database insert, and an unlink,
// which leaves the file readable at both names in between and can end with the
// source still in place. A preview that says `rename` is previewing a saferm
// that no longer exists, so it says instead exactly what the two seam calls do.
//
// The archive directory itself is not minted here: ensureDirectories already
// declared it once, before the first plan was built, and repeating it per file
// would pad the would-do log with a line that says nothing new.
func recordArchival(ctx *strictcli.Context, fx *strictcli.Effects, plan *archive.Plan) error {
	// Structural, not decorative: the content below is a description of an
	// entry, not the bytes anything writes, and outside dry mode the handle
	// would write it. Nothing calls this outside dry mode; this is what keeps
	// that true.
	if !ctx.DryRun() {
		return errors.New("recordArchival describes an archival for a preview and must not run outside --dry-run")
	}

	// What the entry will contain, per node type:
	//
	//   - A symlink's entry IS its target path written out, so the real content
	//     is right here.
	//   - A regular file's entry is a hard link to the source (or a verified
	//     copy), so its size is the source's size. The buffer carries that
	//     length and nothing else: the framework renders a write's content as
	//     its byte count, and a preview minted with no content at all claimed
	//     "(0 bytes)" for every file however large -- a promise to write an
	//     empty file. Reading the file to say the same number would cost a full
	//     read, and a preview must not cost what the operation it previews does
	//     not.
	//   - A directory's entry is a .tar.zst that does not exist until the
	//     compression runs, and its size is not knowable before it does. The
	//     write is declared with no content, which is the only thing that is
	//     true about it here; strictcli's write has no way to say "size
	//     unknown", so the log renders that absence as 0.
	//   - A special file's entry is its `.node` descriptor, which the plan
	//     already holds byte for byte: nothing is read from the node itself.
	content, err := previewEntryContent(plan)
	if err != nil {
		return err
	}
	if _, err := fx.Write(plan.Dest, content, strictcli.Resource("saferm-entry:"+plan.UUID)); err != nil {
		return err
	}
	_, err = fx.Remove(plan.Source, strictcli.Resource("path:"+plan.Source))
	return err
}

// previewEntryContent is what a preview declares the archive entry will hold,
// per node type, as [recordArchival] describes.
func previewEntryContent(plan *archive.Plan) ([]byte, error) {
	switch plan.NodeType {
	case archive.NodeTypeSymlink:
		return []byte(plan.SymlinkTarget), nil
	case archive.NodeTypeFile:
		info, err := os.Lstat(plan.Source)
		if err != nil {
			return nil, err
		}
		return make([]byte, info.Size()), nil
	case archive.NodeTypeDirectory:
		return nil, nil
	case archive.NodeTypeFIFO, archive.NodeTypeSocket, archive.NodeTypeCharacterDevice, archive.NodeTypeBlockDevice:
		return plan.NodeDescriptor(), nil
	}
	return nil, fmt.Errorf("previewing the archival of %s: unknown node type %q", plan.Source, string(plan.NodeType))
}
