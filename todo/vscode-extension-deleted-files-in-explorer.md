# VS Code extension: show files deleted through saferm in the editor, and restore them

## Context

saferm replaces `rm` with archival: every deletion is recorded in the SQLite database under `~/.saferm/` (or `SAFERM_HOME`), the content goes into the archive (files by uuid, directories as `.tar.zst`), and `undelete` brings it back. Agents working in a project delete through saferm, so a project accumulates a history of deletions that is invisible from the editor: a human looking at the file tree in VS Code has no way to see that `db/migrations/` existed a minute ago, who removed it, why, or that it can come back with one command.

An editor extension turns that history into something visible where the files live. The idea: files deleted through saferm show up in the file tree as ghost entries at their original location, with the recorded description, time, and origin on hover, and a restore action on them.

saferm already has the machine surface such an extension needs: `--json` makes the envelope the only document on stdout, and `list`, `info`, `delete`, and `undelete` each declare a payload schema (published by `saferm --dump-schema`). `capabilities` names the features a binary ships, which is how a program decides whether this saferm can drive it.

## Problem

- Deletions through saferm are recoverable but not discoverable: recovery needs the caller to know saferm exists, to run `saferm list --path ...`, and to read identifiers off a table.
- Humans supervising agents in VS Code see files vanish from the explorer with no trace, while the reason for every deletion is sitting in the database.
- VS Code's own delete action sends a file to the OS trash, a record-free path beside saferm's. The editor offers no way to delete through saferm with a description.

## Proposed extension

The extension is written in TypeScript and drives the `saferm` binary as a subprocess, always with `--json`, parsing the envelope. It never reads saferm's database or archive layout directly (see the solutions section for why).

### Features

