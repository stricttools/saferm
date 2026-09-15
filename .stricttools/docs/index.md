+++
title = "saferm"
description = "The `rm` replacement that is safe to hand to your AI Agents: deleted files can be restored, know exactly which session deleted a file, when, and why, with the reason being a required flag"
+++

# saferm

The `rm` replacement that is safe to hand to your AI Agents: deleted files can be restored, know exactly which session deleted a file, when, and why, with the reason being a required flag

## CLI Reference

- [All commands and options](cli-index.md)
- [Machine surface](machine-surface.md) -- the `--json` envelope, each verb's payload, and the `capabilities` probe

## Packages

saferm is organized into internal packages that handle distinct responsibilities: file and directory archival with integrity verification, SQLite-based metadata storage, git context detection, environment and process metadata capture, and reading the shared process trace store that answers which tool ran a deletion. Each package is independently testable and designed for concurrent use via WAL-mode SQLite and atomic file operations. Configuration is handled by strictcli's built-in config system (TOML format at `~/.saferm/config.toml`).

:-: ref path="internal/archive"

:-: ref path="internal/db"

:-: ref path="internal/git"

:-: ref path="internal/meta"

:-: ref path="internal/trace"
