package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/stricttools/saferm/internal/archive"
)

// querier is what reads a table's columns: the pool, one connection, or a
// transaction.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// migrateToNodeTypeColumn is migration 4: one `node_type` column replaces
// is_directory and the inference of a symlink from a non-null symlink_target,
// so what a record archived is stated once, by the row, and is checked by the
// schema.
//
// SQLite can neither add a column carrying a CHECK constraint nor drop one, so
// the table is rebuilt: the new definition is created under a temporary name,
// every row is copied with its node type derived the way the readers used to
// derive it (a symlink target first, because a link to a directory carried both
// markers and what saferm archived was the link), the old table is dropped and
// the new one takes its name. The AUTOINCREMENT counter is carried across, so
// a numeric id is never issued twice.
//
// The derived node type is then checked against the entry it names, read with
// Lstat. saferm versions from before this migration did not recognize symlinks
// or special files, and archived a symlink or a FIFO at `<uuid>` under a file
// record; each such record is repaired (see [archive.EntryRepair]): its entry
// is written in the form its real node type uses and the record takes that
// node type. Any other contradiction refuses the migration as a whole, naming
// every record it found, and changes nothing.
//
// The steps are ordered so a failure leaves the database and the archive
// either as they were or fully migrated:
//
//  1. The database's write lock is taken first (BEGIN IMMEDIATE), so no other
//     saferm process migrates or writes beside this one.
//  2. Every record is read and every repair planned before anything changes.
//  3. The repaired entries are written and flushed. Nothing names them yet; a
//     failure takes back the ones written.
//  4. The table is rebuilt, the repaired records updated, and user_version set
//     to 4, in the one transaction the lock opened, which is then committed. A
//     failure rolls it back and takes back the repaired entries.
//  5. The old entries, which no record names any more, are removed. One that
//     cannot be removed is a hard error naming it; the migration itself is
//     complete.
//
// A binary from before this migration cannot read or write the rebuilt table,
// and that is deliberate: saferm is pre-stable and keeps no second spelling of
// a record's node type for older readers.
func migrateToNodeTypeColumn(conn *sql.DB, archiveDir string) (err error) {
	ctx := context.Background()
	c, err := conn.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if _, rerr := c.ExecContext(ctx, "ROLLBACK"); rerr != nil {
				err = errors.Join(err, fmt.Errorf("rolling back: %w", rerr))
			}
		}
	}()

	// Another process may have migrated between this one reading user_version
	// and taking the lock.
	var version int
	if err := c.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	if version >= 4 {
		return nil
	}
	present, err := hasColumn(c, "deletions", "node_type")
	if err != nil {
		return err
	}

	repairs, err := planEntryRepairs(ctx, c, archiveDir, present)
	if err != nil {
		return err
	}
	if err := writeRepairedEntries(archiveDir, repairs); err != nil {
		return err
	}
	if err := commitNodeTypeColumn(ctx, c, present, repairs); err != nil {
		committed = true // settled here rather than by the deferred rollback
		return errors.Join(err, rollBackRepairs(ctx, c, archiveDir, repairs))
	}
	committed = true
	return removeOldEntries(archiveDir, repairs)
}

// rollBackRepairs rolls the transaction back after a failed rebuild, update or
// commit, then takes back the repaired entries -- unless the database reads
// as migrated after all, which a commit that reported an error yet took effect
// would leave: those entries are then what the records name, and the old ones
// are removed instead.
func rollBackRepairs(ctx context.Context, c *sql.Conn, archiveDir string, repairs []plannedRepair) error {
	// ROLLBACK fails when the failed COMMIT already ended the transaction;
	// user_version below is what says which way it went.
	_, rerr := c.ExecContext(ctx, "ROLLBACK")
	var version int
	if err := c.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return errors.Join(fmt.Errorf("reading user_version after the failure, so the repaired entries were left in place: %w", err), rerr)
	}
	if version >= 4 {
		return removeOldEntries(archiveDir, repairs)
	}
	return discardRepairedEntries(repairs, len(repairs))
}

// plannedRepair is one record migration 4 repairs.
type plannedRepair struct {
	id           int64
	originalPath string
	repair       *archive.EntryRepair
}

func (p plannedRepair) String() string {
	return fmt.Sprintf("[%d] %s %s", p.id, p.repair.UUID, p.originalPath)
}

