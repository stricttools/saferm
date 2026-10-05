package db

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stricttools/saferm/internal/archive"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(dbPath, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func makeRecord(uuid string, path string, deletedAt time.Time) *DeletionRecord {
	return &DeletionRecord{
		UUID:         uuid,
		OriginalPath: path,
		OriginalName: filepath.Base(path),
		Size:         1024,
		Hash:         "abc123",
		NodeType:     archive.NodeTypeFile,
		DeletedAt:    deletedAt,
		Command:      "rm",
		Description:  "test deletion",
		Metadata:     `{"cwd":"/tmp"}`,
	}
}

func TestOpen_CreatesSchema(t *testing.T) {
	d := openTestDB(t)

	// Verify the deletions table exists by querying it.
	rows, err := d.conn.Query("SELECT name FROM sqlite_master WHERE type='table' AND name='deletions'")
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("deletions table was not created")
	}

	// Verify indexes exist.
	var indexNames []string
	idxRows, err := d.conn.Query("SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='deletions'")
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer idxRows.Close()
	for idxRows.Next() {
		var name string
		if err := idxRows.Scan(&name); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		indexNames = append(indexNames, name)
	}

	wantIndexes := map[string]bool{
		"idx_deletions_original_path": false,
		"idx_deletions_deleted_at":    false,
	}
	for _, name := range indexNames {
		if _, ok := wantIndexes[name]; ok {
			wantIndexes[name] = true
		}
	}
	for name, found := range wantIndexes {
		if !found {
			t.Errorf("expected index %q not found", name)
		}
	}
}

// hasColumn used to discard its query error and answer "no such column". Inside
// migrate -- which the contention retry now wraps -- that reading turned a
// transient failure into a schema decision: the ALTER that followed failed with
// its own unrelated message, and the retry never saw the contention error that
// would have classified it. The query error has to surface instead.
func TestHasColumnSurfacesQueryErrors(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(dbPath, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// The column really is there while the connection is open, so the closed
	// connection below is the only thing the second call changes.
	present, err := hasColumn(d.conn, "deletions", "symlink_target")
	if err != nil {
		t.Fatalf("hasColumn on an open connection errored: %v", err)
	}
	if !present {
		t.Fatal("symlink_target is missing from a freshly created schema")
	}

	if err := d.Close(); err != nil {
		t.Fatalf("closing the database: %v", err)
	}

	present, err = hasColumn(d.conn, "deletions", "symlink_target")
	if err == nil {
		t.Fatalf("hasColumn on a closed connection returned (%v, nil); the query error must surface", present)
	}
	if present {
		t.Errorf("hasColumn reported the column present on a failed query")
	}
}

func TestInsertAndQueryByID(t *testing.T) {
	d := openTestDB(t)
	now := time.Now().Truncate(time.Second)

	rec := &DeletionRecord{
		UUID:         "uuid-001",
		OriginalPath: "/home/user/file.txt",
		OriginalName: "file.txt",
		Size:         4096,
		Hash:         "sha256:deadbeef",
		NodeType:     archive.NodeTypeDirectory,
		DeletedAt:    now,
		Command:      "rm -rf",
		Description:  "deleted a directory",
		Metadata:     `{"env":"test"}`,
	}

	id, err := d.Insert(rec)
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}
	if id <= 0 {
		t.Fatalf("Insert returned invalid ID: %d", id)
	}

	got, err := d.QueryByID(id)
	if err != nil {
		t.Fatalf("QueryByID failed: %v", err)
	}

	if got.ID != id {
		t.Errorf("ID = %d, want %d", got.ID, id)
	}
	if got.UUID != "uuid-001" {
		t.Errorf("UUID = %q, want %q", got.UUID, "uuid-001")
	}
	if got.OriginalPath != "/home/user/file.txt" {
		t.Errorf("OriginalPath = %q, want %q", got.OriginalPath, "/home/user/file.txt")
	}
	if got.OriginalName != "file.txt" {
		t.Errorf("OriginalName = %q, want %q", got.OriginalName, "file.txt")
	}
	if got.Size != 4096 {
		t.Errorf("Size = %d, want %d", got.Size, 4096)
	}
	if got.Hash != "sha256:deadbeef" {
		t.Errorf("Hash = %q, want %q", got.Hash, "sha256:deadbeef")
	}
	if got.NodeType != archive.NodeTypeDirectory {
		t.Errorf("NodeType = %q, want directory", got.NodeType)
	}
	if !got.DeletedAt.Equal(now) {
		t.Errorf("DeletedAt = %v, want %v", got.DeletedAt, now)
	}
	if got.Command != "rm -rf" {
		t.Errorf("Command = %q, want %q", got.Command, "rm -rf")
	}
	if got.Description != "deleted a directory" {
		t.Errorf("Description = %q, want %q", got.Description, "deleted a directory")
	}
	if got.Metadata != `{"env":"test"}` {
		t.Errorf("Metadata = %q, want %q", got.Metadata, `{"env":"test"}`)
	}
	if got.RestoredAt != nil {
		t.Errorf("RestoredAt = %v, want nil", got.RestoredAt)
	}
	if got.RestoredTo != nil {
		t.Errorf("RestoredTo = %v, want nil", got.RestoredTo)
	}
}

