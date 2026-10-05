package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// A schema saferm creates fresh and a schema it arrives at by walking the
// migration ladder must be the same schema. They are built by two independent
// pieces of code -- the CREATE TABLE in deletionsTableSQL and the steps in
// migrate -- and a column, a type, a nullability or an index that lands in only
// one of them is how the two silently fork: a fresh install and an upgraded one
// then disagree about what a deletion record is, and every test written against
// the fresh one certifies nothing about the installed base.
//
// The fixtures below are the real predecessors, not stripped-down inventions:
// each is the DDL saferm shipped at that rung of the ladder.

// ancientSchemaSQL is the deletions table as the first released schema created
// it: no symlink_target, no purged_at, no origin or group columns, and
// user_version never set (0).
const ancientSchemaSQL = `
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
	restored_to   TEXT
);

CREATE INDEX IF NOT EXISTS idx_deletions_original_path ON deletions(original_path);
CREATE INDEX IF NOT EXISTS idx_deletions_deleted_at ON deletions(deleted_at);
`

// columnInfo is one row of PRAGMA table_info, which is the whole of what SQLite
// will tell a reader about a column.
type columnInfo struct {
	CID     int
	Name    string
	Type    string
	NotNull int
	Default string
	PK      int
}

// indexInfo is one index as PRAGMA index_list and PRAGMA index_info describe it,
// including the implicit index a UNIQUE column creates.
type indexInfo struct {
	Name    string
	Unique  int
	Origin  string
	Partial int
	Columns []string
}

func readColumns(t *testing.T, conn *sql.DB) []columnInfo {
	t.Helper()
	rows, err := conn.Query("PRAGMA table_info(deletions)")
	if err != nil {
		t.Fatalf("reading columns: %v", err)
	}
	defer rows.Close()

	var cols []columnInfo
	for rows.Next() {
		var c columnInfo
		var dflt sql.NullString
		if err := rows.Scan(&c.CID, &c.Name, &c.Type, &c.NotNull, &dflt, &c.PK); err != nil {
			t.Fatalf("scanning a column: %v", err)
		}
		c.Default = dflt.String
		if !dflt.Valid {
			c.Default = "<null>"
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading columns: %v", err)
	}
	return cols
}

func readIndexes(t *testing.T, conn *sql.DB) []indexInfo {
	t.Helper()
	rows, err := conn.Query("PRAGMA index_list(deletions)")
	if err != nil {
		t.Fatalf("reading indexes: %v", err)
	}
	var indexes []indexInfo
	for rows.Next() {
		var seq int
		var idx indexInfo
		if err := rows.Scan(&seq, &idx.Name, &idx.Unique, &idx.Origin, &idx.Partial); err != nil {
			rows.Close()
			t.Fatalf("scanning an index: %v", err)
		}
		indexes = append(indexes, idx)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("reading indexes: %v", err)
	}
	rows.Close()

	for i := range indexes {
		cols, err := conn.Query(fmt.Sprintf("PRAGMA index_info(%q)", indexes[i].Name))
		if err != nil {
			t.Fatalf("reading the columns of %s: %v", indexes[i].Name, err)
		}
		for cols.Next() {
			var seqno, cid int
			var name sql.NullString
			if err := cols.Scan(&seqno, &cid, &name); err != nil {
				cols.Close()
				t.Fatalf("scanning an index column: %v", err)
			}
			indexes[i].Columns = append(indexes[i].Columns, name.String)
		}
		if err := cols.Err(); err != nil {
			cols.Close()
			t.Fatalf("reading the columns of %s: %v", indexes[i].Name, err)
		}
		cols.Close()
	}

	// Index order is SQLite's own bookkeeping and carries no meaning; the SET is
	// what has to agree.
	sort.Slice(indexes, func(i, j int) bool { return indexes[i].Name < indexes[j].Name })
	return indexes
}

// openAtSchema creates a database with the given DDL and returns its path,
// having written one row through it so the migration runs against a populated
// table rather than an empty one.
func openAtSchema(t *testing.T, name, ddl string) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), name+".db")
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening the %s database: %v", name, err)
	}
	defer conn.Close()

	if _, err := conn.Exec(ddl); err != nil {
		t.Fatalf("creating the %s schema: %v", name, err)
	}
	if _, err := conn.Exec(
		`INSERT INTO deletions (uuid, original_path, original_name, size, hash, is_directory, deleted_at, command, description, metadata)
		 VALUES (?, '/tmp/old.txt', 'old.txt', 7, 'deadbeef', 0, ?, 'rm', 'a deletion from the '||?||' schema', '{}')`,
		name+"-uuid", time.Now().Format(time.RFC3339), name,
	); err != nil {
		t.Fatalf("inserting a row into the %s database: %v", name, err)
	}
	return dbPath
}

