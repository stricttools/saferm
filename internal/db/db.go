package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a queried record does not exist.
var ErrNotFound = errors.New("record not found")

// ErrOriginVersionWithoutName is returned by Insert when a record carries an
// origin version but no origin name.
//
// SQLite cannot add a CHECK constraint to an existing table, so the invariant
// is enforced here instead of in the schema: one enforcement path, identical on
// a fresh database and on one that reached this schema through the migration
// ladder, and no table rebuild. The cost is stated rather than hidden -- a
// hand-edited database, or an older binary writing into a newer one, bypasses
// it.
var ErrOriginVersionWithoutName = errors.New("origin_version requires origin_name")

// ErrOriginEmpty is returned by Insert when an origin field is present but
// empty. Both fields are nullable and never empty: an empty string would be a
// third state beside "a tool claimed this" and "none did", and nothing can read
// it as either.
var ErrOriginEmpty = errors.New("origin fields must be absent or non-empty")

// recordColumns is the column list every read of a deletion record selects, in
// the order scanOne scans them. It is spelled once because a query that drifts
// from the scanner produces a mismatch at run time, not at compile time.
const recordColumns = `id, uuid, original_path, original_name, size, hash, is_directory, deleted_at, command, description, metadata, restored_at, restored_to, symlink_target, purged_at, origin_name, origin_version, group_id`

// DB wraps a *sql.DB connection to the saferm SQLite database.
type DB struct {
	conn   *sql.DB
	notify RetryNotifier
}

// DeletionRecord represents a single archived deletion in the database.
type DeletionRecord struct {
	ID            int64
	UUID          string
	OriginalPath  string
	OriginalName  string
	Size          int64
	Hash          string
	IsDirectory   bool
	DeletedAt     time.Time
	Command       string // may be empty
	Description   string
	Metadata      string     // JSON blob
	RestoredAt    *time.Time // nil if not restored
	RestoredTo    *string    // nil if not restored
	SymlinkTarget *string    // nil if not a symlink
	PurgedAt      *time.Time // nil if not purged

	// OriginName and OriginVersion name the tool that ran this deletion, as
	// derived at insert time from the process trace store. Both are nil when no
	// tool claimed the deletion -- which is every deletion made before the
	// trace store existed, and every deletion made from a shell.
	OriginName    *string
	OriginVersion *string

	// GroupID is shared by every record one delete invocation writes. Nil on
	// rows written before the column existed.
	GroupID *string
}

// busyTimeoutMS is how long SQLite itself waits for a held lock before handing
// back SQLITE_BUSY. saferm's own bounded retry (see contention.go) sits on top
// of it: SQLite absorbs the brief overlaps, the retry absorbs the long ones,
// and only a lock that outlives both reaches the caller.
const busyTimeoutMS = 5000

// Open opens (or creates) the SQLite database at dbPath with WAL mode and
// busy_timeout, then runs the schema DDL.
//
// notify, when non-nil, is called before each contention retry -- for every
// operation on the returned DB as well as for the schema work below, which is
// why it is supplied here rather than set afterwards. Pass nil for no
// reporting.
func Open(dbPath string, notify RetryNotifier) (*DB, error) {
	return open(dbPath, busyTimeoutMS, notify)
}

// open is Open with the busy timeout exposed, so tests can produce real
// contention without waiting seconds for it.
func open(dbPath string, busyTimeout int, notify RetryNotifier) (*DB, error) {
	// Pass pragmas via DSN so they take effect on every connection in the pool.
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout%%3d%d&_pragma=journal_mode%%3dWAL", dbPath, busyTimeout)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// Create tables and indexes, then run schema migrations. Both are write
	// paths and both are idempotent, so both are retried under contention.
	if err := retryBusy(notify, func() error {
		if _, err := conn.Exec(SchemaSQL); err != nil {
			return err
		}
		return migrate(conn)
	}); err != nil {
		conn.Close()
		return nil, err
	}

	return &DB{conn: conn, notify: notify}, nil
}

