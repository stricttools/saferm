+++
title = "README.md"
+++
# saferm

The `rm` replacement that is safe to hand to your AI Agents: deleted files can be restored, know exactly which session deleted a file, when, and why, with the reason being a required flag

## Quick start

```
go install github.com/stricttools/saferm@v0
```

Or via Homebrew (macOS/Linux):

```
brew install smm-h/tap/saferm
```

Delete a file (`--description` and `--on-error` are both mandatory):

```
saferm delete --on-error abort --description "removing stale config" old-config.yaml
archived: [3] 6f1c0e2a-6c9e-4a24-9d1f-2b0f3f5b7c11 /home/user/project/old-config.yaml (612 B)
```

Every archived path is named with both of its identifiers: the numeric database id and the uuid. The uuid is the durable handle -- `undelete`, `info` and `purge` all take it.

See what you've archived -- the newest 50 entries, with a last line saying how many more there are (`--limit 0` shows them all, `--since 2d` only the last two days, and `--path` a glob over the original paths):

```
saferm list
```

See how much disk the archive takes, by age and by the directories things came from:

```
saferm usage
```

Bring it back:

```
saferm undelete old-config.yaml
```

A restore consumes the archived copy -- it is moved back out, not copied -- and it never overwrites anything by accident. If something is already standing at the destination, `--on-conflict` is required and has no default: `overwrite` checks the archived copy against the record before replacing what is there, `abort` refuses and changes nothing. `--destination <path>` restores somewhere else and writes that path to the record, so `info` names where the content went.

## Install

| Method | Command |
|--------|---------|
| Go | `go install github.com/stricttools/saferm@v0` |
| Homebrew | `brew install smm-h/tap/saferm` |
| npm | `npm install -g saferemove` (not yet published) |
| PyPI | `pip install saferm` (not yet published) |

## Commands

:-: table-commands

## Example workflow

```
$ saferm delete --on-error abort --description "broken migration, rewriting from scratch" -r db/migrations/
archived: [3] 6f1c0e2a-6c9e-4a24-9d1f-2b0f3f5b7c11 /home/user/project/db/migrations (14 KB)

$ saferm list
ID  PATH                   SIZE   DELETED
3   db/migrations/         14K    2 minutes ago

$ saferm info 3
ID:          3
UUID:        6f1c0e2a-6c9e-4a24-9d1f-2b0f3f5b7c11
Path:        /home/user/project/db/migrations/
Size:        14382
Type:        directory
Status:      restorable
Description: broken migration, rewriting from scratch
Deleted:     2026-05-16 14:32:01 UTC
Git branch:  feature/new-schema
Git HEAD:    a1b2c3d
Parent PID:  12345
Parent cmd:  claude

$ saferm undelete 3
Restored db/migrations/
```

## Identifiers and the error mode

`delete` prints one line per archived path carrying the record's numeric id and its uuid, so a caller never has to run `list` afterwards and guess which row was its own. `undelete`, `info` and `purge` accept either, and `undelete` also accepts an original path. An identifier argument is read by shape, in a fixed order: a 36-character hyphenated hex string is a uuid, an all-digit string is a numeric id, anything else is a path.

`--on-error` is mandatory on `delete` and has no default, because a batch that meets a bad path has two defensible answers and they suit opposite callers:

| Value | Behaviour |
|-------|-----------|
| `abort` | stop at the first failing path; everything archived before it keeps its record and its printed identifiers |
| `continue` | archive the remaining paths, report every failure, and exit non-zero at the end with the first failure's code |

Either way the identifiers of everything already archived are on stdout before the failure is reported.

`info` also states the record's status in one line: `restorable`, `restored at <time>`, `purged at <time>`, or both stamps when a record was restored and later purged. Where neither stamp is set and the archived copy is not there -- the state an archival leaves when it discards its entry because the source changed inside its window -- the status says so instead of claiming the record is restorable, and points at `purge` as the way to clear the row.

## Driving saferm from a program

