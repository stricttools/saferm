package db

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// queryPlan returns the EXPLAIN QUERY PLAN detail lines of a statement.
func queryPlan(t *testing.T, d *DB, q namedQuery) string {
	t.Helper()
	rows, err := d.conn.Query("EXPLAIN QUERY PLAN "+q.sql, q.args...)
	if err != nil {
		t.Fatalf("%s: EXPLAIN QUERY PLAN failed: %v", q.name, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("%s: scanning plan: %v", q.name, err)
		}
		lines = append(lines, detail)
	}
	return strings.Join(lines, "\n")
}

// The first phase of a --path filter reads (id, original_path) out of the
// path index alone -- never a table row, whose metadata blob is what makes
// the table large.
func TestQueryPathRangeReadsOnlyThePathIndex(t *testing.T) {
	d := openTestDB(t)
	for _, q := range []namedQuery{
		{"QueryPathRange(bounded)", queryPathRangeSQL(true), []any{"/a/", "/a0"}},
		{"QueryPathRange(unbounded)", queryPathRangeSQL(false), []any{""}},
	} {
		plan := queryPlan(t, d, q)
		if !strings.Contains(plan, "COVERING INDEX idx_deletions_original_path") {
			t.Errorf("%s: plan does not read the covering path index:\n%s", q.name, plan)
		}
	}
}

// The second phase fetches the matched rows by id, not by scanning.
func TestQueryByIDsLooksUpByRowid(t *testing.T) {
	d := openTestDB(t)
	for _, all := range []bool{true, false} {
		plan := queryPlan(t, d, namedQuery{"QueryByIDs", queryByIDsSQL(all), []any{"[1,2]"}})
		if !strings.Contains(plan, "INTEGER PRIMARY KEY") {
			t.Errorf("includeAll=%v: plan does not look rows up by id:\n%s", all, plan)
		}
	}
}

func TestQueryPathRange(t *testing.T) {
	d := openTestDB(t)
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	paths := []string{"/a/x", "/a/y/z", "/a0", "/b/x", "/a\xff/q", "/a\xff\xff", "/a"}
	ids := map[string]int64{}
	for i, p := range paths {
		id, err := d.Insert(makeRecord(fmt.Sprintf("uuid-range-%d", i), p, base.Add(time.Duration(i)*time.Minute)))
		if err != nil {
			t.Fatalf("Insert %q failed: %v", p, err)
		}
		ids[p] = id
	}

	got := func(lo, hi string, bounded bool) string {
		t.Helper()
		cands, err := d.QueryPathRange(lo, hi, bounded)
		if err != nil {
			t.Fatalf("QueryPathRange(%q, %q, %v) failed: %v", lo, hi, bounded, err)
		}
		var out []string
		for _, c := range cands {
			if ids[c.Path] != c.ID {
				t.Errorf("candidate %q carries id %d, want %d", c.Path, c.ID, ids[c.Path])
			}
			out = append(out, fmt.Sprintf("%q", c.Path))
		}
		return strings.Join(out, " ")
	}

	if g, w := got("/a/", "/a0", true), `"/a/x" "/a/y/z"`; g != w {
		t.Errorf("range [/a/, /a0) = %s, want %s", g, w)
	}
	// A 0xff byte is a real path byte, and the range must hold it.
	if g, w := got("/a\xff", "/b", true), `"/a\xff/q" "/a\xff\xff"`; g != w {
		t.Errorf("range [/a\\xff, /b) = %s, want %s", g, w)
	}
	// Unbounded: everything from the lower bound on, in path order.
	if g, w := got("/a\xff\xff", "", false), `"/a\xff\xff" "/b/x"`; g != w {
		t.Errorf("unbounded range from /a\\xff\\xff = %s, want %s", g, w)
	}
	if g := got("", "", false); strings.Count(g, `"`)/2 != len(paths) {
		t.Errorf("unbounded range from the empty string = %s, want every path", g)
	}
}

func TestQueryByIDs(t *testing.T) {
	d := openTestDB(t)
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	var ids []int64
	for i, ts := range []time.Time{base, base.Add(time.Hour), base.Add(time.Hour), base.Add(2 * time.Hour)} {
		id, err := d.Insert(makeRecord(fmt.Sprintf("uuid-ids-%d", i), fmt.Sprintf("/ids/%d", i), ts))
		if err != nil {
			t.Fatalf("Insert %d failed: %v", i, err)
		}
		ids = append(ids, id)
	}
	if err := d.MarkRestored(ids[3], "/ids/back"); err != nil {
		t.Fatalf("MarkRestored failed: %v", err)
	}

	order := func(recs []*DeletionRecord, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("QueryByIDs failed: %v", err)
		}
		var out []int64
		for _, r := range recs {
			out = append(out, r.ID)
		}
		return fmt.Sprint(out)
	}

	pick := []int64{ids[0], ids[3], ids[1], ids[2]}
	if g, w := order(d.QueryByIDs(pick, true)), fmt.Sprint([]int64{ids[3], ids[2], ids[1], ids[0]}); g != w {
		t.Errorf("QueryByIDs(all) = %s, want newest first with the tie highest id first %s", g, w)
	}
	if g, w := order(d.QueryByIDs(pick, false)), fmt.Sprint([]int64{ids[2], ids[1], ids[0]}); g != w {
		t.Errorf("QueryByIDs(live) = %s, want %s", g, w)
	}
	if g := order(d.QueryByIDs(nil, true)); g != "[]" {
		t.Errorf("QueryByIDs(nil) = %s, want []", g)
	}
}

// deleted_at is text in the deleting process's zone, so an instant written in
// a zone far behind UTC reads as an earlier clock time. QueryDeletedSince
// compares instants, whatever zones the rows were written in.
func TestQueryDeletedSinceComparesInstantsAcrossZones(t *testing.T) {
	d := openTestDB(t)
	since := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	west := time.FixedZone("west", -11*3600)
	east := time.FixedZone("east", 14*3600)
	rows := []struct {
		at   time.Time
		keep bool
	}{
		{since.Add(-time.Second).In(east), false},
		{since.In(west), true},
		{since.Add(time.Hour).In(west), true},
		{since.Add(-time.Hour).In(west), false},
		{since.Add(30 * time.Minute).In(east), true},
		{since.Add(-40 * time.Hour), false},
	}
	want := map[int64]bool{}
	for i, r := range rows {
		id, err := d.Insert(makeRecord(fmt.Sprintf("uuid-since-%d", i), fmt.Sprintf("/since/%d", i), r.at))
		if err != nil {
			t.Fatalf("Insert %d failed: %v", i, err)
		}
		if r.keep {
			want[id] = true
		}
	}
	got, err := d.QueryDeletedSince(since, false)
	if err != nil {
		t.Fatalf("QueryDeletedSince failed: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("QueryDeletedSince kept %d records, want %d", len(got), len(want))
	}
	for _, rec := range got {
		if !want[rec.ID] {
			t.Errorf("record %d deleted at %s is before %s", rec.ID, rec.DeletedAt, since)
		}
	}
}
