+++
title = "Configuration"
description = "How saferm finds its archive and database, plus SAFERM_HOME, the config file, hermetic mode, the SQLite schema, the bounded contention retry, and environment-variable filtering."
+++

# Configuration

saferm's configuration controls where deleted files are stored and how the tool behaves. There are three layers: the `SAFERM_HOME` environment variable (infrastructure), the config file (persistent settings), and CLI flags (per-invocation overrides).

## SAFERM_HOME and the default archive location

The `SAFERM_HOME` environment variable sets the root directory for all saferm data, including the archive directory, the SQLite database, and the config file. When unset, it defaults to `~/.saferm/`. This variable is classified as infrastructure, not configuration, which means it is never suppressed by hermetic mode and behaves like `HOME` -- it tells saferm where to find its data.

The root directory contains:

- `archive/` -- archived files and directories
- `db/saferm.db` -- SQLite database tracking all deletions
- `config.toml` -- persistent configuration file

All three paths derive from `SAFERM_HOME` by default, but `archive_dir` and `db_path` can be individually overridden via config or CLI flags.

```bash
# Use a custom location for all saferm data
export SAFERM_HOME=/data/saferm

# Resulting default paths:
#   /data/saferm/archive/     (archive directory)
#   /data/saferm/db/saferm.db (database)
#   /data/saferm/config.toml  (config file)
```

saferm creates all necessary directories automatically with `0700` permissions on first use.

## Config file

The config file lives at `$SAFERM_HOME/config.toml` (default: `~/.saferm/config.toml`). It uses TOML format and is managed through strictcli's built-in config system. The file stores persistent settings like custom archive directory and database path, and supports view, edit, and reset operations through the `saferm config` subcommand family. A malformed config file is always a hard error with a parse position, never silently ignored.

### Configurable fields

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `archive_dir` | string | `$SAFERM_HOME/archive` | Directory where deleted files are archived |
| `db_path` | string | `$SAFERM_HOME/db/saferm.db` | Path to the SQLite database |

Use `saferm config` subcommands to manage the file:

```bash
# Show all config values and their sources
saferm config show

# Set archive directory
saferm config set archive_dir --value /mnt/backup/saferm/archive

# Reset a key to its default
saferm config set archive_dir --default

# Empty a repeatable key
saferm config set exclude_env_patterns --clear

# Open config file in $EDITOR
saferm config edit

# Print config file path
saferm config path
```

### Conflict mode

Both `archive_dir` and `db_path` use `error` conflict mode. If the config file sets a value and a different value is passed via CLI flag in the pre-command position (`saferm --archive-dir /x delete ...`), saferm exits with an error rather than silently picking one. This prevents accidental operations against the wrong archive.

A malformed `config.toml` is always a hard error (exit 1) with a parse position. It is never silently ignored. Unknown keys in the config file are also hard errors.

## Infrastructure vs. config boundary

saferm distinguishes between **infrastructure** settings and **config** settings. This distinction matters for hermetic mode, which suppresses config values but never infrastructure values. Understanding the boundary prevents confusion when using `--hermetic`: infrastructure controls where saferm lives on disk, while config controls how saferm behaves within that location.

**Infrastructure** (`SAFERM_HOME`): selects _where_ saferm lives. It is in the same category as `HOME` -- a location override that tells saferm which directory tree to use. Infrastructure settings are never suppressed.

**Config** (`archive_dir`, `db_path` in `config.toml`): selects _how_ saferm behaves within that location. Config values can be suppressed by hermetic mode.

## Hermetic mode

The `--hermetic` flag suppresses all config-file and environment values for config-managed settings, forcing saferm to use default paths derived from the `SAFERM_HOME` root directory. When active, `archive_dir` and `db_path` fall back to their defaults regardless of what the config file specifies. This is useful for testing and automation, where you need predictable behavior that is not influenced by a user's custom configuration.

`SAFERM_HOME` itself is **not** suppressed by `--hermetic` because it is infrastructure, not config. A hermetic invocation still respects the `SAFERM_HOME` location -- it only strips the behavioral overrides from the config file.

```bash
# Uses SAFERM_HOME (or ~/.saferm/) with default subdirectory layout,
# ignoring any archive_dir or db_path set in config.toml
saferm --hermetic delete --on-error abort --description "reason" file.txt
```

## Archive directory discovery

saferm resolves the archive directory through a three-level priority chain, where the first non-empty value wins. CLI flags take highest priority, followed by config file values, and finally the default path derived from the saferm home directory. The hermetic flag short-circuits this chain by suppressing the config layer entirely:

