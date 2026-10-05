+++
title = "Architecture"
description = "How saferm's internal components fit together: archive storage for each kind of file (hard-linked files, compressed trees, symlink targets, special-file descriptors), SQLite metadata and the migration that repairs records older versions wrote, the checks guarding removal of the original, the restore path with its conflict mode, verification before an overwrite and partial-extraction rollback, and the origin and ancestry derived from the process trace store, and how the archive is read: oldest-first queries, the index-driven path filter, and the disk-use report."
+++

# Architecture

saferm replaces `rm` with a multi-stage deletion pipeline: archive the content, record metadata, and leave a trail that supports undo. This page explains how the internal components work together.

## Component overview

saferm is organized into five internal packages, each with a single responsibility. Four of them are fully independent of one another, communicating only through the top-level command handlers that orchestrate them; `internal/meta` is the one exception, since capturing the context of a deletion includes resolving who invoked it. This separation keeps the codebase modular and testable, with clear boundaries between physical storage, metadata persistence, context capture, ancestry resolution, and git integration:

| Package | Responsibility |
|---------|---------------|
| `internal/archive` | Physical file storage: moving files into the archive, compressing directories, restoring content |
| `internal/db` | Structured metadata: SQLite schema, CRUD operations, query filters, schema migrations |
| `internal/meta` | Context capture: environment variables, git state, parent process info, custom key-value pairs, the resolved ancestry chain |
| `internal/trace` | Ancestry resolution: reads the strictcli process trace store to answer "which tool ran this deletion" |
| `internal/git` | Git index management: detecting tracked files, staging removals on delete, staging additions on restore |

## Storage layout

All saferm data lives under a single root directory (default `~/.saferm/`, overridable via the `SAFERM_HOME` environment variable). This directory contains three categories of content: archived file data stored by UUID, an SQLite database tracking deletion metadata, and a TOML configuration file for persistent settings. The layout is self-contained, so relocating the entire directory moves all saferm state at once:

```
~/.saferm/
  archive/          # Archived file content
    <uuid>          # Regular file (hard-linked, or copied, from the original)
    <uuid>.tar.zst  # Directory (compressed archive)
    <uuid>.symlink  # Symlink metadata (target path as plain text)
    <uuid>.node     # Special file descriptor (node type, mode, device numbers)
  db/
    saferm.db        # SQLite database (WAL mode)
    saferm.db-wal    # WAL journal (SQLite-managed)
    saferm.db-shm    # Shared memory (SQLite-managed)
  config.toml       # User configuration (managed by strictcli)
```

## Deletion lifecycle

A deletion passes through five stages: validation, archival, metadata recording, removal of the original, and git index update. Each stage must succeed before the next begins, and failures at any point leave the system in a consistent state. The order is what makes that true: the archive entry is written while the original is still in place, the record is inserted next, and only then is the original removed. A record that fails discards the archive entry it just wrote and leaves the caller's path untouched, so there is no window in which content exists in the archive under a name nothing can resolve.

### 1. Validation

`archive.NewPlan` stats the target path and classifies it as one of the node types -- a regular file, a directory, a symlink, a FIFO, a socket, a character device, or a block device -- resolving where archiving it would put it without mutating anything. Any other type of file is refused with an `UnsupportedFileError` naming the type. Directories require the `-r` flag; without it, the operation fails with `ErrRecursiveRequired`. Missing files fail with `ErrFileNotFound` unless `-f` (ignore-missing) is set.

### 2. Archival

The archival strategy depends on the target's node type. Regular files are hard-linked into the archive, and copied with integrity verification where the link is refused. Directories are compressed into tar archives with zstandard compression. Symlinks store only their target path, and special files only a description of themselves, since neither carries content saferm can archive:

**Regular files.** The file is hashed (SHA-256, streaming), then hard-linked into the archive with `os.Link`: no content is copied and the archive entry and the original are the same inode until the original's name is removed. If the link is refused -- `EXDEV` across filesystems, `EPERM` from Linux's `protected_hardlinks` or a filesystem that rejects links, `EOPNOTSUPP`/`ENOSYS` where hard links do not exist, `EMLINK` at the inode's link limit -- the fallback path copies the file and verifies the copy's hash against the pre-computed one. Any other error is reported as itself. The archived file is stored as `<uuid>` (no extension) in the archive directory.