1. **Ghost entries for deleted files.** For each workspace folder, the extension runs `saferm --json list --path "<folder>/*"` (the glob's `*` spans directory separators, so this reaches any depth) and shows the restorable records at their original paths, dimmed and marked as deleted. Restored and purged records are left out, since `list` without `--all` shows only records still held. Where a path was deleted more than once, every record is shown, newest first, each with its own uuid.
2. **Decorations on real folders.** A `FileDecorationProvider` puts a badge and a tooltip on existing folders in the ordinary explorer that contain deleted entries, so the file tree itself signals where ghosts are, even when the dedicated view is collapsed. Decorations can only attach to URIs the explorer already shows, which is why this complements the ghost view rather than replacing it.
3. **Details on hover and in a panel.** Hovering a ghost shows the record's description, deletion time, size, kind, and status; a "Show details" action runs `saferm --json info <uuid>` and renders the full record (git branch and HEAD at deletion time, parent process, origin tool and version from the trace store, group id, custom `--meta` pairs) in a read-only webview or virtual document.
4. **Restore.** A context-menu action on a ghost runs `saferm --json undelete <uuid>`, always by uuid, never by path, so the record acted on is the one the user clicked. When something is already standing at the destination, saferm refuses without `--on-conflict` (exit 2 naming both values). The extension surfaces that as a modal naming the occupied path and offering `overwrite` and `abort`, with neither pre-selected, and re-runs with the chosen value. "Restore to..." opens a save dialog and passes `--destination`. The git-index effect (`--update-git-index` on by default, `--no-update-git-index` to turn it off) is stated in the confirmation so the user knows the path is about to be staged.
5. **Restore a batch.** Every deletion invocation stamps one `group_id` on all its records; a "Restore everything deleted with this" action restores the records sharing the group, one `undelete` per uuid, stopping at the first refusal and reporting what was already restored.
6. **Delete through saferm.** An explorer context-menu action "Delete with saferm" asks for a description in an input box (an empty or whitespace-only description is refused in the box, before saferm is invoked), passes `--on-error abort`, adds `-r` for directories, and shows the printed identifiers afterwards. It is an addition beside VS Code's own delete, which the extension cannot intercept (`workspace.onWillDeleteFiles` observes a deletion but cannot cancel or redirect it).
7. **Live refresh.** The extension resolves the database location once from `saferm --json config show` (honoring `SAFERM_HOME`) and watches the database file and its WAL companion with a `FileSystemWatcher`, re-listing on change with a short debounce; a manual refresh command exists too.
8. **Capability check at activation.** The extension runs `saferm --json capabilities` and requires the features it uses (`machine-payloads`, `uuid-handles`, `on-conflict-modes`, `restore-destination`, `group-id`, `on-error-modes`). A missing binary, a missing verb, or a missing feature is one state: the extension shows an error naming what is missing and offers nothing, rather than guessing at an older interface. It never compares version strings (a locally built saferm reports a Go pseudo-version).

### What the extension never offers

- **No purge, no "delete permanently".** `purge` is saferm's one consequential command, confirmed at a terminal, and deliberately has no payload so that nothing drives it from a parsed document. The extension has no purge action and never passes `--approve-consequential`.
- **No keep-both restore and no copy-out that leaves the archive intact.** A restore consumes the archived copy by design; the extension does not emulate a keep mode by restoring and re-deleting.
- **No default for `--on-conflict` or `--on-error`.** Both are asked or fixed per action as described above; there is no setting that pre-answers the conflict question.

### VS Code APIs involved

| Need | API |
| --- | --- |
| Ghost entries | `TreeDataProvider` contributed to the Explorer view container, or a read-only `FileSystemProvider` on a custom scheme (see solutions) |
| Signals on real folders | `FileDecorationProvider` |
| Actions | `contributes.commands` and `contributes.menus` (`view/item/context`, `explorer/context`) |
| Description input and conflict choice | `window.showInputBox`, `window.showWarningMessage` with `modal: true` |
| Record details | `TextDocumentContentProvider` or a `WebviewPanel` |
| Refresh | `workspace.createFileSystemWatcher` |
| Remote workspaces | `extensionKind: ["workspace"]`, so the extension runs where the files and the `saferm` binary are (Remote SSH, WSL, dev containers) |

## Solutions for the ghost display

### A. A dedicated tree view inside the Explorer container

A `TreeDataProvider` view ("Deleted files", or whatever name is chosen) placed in the Explorer sidebar, mirroring the directory structure of the deleted paths relative to each workspace folder.

- Pros: stable API, no workspace changes, full control over icons, grouping (by path, by time, by group id), and context menus.
- Cons: the ghosts sit in a separate section, not interleaved with the real files; the user's eye has to move between the two trees.

### B. A read-only `FileSystemProvider` added as a workspace folder

The extension registers a custom URI scheme whose tree is the set of deleted records under a folder, and adds it to the workspace as an extra root.

- Pros: ghosts appear in the explorer proper, and opening a ghost file could show its archived content in an editor tab.
- Cons: adding a workspace folder turns a single-folder window into a multi-root workspace, which changes settings resolution and can write a `.code-workspace` file; showing content needs a way to read an archived file without restoring it, which saferm does not offer (see below).

### C. Interleave ghosts into the ordinary file tree

- Not possible with the stable extension API: the explorer lists what the `file` scheme's file system reports, and an extension cannot inject nodes into it. Decorations (feature 2) are the closest stable approximation and are part of the proposal anyway.

**Recommendation:** A plus the folder decorations for the first version; B only if reading archived content becomes available through saferm itself.

## Reading archived content without restoring

Previewing a deleted file before deciding to restore it is the natural next ask, and saferm has no verb for it: `undelete --destination` restores elsewhere but still consumes the archived copy.

- **Read the archive directly** from `archive_dir` by uuid (and unpack `.tar.zst` for directories). Cons: couples the extension to saferm's internal storage layout, bypasses the hash verification saferm performs, and breaks silently if the layout changes. Rejected.
- **A read-only saferm verb** that writes one archived file's content (or a member of an archived tree) to stdout, classified `read_only`, verifying the recorded hash before emitting. Pros: one authority for the archive format; also useful to agents from the command line. Cons: a new command surface to design; needs a decision on how it relates to the restore-consumes-the-copy rule (it does not restore, so it is not a keep mode, but that reasoning should be recorded).

This is a design decision for saferm itself and a prerequisite for content preview; the rest of the extension does not depend on it.

## Where the extension lives

- **A subdirectory of this repository**, beside `npm/` and `pypi/`. Pros: the extension and the payload schemas it parses change in one commit; one changelog. Cons: a Node toolchain (`vsce`, `ovsx`, a bundler) enters a Go repository; the release pipeline needs a new target.
- **A separate repository.** Pros: independent release cadence and toolchain. Cons: schema changes in saferm and parser changes in the extension drift apart unless the extension tests against `saferm --dump-schema` output.

Either way, the extension's tests should run against a real `saferm` binary with `SAFERM_HOME` pointed at a fixture directory, the same isolation the integration tests in `internal/test/` use.

## Distribution

- Publish to both the Visual Studio Marketplace (`vsce publish`) and Open VSX (`ovsx publish`), the open registry VSCodium, Cursor, and other VS Code derivatives install from. Each needs its own publisher account and token.
- The extension does not bundle saferm; it requires the binary on `PATH` (or a configured path) and says so through the capability check.
- saferm releases through rlsbl, and rlsbl has no VS Code extension publishing target, so either rlsbl gains one or the extension is published by its own workflow.
- The extension's name, publisher ID, and package name are to be chosen.

## Affected files and new components

- New: the extension package (TypeScript sources, `package.json` manifest with `contributes`, bundler config, tests).
- saferm itself: no change for the first version. The `list`, `info`, `undelete`, `delete`, `capabilities`, and `config show` payloads are the contract, so any change to them is a change the extension must follow.
- Optional, separate decision: a read-only content verb in saferm (new command file beside `info.go`, payload or raw-output design, integration tests).
- Release configuration (`.rlsbl/config.json`, CI workflow) if the extension ships from this repository.

## Effort estimate

- Tree view, decorations, hover, details, restore with the conflict modal, delete with description, refresh, and the capability check: about 3 to 5 days, including tests against a real binary.
- Marketplace and Open VSX publishing setup and CI: about 1 day.
- Content preview, if saferm gains a read-only content verb: about 1 to 2 days on the saferm side and 1 day in the extension.