// planEntryRepairs reads every record, with the node type the migration gives
// it, and plans the repair of each one whose entry contradicts it, in id
// order. A record no repair resolves refuses the whole migration; the error
// names every such record. It changes nothing.
func planEntryRepairs(ctx context.Context, c *sql.Conn, archiveDir string, nodeTypeColumnPresent bool) ([]plannedRepair, error) {
	nodeType := `node_type`
	if !nodeTypeColumnPresent {
		nodeType = derivedNodeTypeSQL
	}
	rows, err := c.QueryContext(ctx, `SELECT id, uuid, original_path, `+nodeType+` FROM deletions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("reading the records: %w", err)
	}
	type row struct {
		id           int64
		uuid         string
		originalPath string
		nodeType     string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.uuid, &r.originalPath, &r.nodeType); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading the records: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("reading the records: %w", err)
	}
	rows.Close()

	var repairs []plannedRepair
	var refusals []string
	for _, r := range all {
		recorded, err := archive.ParseNodeType(r.nodeType)
		if err == nil {
			var repair *archive.EntryRepair
			repair, err = archive.PlanEntryRepair(archiveDir, r.uuid, recorded)
			if err == nil && repair != nil {
				repairs = append(repairs, plannedRepair{id: r.id, originalPath: r.originalPath, repair: repair})
			}
		}
		if err != nil {
			refusals = append(refusals, fmt.Sprintf("[%d] %s %s: %v", r.id, r.uuid, r.originalPath, err))
		}
	}
	if len(refusals) > 0 {
		return nil, fmt.Errorf("%d record(s) contradict their archive entries in a way no repair resolves, so nothing was changed:\n  %s",
			len(refusals), strings.Join(refusals, "\n  "))
	}
	return repairs, nil
}

// derivedNodeTypeSQL derives a version-3 row's node type the way the readers
// of that schema did.
var derivedNodeTypeSQL = `CASE` +
	` WHEN symlink_target IS NOT NULL THEN '` + string(archive.NodeTypeSymlink) + `'` +
	` WHEN is_directory != 0 THEN '` + string(archive.NodeTypeDirectory) + `'` +
	` ELSE '` + string(archive.NodeTypeFile) + `' END`

// writeRepairedEntries writes every repaired entry and flushes the archive
// directory. When one fails, the ones already written are taken back and the
// error names the record.
func writeRepairedEntries(archiveDir string, repairs []plannedRepair) error {
	if len(repairs) == 0 {
		return nil
	}
	for i, p := range repairs {
		if err := p.repair.WriteEntry(); err != nil {
			return errors.Join(
				fmt.Errorf("writing the repaired entry of %s, so nothing was changed: %w", p, err),
				discardRepairedEntries(repairs, i))
		}
	}
	if err := archive.SyncDir(archiveDir); err != nil {
		return errors.Join(
			fmt.Errorf("flushing %s, so nothing was changed: %w", archiveDir, err),
			discardRepairedEntries(repairs, len(repairs)))
	}
	return nil
}

// discardRepairedEntries takes back the first n repaired entries.
func discardRepairedEntries(repairs []plannedRepair, n int) error {
	var errs []error
	for _, p := range repairs[:n] {
		if err := p.repair.DiscardEntry(); err != nil {
			errs = append(errs, fmt.Errorf("the repaired entry %s could not be taken back: %w", p.repair.NewEntry, err))
		}
	}
	return errors.Join(errs...)
}

// commitNodeTypeColumn rebuilds the table when it still has the version-3
// shape, updates every repaired record, sets user_version to 4 and commits,
// all inside the transaction the caller opened.
func commitNodeTypeColumn(ctx context.Context, c *sql.Conn, nodeTypeColumnPresent bool, repairs []plannedRepair) error {
	if !nodeTypeColumnPresent {
		if err := rebuildWithNodeTypeColumn(ctx, c); err != nil {
			return err
		}
	}
	for _, p := range repairs {
		var target any
		if p.repair.NodeType == archive.NodeTypeSymlink {
			target = p.repair.SymlinkTarget
		}
		if _, err := c.ExecContext(ctx,
			`UPDATE deletions SET node_type = ?, symlink_target = ?, hash = ?, size = 0 WHERE id = ?`,
			string(p.repair.NodeType), target, p.repair.Hash, p.id); err != nil {
			return fmt.Errorf("updating %s: %w", p, err)
		}
	}
	if _, err := c.ExecContext(ctx, "PRAGMA user_version = 4"); err != nil {
		return fmt.Errorf("setting user_version to 4: %w", err)
	}
	return commitNodeTypeMigration(c)
}

// commitNodeTypeMigration commits the migration's transaction. Tests replace
// it to fail the commit.
var commitNodeTypeMigration = func(c *sql.Conn) error {
	_, err := c.ExecContext(context.Background(), "COMMIT")
	return err
}

// rebuildWithNodeTypeColumn replaces the version-3 table with one carrying the
// node_type column, keeping every row and the id counter.
func rebuildWithNodeTypeColumn(ctx context.Context, c *sql.Conn) error {
	var seq sql.NullInt64
	err := c.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'deletions'`).Scan(&seq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading the id counter: %w", err)
	}
	const kept = `id, uuid, original_path, original_name, size, hash, deleted_at, command, description, metadata, restored_at, restored_to, symlink_target, purged_at, origin_name, origin_version, group_id`
	steps := []string{
		deletionsTableSQL("deletions_rebuild"),
		`INSERT INTO deletions_rebuild (` + kept + `, node_type) SELECT ` + kept + `, ` + derivedNodeTypeSQL + ` FROM deletions`,
		`DROP TABLE deletions`,
		`ALTER TABLE deletions_rebuild RENAME TO deletions`,
	}
	for _, step := range steps {
		if _, err := c.ExecContext(ctx, step); err != nil {
			return err
		}
	}
	if seq.Valid {
		if _, err := c.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name = 'deletions'`); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `INSERT INTO sqlite_sequence (name, seq) VALUES ('deletions', MAX(?, (SELECT IFNULL(MAX(id), 0) FROM deletions)))`, seq.Int64); err != nil {
			return err
		}
	}
	return nil
}

// removeOldEntries removes the entries the repaired records named before the
// migration. The migration is committed by then, so one that cannot be removed
// is no longer named by any record; the error lists every such path.
func removeOldEntries(archiveDir string, repairs []plannedRepair) error {
	if len(repairs) == 0 {
		return nil
	}
	var leftovers []string
	for _, p := range repairs {
		if err := removeOldEntry(p.repair.OldEntry); err != nil && !errors.Is(err, os.ErrNotExist) {
			leftovers = append(leftovers, fmt.Sprintf("%s: %v", p.repair.OldEntry, err))
		}
	}
	if len(leftovers) > 0 {
		return fmt.Errorf("the migration is committed, but %d old archive entr(ies) that no record names any more could not be removed:\n  %s",
			len(leftovers), strings.Join(leftovers, "\n  "))
	}
	return archive.SyncDir(archiveDir)
}

// removeOldEntry removes one old entry. Tests replace it to fail the removal.
var removeOldEntry = os.Remove