**Directories.** The directory tree is walked to compute total size, then compressed into a `.tar.zst` archive (tar format with zstandard compression via `github.com/klauspost/compress/zstd`). The tar preserves relative paths, permissions, symlinks, and special files within the tree: a FIFO or a device is written as tar's own entry type for it, and a socket, which tar has no entry type for, as a zero-length regular member carrying the PAX record `SAFERM.nodetype=socket`. Any other type of file in the tree refuses the archival. The original tree stays where it is until the deletion has been recorded; it is then removed with `os.RemoveAll`. Partial archives are cleaned up on failure.

**Symlinks.** The symlink's target path is read via `os.Readlink` and written to a `.symlink` metadata file in the archive directory. The symlink itself is removed after the deletion is recorded, like every other node type. No content is archived because symlinks have no content -- the target path is sufficient for reconstruction.

**Special files.** A FIFO, a socket, or a character or block device is never opened: reading a FIFO would block until a writer appeared and then drain what it sent, a socket cannot be opened at all, and a device would be read to its end. What the archive keeps is a `.node` descriptor of the node's type, its mode bits, and for a device its major and minor numbers, and the record's hash is the descriptor's SHA-256. Every read of a regular file's content -- the hash, the copy fallback, a tree member going into the tar -- opens it non-blocking and without following a symlink and refuses anything the opened descriptor shows is not a regular file, so a path swapped for a FIFO between the stat and the read fails the archival instead of blocking it.

### 3. Metadata recording

After the archive entry is written -- and while the original is still on disk -- the command handler collects contextual metadata via the `meta` package and inserts a `DeletionRecord` into the SQLite database. An insert that fails removes the archive entry again (`archive.DiscardBlob`) and reports that the path was left in place; a discard that itself fails is reported by name, because that is the one remaining way to end up with an archive entry no command can resolve. This metadata is what makes saferm's deletions auditable and reversible: every record links the archived content to its original location, captures who deleted it and why, and stores enough context to understand the circumstances of the deletion months later. The record captures:

- **Identity**: auto-increment ID, UUID (matches archive filename), original absolute path, original filename
- **Content**: file size (bytes), SHA-256 hash, directory flag, symlink target
- **Context**: deletion timestamp, user-provided description, optional original rm command, JSON metadata blob
- **Origin**: `origin_name`/`origin_version` -- which tool ran the deletion, derived from the process trace store (see below)
- **Batch**: `group_id`, shared by every record one delete invocation writes
- **Lifecycle**: `restored_at`/`restored_to` (set on undelete), `purged_at` (set on purge)

:-: ref path="internal/db" target="DeletionRecord"

The metadata JSON blob, produced by `meta.Collect`, includes:

- **Environment variables**: all env vars except those matching configurable denylist patterns (secrets, tokens, keys, credentials by default)
- **Git context**: current branch, HEAD SHA, and repository root -- empty strings when not in a git repo
- **Parent process**: PPID and parent command line (read from `/proc/<pid>/cmdline` on Linux, `ps` on macOS)
- **Custom pairs**: arbitrary key-value strings passed via `--meta key=value`
- **Ancestry**: the chain of invocations that led to this deletion, resolved from the process trace store

:-: ref path="internal/meta" target="Collect"

### Origin and ancestry

Every deletion records which tool ran it, and neither field is ever declared by a caller. saferm derives them: it reads `STRICTCLI_TRACE_PARENT` from its own environment, resolves that entry from the shared process trace store at `~/.local/share/strictcli/trace/`, and takes the entry's declared `app` and `version` into `origin_name` and `origin_version`. Both columns are null when nothing claimed the invocation, which is the ordinary state of a deletion run from a shell and of every deletion recorded before callers carried the variable. There is no `--origin` flag anywhere, nothing is inferred from any other environment variable, and history is not backfilled.

The store is a shared, append-only JSONL record of process ancestry: at the seam where one tool spawns another, the spawning invocation appends one line describing itself and hands that line's identifier to the child. saferm is a consumer of it, never a writer, and reads it itself -- the framework deliberately exposes no accessor, because nothing in the framework may branch on ancestry.

