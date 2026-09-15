+++
title = "CLAUDE.md"
+++
# saferm

Go CLI that replaces `rm` with safe archival to `~/.saferm/`. AI-first design: every deletion requires `--description` explaining why.

## Project structure

```
main.go          -- app setup, registers commands via strictcli
delete.go        -- saferm delete: archive files/dirs
undelete.go      -- saferm undelete: restore by uuid, ID or path
identifiers.go   -- the one identifier resolver: uuid, then numeric ID, then path
list.go          -- saferm list: show archived items
purge.go         -- saferm purge: permanently remove from archive
info.go          -- saferm info: full metadata for a deletion
capabilities.go  -- saferm capabilities: the feature names a program negotiates on
helpers.go       -- say/emit, humanSize, humanAge, parseDuration, the record kinds
exitcodes.go     -- exit codes 0-8
version.go       -- version from ldflags or debug.ReadBuildInfo

internal/
  archive/       -- file/dir archival (os.Link, copy+verify when the link is refused, tar+zstd for dirs); Execute writes the entry, RemoveSource removes the original, DiscardBlob takes it back. On the way back: NewRestorePlan, EntryPresent, VerifyEntry, ExtractTree/RollbackExtraction, RestoreSymlink, CopyOut -- primitives only, so undelete.go owns the conflict decision and the step order
  db/            -- SQLite database (WAL mode, busy_timeout=5000, bounded contention retry, CRUD operations)
  meta/          -- metadata collection (env vars, git context, PPID + parent cmdline, the resolved trace chain)
  trace/         -- reads the strictcli process trace store: parses STRICTCLI_TRACE_PARENT and walks the ancestry chain
  test/          -- integration tests (builds binary, runs as subprocess)
  testutil/      -- test helpers

npm/             -- npm wrapper package (saferemove)
pypi/            -- PyPI wrapper package (saferm)
docs/            -- selfdoc source for saferm.smmh.dev
```

## Build and test

```bash
go build .                  # build
go test ./...               # all tests
go test ./... -short        # skip long tests
go install .                # install locally (picks up changes)
```

## Key conventions

