package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// legacySchemaSQL is the deletions table exactly as saferm created it before
// the origin columns existed, with the user_version the ladder had reached at
// that point. A database of this shape is what every already-installed saferm
// has on disk, so the migration is tested against the real predecessor rather
// than against a stripped-down invention.
const legacySchemaSQL = `
CREATE TABLE IF NOT EXISTS deletions (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	uuid          TEXT NOT NULL UNIQUE,
	original_path TEXT NOT NULL,
	original_name TEXT NOT NULL,
	size          INTEGER NOT NULL,
	hash          TEXT NOT NULL,
	is_directory  INTEGER NOT NULL DEFAULT 0,
	deleted_at    TEXT NOT NULL,
	command       TEXT,
	description   TEXT NOT NULL,
	metadata      TEXT,
	restored_at   TEXT,
	restored_to   TEXT,
	symlink_target TEXT,
	purged_at     TEXT
);

CREATE INDEX IF NOT EXISTS idx_deletions_original_path ON deletions(original_path);
CREATE INDEX IF NOT EXISTS idx_deletions_deleted_at ON deletions(deleted_at);

PRAGMA user_version = 2;
`

// openLegacyDB creates a database at the pre-origin schema version, writes one
// row into it through raw SQL, and returns its path. Nothing in it knows about
// the origin or group columns.
func openLegacyDB(t *testing.T) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening legacy database: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Exec(legacySchemaSQL); err != nil {
		t.Fatalf("creating legacy schema: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO deletions (uuid, original_path, original_name, size, hash, is_directory, deleted_at, command, description, metadata)
		 VALUES ('legacy-uuid', '/tmp/legacy.txt', 'legacy.txt', 12, 'deadbeef', 0, ?, 'rm', 'a deletion from before the origin columns', '{}')`,
		time.Now().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("inserting legacy row: %v", err)
	}
	return dbPath
}

func TestMigration_OldDatabaseGainsOriginAndGroupColumns(t *testing.T) {
	dbPath := openLegacyDB(t)

	d, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open on a legacy database failed: %v", err)
	}
	defer d.Close()

	for _, column := range []string{"origin_name", "origin_version", "group_id"} {
		present, err := hasColumn(d.conn, "deletions", column)
		if err != nil {
			t.Fatalf("hasColumn(%s): %v", column, err)
		}
		if !present {
			t.Errorf("migrated database has no %s column", column)
		}
	}

	var version int
	if err := d.conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if version < 3 {
		t.Errorf("user_version is %d after migrating; the origin migration did not run", version)
	}

	// The pre-existing row survives and reads back with no origin claimed --
	// there is no backfill, and null is the honest answer for a deletion no
	// tool claimed.
	rec, err := d.QueryByUUID("legacy-uuid")
	if err != nil {
		t.Fatalf("querying the legacy row: %v", err)
	}
	if rec.OriginName != nil || rec.OriginVersion != nil {
		t.Errorf("legacy row carries an origin: name=%v version=%v", rec.OriginName, rec.OriginVersion)
	}
	if rec.GroupID != nil {
		t.Errorf("legacy row carries a group id: %v", *rec.GroupID)
	}
}

// insertOrigin builds a record carrying the given origin and inserts it.
func insertOrigin(d *DB, uuid string, name, version, group *string) error {
	rec := makeRecord(uuid, "/tmp/"+uuid, time.Now())
	rec.OriginName = name
	rec.OriginVersion = version
	rec.GroupID = group
	_, err := d.Insert(rec)
	return err
}

func strptr(s string) *string { return &s }

// The version-requires-name rule is enforced in code rather than by a CHECK
// constraint, so it must hold identically on a fresh database and on one that
// arrived at this schema through the ladder -- there is one enforcement path
// and it sits above both.
func TestInsert_VersionWithoutNameIsRejected(t *testing.T) {
	fresh := openTestDB(t)

	migratedPath := openLegacyDB(t)
	migrated, err := Open(migratedPath, nil)
	if err != nil {
		t.Fatalf("Open on a legacy database failed: %v", err)
	}
	defer migrated.Close()

	for _, tc := range []struct {
		label string
		d     *DB
	}{
		{"fresh", fresh},
		{"migrated", migrated},
	} {
		err := insertOrigin(tc.d, "version-without-name-"+tc.label, nil, strptr("0.9.0"), nil)
		if !errors.Is(err, ErrOriginVersionWithoutName) {
			t.Errorf("%s database: inserting a version with no name returned %v; want ErrOriginVersionWithoutName", tc.label, err)
		}

		// An empty name is not a name: both fields are nullable but never empty.
		err = insertOrigin(tc.d, "empty-name-"+tc.label, strptr(""), strptr("0.9.0"), nil)
		if !errors.Is(err, ErrOriginEmpty) {
			t.Errorf("%s database: inserting an empty name returned %v; want ErrOriginEmpty", tc.label, err)
		}

		err = insertOrigin(tc.d, "empty-version-"+tc.label, strptr("rlsbl"), strptr(""), nil)
		if !errors.Is(err, ErrOriginEmpty) {
			t.Errorf("%s database: inserting an empty version returned %v; want ErrOriginEmpty", tc.label, err)
		}

		// A name with no version is legal: a caller whose entry named it but
		// carried no version is still a caller that claimed the deletion.
		if err := insertOrigin(tc.d, "name-only-"+tc.label, strptr("rlsbl"), nil, nil); err != nil {
			t.Errorf("%s database: inserting a name with no version failed: %v", tc.label, err)
		}
	}
}

// TestMigratedDatabase_RefusesPreKindWrites pins the other side of migration 4:
// a binary from before the kind column writes is_directory, which the rebuilt
// table no longer has, so its insert fails outright instead of writing a row
// whose kind nothing states. saferm is pre-stable and keeps no second spelling
// of a record's kind for older writers.
//
// The old binary's SQL is what is exercised here rather than the old binary
// itself: building a previous release inside the suite would make every run
// depend on the network and on a tag that keeps moving.
func TestMigratedDatabase_RefusesPreKindWrites(t *testing.T) {
	dbPath := openLegacyDB(t)

	d, err := Open(dbPath, nil)
	if err != nil {
		t.Fatalf("Open on a legacy database failed: %v", err)
	}
	d.Close()

	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopening as an older binary would: %v", err)
	}
	defer conn.Close()
	_, err = conn.Exec(
		`INSERT INTO deletions (uuid, original_path, original_name, size, hash, is_directory, deleted_at, command, description, metadata, restored_at, restored_to, symlink_target)
		 VALUES ('old-writer', '/tmp/old.txt', 'old.txt', 3, 'cafe', 0, ?, '', 'written by a pre-migration binary', '{}', NULL, NULL, NULL)`,
		time.Now().Format(time.RFC3339),
	)
	if err == nil {
		t.Fatal("a pre-kind insert into the migrated database succeeded and wrote a row whose kind nothing states")
	}
}

func TestInsert_RoundTripsOriginAndGroup(t *testing.T) {
	d := openTestDB(t)

	if err := insertOrigin(d, "claimed", strptr("rlsbl"), strptr("0.61.2"), strptr("group-1")); err != nil {
		t.Fatalf("insert with an origin failed: %v", err)
	}
	rec, err := d.QueryByUUID("claimed")
	if err != nil {
		t.Fatalf("querying the claimed row: %v", err)
	}
	if rec.OriginName == nil || *rec.OriginName != "rlsbl" {
		t.Errorf("origin_name round-tripped as %v", rec.OriginName)
	}
	if rec.OriginVersion == nil || *rec.OriginVersion != "0.61.2" {
		t.Errorf("origin_version round-tripped as %v", rec.OriginVersion)
	}
	if rec.GroupID == nil || *rec.GroupID != "group-1" {
		t.Errorf("group_id round-tripped as %v", rec.GroupID)
	}

	// Nothing claimed this one, and that is a state the column must be able to
	// hold: null means "no tool claimed this".
	if err := insertOrigin(d, "unclaimed", nil, nil, strptr("group-1")); err != nil {
		t.Fatalf("insert with no origin failed: %v", err)
	}
	rec, err = d.QueryByUUID("unclaimed")
	if err != nil {
		t.Fatalf("querying the unclaimed row: %v", err)
	}
	if rec.OriginName != nil || rec.OriginVersion != nil {
		t.Errorf("unclaimed row carries an origin: name=%v version=%v", rec.OriginName, rec.OriginVersion)
	}
}