Resolution happens at capture time and keeps the whole chain, not only the identifier:

- The identifier is parsed under a strict profile: exactly 26 canonical-uppercase Crockford base32 characters, lowercase and 128-bit overflow rejected rather than repaired.
- The entry is found by binary-searching the partition filenames. A filename is its range start, and a writer whose clock reads earlier than the active range clamps its identifier into it, so no entry lands in a file whose range begins after it -- one search, one file read in the ordinary case. The clamp is one-sided, though: nothing stops a writer that already selected the active partition from appending an entry whose timestamp belongs to the file another writer has meanwhile rolled to, which strands the entry in an older-labelled file. So a miss at the searched file walks backward through the older partitions until the entry is found or the labels run out, and each partition is parsed at most once per capture.
- `parent_id` is then walked to the root, and the flattened entries are written into the metadata blob under `trace`, alongside the bare identifiers for correlation with whatever store data still exists. Embedding rather than referencing is what makes a record self-contained: pruning or compressing old partitions can never orphan it.

The capture is observational and cannot fail a deletion. A polluted variable, a missing or pruned store, a torn line, a line missing one of the entry's thirteen keys, or a parent that resolves to nothing is each recorded as an anomaly under `trace.anomalies`, and the deletion proceeds. That is deliberate: a dangling parent noticed by a consumer is the trace store's own primary failure-detection channel, so an anomaly that vanished silently would be indistinguishable from a chain that was fine.

What the store can put into a record is bounded, because the whole capture is copied into every row the invocation writes and nothing about a partition's contents is under saferm's control. An anomaly keeps the first 1024 bytes of what it saw and says that it truncated; a capture keeps the first 32 anomalies and then one synthetic `anomalies-dropped` line naming how many more there were; every unbounded string an entry contributes to the embedded chain -- `app`, `version`, `command`, `effect`, `spawned_at`, `parent_id` -- is capped at 1024 bytes with the capping itself recorded as an `oversized-entry-field` anomaly. A partition larger than 64 MB, or a store file that is not a regular file at all, is skipped and recorded rather than read: the specification's own worst case is 8 MB plus one hour of writes, so anything past that is not a partition this reader will spend memory on, and a FIFO named like a partition would otherwise block the deletion forever.

:-: ref path="internal/trace" target="Collect"

The version-requires-name invariant -- a record may not carry `origin_version` without `origin_name` -- is enforced in `db.Insert` rather than by a CHECK constraint. SQLite cannot add a constraint to an existing table without rebuilding it, and code-level enforcement is one path covering fresh and migrated databases alike. The accepted cost is stated rather than hidden: a hand-edited database, or a binary from before the migration, bypasses it.

### 4. Removal of the original

`archive.RemoveSource` removes what was archived -- by identity, not by name, and only once it has seen the archived copy. The database insert sits between the archival and this removal, and a contended SQLite write retries for tens of seconds while both the source path and the archive entry stay live, so both sides are re-checked against what `archive.Execute` recorded before anything is destroyed:

- **Identity**, for every node type: the path must still resolve to the same inode (`os.SameFile` against the `os.FileInfo` taken at archival). A path that was renamed over or removed and recreated in the meantime is a different file, and removing it would destroy something nothing archived.
- **The archive entry's existence**, for every node type: the entry must still be there (`os.Lstat`, never `os.Stat` -- a `Stat` follows a symlink, so an entry replaced by a link back at the source would satisfy `os.SameFile`). The record is inserted before the removal, which is what makes the archived copy findable -- including by a concurrent `saferm purge --all`, which will select that row and destroy its blob perfectly legitimately. Removing the source with the entry gone would leave no copy of the content anywhere.
- **Content**, for regular files: the archive entry is a hard link, so a write through the original path rewrites the archived bytes and leaves the recorded hash describing content that no longer exists. The size and mtime as of the hash are compared against the current stat, and the file is re-hashed only when they differ, so a plain `touch` is not mistaken for a rewrite.
- **The node, for special files**: the node type, mode bits, and device numbers must still be the ones the descriptor holds, because the identity check can be fooled by a reused inode number.
- **Coverage, for directories**: a tree's identity is one inode and says nothing about what is inside it, so the tree is walked again against the member list the archiving walk recorded (every path, with its node type and the size and mtime it had as it went into the tar). A path the tar does not hold at all, a path whose node type changed, a regular file whose size or mtime no longer matches, or a special file whose mode or device numbers changed refuses the removal -- `os.RemoveAll` would otherwise destroy a file written into the tree after the tar was closed, which is in no archive anywhere. A path the tar holds and the tree no longer does is not a refusal: the archive then covers more than the tree, which is what a completed archival aims for.

