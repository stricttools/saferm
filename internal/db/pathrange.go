package db

import (
	"encoding/json"
)

// PathCandidate is one entry of the original_path index: a record's id and
// the path it was archived from, and nothing else.
type PathCandidate struct {
	ID   int64
	Path string
}

// queryPathRangeSQL selects (id, original_path) for every path in
// [lower, upper), or from lower on when the range is unbounded. It names only
// columns the original_path index carries, so SQLite answers it from the
// index and never reads a table row -- the rows are what hold the metadata
// blobs that make the table large.
func queryPathRangeSQL(bounded bool) string {
	query := `SELECT id, original_path FROM deletions WHERE original_path >= ?`
	if bounded {
		query += ` AND original_path < ?`
	}
	return query + ` ORDER BY original_path ASC`
}

// QueryPathRange returns the id and original path of every record, live or
// not, whose original path lies in [lower, upper) -- or is at least lower when
// bounded is false. The comparison is SQLite's binary one, byte by byte, so a
// range built from a byte prefix holds every path carrying that prefix.
func (d *DB) QueryPathRange(lower, upper string, bounded bool) ([]PathCandidate, error) {
	args := []any{lower}
	if bounded {
		args = append(args, upper)
	}
	var candidates []PathCandidate
	err := d.retry(func() error {
		candidates = nil
		rows, err := d.conn.Query(queryPathRangeSQL(bounded), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c PathCandidate
			if err := rows.Scan(&c.ID, &c.Path); err != nil {
				return err
			}
			candidates = append(candidates, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return candidates, nil
}

// queryByIDsSQL selects the records whose ids are listed in one JSON array
// parameter, or only the live ones among them.
func queryByIDsSQL(includeAll bool) string {
	query := `SELECT ` + recordColumns + ` FROM deletions WHERE id IN (SELECT value FROM json_each(?))`
	if !includeAll {
		query += ` AND ` + liveOnly
	}
	return query + oldestFirst
}

// QueryByIDs returns the records with the given ids, newest first. If
// includeAll is false, restored and purged records are left out. The ids go to
// SQLite as one JSON array bound to one parameter, so no id list, however
// long, meets SQLite's limit on the number of parameters in a statement.
func (d *DB) QueryByIDs(ids []int64, includeAll bool) ([]*DeletionRecord, error) {
	if len(ids) == 0 {
		return []*DeletionRecord{}, nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	records, err := d.queryRecords(queryByIDsSQL(includeAll), string(list))
	if err != nil {
		return nil, err
	}
	if records == nil {
		records = []*DeletionRecord{}
	}
	return records, nil
}