func TestQueryByID_NotFound(t *testing.T) {
	d := openTestDB(t)

	_, err := d.QueryByID(999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// The uuid is the handle `delete` hands back, so looking a record up by it has
// to be exact and has to reach restored and purged records too -- a caller
// asking about a consumed record is asking a legitimate question.
func TestQueryByUUID(t *testing.T) {
	d := openTestDB(t)

	rec := makeRecord("11111111-2222-4333-8444-555555555555", "/home/user/handle.txt", time.Now())
	id, err := d.Insert(rec)
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	got, err := d.QueryByUUID(rec.UUID)
	if err != nil {
		t.Fatalf("QueryByUUID failed: %v", err)
	}
	if got.ID != id || got.OriginalPath != rec.OriginalPath {
		t.Errorf("QueryByUUID returned id=%d path=%q, want id=%d path=%q",
			got.ID, got.OriginalPath, id, rec.OriginalPath)
	}

	if err := d.MarkRestored(id, rec.OriginalPath); err != nil {
		t.Fatalf("MarkRestored failed: %v", err)
	}
	got, err = d.QueryByUUID(rec.UUID)
	if err != nil {
		t.Fatalf("QueryByUUID after restore failed: %v", err)
	}
	if got.RestoredAt == nil {
		t.Error("QueryByUUID must reach a consumed record and report it as restored")
	}
}

func TestQueryByUUID_NotFound(t *testing.T) {
	d := openTestDB(t)

	_, err := d.QueryByUUID("99999999-9999-4999-8999-999999999999")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestQueryByPath(t *testing.T) {
	d := openTestDB(t)

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	path := "/home/user/data.csv"

	// Insert 3 records with same path at different times.
	for i := 0; i < 3; i++ {
		rec := makeRecord(fmt.Sprintf("uuid-path-%d", i), path, base.Add(time.Duration(i)*time.Hour))
		if _, err := d.Insert(rec); err != nil {
			t.Fatalf("Insert %d failed: %v", i, err)
		}
	}

	// Insert one with a different path.
	other := makeRecord("uuid-other", "/different/path.txt", base)
	if _, err := d.Insert(other); err != nil {
		t.Fatalf("Insert other failed: %v", err)
	}

	results, err := d.QueryByPath(path)
	if err != nil {
		t.Fatalf("QueryByPath failed: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("QueryByPath returned %d records, want 3", len(results))
	}

	// Verify newest first.
	for i := 1; i < len(results); i++ {
		if results[i].DeletedAt.After(results[i-1].DeletedAt) {
			t.Errorf("results not ordered by deleted_at DESC: [%d]=%v > [%d]=%v",
				i, results[i].DeletedAt, i-1, results[i-1].DeletedAt)
		}
	}
}

func TestQueryByPath_ExcludesRestored(t *testing.T) {
	d := openTestDB(t)

	path := "/home/user/restored.txt"
	now := time.Now().Truncate(time.Second)

	rec := makeRecord("uuid-restored", path, now)
	id, err := d.Insert(rec)
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	if err := d.MarkRestored(id, "/home/user/restored.txt"); err != nil {
		t.Fatalf("MarkRestored failed: %v", err)
	}

	results, err := d.QueryByPath(path)
	if err != nil {
		t.Fatalf("QueryByPath failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("QueryByPath returned %d records, want 0 (restored should be excluded)", len(results))
	}
}

func TestQueryAll(t *testing.T) {
	d := openTestDB(t)

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// Insert 3 records.
	for i := 0; i < 3; i++ {
		rec := makeRecord(fmt.Sprintf("uuid-all-%d", i), fmt.Sprintf("/path/%d", i), base.Add(time.Duration(i)*time.Hour))
		if _, err := d.Insert(rec); err != nil {
			t.Fatalf("Insert %d failed: %v", i, err)
		}
	}

	// Mark one as restored.
	if err := d.MarkRestored(2, "/restored/path"); err != nil {
		t.Fatalf("MarkRestored failed: %v", err)
	}

	// Query without restored.
	results, err := d.QueryAll(false)
	if err != nil {
		t.Fatalf("QueryAll(false) failed: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("QueryAll(false) returned %d records, want 2", len(results))
	}

	// Query with restored.
	results, err = d.QueryAll(true)
	if err != nil {
		t.Fatalf("QueryAll(true) failed: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("QueryAll(true) returned %d records, want 3", len(results))
	}

	// Verify newest first.
	for i := 1; i < len(results); i++ {
		if results[i].DeletedAt.After(results[i-1].DeletedAt) {
			t.Errorf("results not ordered by deleted_at DESC: [%d]=%v > [%d]=%v",
				i, results[i].DeletedAt, i-1, results[i-1].DeletedAt)
		}
	}
}

func TestMarkRestored(t *testing.T) {
	d := openTestDB(t)

	now := time.Now().Truncate(time.Second)
	rec := makeRecord("uuid-mark", "/home/user/mark.txt", now)

	id, err := d.Insert(rec)
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	restoredTo := "/home/user/restored-mark.txt"
	if err := d.MarkRestored(id, restoredTo); err != nil {
		t.Fatalf("MarkRestored failed: %v", err)
	}

	got, err := d.QueryByID(id)
	if err != nil {
		t.Fatalf("QueryByID after restore failed: %v", err)
	}

	if got.RestoredAt == nil {
		t.Fatal("RestoredAt is nil after MarkRestored")
	}
	if got.RestoredTo == nil {
		t.Fatal("RestoredTo is nil after MarkRestored")
	}
	if *got.RestoredTo != restoredTo {
		t.Errorf("RestoredTo = %q, want %q", *got.RestoredTo, restoredTo)
	}
}

func TestMarkRestored_NotFound(t *testing.T) {
	d := openTestDB(t)

	err := d.MarkRestored(999, "/nowhere")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestQueryOlderThan(t *testing.T) {
	d := openTestDB(t)

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// Insert records at day 1, day 10, day 20.
	times := []time.Time{
		base,
		base.Add(10 * 24 * time.Hour),
		base.Add(20 * 24 * time.Hour),
	}
	for i, ts := range times {
		rec := makeRecord(fmt.Sprintf("uuid-older-%d", i), fmt.Sprintf("/path/older/%d", i), ts)
		if _, err := d.Insert(rec); err != nil {
			t.Fatalf("Insert %d failed: %v", i, err)
		}
	}

	// Query for records older than day 15.
	cutoff := base.Add(15 * 24 * time.Hour)
	results, err := d.QueryOlderThan(cutoff)
	if err != nil {
		t.Fatalf("QueryOlderThan failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("QueryOlderThan returned %d records, want 2", len(results))
	}

	// All returned records should be before the cutoff.
	for _, r := range results {
		if !r.DeletedAt.Before(cutoff) {
			t.Errorf("record with deleted_at=%v should be before %v", r.DeletedAt, cutoff)
		}
	}
}

func TestQueryOlderThan_ExcludesRestored(t *testing.T) {
	d := openTestDB(t)

	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := makeRecord("uuid-old-restored", "/old/file.txt", old)
	id, err := d.Insert(rec)
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	if err := d.MarkRestored(id, "/restored/file.txt"); err != nil {
		t.Fatalf("MarkRestored failed: %v", err)
	}

	results, err := d.QueryOlderThan(time.Now())
	if err != nil {
		t.Fatalf("QueryOlderThan failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("QueryOlderThan returned %d records, want 0 (restored excluded)", len(results))
	}
}

func TestConcurrentInserts(t *testing.T) {
	d := openTestDB(t)

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := makeRecord(
				fmt.Sprintf("uuid-concurrent-%d", idx),
				fmt.Sprintf("/concurrent/path/%d", idx),
				time.Now().Truncate(time.Second),
			)
			_, err := d.Insert(rec)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", idx, err)
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent insert error: %v", err)
	}

	// Verify all records were inserted.
	results, err := d.QueryAll(true)
	if err != nil {
		t.Fatalf("QueryAll failed: %v", err)
	}
	if len(results) != goroutines {
		t.Errorf("expected %d records, got %d", goroutines, len(results))
	}
}