A mismatch refuses the removal and exits `6` (`ExitArchive`). Where the record is still truthful -- a replaced path, or a diverged source whose entry is an independent copy -- nothing is undone, and the failure names both what the record holds and what the path holds now. Where it is not -- a hard-linked file written through, whose recorded hash no longer matches the blob -- the archive entry is discarded, which drops one of two names for the inode and leaves the file in place with its current content, and the caller is told the row now names nothing and to run the delete again. Where the entry itself is gone or is no longer what was archived (`ErrArchiveEntryMissing`, `ErrArchiveEntryReplaced`), nothing is removed and nothing is discarded: the row names nothing, the source is the only copy of its content left, and the message says so rather than claiming the record holds the archived content. Where a tree changed under the archival (`ErrDirectoryChanged`), the tree is left whole -- including the part nothing archived -- and the incomplete `.tar.zst` is discarded, so the row names nothing and the caller is told to run the delete again.

Two residual windows stay open deliberately, both of them microseconds wide against the tens of seconds the insert can take: a write to a regular file that restores both its size and its mtime is not detected without a full re-read, and a write into a tree landing between the verification walk and the `os.RemoveAll` that follows it is not seen at all.

:-: ref path="internal/archive" target="RemoveSource"

### 5. Git index update

When `--update-git-index` is true (the default) and the file resides in a git repository, saferm runs `git rm --cached` to stage the removal in the git index. This keeps the git working tree consistent with the filesystem without requiring a separate `git rm` step. The check uses `git ls-files --error-unmatch` to determine whether the file is tracked; untracked files are silently skipped.

:-: ref path="internal/git" target="GitRmCached"

## Restoration (undelete)

Restoration reverses the archival process, moving content from the archive back to its original location on disk. It does not mirror archival, which links rather than moves: a restore **always consumes** the archived copy. There is no keep mode and no per-restore flag for one — keeping a copy in the archive after restoring it is the parking workflow saferm exists to prevent, and a restored file that is wanted in the archive again is simply deleted again. A record already restored has nothing left to restore, and `undelete` says so in the record's own vocabulary rather than failing at the archive layer.

`undelete --destination <path>` restores somewhere other than the record's original path. The path is resolved to an absolute one and written to the record's `restored_to` column, so `info` names where the content actually went.

### The step list

A restore is one list of steps built from one `RestorePlan`, walked by both modes: in `--dry-run` every step is recorded on the effects handle, otherwise the handle performs the steps it can and the archive package performs the rest. The effects handle's closed method set covers removing an occupied destination, making the parent directory, renaming a file out of the archive and dropping a consumed entry; it has no primitive for recreating a symlink, recreating a special file, or extracting a tar+zstd tree, so those are described on the handle and performed beside it. Building the list once is what keeps the preview and the real restore from drifting apart — the real path used to bypass the handle entirely, with only the dry branch minting anything.

- **Regular files**: `Rename` from the archive to the destination, with a cross-device copy fallback that removes the entry only once the copy is complete
- **Directories**: extract the `.tar.zst` into the destination (stripping the top-level directory entry so contents land directly there), then remove the container
- **Symlinks**: recreate the link via `os.Symlink` from the recorded target, then remove the `.symlink` entry
- **Special files**: decode the `.node` descriptor, refuse one whose node type is not the record's, recreate the node with `mkfifo` or `mknod` and its exact mode bits, then remove the descriptor. A socket is recreated on Linux only. A device needs the privilege `mknod` asks for (`CAP_MKNOD` on Linux, root on macOS); without it the restore fails and the descriptor stays, so the record is still restorable. A FIFO, socket, or device inside a tree is recreated the same way during the extraction, and a tar member type the extraction has no case for fails the restore rather than being skipped