// migrate applies schema migrations based on PRAGMA user_version.
func migrate(conn *sql.DB) error {
	var version int
	if err := conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}

	if version < 1 {
		// Migration 1: add symlink_target column.
		// The column exists in fresh databases (from CREATE TABLE) but not
		// in databases created before this migration was added.
		present, err := hasColumn(conn, "deletions", "symlink_target")
		if err != nil {
			return fmt.Errorf("migration 1 (add symlink_target): %w", err)
		}
		if !present {
			if _, err := conn.Exec("ALTER TABLE deletions ADD COLUMN symlink_target TEXT"); err != nil {
				return fmt.Errorf("migration 1 (add symlink_target): %w", err)
			}
		}
		if _, err := conn.Exec("PRAGMA user_version = 1"); err != nil {
			return fmt.Errorf("setting user_version to 1: %w", err)
		}
	}

	if version < 2 {
		// Migration 2: add purged_at column.
		// The column exists in fresh databases (from CREATE TABLE) but not
		// in databases created before this migration was added.
		present, err := hasColumn(conn, "deletions", "purged_at")
		if err != nil {
			return fmt.Errorf("migration 2 (add purged_at): %w", err)
		}
		if !present {
			if _, err := conn.Exec("ALTER TABLE deletions ADD COLUMN purged_at TEXT"); err != nil {
				return fmt.Errorf("migration 2 (add purged_at): %w", err)
			}
		}
		if _, err := conn.Exec("PRAGMA user_version = 2"); err != nil {
			return fmt.Errorf("setting user_version to 2: %w", err)
		}
	}

	if version < 3 {
		// Migration 3: add origin_name, origin_version and group_id.
		// The columns exist in fresh databases (from CREATE TABLE) but not in
		// databases created before this migration was added -- and a column
		// that lands in only one of the two places is how fresh and migrated
		// databases silently fork.
		//
		// Every column here is nullable and none carries a constraint, which is
		// the whole of what the ladder can express: existing rows stay valid,
		// existing readers keep reading, and a binary from before this
		// migration still opens and writes the migrated database.
		for _, column := range []string{"origin_name", "origin_version", "group_id"} {
			present, err := hasColumn(conn, "deletions", column)
			if err != nil {
				return fmt.Errorf("migration 3 (add %s): %w", column, err)
			}
			if !present {
				if _, err := conn.Exec("ALTER TABLE deletions ADD COLUMN " + column + " TEXT"); err != nil {
					return fmt.Errorf("migration 3 (add %s): %w", column, err)
				}
			}
		}
		if _, err := conn.Exec("PRAGMA user_version = 3"); err != nil {
			return fmt.Errorf("setting user_version to 3: %w", err)
		}
	}

	return nil
}

