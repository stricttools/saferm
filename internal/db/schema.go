package db

import (
	"fmt"
	"strings"

	"github.com/stricttools/saferm/internal/archive"
)

// deletionsTableSQL is the deletions table's definition under the given name.
// A fresh database creates it as `deletions`; migration 4 creates it under a
// temporary name, copies the rows across and renames it, because SQLite cannot
// add a column with a CHECK constraint or drop one in place.
//
// `kind` is the single authority for what a record archived. Its CHECK lists
// [archive.Kinds], generated rather than typed, and a symlink is the one kind
// that carries a target, which the table-level CHECK holds the row to.
func deletionsTableSQL(name string) string {
	quoted := make([]string, 0, len(archive.Kinds()))
	for _, k := range archive.Kinds() {
		quoted = append(quoted, "'"+string(k)+"'")
	}
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	uuid          TEXT NOT NULL UNIQUE,
	original_path TEXT NOT NULL,
	original_name TEXT NOT NULL,
	size          INTEGER NOT NULL,
	hash          TEXT NOT NULL,
	kind          TEXT NOT NULL CHECK (kind IN (%s)),
	deleted_at    TEXT NOT NULL,
	command       TEXT,
	description   TEXT NOT NULL,
	metadata      TEXT,
	restored_at   TEXT,
	restored_to   TEXT,
	symlink_target TEXT,
	purged_at     TEXT,
	-- Who ran the deletion, derived at insert time from the process trace store
	-- (STRICTCLI_TRACE_PARENT). Both nullable and never empty; null means no
	-- tool claimed this deletion. A version without a name is refused in code
	-- rather than by a CHECK constraint -- see Insert.
	origin_name    TEXT,
	origin_version TEXT,
	-- The identifier every record of one delete invocation shares. Minted per
	-- invocation, so a batch is recoverable as a batch; null on rows written
	-- before the column existed.
	group_id       TEXT,
	CHECK ((kind = '%s') = (symlink_target IS NOT NULL))
);`, name, strings.Join(quoted, ", "), archive.KindSymlink)
}

// indexesSQL creates the deletions table's indexes. It runs after the
// migrations, because migration 4 replaces the table and its indexes go with
// the table it drops.
const indexesSQL = `
CREATE INDEX IF NOT EXISTS idx_deletions_original_path ON deletions(original_path);
CREATE INDEX IF NOT EXISTS idx_deletions_deleted_at ON deletions(deleted_at);
`