`--json` puts saferm in machine mode, where stdout carries one document and nothing else -- the envelope -- and everything saferm would have printed is carried inside it. `delete`, `undelete`, `list`, `info`, and `usage` each answer with a structured payload: the records a delete wrote (both identifiers, path and size, plus the invocation's group id) and every path it could not archive with the reason, where a restore put the content, the rows of a listing with the total the selection matched, the full record with its status, origin, and group, and the archive's disk use in bytes. `purge` deliberately has no payload.

```
$ saferm --json capabilities
{"interface_version":3,"app":"saferm","command":"capabilities","exit_code":0,
 "payload":{"features":["git-index-switches","group-id","list-limit","list-since","machine-payloads",
 "on-conflict-modes","on-error-modes","restore-destination","trace-origin","usage-report","uuid-handles",
 "node-type-file","node-type-directory","node-type-symlink","node-type-fifo","node-type-socket",
 "node-type-character-device","node-type-block-device"]}, ...}
```

`capabilities` is how a program decides what this saferm can do. It names features, never a version -- a locally built binary reports a Go pseudo-version no semver parser accepts -- and a missing verb or a missing feature means the same thing as saferm not being installed. The verb reads nothing, so it answers on a machine where saferm has never run.

The payload schemas are declared in the code and published verbatim by `saferm help --json`, which is the one channel that carries them. The MCP tool descriptors (`saferm --mcp`) carry each command's effect classification and its argument schema, never its payload schema. The machine-surface page in the docs is the specification.

## Metadata

Every deletion automatically captures:

- **Description** -- the mandatory `--description` flag
- **Git context** -- branch, HEAD commit, repo root (auto-detected)
- **Environment variables** -- filtered by a configurable denylist to exclude secrets
- **Parent process** -- PID and full command line of the calling process
- **Claude Code session** -- via `CLAUDE_CODE_SESSION_ID` env var, if present
- **Custom metadata** -- arbitrary key=value pairs via `--meta`

## Storage

```
~/.saferm/
  archive/       files stored by UUID; directories as .tar.zst; symlinks as .symlink;
                 FIFOs, sockets, and devices as .node descriptors (never read)
  db/saferm.db   SQLite database (WAL mode)
  config.toml    optional configuration
```

Override the base directory with the `SAFERM_HOME` environment variable. `SAFERM_HOME` is *location infrastructure* -- the same category as `HOME` -- not a config value: it selects where saferm lives. Unlike config-file and environment *values*, `SAFERM_HOME` is not suppressed by `--hermetic`.

## Configuration

Optional file at `~/.saferm/config.toml`:

```toml
archive_dir = "/custom/archive"
db_path = "/custom/db.sqlite"
exclude_env_patterns = [
  "(?i)token",
  "(?i)secret",
  "(?i)password",
  "(?i)key",
  "(?i)credential",
]
```

The `exclude_env_patterns` list controls which environment variables are redacted from captured metadata. The values shown above are the defaults. Each entry is a Go regular expression matched against the variable *name*; Go uses RE2, so lookahead (`(?!...)`) and backreferences are not available. A pattern that does not compile is a hard error -- saferm refuses to run rather than proceed with a redaction it cannot apply.

A malformed `config.toml` is a hard error (exit 1) reporting the parse position, never silently ignored. Unknown keys are rejected, and for `archive_dir`/`db_path`, passing a CLI value that diverges from the config value is a hard error rather than silently letting one win. This conflict check only fires when the global flag is given in the pre-command position (`saferm --archive-dir X delete ...`); a post-command placement (`saferm delete --archive-dir X`) is not currently conflict-checked. `--hermetic` suppresses config-file and environment *values*, falling back to defaults -- but it does not touch `SAFERM_HOME`, which is infrastructure, not configuration.

## Concurrency

saferm is safe for concurrent use. UUID-based archive naming needs no coordination between processes, and the archive database is protected in two layers: SQLite's own `busy_timeout` waits up to 5 seconds for a lock held by another process, and saferm retries a contended operation up to 5 times in total on top of that, pausing 50ms, 100ms, 150ms and 200ms between attempts. Under `--verbose` each retry is reported on stderr.

Contention that outlives the whole budget is reported as such and exits **8** rather than the generic database code -- nothing is wrong with the archive, another process simply held the write lock throughout, and running the command again is the right response.

## Exit codes

:-: table-exit-codes

Config-layer failures -- a malformed `config.toml`, an unknown key, or a CLI value that conflicts with `archive_dir`/`db_path` in the config -- exit **1** (they are reported by the CLI framework before saferm runs). saferm's own semantic conflicts exit **7**. The distinction: exit 1 means the configuration could not be loaded or reconciled; exit 7 means saferm ran and hit a semantic conflict.

Exit **5** and exit **8** are likewise distinct: 5 means the database itself failed, 8 means another process held its write lock for longer than saferm's whole retry budget. 8 is the one exit code that says "try again". Code 4 is deliberately absent (it was a permission code nothing ever returned) and is never reused, so the numbers below it keep their meaning.

## Platforms

Linux and macOS (amd64, arm64).

## License

MIT

## Links

- GitHub: https://github.com/stricttools/saferm
- Docs: https://saferm.smmh.dev