1. CLI flag `--archive-dir` (pre-command position)
2. Config file key `archive_dir` (suppressed by `--hermetic`)
3. Default: `$SAFERM_HOME/archive` (where `SAFERM_HOME` defaults to `~/.saferm/`)

The archive directory is created automatically with `0700` permissions if it does not exist.

### Archive storage format

- **Files** are stored as bare files named by UUID (e.g., `archive/a1b2c3d4-...`)
- **Directories** are compressed into `.tar.zst` archives (e.g., `archive/a1b2c3d4-....tar.zst`)
- **Symlinks** store their target path in a `.symlink` metadata file (e.g., `archive/a1b2c3d4-....symlink`)

Cross-device moves (when the archive is on a different filesystem) are handled via copy-and-verify with SHA-256 integrity checks.

## SQLite database

### Path resolution

The database path follows the same three-level resolution order as the archive directory, with CLI flags taking highest priority, then config file values, and finally the default derived from the saferm home directory. Hermetic mode suppresses the config layer here as well, forcing the database path back to its default location:

1. CLI flag `--db-path` (pre-command position)
2. Config file key `db_path` (suppressed by `--hermetic`)
3. Default: `$SAFERM_HOME/db/saferm.db`

### Database configuration

The database opens with two SQLite pragmas that configure it for concurrent access by multiple saferm processes. These settings are applied at connection time through the DSN string and cannot be overridden by the user, ensuring that the database always operates in a mode safe for parallel AI agent sessions:

- `journal_mode=WAL` -- write-ahead logging for concurrent read/write access
- `busy_timeout=5000` -- wait up to 5 seconds for locks, supporting multiple simultaneous saferm sessions

Above those pragmas, saferm applies its own bounded retry: an operation that still meets `SQLITE_BUSY` or `SQLITE_LOCKED` after the busy timeout is tried again, up to five attempts in total, pausing 50ms, 100ms, 150ms and 200ms between them. Each retry is reported on stderr under `--verbose`. Contention that survives the whole budget exits **8** rather than the generic database code, which tells a caller to run the command again rather than to investigate the archive. Neither the retry budget nor the pragmas are user-configurable.

### Schema

The database has a single `deletions` table that stores the complete lifecycle of every archived item. Each row tracks the original file identity, content hash, deletion context, and restoration or purge status. Schema migrations are tracked via SQLite's `PRAGMA user_version` and run automatically on database open, so the table evolves safely across saferm upgrades:

| Column | Type | Description |
| --- | --- | --- |
| `id` | INTEGER | Auto-incrementing primary key |
| `uuid` | TEXT | Unique identifier linking to the archive file |
| `original_path` | TEXT | Absolute path of the deleted file |
| `original_name` | TEXT | Base name of the deleted file |
| `size` | INTEGER | File size in bytes (total for directories) |
| `hash` | TEXT | SHA-256 hex digest |
| `is_directory` | INTEGER | 1 if the entry was a directory |
| `deleted_at` | TEXT | RFC 3339 timestamp of deletion |
| `command` | TEXT | Original rm command being replaced (optional) |
| `description` | TEXT | Mandatory explanation of why the deletion happened |
| `metadata` | TEXT | JSON blob with environment, git context, and process metadata |
| `restored_at` | TEXT | RFC 3339 timestamp of restoration (null if not restored) |
| `restored_to` | TEXT | Path where the file was restored (null if not restored) |
| `symlink_target` | TEXT | Original symlink target (null if not a symlink) |
| `purged_at` | TEXT | RFC 3339 timestamp of permanent removal (null if not purged) |

Indexes exist on `original_path` and `deleted_at` for query performance.

Schema migrations are tracked via SQLite's `PRAGMA user_version` and run automatically on database open.

## Environment variable filtering

saferm captures environment variables as part of deletion metadata to provide full context about the circumstances of each deletion. Sensitive variables are excluded by regex patterns that match against variable names, filtering out tokens, secrets, passwords, keys, and credentials by default. Additional patterns can be added via the `--exclude-env-patterns` flag to extend the denylist for project-specific secrets. The default exclusion patterns are:

- `(?i)token`
- `(?i)secret`
- `(?i)password`
- `(?i)key`
- `(?i)credential`

Custom patterns can be set via the `--exclude-env-patterns` flag (repeatable). This flag is CLI-only and not configurable via `config.toml`.

Patterns are Go regular expressions, which means RE2: lookahead and backreferences do not exist there, and a pattern borrowed from a flavour that has them will not compile. An exclude pattern that does not compile is a hard error naming the offending entry -- saferm refuses to run rather than proceed with a redaction it cannot apply.