The ordering carries one invariant: **the archived copy is consumed last**. A file's move out of the archive is itself the consumption and cannot half-happen; every other node type writes the destination first and drops the entry only once that has worked. Any failure therefore leaves the entry in place and the record restorable.

### Conflicts and the empty-destination rule

`--on-conflict` is required exactly when something is standing at the destination, with no default and two values: `overwrite` replaces what is there, `abort` refuses and changes nothing. Omitting it when the destination is occupied is an argument error (exit `2`) naming both values; `abort` is a stated refusal and exits `7` (`ExitConflict`). There is no keep-both or backup mode.

An **empty destination directory is not a conflict for a tree**: it is that tree's own original place, emptied, and extracting into it replaces nothing. The rule is for directory records only — an empty directory standing where a *file* was archived is still occupied, because a file cannot be renamed over a directory and removing it is a decision the caller has to state.

### Verification before an overwrite

An overwrite reads the archived copy through once **before** the destination is touched. Restoration used to remove the destination first and read the archive afterwards, so a corrupted or truncated copy cost the caller whatever was standing there.

The recorded hash means a different thing per node type, and verification defines each:

| Node type | What the record holds | What verification proves |
| --- | --- | --- |
| Regular file | SHA-256 of the file's content, and the entry *is* that file | Exact: a byte that rotted in the archive is found |
| Directory | SHA-256 of the `.tar.zst` **container** | The container arrived intact. There is no per-member digest anywhere in the archive, so nothing here can promise anything about individual extracted members |
| Symlink | Nothing — a symlink has no content and its recorded hash is empty by construction | The entry still names the target the record names. A hash comparison would fail on every symlink ever archived |
| Special file | SHA-256 of the `.node` descriptor, which is everything the archive holds of the node | Exact, and the descriptor decodes to the node type the record names |

A file, tree, or special file whose record carries no hash at all is `ErrUnverifiable` rather than a pass: the caller asked to destroy a destination on the strength of a check nothing can make.

Verification is proportional. A restore into an absent or empty destination gets **no** verify pass — a corrupt copy simply fails the restore, which destroys nothing and keeps the copy for a retry.

### Partial extraction

A tree extraction that fails partway leaves a half tree at the destination, and that half tree is **taken back**: the destination held nothing but bytes the extraction had just written there (it was absent, or empty, or removed by a verified overwrite), and the content is still in the archive because the entry is consumed only on success. Directories the extraction created are removed with `os.Remove` in reverse order, so a directory that is somehow not empty holds something the extraction did not write and is reported as stuck rather than destroyed; a destination directory the extraction *found* rather than created survives. The failure names what had been extracted before it, and states that the record is still restorable.

### Git index

If the destination is inside a git repository and `--update-git-index` is true (the default), `git add` stages the restored path. The switch mirrors the delete side's, so a programmatic caller can turn the index side effects off on both halves of the round trip.

:-: ref path="internal/archive" target="VerifyEntry"

## Purge (permanent destruction)

Purging permanently removes the archived content while preserving the metadata record. The archive file (`<uuid>`, `<uuid>.tar.zst`, `<uuid>.symlink`, or `<uuid>.node`) is deleted from disk, and the database record's `purged_at` field is set. This means `saferm list --all` still shows the deletion history, but `saferm undelete` will refuse to restore a purged item because the content is gone.

Records can be selected for purging by record UUID or numeric ID, by age (`--older-than`), by size (`--larger-than`), or all at once (`--all`). The age, size, and all-at-once selections choose among the records still in the archive; a restored record's content is back where it came from and a purged record's is already destroyed, so neither is selected. The dry run's `freeing ~<size>` sums what the selected archive entries occupy on disk as the filesystem allocates them -- a directory's `.tar.zst`, not the tree it was made from -- and leaves out a file entry another name outside the archive still links, because removing the archive's name frees nothing while that link remains; it says how many such entries were selected. At least one of those four must be given, and that rule is a declaration rather than a check inside the handler: the constraint `purge-selection` is registered with the command, rendered in `saferm purge --help`, published in the dumped schema, and enforced by the parser before dispatch. A purge that selects nothing is refused with `constraint "purge-selection": at least one of targets, --older-than, --larger-than, --all is required` and exits 1. `--all` counts only when it is true, so `--no-all` declines the option rather than choosing one, and the refusal says so.