- **CLI framework:** strictcli (`github.com/smm-h/strictcli/go/strictcli`). Commands use functional options. Handlers receive `map[string]interface{}`.
- **Only `purge` asks for confirmation.** strictcli's confirm protocol keys on the `consequential` declaration, not on the `mutating` classification, and `purge` is the one saferm command that declares it: it destroys archived content permanently and nothing in the tool can bring it back. `delete` and `undelete` are recoverable by construction and run bare -- `saferm delete --on-error abort --description "why" <files>` is the correct, complete invocation from a script or an agent, with no approval flag. `purge` prompts on a terminal and refuses outright (`error: stdin is not interactive; a consequential command must be confirmed at a terminal`) where there is none, so a non-interactive purge is `saferm --approve-consequential purge --all`. That framework gate is the **only** consent purge asks for: saferm's own `--skip-confirmation`/`-f` prompt is gone, because one operation asking twice meant the second ask was unanswerable exactly where the first had already been given. What the prompt was for survives it -- the per-record listing of everything about to be destroyed prints unconditionally after consent and before the first removal, and `--quiet` does not suppress it. `list` and `info` are `read_only` and cannot be consequential at all.
- **`--quiet`, `--verbose`, `--dry-run` and `--approve-consequential` belong to the framework**, are recognized anywhere on the command line, and have no short forms. The approval flag is deliberately unwieldy so it cannot decay into muscle memory. saferm's own `--verbose` global and `purge --dry-run` flag are gone -- both spellings still work, they are just delivered by the framework now, and `--dry-run` applies to every command rather than only to `purge`.
- **`--quiet` silences chatter, never answers.** It suppresses the counted summaries (`3 file(s) archived`, `2 item(s) purged`), the `--verbose` per-item progress, `Nothing to purge.` and the `Restored <path>` confirmation, and it dominates `--verbose` when both are passed. It never suppresses the outputs that ARE the command: `list`'s and `info`'s tables, the `--dry-run` previews and the framework's would-do log, `purge`'s listing of what it is destroying, or anything on stderr.
- **`--dry-run` records instead of acting.** Every mutation `delete`, `undelete` and `purge` perform is minted on `ctx.Effects()`, so a dry run prints a would-do log naming each path it would move, write or destroy, and touches nothing. The database row is the one exception: no member of the effects handle's closed method set can describe a SQLite row change, so those writes sit outside the handle and are skipped in dry mode.
- **`--json` is the machine surface.** It is the framework's flag, recognized anywhere, and it makes the envelope the ONLY document on stdout: everything saferm would print rides inside it as diagnostics. `delete`, `undelete`, `list` and `info` each declare a payload schema and supply their value in both modes, once the run has reached its answer -- a run that fails before it has one (a `--meta` value that is not `key=value`, an identifier naming no record) carries `payload: null`, because the exit code is the verdict and a payload is never one. `delete`'s aborted batch still names everything it archived above the failure. `purge` deliberately declares no payload at all, because nothing should drive the irreversible operation from a parsed document. `capabilities` is the probe: it names the features this binary ships and reads nothing, so it answers where saferm has never run. A consumer treats a missing verb or a missing feature exactly like saferm being absent, and never compares version strings -- a locally built saferm reports a Go pseudo-version no semver parser accepts. The schemas are published verbatim by `--dump-schema` and by nothing else; the MCP tool descriptors carry each command's `effect`/`consequential` classification beside its argument schema, and no payload schema at all. Full specification: `docs/machine-surface.md`.
- **Human output goes through the context writers, never straight to stdout.** `say()` is chatter (`--quiet` silences it), `emit()` is the answer itself (`--quiet` never does). Both route through `ctx.Info` in machine mode, which is what keeps the envelope alone on stdout; a bare `fmt.Printf` in a handler would print a table beside the document and break every consumer.
- **Every flag and every positional argument declares its own presence**, and `--help` renders exactly one presence part per line: `[required]`, `[optional]` or `[default: v]`. On `delete`, `undelete` and `purge` -- all three `mutating` -- **no flag or argument may carry a value default at all**: the framework refuses one at registration, because on a mutating command a value the framework picked is a value the framework writes. Every switch on those three therefore declares `Optional()` and names its fallback in its own help text, and `optBool` / `optStr` / `optStrSlice` in `helpers.go` are the ONLY place absence becomes that fallback. Behaviour on absence is unchanged: `-r` still opts in, `--update-git-index` still defaults to updating, an omitted `--command` still records nothing. Never reach for `Default()` on a mutating command's flag -- it will not register.
- **`--description` is mandatory** on delete (declared `Required()`). Never make it optional or give it a default.
- **`--on-error` is mandatory** on delete too, with no default and the values `abort` and `continue`. A batch that meets a bad path has two defensible answers -- stop, or archive the rest -- and they suit opposite callers, so saferm refuses to pick one silently. `abort` stops at the first failing path; `continue` archives the remaining paths, reports every failure, and exits at the end with the FIRST failure's code. Either way the identifiers of everything already archived are on stdout before the failure is reported, and under `--json` the payload's `failed` array names every path that could not be archived with the message saferm printed about it -- so a machine reads the failures instead of diffing its own argument list against `archived`. Never add a default. Both values carry their own help record, so `delete --help` says what each one does rather than only that it exists.
- **`purge`'s selection is a declared constraint, not a handler guard.** `AtLeastOne("purge-selection", ...)` over the `targets` argument and `--older-than`, `--larger-than`, `--all` is what makes an empty selection illegal: the parser refuses it before dispatch, `--help` renders the rule in a `Constraints:` block, and the dumped schema publishes it. `--all` elects on `true` alone, so `--no-all` selects nothing and the refusal says so. Never re-add a hand-written selection check -- there is one place the rule lives.
- **`--on-conflict` is required on undelete exactly when the destination is occupied**, with no default and the values `overwrite` and `abort`. Omitting it there is an argument error (exit 2) naming both values; `abort` is a stated refusal and exits 7. An absent destination needs no answer, and neither does an EMPTY directory standing where a tree was archived -- that is the tree's own emptied place, and extracting into it replaces nothing. The rule is for directory records only: an empty directory over a file record is still a conflict, because a file cannot be renamed over a directory. There is deliberately no keep-both or backup mode.
- **A restore always consumes the archived copy**, and the failure path is what makes that safe: the copy is dropped only as the LAST step, so a refused symlink, a truncated tar or a copy that ran out of space leaves the entry in place and the record restorable. A tree extraction that fails partway takes its own half tree back and names what it had extracted. A restored file that is wanted in the archive again is simply deleted again; there is no keep mode, because keeping a copy in the archive after restoring it is the parking workflow saferm exists to prevent.
- **An overwrite verifies the archived copy before the destination is touched**, and only an overwrite does. The recorded hash means three things: a file's hash covers its content exactly, a directory's covers the `.tar.zst` container and nothing about individual members (no per-member digest exists anywhere in the archive), and a symlink has no hash at all -- its entry is compared against the target the record names. A file or tree whose record carries no hash refuses the overwrite rather than passing it. A restore into an absent or empty destination gets no verify pass: a corrupt copy just fails the restore, which destroys nothing.
- **`--destination` restores somewhere else.** The path is resolved to an absolute one and written to the record's `restored_to` column, so `info` names where the content went. The conflict rules follow the destination, not the record.
- **`--update-git-index` exists on both sides.** `delete` runs `git rm --cached`, `undelete` runs `git add`, both default to true, and both can be turned off -- a programmatic caller may want no index side effects at all.
- **`delete` prints both identifiers per archived path**: `archived: [<id>] <uuid> <path> (<size>)`, one line per record, through `say()` so `--quiet` still silences it. The uuid is the durable handle -- `undelete`, `info` and `purge` all accept it, and `undelete` accepts an original path as well.
- **Identifier disambiguation is by shape, in one place** (`identifiers.go`): a 36-character hyphenated hex string is a record UUID, an all-digit string is a numeric database ID, anything else is a path. The order is total and independent of what happens to exist, so the same argument always means the same thing. `info` and `purge` refuse a path outright, naming the two forms they take.
- **`info` states a record's status** in one derived line: `restorable`, `restored at <time>`, `purged at <time>`, or both when a record was restored and later purged. It is read off `restored_at`/`purged_at`, plus one stat of the record's own archive entry where neither column is set: an archival that meets a changed source inside its window commits its row and discards its entry on purpose, so a row that names nothing is a state saferm produces itself, and reporting it as `restorable` would send the caller into an undelete that cannot work. `purge` says the same thing in its own vocabulary -- purging such a row destroys nothing, which it notes on stderr and does not treat as an error.
- **`-r` required for directories** (like rm).
- **`-f` skips errors** on nonexistent files (`delete --ignore-missing`). It is `delete`'s flag only; `purge` no longer has one.
- **Files** archived via `os.Link` into the archive (copy+verify when the filesystem or policy refuses the link, e.g. across devices), and the source is removed only after the database row exists.
- **Directories** archived as `.tar.zst` (tar + zstandard compression).
- **SHA-256 hash** computed for integrity verification.
- **SQLite WAL** with `busy_timeout=5000`, plus saferm's own bounded retry on top of it: every database operation that meets SQLITE_BUSY/SQLITE_LOCKED contention is retried up to 5 attempts total with a linear 50ms-per-attempt backoff, reported on stderr under `--verbose`. A lock that outlives the whole budget exits **8** (`ExitContention`), not 5 -- the archive is fine, another process is simply holding the write lock. The classifier (`db.IsContention`) reads the driver's result code, not its message, and covers every extended flavour of BUSY and LOCKED.
- **All env vars captured** except those matching denylist patterns (secrets, tokens, keys, etc.).
- **Git context** auto-detected (branch, HEAD, root).
- **Origin columns are derived, never declared.** `origin_name` and `origin_version` record which tool ran a deletion, and saferm fills them by reading the strictcli process trace store: it parses `STRICTCLI_TRACE_PARENT` from its own environment, resolves that entry from `~/.local/share/strictcli/trace/`, and takes the entry's `app` and `version`. There are **no `--origin-*` flags** on any caller, no agent identity is inferred from any other variable, and history is not backfilled -- both columns are null on every row written before a caller carried the variable, and null means "no tool claimed this". A version can never exist without a name: SQLite cannot add a CHECK constraint to an existing table, so `db.Insert` enforces it in code, identically on fresh and migrated databases. The cost is stated rather than hidden -- a hand-edited database, or a binary from before the migration, bypasses it.
- **The ancestry chain is embedded in the record's metadata**, not just referenced: the capture walks `parent_id` to the root and stores the flattened entries plus their identifiers, so age-based pruning of the trace store can never orphan a record. The capture is observational and cannot fail a deletion -- a polluted variable, a pruned store, a torn line or a parent that resolves to nothing is recorded as an anomaly under `trace.anomalies` and the delete proceeds. Consumers noticing dangling parents is the trace store's own primary failure-detection channel, which is why the anomalies are written down rather than discarded. What a hostile or broken store can put into a row is bounded: 1024 bytes per anomaly value, 32 anomalies per capture plus one `anomalies-dropped` line naming the rest, 1024 bytes per string an entry contributes to the embedded chain, no read of a partition over 64 MB, and no read of a store file that is not a regular file.
- **Every delete invocation stamps one `group_id`** on every record it writes, minted unconditionally with no way to opt out, so a batch stays recoverable as a batch.
- **PPID + parent cmdline** captured (platform-specific: `proc_linux.go`, `proc_darwin.go`).
- **Config:** `~/.saferm/config.toml` via strictcli's built-in config system (`WithConfig`). Key fields: `archive_dir`, `db_path`, `exclude_env_patterns`. Manage with `saferm config show/set/path/edit`. `config set` takes the key positionally and then exactly one of `--value <v>`, `--clear` or `--default`; the trailing positional value form is gone, so `saferm config set archive_dir /mnt/x` is now refused. A malformed `config.toml` is a hard error (exit 1) with a parse position -- it is never silently ignored. Unknown keys and, for `archive_dir`/`db_path`, a CLI/config value that diverges from the config are also hard errors (conflict-mode `error`). The divergence check only fires for a global flag in the pre-command position (`saferm --archive-dir X delete ...`); post-command placement (`saferm delete --archive-dir X`) is not currently conflict-checked.
- **Config vs infrastructure boundary:** `--hermetic` suppresses config-file and env *values* (config-managed settings fall back to defaults). `SAFERM_HOME` is NOT a config value -- it is location infrastructure, the same category as `HOME`, and is NOT suppressed by `--hermetic`. It selects where saferm lives; config values select how saferm behaves.
- **`SAFERM_HOME` env var** overrides `~/.saferm/` base dir. Used by tests for isolation. (Infrastructure, not config -- see the boundary note above.)
- **Exit codes:** 0 (success), 1 (general), 2 (usage), 3 (file not found), 5 (database), 6 (archive), 7 (conflict), 8 (database contention outlived the retry budget). Defined in `exitcodes.go`. A refusal the FRAMEWORK makes -- an unstated required flag, a missing argument, an unsatisfied constraint such as `purge`'s empty selection -- exits **1**, not 2: `exitcodes.go` covers what saferm's own handlers return, and a command line the parser never accepted never reaches one. 4 is deliberately absent: it was `ExitPermission`, which nothing ever returned, and the codes above it are not renumbered -- a new code takes the next number after the highest in use (8), so an old script's comparisons keep meaning what they meant. Config-layer failures (malformed/unknown-key/conflicting config) are strictcli's and exit **1**; saferm's own semantic conflicts (e.g. an undelete target already exists) exit **7** (`ExitConflict`). The two are distinct: exit 1 means the config could not be loaded/reconciled; exit 7 means saferm ran but hit a semantic conflict.
- **Version:** set via ldflags (`-X main.Version=x.y.z`) at build time; falls back to `debug.ReadBuildInfo`, then `"dev"`. The symbol name must match `.goreleaser.yml` exactly -- the linker silently ignores an `-X` naming a symbol that does not exist.