func TestSchema_FreshAndMigratedDatabasesAgree(t *testing.T) {
	fresh := openTestDB(t)
	wantColumns := readColumns(t, fresh.conn)
	wantIndexes := readIndexes(t, fresh.conn)

	// A fresh database must itself be at the ladder's top: a migration that
	// stops short would otherwise make every comparison below agree on the wrong
	// schema.
	var freshVersion int
	if err := fresh.conn.QueryRow("PRAGMA user_version").Scan(&freshVersion); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}

	for _, tc := range []struct {
		label string
		path  string
	}{
		{"ancient", openAtSchema(t, "ancient", ancientSchemaSQL)},
		{"legacy", openAtSchema(t, "legacy", legacySchemaSQL)},
	} {
		d, err := Open(tc.path, t.TempDir(), nil)
		if err != nil {
			t.Fatalf("%s: Open failed: %v", tc.label, err)
		}

		var version int
		if err := d.conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatalf("%s: reading user_version: %v", tc.label, err)
		}
		if version != freshVersion {
			t.Errorf("%s: user_version is %d after migrating, fresh is %d", tc.label, version, freshVersion)
		}

		got := readColumns(t, d.conn)
		if len(got) != len(wantColumns) {
			t.Errorf("%s: has %d columns, fresh has %d\n  migrated: %+v\n  fresh:    %+v",
				tc.label, len(got), len(wantColumns), got, wantColumns)
		} else {
			for i := range got {
				if got[i] != wantColumns[i] {
					t.Errorf("%s: column %d is %+v, fresh has %+v", tc.label, i, got[i], wantColumns[i])
				}
			}
		}

		gotIndexes := readIndexes(t, d.conn)
		if len(gotIndexes) != len(wantIndexes) {
			t.Errorf("%s: has %d indexes, fresh has %d\n  migrated: %+v\n  fresh:    %+v",
				tc.label, len(gotIndexes), len(wantIndexes), gotIndexes, wantIndexes)
		} else {
			for i := range gotIndexes {
				a, b := gotIndexes[i], wantIndexes[i]
				if a.Name != b.Name || a.Unique != b.Unique || a.Origin != b.Origin || a.Partial != b.Partial ||
					fmt.Sprint(a.Columns) != fmt.Sprint(b.Columns) {
					t.Errorf("%s: index %+v, fresh has %+v", tc.label, a, b)
				}
			}
		}

		// The row written before the migration is still readable through the
		// migrated schema, with nothing backfilled into the new columns.
		rec, err := d.QueryByUUID(tc.label + "-uuid")
		if err != nil {
			t.Fatalf("%s: querying the pre-migration row: %v", tc.label, err)
		}
		if rec.OriginName != nil || rec.OriginVersion != nil || rec.GroupID != nil {
			t.Errorf("%s: the pre-migration row was backfilled: name=%v version=%v group=%v",
				tc.label, rec.OriginName, rec.OriginVersion, rec.GroupID)
		}
		d.Close()
	}
}