// hasColumn reports whether a table has a column with the given name.
//
// A failed query is an error, never a "no". It used to be a "no": the query
// error was discarded and the caller read the false as "the column is missing",
// then ran an ALTER TABLE that failed with its own unrelated message. Under the
// contention retry that wraps migrate, the swallowed error is worse than
// misleading -- SQLITE_BUSY read as "column missing" produces a follow-up
// failure that the classifier cannot recognize as contention, so the operation
// that would have succeeded on the next attempt fails permanently instead.
func hasColumn(conn *sql.DB, table, column string) (bool, error) {
	rows, err := conn.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("reading columns of %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name string
		var ctype sql.NullString
		var notnull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return false, fmt.Errorf("reading columns of %s: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("reading columns of %s: %w", table, err)
	}
	return false, nil
}

// Close closes the underlying database connection.
func (d *DB) Close() error {
	return d.conn.Close()
}

// Insert inserts a DeletionRecord and returns the auto-increment ID.
//
// The origin invariants are checked here, before anything is written: see
// ErrOriginVersionWithoutName for why they live in code rather than in the
// schema.
func (d *DB) Insert(rec *DeletionRecord) (int64, error) {
	if err := validateOrigin(rec); err != nil {
		return 0, err
	}

	var id int64
	err := d.retry(func() error {
		result, err := d.conn.Exec(
			`INSERT INTO deletions (uuid, original_path, original_name, size, hash, is_directory, deleted_at, command, description, metadata, restored_at, restored_to, symlink_target, origin_name, origin_version, group_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.UUID,
			rec.OriginalPath,
			rec.OriginalName,
			rec.Size,
			rec.Hash,
			boolToInt(rec.IsDirectory),
			rec.DeletedAt.Format(time.RFC3339),
			nullableString(rec.Command),
			rec.Description,
			nullableString(rec.Metadata),
			nullableTime(rec.RestoredAt),
			rec.RestoredTo,
			rec.SymlinkTarget,
			rec.OriginName,
			rec.OriginVersion,
			rec.GroupID,
		)
		if err != nil {
			return err
		}
		id, err = result.LastInsertId()
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// validateOrigin enforces the two origin invariants: neither field may be
// present-but-empty, and a version cannot exist without a name.
func validateOrigin(rec *DeletionRecord) error {
	if (rec.OriginName != nil && *rec.OriginName == "") ||
		(rec.OriginVersion != nil && *rec.OriginVersion == "") {
		return ErrOriginEmpty
	}
	if rec.OriginVersion != nil && rec.OriginName == nil {
		return ErrOriginVersionWithoutName
	}
	return nil
}

// QueryByID retrieves a single record by ID. Returns ErrNotFound if it does
// not exist.
func (d *DB) QueryByID(id int64) (*DeletionRecord, error) {
	var rec *DeletionRecord
	err := d.retry(func() error {
		row := d.conn.QueryRow(
			`SELECT `+recordColumns+`
			 FROM deletions WHERE id = ?`, id)
		var err error
		rec, err = scanRecord(row)
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return rec, nil
}

// QueryByUUID retrieves a single record by its archive uuid. Returns
// ErrNotFound if it does not exist.
//
// The uuid is the identifier a record keeps: the numeric id is this database's
// autoincrement counter, while the uuid names the archived entry on disk and is
// what `delete` hands back to its caller.
func (d *DB) QueryByUUID(uuid string) (*DeletionRecord, error) {
	var rec *DeletionRecord
	err := d.retry(func() error {
		row := d.conn.QueryRow(
			`SELECT `+recordColumns+`
			 FROM deletions WHERE uuid = ?`, uuid)
		var err error
		rec, err = scanRecord(row)
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return rec, nil
}

// oldestFirst is the only order a multi-row read asks SQLite for. Every such
// query walks its table or index forward and the caller's newest-first order
// -- ties broken by the highest id first -- is produced by reversing the result
// in memory (see queryRecords).
//
// A backward walk is not a matter of taste here. SQLite reading a b-tree
// backward issues one page read at a time in descending file order, and on a
// copy-on-write, compressing filesystem that defeats read-ahead entirely: on an
// archive of about 25,000 records in daily use, the newest-first read of every
// row took 36 to 191 seconds cold, and the same rows read oldest-first took 0.5
// to 2.5.
// TestMultiRowQueriesNeverScanBackward holds every multi-row query to this.
const oldestFirst = ` ORDER BY deleted_at ASC, id ASC`

// liveOnly is the predicate that keeps a record neither restored nor purged.
const liveOnly = `restored_at IS NULL AND purged_at IS NULL`

// queryByPathSQL selects the live records archived from one given path.
const queryByPathSQL = `SELECT ` + recordColumns + ` FROM deletions WHERE original_path = ? AND ` + liveOnly + oldestFirst

// queryAllSQL selects every record, or only the live ones.
func queryAllSQL(includeAll bool) string {
	query := `SELECT ` + recordColumns + ` FROM deletions`
	if !includeAll {
		query += ` WHERE ` + liveOnly
	}
	return query + oldestFirst
}

// queryOlderThanSQL selects the live records deleted before a time.
const queryOlderThanSQL = `SELECT ` + recordColumns + ` FROM deletions WHERE deleted_at < ? AND ` + liveOnly + oldestFirst

// QueryByPath returns all non-restored records matching the given original_path,
// newest first.
func (d *DB) QueryByPath(path string) ([]*DeletionRecord, error) {
	return d.queryRecords(queryByPathSQL, path)
}

// QueryAll returns all records, newest first. If includeAll is false, restored
// and purged records are excluded.
func (d *DB) QueryAll(includeAll bool) ([]*DeletionRecord, error) {
	return d.queryRecords(queryAllSQL(includeAll))
}

// queryDeletedFromSQL selects every record, or only the live ones, whose
// deleted_at text is at least a bound. It reads the deleted_at index forward
// from the bound, so only the rows at or after it are touched.
func queryDeletedFromSQL(includeAll bool) string {
	query := `SELECT ` + recordColumns + ` FROM deletions WHERE deleted_at >= ?`
	if !includeAll {
		query += ` AND ` + liveOnly
	}
	return query + oldestFirst
}

// deletedAtSlack widens the text bound QueryDeletedSince hands SQLite.
// deleted_at is RFC3339 text in whatever zone the deleting process ran in,
// and text compares by clock reading, not by instant: a record deleted at a
// given instant in a zone behind UTC reads earlier than the same instant in
// UTC. No zone is more than 12 hours behind UTC, so a bound a day earlier
// than the instant, written in UTC, is below every record at or after it.
const deletedAtSlack = 24 * time.Hour

// QueryDeletedSince returns the records deleted at or after since, newest
// first. If includeAll is false, restored and purged records are excluded.
// SQLite narrows by the widened text bound; the exact instant is compared
// here, on the parsed timestamps.
func (d *DB) QueryDeletedSince(since time.Time, includeAll bool) ([]*DeletionRecord, error) {
	bound := since.Add(-deletedAtSlack).UTC().Format(time.RFC3339)
	records, err := d.queryRecords(queryDeletedFromSQL(includeAll), bound)
	if err != nil {
		return nil, err
	}
	return DeletedSince(records, since), nil
}

// DeletedSince keeps, in order, the records deleted at or after since.
func DeletedSince(records []*DeletionRecord, since time.Time) []*DeletionRecord {
	kept := records[:0:0]
	for _, rec := range records {
		if !rec.DeletedAt.Before(since) {
			kept = append(kept, rec)
		}
	}
	return kept
}

// queryRecords runs a multi-row read under the contention retry, scans every
// row it returns, and hands them back newest first. The query reads
// oldestFirst, so reversing it gives deleted_at descending with ties broken by
// the highest id first.
func (d *DB) queryRecords(query string, args ...any) ([]*DeletionRecord, error) {
	var records []*DeletionRecord
	err := d.retry(func() error {
		rows, err := d.conn.Query(query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		records, err = scanRecords(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	reverseRecords(records)
	return records, nil
}

// reverseRecords reverses a slice in place.
func reverseRecords(records []*DeletionRecord) {
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
}

// MarkRestored sets restored_at to now and restored_to to the given path.
// Returns ErrNotFound if the record does not exist.
func (d *DB) MarkRestored(id int64, restoredTo string) error {
	now := time.Now().Format(time.RFC3339)
	return d.retry(func() error {
		result, err := d.conn.Exec(
			`UPDATE deletions SET restored_at = ?, restored_to = ? WHERE id = ?`,
			now, restoredTo, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// MarkPurged sets purged_at to now, preserving the metadata record.
// Returns ErrNotFound if the record does not exist.
func (d *DB) MarkPurged(id int64) error {
	now := time.Now().Format(time.RFC3339)
	return d.retry(func() error {
		result, err := d.conn.Exec(
			`UPDATE deletions SET purged_at = ? WHERE id = ?`,
			now, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// QueryOlderThan returns all non-restored, non-purged records deleted before
// the given time, newest first.
func (d *DB) QueryOlderThan(before time.Time) ([]*DeletionRecord, error) {
	return d.queryRecords(queryOlderThanSQL, before.Format(time.RFC3339))
}

// scanner is the common interface between *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanOne scans a single row from any scanner into a DeletionRecord.
func scanOne(s scanner) (*DeletionRecord, error) {
	var rec DeletionRecord
	var isDir int
	var deletedAtStr string
	var command sql.NullString
	var metadata sql.NullString
	var restoredAtStr sql.NullString
	var restoredTo sql.NullString
	var purgedAtStr sql.NullString

	err := s.Scan(
		&rec.ID, &rec.UUID, &rec.OriginalPath, &rec.OriginalName,
		&rec.Size, &rec.Hash, &isDir, &deletedAtStr,
		&command, &rec.Description, &metadata,
		&restoredAtStr, &restoredTo, &rec.SymlinkTarget,
		&purgedAtStr,
		&rec.OriginName, &rec.OriginVersion, &rec.GroupID,
	)
	if err != nil {
		return nil, err
	}

	rec.IsDirectory = isDir != 0
	rec.DeletedAt, err = time.Parse(time.RFC3339, deletedAtStr)
	if err != nil {
		return nil, err
	}
	if command.Valid {
		rec.Command = command.String
	}
	if metadata.Valid {
		rec.Metadata = metadata.String
	}
	if restoredAtStr.Valid {
		t, err := time.Parse(time.RFC3339, restoredAtStr.String)
		if err != nil {
			return nil, err
		}
		rec.RestoredAt = &t
	}
	if restoredTo.Valid {
		rec.RestoredTo = &restoredTo.String
	}
	if purgedAtStr.Valid {
		t, err := time.Parse(time.RFC3339, purgedAtStr.String)
		if err != nil {
			return nil, err
		}
		rec.PurgedAt = &t
	}

	return &rec, nil
}

// scanRecord scans a single *sql.Row into a DeletionRecord.
func scanRecord(row *sql.Row) (*DeletionRecord, error) {
	return scanOne(row)
}

// scanRecords scans multiple *sql.Rows into a slice of DeletionRecords.
func scanRecords(rows *sql.Rows) ([]*DeletionRecord, error) {
	var records []*DeletionRecord
	for rows.Next() {
		rec, err := scanOne(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

// boolToInt converts a bool to an int for SQLite storage.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullableString returns a sql.NullString for optional string fields.
func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// nullableTime formats a *time.Time as a sql.NullString for SQLite storage.
func nullableTime(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: t.Format(time.RFC3339), Valid: true}
}