## Testing

- Unit tests in each `internal/` package (`*_test.go`).
- Integration tests in `internal/test/` -- builds the binary, runs as subprocess, uses `SAFERM_HOME` for isolation.
- Pre-push hook runs `go test ./... -short -count=1`.

## Distribution

- Go binary (GoReleaser via GitHub Actions)
- npm wrapper: `saferemove` (in `npm/`)
- PyPI wrapper: `saferm` (in `pypi/`)
- Homebrew tap: `smm-h/tap/saferm`

## Tooling

- **rlsbl** for releases (`rlsbl release run --no-allow-dirty --watch --approve-consequential`)
- **safegit** for commits (`safegit commit -m "message" -- file1 file2`)
- **selfdoc** for docs site (saferm.smmh.dev)
- **strictcli** for CLI framework

## How AI agents should use saferm

saferm is designed for AI agents to use instead of `rm`. Always provide a meaningful `--description`.

### Basic usage

```bash
# Delete a file (--on-error is mandatory: abort or continue)
saferm delete --on-error abort --description "Removing stale config after migration" old-config.yaml

# Delete a directory (requires -r)
saferm delete --on-error abort -r --description "Removing build artifacts after successful CI" ./build/

# Delete with force (ignore nonexistent files)
saferm delete --on-error abort -f --description "Pruning stale cache files" cache/*.json

# Archive as much of a batch as possible, and still fail at the end
saferm delete --on-error continue --description "Pruning generated reports" report-*.json

# Record the original rm command that was replaced
saferm delete --on-error abort --description "Cleaning temp test output" --command "rm -rf test-output/" -r test-output/

# Add custom metadata
saferm delete --on-error abort --description "Removing deprecated module" --meta reason=deprecated --meta ticket=PROJ-123 old-module.go
```

