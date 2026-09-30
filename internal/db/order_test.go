package db

import (
	"fmt"
	"testing"
	"time"
)

// namedQuery is one multi-row read, with arguments that make EXPLAIN compile
// the same plan the method runs.
type namedQuery struct {
	name string
	sql  string
	args []any
}

// multiRowQueries is every multi-row read this package issues. A new one is
// added here, so the backward-scan check below covers it.
func multiRowQueries() []namedQuery {
	now := time.Now().Format(time.RFC3339)
	return []namedQuery{
		{"QueryAll(live)", queryAllSQL(false), nil},
		{"QueryAll(all)", queryAllSQL(true), nil},
		{"QueryByPath", queryByPathSQL, []any{"/a/b"}},
		{"QueryOlderThan", queryOlderThanSQL, []any{now}},
		{"QueryPathRange(bounded)", queryPathRangeSQL(true), []any{"/a/", "/a0"}},
		{"QueryPathRange(unbounded)", queryPathRangeSQL(false), []any{""}},
		{"QueryByIDs(live)", queryByIDsSQL(false), []any{"[1,2,3]"}},
		{"QueryByIDs(all)", queryByIDsSQL(true), []any{"[1,2,3]"}},
	}
}

// explainOpcodes compiles a statement under EXPLAIN and returns its opcodes.
func explainOpcodes(t *testing.T, d *DB, q namedQuery) []string {
	t.Helper()
	rows, err := d.conn.Query("EXPLAIN "+q.sql, q.args...)
	if err != nil {
		t.Fatalf("%s: EXPLAIN failed: %v", q.name, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("%s: reading EXPLAIN columns: %v", q.name, err)
	}
	var ops []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("%s: scanning EXPLAIN row: %v", q.name, err)
		}
		ops = append(ops, fmt.Sprint(vals[1]))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: EXPLAIN rows: %v", q.name, err)
	}
	return ops
}

// A multi-row read must walk its table or index forward. The archive's
// database lives on disks with no read-ahead for a backward walk, where
// reading the same rows newest-first took minutes and oldest-first took about
// a second -- so every such query reads oldest-first and the caller's order is
// produced by reversing in memory. A backward walk shows up in the compiled
// program as a Last opcode positioning the cursor at the end and Prev stepping
// it back.
func TestMultiRowQueriesNeverScanBackward(t *testing.T) {
	d := openTestDB(t)
	for _, q := range multiRowQueries() {
		for _, op := range explainOpcodes(t, d, q) {
			if op == "Prev" || op == "Last" {
				t.Errorf("%s scans backward (opcode %s):\n%s", q.name, op, q.sql)
				break
			}
		}
	}
}

// The visible order is newest first, and records deleted in the same second
// come highest id first -- the later insert first. Reading oldest-first and
// reversing has to keep that tie-break, not merely the timestamps' order.
func TestNewestFirstTieBreakIsHighestIDFirst(t *testing.T) {
	d := openTestDB(t)

	older := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	same := older.Add(time.Hour)
	path := "/tie/break.txt"

	var ids []int64
	for i, ts := range []time.Time{older, same, same, same} {
		id, err := d.Insert(makeRecord(fmt.Sprintf("uuid-tie-%d", i), path, ts))
		if err != nil {
			t.Fatalf("Insert %d failed: %v", i, err)
		}
		ids = append(ids, id)
	}
	want := []int64{ids[3], ids[2], ids[1], ids[0]}

	check := func(name string, recs []*DeletionRecord, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s failed: %v", name, err)
		}
		var got []int64
		for _, r := range recs {
			got = append(got, r.ID)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s order = %v, want %v", name, got, want)
		}
	}

	recs, err := d.QueryAll(false)
	check("QueryAll(false)", recs, err)
	recs, err = d.QueryAll(true)
	check("QueryAll(true)", recs, err)
	recs, err = d.QueryByPath(path)
	check("QueryByPath", recs, err)
	recs, err = d.QueryOlderThan(same.Add(time.Minute))
	check("QueryOlderThan", recs, err)
}
