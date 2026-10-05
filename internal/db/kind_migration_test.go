package db

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stricttools/saferm/internal/archive"
)

// v3SchemaSQL is the deletions table as saferm shipped it at user_version 3,
// the last schema with is_directory and no kind column.
const v3SchemaSQL = `
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
	purged_at     TEXT,
	origin_name    TEXT,
	origin_version TEXT,
	group_id       TEXT
);
CREATE INDEX IF NOT EXISTS idx_deletions_original_path ON deletions(original_path);
CREATE INDEX IF NOT EXISTS idx_deletions_deleted_at ON deletions(deleted_at);
PRAGMA user_version = 3;
`

// openV3DB writes one file, one tree, one symlink and one symlink to a
// directory (which carries both markers) through the version-3 schema, then
// removes a fifth row so the id counter runs ahead of the highest id left.
func openV3DB(t *testing.T) string {
	t.Helper()
	dbPath := t.TempDir() + "/v3.db"
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(v3SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	for _, row := range []struct {
		uuid   string
		isDir  int
		target any
	}{
		{"file-uuid", 0, nil},
		{"dir-uuid", 1, nil},
		{"link-uuid", 0, "elsewhere"},
		{"dirlink-uuid", 1, "a-directory"},
		{"gone-uuid", 0, nil},
	} {
		if _, err := conn.Exec(
			`INSERT INTO deletions (uuid, original_path, original_name, size, hash, is_directory, deleted_at, description, symlink_target)
			 VALUES (?, '/x/'||?, ?, 1, 'h', ?, ?, 'd', ?)`,
			row.uuid, row.uuid, row.uuid, row.isDir, now, row.target); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(`DELETE FROM deletions WHERE uuid = 'gone-uuid'`); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

func TestMigration4_DerivesEveryRowsKind(t *testing.T) {
	d, err := Open(openV3DB(t), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	want := map[string]archive.Kind{
		"file-uuid":    archive.KindFile,
		"dir-uuid":     archive.KindDirectory,
		"link-uuid":    archive.KindSymlink,
		"dirlink-uuid": archive.KindSymlink,
	}
	for uuid, kind := range want {
		rec, err := d.QueryByUUID(uuid)
		if err != nil {
			t.Fatalf("%s: %v", uuid, err)
		}
		if rec.Kind != kind {
			t.Errorf("%s: kind %q, want %q", uuid, rec.Kind, kind)
		}
	}
	if present, err := hasColumn(d.conn, "deletions", "is_directory"); err != nil || present {
		t.Errorf("is_directory survived the rebuild (present=%v, err=%v)", present, err)
	}
}

func TestMigration4_NeverReissuesAnID(t *testing.T) {
	d, err := Open(openV3DB(t), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	id, err := d.Insert(&DeletionRecord{UUID: "new", OriginalPath: "/x/new", OriginalName: "new",
		Hash: "h", Kind: archive.KindFile, DeletedAt: time.Now(), Description: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if id != 6 {
		t.Errorf("the first id after the migration is %d; the old counter stood at 5, so it must be 6", id)
	}
}

func TestKindColumn_RefusesAnUnknownKindAndAnInconsistentSymlink(t *testing.T) {
	for _, label := range []string{"fresh", "migrated"} {
		path := t.TempDir() + "/k.db"
		if label == "migrated" {
			path = openV3DB(t)
		}
		d, err := Open(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().Format(time.RFC3339)
		insert := `INSERT INTO deletions (uuid, original_path, original_name, size, hash, kind, deleted_at, description, symlink_target) VALUES (?, '/p', 'p', 0, '', ?, ?, 'd', ?)`
		if _, err := d.conn.Exec(insert, "u1", "bogus", now, nil); err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("%s: an unknown kind was accepted (%v)", label, err)
		}
		if _, err := d.conn.Exec(insert, "u2", string(archive.KindSymlink), now, nil); err == nil {
			t.Errorf("%s: a symlink without a target was accepted", label)
		}
		if _, err := d.conn.Exec(insert, "u3", string(archive.KindFile), now, "t"); err == nil {
			t.Errorf("%s: a file carrying a symlink target was accepted", label)
		}
		for i, k := range archive.Kinds() {
			var target any
			if k == archive.KindSymlink {
				target = "t"
			}
			if _, err := d.conn.Exec(insert, "k"+string(rune('a'+i)), string(k), now, target); err != nil {
				t.Errorf("%s: kind %q refused: %v", label, k, err)
			}
		}
		d.Close()
	}
}