### Other commands

```bash
# List archived items
saferm list
saferm list --all              # include restored items
saferm list --path "/home/m/Projects/*"   # glob over the full original path; * spans directories
saferm list --path "*/build/*"            # anything archived from a build/ directory, at any depth

# Show full metadata for a deletion (numeric ID or uuid)
saferm info 42
saferm info 6f1c0e2a-6c9e-4a24-9d1f-2b0f3f5b7c11

# Restore a file (by uuid, ID or path)
saferm undelete 42
saferm undelete 6f1c0e2a-6c9e-4a24-9d1f-2b0f3f5b7c11
saferm undelete /path/to/file

# Restore where something is already standing: --on-conflict is required
# there and has no default (an absent destination, or the emptied original
# directory of an archived tree, needs no answer)
saferm undelete --on-conflict overwrite 42   # check the archived copy, then replace
saferm undelete --on-conflict abort 42       # refuse and change nothing

# Restore somewhere else; the path is recorded on the record
saferm undelete --destination /tmp/inspect/config.yaml 42

# Restore without touching the git index
saferm undelete --no-update-git-index 42

# Permanently remove from archive
saferm --approve-consequential purge 42 43 44             # by IDs or uuids
saferm --approve-consequential purge --older-than 30d     # by age (h/d/w/m)
saferm --approve-consequential purge --all                # everything
```

### Guidelines for --description

The description should explain **why** the deletion is happening, not just what is being deleted. Good examples:

- "Removing build artifacts after successful CI"
- "Cleaning up temp test file created during debugging"
- "Deleting deprecated config -- replaced by new TOML format"
- "Pruning stale cache files older than 30 days"

Bad examples (too vague):

- "cleanup"
- "removing file"
- "not needed"
