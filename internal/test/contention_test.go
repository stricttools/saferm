package test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stricttools/saferm/internal/testutil"
	_ "modernc.org/sqlite"
)

// exitContention and exitDatabase mirror main.ExitContention and
// main.ExitDatabase. The command package is package main and cannot be
// imported, so the numbers are restated here on purpose: these tests are the
// promise that the codes saferm returns for a contended and for a broken
// database never silently change.
const (
	exitContention = 8
	exitDatabase   = 5
)

// holdArchiveWriteLock takes SQLite's write lock on the archive database from a
// second connection and holds it until the returned release func runs.
//
// BEGIN IMMEDIATE acquires the write lock at once (rather than on first write),
// so from the moment this returns every other process that tries to write is
// met with SQLITE_BUSY once its own busy_timeout expires. That is the
// deterministic contention the retry exists for, and the concurrency tests'
// hand-rolled retry loops used to paper over.
func holdArchiveWriteLock(t *testing.T, homeDir string) func() {
	t.Helper()

	dbPath := filepath.Join(homeDir, ".saferm", "db", "saferm.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("archive database %s does not exist yet: %v", dbPath, err)
	}

	sqlDB, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout%3d5000&_pragma=journal_mode%3dWAL")
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

// TestDeleteExitsWithDatabaseCodeWhenTheArchiveDatabaseIsUnreadable proves that
// a command's database failure reaches the exit code through main.dbExit rather
// than through a code written out at the call site: a corrupt archive database
// is a non-contention database failure, and dbExit's other branch is the only
// thing that turns it into 5.
//
// It runs under -short, and it is deliberately the fast half of the pair with
// TestDeleteExitsWithContentionCodeWhenLockNeverReleased below: that one takes
// dbExit's contention branch end to end but needs ~25s to do it.
func TestDeleteExitsWithDatabaseCodeWhenTheArchiveDatabaseIsUnreadable(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	workDir := t.TempDir()
	// One successful delete first, so the archive database exists and the
	// command gets past "no archive yet" to actually opening it.
	seedArchive(t, homeDir, workDir, "seed.txt")

	dbPath := filepath.Join(homeDir, ".saferm", "db", "saferm.db")
	if err := os.WriteFile(dbPath, []byte("this is not a SQLite database\n"), 0o600); err != nil {
		t.Fatalf("overwriting the archive database: %v", err)
	}

	path := testutil.CreateTempFile(t, workDir, "unreadable.txt", "unreadable")
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "archive database is corrupt", path)

	if code != exitDatabase {
		t.Fatalf("expected exit %d (generic database failure), got %d: stderr=%q", exitDatabase, code, stderr)
	}
	if !strings.Contains(stderr, "database") {
		t.Errorf("stderr does not name the database failure: %q", stderr)
	}
	if strings.Contains(stderr, "contention") {
		t.Errorf("a corrupt database must not be reported as contention: %q", stderr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s was archived despite the database failure: %v", path, err)
	}
}

// The -short skip below is deliberate, and the gap it leaves is covered by fast
// tests that DO run under -short (which is what CI, the pre-push hook and the
// release preflight run):
//
//   - internal/db.TestInsertExhaustsTheRetryBudgetUnderAHeldLock -- a held write
//     lock really produces a *db.ContentionError, fast (busy_timeout=0).
//   - main.TestDbExit -- dbExit maps that type onto exit 8, and everything else
//     onto 5, with both numbers pinned as literals.
//   - TestDeleteExitsWithDatabaseCodeWhenTheArchiveDatabaseIsUnreadable above --
//     a command's database error path really routes through dbExit.
//
// Composed, those three assert what this test asserts end to end. This one stays
// as the real thing: a whole process, a real lock, a real budget.
func TestDeleteExitsWithContentionCodeWhenLockNeverReleased(t *testing.T) {
	if testing.Short() {
		t.Skip("exhausting the retry budget takes ~25s (five attempts against a 5s busy_timeout); the fast compositional coverage is named above")
	}
	t.Parallel()

	homeDir := t.TempDir()
	workDir := t.TempDir()
	// One successful delete first, so the database file and its schema exist
	// and a lock can be taken on them.
	seedArchive(t, homeDir, workDir, "seed.txt")

	holdArchiveWriteLock(t, homeDir)

	path := testutil.CreateTempFile(t, workDir, "blocked.txt", "blocked")
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "blocked by a held write lock", path)

	if code != exitContention {
		t.Fatalf("expected exit %d (contention exhausted), got %d: stderr=%q", exitContention, code, stderr)
	}
	if !strings.Contains(stderr, "database contention") {
		t.Errorf("stderr does not name the contention: %q", stderr)
	}
}

// This test's own -short gap is covered fast by
// internal/db.TestInsertRetriesAndSucceedsWhenTheLockIsReleased, which asserts
// the same thing one layer down (the retry completes the write, and the
// notifier is called) in milliseconds. What only this test can see is the
// --verbose retry line on the process's stderr.
func TestDeleteSucceedsWhenLockIsReleasedWithinRetryBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("waiting past one busy_timeout to reach the second attempt takes ~7s; the fast compositional coverage is named above")
	}
	t.Parallel()

	homeDir := t.TempDir()
	workDir := t.TempDir()
	// One successful delete first, so the database file and its schema exist
	// and a lock can be taken on them.
	seedArchive(t, homeDir, workDir, "seed.txt")

	release := holdArchiveWriteLock(t, homeDir)
	// Held past one full busy_timeout (5s), so the first attempt really fails
	// and the delete can only succeed by retrying -- but well inside the retry
	// budget, so it does.
	go func() {
		time.Sleep(7 * time.Second)
		release()
	}()

	path := testutil.CreateTempFile(t, workDir, "delayed.txt", "delayed")
	start := time.Now()
	_, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--verbose", "--description", "released within the retry budget", path)
	elapsed := time.Since(start)

	if code != 0 {
		t.Fatalf("expected success after the lock was released (exit %d): stderr=%q", code, stderr)
	}
	if elapsed < 5*time.Second {
		t.Errorf("delete returned in %s, which is before the first attempt could have hit busy_timeout; the lock was not contended", elapsed)
	}
	if !strings.Contains(stderr, "retrying") {
		t.Errorf("--verbose did not report the retry: stderr=%q", stderr)
	}
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s still exists after a successful delete", path)
	}
	t.Logf("contention retry succeeded after %s", elapsed)
}
