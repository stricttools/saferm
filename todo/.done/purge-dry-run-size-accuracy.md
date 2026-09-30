# purge --dry-run "freeing ~size" overstates actual disk reclaim

## Context

`purge --dry-run` prints a table of selected records ending with
`Would purge N item(s), freeing ~<size>`, where the size sums each record's
stored `size` field (`purge.go`, the dry-run summary path). That field is
the ORIGINAL size captured at delete time.

## Problem

The original size is not what purging frees, in two cases:

1. **Directories.** The archive entry is a `.tar.zst` — compressed, often
   much smaller than the original tree (the recorded size is the
   uncompressed sum of regular files). Purging frees the compressed
   container's size, not the tree's size. For highly compressible trees the
   estimate can overstate by a large factor.
2. **Hard-linked regular files.** The archive entry holds the inode via
   `os.Link`. If any other directory entry elsewhere still references that
   inode (pre-existing hard links, or a copy restored by `undelete` on the
   same filesystem), removing the archive name frees nothing until the last
   link goes. The recorded size is the true reclaim only when the archive
   entry is the inode's last remaining name.

The result: an agent or human deciding what to purge based on the dry-run
figure is systematically misled toward expecting more free space than the
purge delivers. The non-dry-run purge output inherits the same figure.

## Solutions

1. **Lstat the archive entry at reporting time** (recommended): for each
   selected record, report the archive entry's actual on-disk size
   (`Lstat` on the blob; for regular-file entries additionally check the
   link count and either count the size only when `nlink == 1` or annotate
   the row as "shared inode — reclaim not guaranteed"). Accurate, no schema
   change; cost is one `Lstat` per selected record, which is negligible at
   purge-selection sizes.
2. **Record the archived-entry size in the database at delete time** (new
   column alongside `size`): avoids the stat pass and preserves the figure
   even for reporting on purged rows. Requires a migration and a backfill
   story for existing rows (backfill by statting existing entries once).
   Complements option 1 rather than replacing it — the link-count caveat
   still needs a live check.
3. **Wording-only fix**: keep the number but label it honestly
   ("original size; actual reclaim may be lower"). Cheapest; leaves the
   number wrong, which is against the spirit of honest previews.

Recommendation: option 1 now; consider option 2 if the stat pass ever shows
up in practice. Either way, per the red-green convention: first a test that
archives a compressible directory, runs `purge --dry-run`, and asserts the
reported figure matches the archive entry's size (failing today), then the
fix.

## Affected files

- `purge.go` — dry-run table and summary, non-dry-run completion output
- possibly `internal/db` + a migration (option 2 only)
- tests under `internal/test/` covering the directory case and the
  hard-link case

## Effort

Small. The only design decision is how the hard-link caveat is presented
(exclude from the sum vs annotate).