## Reading the archive

Every read of more than one record asks SQLite for oldest-first order (`deleted_at` ascending, then `id` ascending) and reverses the result in memory, so `list` still shows the newest first, with records deleted in the same second ordered highest id first. A b-tree walked backward is read one page at a time in descending file order, which defeats read-ahead on a copy-on-write, compressing filesystem: on an archive of about 25,000 records in daily use, reading every row newest-first took 36 to 191 seconds from a cold cache, and the same rows read oldest-first took 0.5 to 2.5 seconds.

`list --path` does not read every row. It takes the pattern's literal prefix -- everything before the first `*`, `?`, `[`, or `\` -- reads `(id, original_path)` for the paths in that prefix's range out of the `original_path` index alone, matches those paths with the same matcher `list` has always used, and fetches only the matching rows by id. The pattern is checked for syntax before anything is read, so a malformed one is a usage error whatever the archive holds.

`usage` reports the archive's disk use without changing anything: it stats every file in the archive directory (counting a hard-linked inode once) and every live record's entry, and groups the records by age and by original directory.

## Cross-device handling

When the source file and the archive directory reside on different filesystems (different mount points, network shares, container bind mounts), `os.Link` fails with `EXDEV`. saferm detects this specific error by unwrapping the `*os.LinkError` and checking for `syscall.EXDEV`, and treats three further system error values the same way (`EPERM`, `EOPNOTSUPP`/`ENOSYS`, `EMLINK`): each means the filesystem or the kernel's policy will not link this file, not that the archival has failed.

The fallback path for files is copy-and-verify: copy the content, compute the SHA-256 hash of the copy, compare it against the hash of the original. If the hashes do not match, the copy is deleted and the operation fails with `ErrHashMismatch`. The original is removed later, after the record exists, exactly as in the linked case. This ensures no data loss even when hard links are unavailable.

Directories always use tar+zstd compression, which inherently handles cross-device scenarios because `createTarZst` reads the source tree and writes to the archive directory independently.

Restoration also handles cross-device: `restoreFile` attempts `os.Rename` first and falls back to copy-then-delete on `EXDEV`.

## Integrity verification

saferm uses SHA-256 hashing to verify file integrity at two points in the deletion lifecycle. This ensures that archived content is identical to the original, catching corruption from disk errors, interrupted copies, or filesystem bugs before the original file is removed. The hashing is streaming-based, processing data through an `io.Copy` pipeline into `crypto/sha256`, so memory usage remains constant regardless of file size:

1. **At archival time**: every regular file is hashed before being moved or copied. The hash is stored in the database record. For directories, the hash covers the `.tar.zst` archive file itself, and for special files the `.node` descriptor.

2. **At cross-device copy time**: after copying a file to the archive, the copy is hashed and compared against the pre-archival hash. A mismatch aborts the operation and removes the corrupt copy.

Hashing is streaming (`io.Copy` into `crypto/sha256`), so memory usage is constant regardless of file size.

## Concurrency model

saferm is designed for concurrent use by multiple processes, such as parallel AI agent sessions running simultaneously in the same directory. Three mechanisms prevent conflicts: UUID-based archive naming guarantees unique filenames without inter-process coordination, WAL-mode SQLite allows concurrent reads alongside writes, and every database operation carries a bounded retry for the lock contention WAL mode cannot avoid. Together, these ensure that simultaneous deletions never corrupt each other's data:

### UUID-based archive naming

Every archived item receives a UUID v4 generated from `crypto/rand`. This guarantees unique filenames in the archive directory even when multiple saferm processes archive files simultaneously. There is no coordination between processes -- each generates its own UUID independently.

### WAL-mode SQLite

The database connection is opened with two pragmas passed via the SQLite DSN connection string. These pragmas configure write-ahead logging for concurrent access and a busy timeout to handle lock contention when multiple saferm processes operate simultaneously on the same archive:

- `journal_mode=WAL`: Write-Ahead Logging allows concurrent readers and a single writer without blocking. Multiple saferm processes can query the database while another is inserting a new deletion record.
- `busy_timeout=5000`: if a write lock is held by another process, SQLite retries for up to 5 seconds before returning `SQLITE_BUSY`. This handles the brief window where two processes try to insert simultaneously.

