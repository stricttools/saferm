package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// holdWriteLock takes SQLite's write lock on dbPath from a second connection
// and holds it until the returned release func runs. BEGIN IMMEDIATE acquires
// the lock at once rather than on first write, so contention is deterministic
// from the moment this returns.
func holdWriteLock(t *testing.T, dbPath string) func() {
	t.Helper()

	sqlDB, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout%3d0&_pragma=journal_mode%3dWAL")
	if err != nil {
		t.Fatalf("opening lock-holder connection: %v", err)
	}
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("reserving lock-holder connection: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("acquiring write lock: %v", err)
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			conn.Close()
			sqlDB.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// openContended opens a database whose own busy_timeout is zero, so SQLite
// reports contention immediately instead of waiting five seconds for it. That
// is what makes these tests fast: what they are about is saferm's retry, not
// SQLite's wait.
func openContended(t *testing.T, dbPath string, notify RetryNotifier) *DB {
	t.Helper()
	d, err := open(dbPath, t.TempDir(), 0, notify)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestIsContentionRecognizesBusyAndNothingElse(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	seed := openContended(t, dbPath, nil)
	if _, err := seed.Insert(makeRecord("uuid-seed", "/tmp/seed.txt", time.Now())); err != nil {
		t.Fatalf("seed insert failed: %v", err)
	}

	release := holdWriteLock(t, dbPath)
	defer release()

	// The raw driver error, taken without any retry around it.
	_, rawErr := seed.conn.Exec(`UPDATE deletions SET description = 'contended'`)
	if rawErr == nil {
		t.Fatal("expected the held write lock to make this UPDATE fail")
	}
	if !IsContention(rawErr) {
		t.Errorf("IsContention did not recognize %#v (%s) as contention", rawErr, rawErr)
	}

	if IsContention(nil) {
		t.Error("IsContention(nil) must be false")
	}
	if IsContention(ErrNotFound) {
		t.Error("a missing record is not contention")
	}
	if IsContention(errors.New("disk exploded")) {
		t.Error("an arbitrary error is not contention")
	}
}

func TestInsertExhaustsTheRetryBudgetUnderAHeldLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	d := openContended(t, dbPath, nil)
	if _, err := d.Insert(makeRecord("uuid-seed", "/tmp/seed.txt", time.Now())); err != nil {
		t.Fatalf("seed insert failed: %v", err)
	}

	release := holdWriteLock(t, dbPath)
	defer release()

	_, err := d.Insert(makeRecord("uuid-blocked", "/tmp/blocked.txt", time.Now()))
	if err == nil {
		t.Fatal("expected the insert to fail against a held write lock")
	}
	if !IsContentionExhausted(err) {
		t.Fatalf("expected an exhausted-contention error, got %#v (%s)", err, err)
	}

	var ce *ContentionError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ContentionError, got %T", err)
	}
	if ce.Attempts != busyMaxAttempts {
		t.Errorf("reported %d attempts, want %d", ce.Attempts, busyMaxAttempts)
	}
	// The underlying driver error survives, so a caller can still see what
	// SQLite actually said.
	if !IsContention(errors.Unwrap(err)) {
		t.Errorf("the wrapped error is not the driver's contention error: %v", errors.Unwrap(err))
	}
	// Other database failures must not be mistaken for contention.
	if IsContentionExhausted(ErrNotFound) {
		t.Error("a missing record must not classify as exhausted contention")
	}
}

func TestInsertRetriesAndSucceedsWhenTheLockIsReleased(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	var mu sync.Mutex
	var attempts []int
	notify := func(attempt, maxAttempts int, delay time.Duration, err error) {
		mu.Lock()
		defer mu.Unlock()
		attempts = append(attempts, attempt)
		if maxAttempts != busyMaxAttempts {
			t.Errorf("notifier reported budget %d, want %d", maxAttempts, busyMaxAttempts)
		}
		if delay != time.Duration(attempt)*busyBackoffStep {
			t.Errorf("notifier reported delay %s for attempt %d, want %s", delay, attempt, time.Duration(attempt)*busyBackoffStep)
		}
		if !IsContention(err) {
			t.Errorf("notifier reported a non-contention error: %v", err)
		}
	}

	d := openContended(t, dbPath, notify)
	if _, err := d.Insert(makeRecord("uuid-seed", "/tmp/seed.txt", time.Now())); err != nil {
		t.Fatalf("seed insert failed: %v", err)
	}

	release := holdWriteLock(t, dbPath)
	// Released after the first attempt has certainly failed but well inside the
	// budget, so the insert can only succeed by retrying.
	go func() {
		time.Sleep(80 * time.Millisecond)
		release()
	}()

	id, err := d.Insert(makeRecord("uuid-delayed", "/tmp/delayed.txt", time.Now()))
	if err != nil {
		t.Fatalf("insert should have succeeded once the lock was released: %v", err)
	}
	if id == 0 {
		t.Error("insert returned no row id")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) == 0 {
		t.Fatal("the retry was never reported to the notifier")
	}
	if attempts[0] != 1 {
		t.Errorf("first reported attempt is %d, want 1", attempts[0])
	}

	// The record really exists: the retry completed the write rather than
	// reporting a success it did not perform.
	recs, err := d.QueryByPath("/tmp/delayed.txt")
	if err != nil {
		t.Fatalf("querying the retried record: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record at /tmp/delayed.txt, got %d", len(recs))
	}
}