:-: ref path="internal/db" target="Open"

### Bounded contention retry

The busy timeout covers brief overlaps; a lock held longer than five seconds still surfaces as `SQLITE_BUSY`, and before this retry existed that raw driver error travelled all the way out to the caller as an ordinary database failure. saferm now classifies it and retries around it.

- **Classification.** `IsContention` reads the driver's result code rather than its message, and compares the low byte, so every extended flavour of `SQLITE_BUSY` and `SQLITE_LOCKED` (`SQLITE_BUSY_SNAPSHOT` and the rest) classifies with its primary code. Nothing else in the database layer is retried.
- **Budget.** Five attempts in total, the first one included, with a linear backoff of 50ms before the first retry and 50ms more before each subsequent one -- 500ms of waiting on top of SQLite's own. The numbers are the ones saferm's concurrency tests previously hand-rolled around the binary, which is the measured record of what the suite needed.
- **Scope.** Every operation on the database goes through it, reads included, along with the schema creation and migration that run at open time. All of them are safe to run again: a statement that met `SQLITE_BUSY` never took the write lock, so it never took effect.
- **Reporting.** Under `--verbose` each retry prints on stderr, naming the attempt and the pause. It is stderr rather than stdout because for `list` and `info` stdout is the command's output, and `--quiet` never touches stderr.
- **Exhaustion.** A lock that outlives the whole budget produces a `ContentionError`, which the commands map to exit code 8 instead of the generic database code 5. The distinction is actionable: 5 means the database failed, 8 means the caller should run the command again.

:-: ref path="internal/db" target="IsContention"

### Schema migrations

The database uses `PRAGMA user_version` to track schema version. Migrations are idempotent: each checks whether the target column already exists (via `PRAGMA table_info`) before changing anything. This prevents errors when multiple processes race through the migration path on first run.

Every schema change lands twice -- in the `CREATE TABLE` that builds a fresh database, and in the version ladder that upgrades an existing one -- or the two shapes silently fork. Migrations 1 to 3 each add a nullable column. Migration 4 rebuilds the table in one transaction: a `node_type` column, checked against the list of node types, replaces `is_directory` and the inference of a symlink from a non-null `symlink_target`, every row's node type is derived the way the readers derived it, and the id counter is carried across. A saferm from before migration 4 can neither read nor write the rebuilt table, so every saferm on a machine is upgraded together.

Records written before saferm recognized symlinks and special files hold a symlink or a FIFO at `<uuid>` under a file record. Migration 4 repairs them: it reads every record's entry with `Lstat`, writes each such entry in its node type's form (`<uuid>.symlink` holding the target, or a `<uuid>.node` descriptor), and gives the record that node type -- a repaired symlink with no hash and size 0, a repaired FIFO with its descriptor's hash. Any other contradiction refuses the migration as a whole, naming the records, and nothing changes. The order keeps a failure safe: the database's write lock is taken first, every repair is planned before anything changes, the new entries are written and flushed, the database is committed, and the old entries, which no record names any more, are removed last. A failure before the commit takes the new entries back, so the database and the archive are either unchanged or fully migrated. An old entry that cannot be removed after the commit fails the command that ran the migration, naming the path; the migration itself is complete.

## Security considerations

### Tar extraction safety

The `extractTarZst` function includes path traversal protections to prevent malicious or corrupted tar archives from writing outside the intended destination directory. Every tar entry's path is validated against three rules before extraction proceeds, ensuring that archived directories cannot escape their restoration target:

- Absolute paths (starting with `/`) are rejected
- Parent directory references (`..`) are rejected
- After joining with the destination directory, the resolved absolute path is checked to ensure it remains within the destination tree

### Secret filtering

Environment variable capture excludes values matching configurable regex patterns. The default patterns filter out variables whose names contain `token`, `secret`, `password`, `key`, or `credential` (case-insensitive). Additional patterns can be added via the `--exclude-env-patterns` flag or `config.toml`.

### Archive permissions

The archive directory is created with mode `0700` (owner-only access), preventing other users on the system from reading archived content. This is particularly important because saferm captures environment variables and git context as part of the deletion metadata, which may contain paths, branch names, or other contextual information that should remain private to the user who performed the deletion.
