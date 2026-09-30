package main

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stricttools/saferm/internal/db"
)

// dbExit is the single seam that turns a database-layer failure into an exit
// code, and the only place exhausted contention becomes 8 rather than 5. The
// end-to-end proof of that number (internal/test/contention_test.go's
// TestDeleteExitsWithContentionCodeWhenLockNeverReleased) has to exhaust a real
// retry budget against a real busy_timeout, so it is skipped under -short --
// which is exactly what CI, the pre-push hook and the release preflight run.
//
// This test covers the mapping in microseconds, so automation verifies it on
// every run. It is one third of a compositional proof: the db layer really
// produces a *db.ContentionError under a held lock
// (internal/db.TestInsertExhaustsTheRetryBudgetUnderAHeldLock, fast), dbExit
// really maps that type onto ExitContention (here), and a command's database
// error path really routes through dbExit
// (internal/test.TestDeleteExitsWithDatabaseCodeWhenTheArchiveDatabaseIsUnreadable,
// fast).
func TestDbExit(t *testing.T) {
	// The literal numbers, not the constants: a test written against the
	// constants would keep passing if the value changed underneath it, and the
	// exit code is a promise made to scripts, which compare numbers.
	if ExitContention != 8 {
		t.Errorf("ExitContention is %d, want 8", ExitContention)
	}
	if ExitDatabase != 5 {
		t.Errorf("ExitDatabase is %d, want 5", ExitDatabase)
	}

	exhausted := &db.ContentionError{
		Attempts: 5,
		Elapsed:  250 * time.Millisecond,
		Err:      errors.New("database is locked (5) (SQLITE_BUSY)"),
	}

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"exhausted contention", exhausted, ExitContention},
		{"exhausted contention wrapped by a caller", fmt.Errorf("inserting record: %w", exhausted), ExitContention},
		{"a record that is not there", db.ErrNotFound, ExitDatabase},
		{"an empty result", sql.ErrNoRows, ExitDatabase},
		{"a corrupt database file", errors.New("file is not a database (26) (SQLITE_NOTADB)"), ExitDatabase},
		{"an arbitrary failure", errors.New("disk exploded"), ExitDatabase},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dbExit(tc.err); got != tc.want {
				t.Errorf("dbExit(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestMatchArchivePath(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"star spans one separator", "/home/*", "/home/m/notes.txt", true},
		{"star spans many separators", "/home/*", "/home/m/Projects/saferm/list.go", true},
		{"star still matches a direct child", "/home/*", "/home/m", true},
		{"star does not escape its prefix", "/home/*", "/var/log/syslog", false},
		{"literal segments around a star", "/home/*/build/*", "/home/m/p/q/build/out/a.o", true},
		{"literal segments must all appear", "/home/*/build/*", "/home/m/p/src/a.go", false},
		{"filename glob", "/home/m/*.txt", "/home/m/deep/er/notes.txt", true},
		{"exact path", "/home/m/notes.txt", "/home/m/notes.txt", true},
		{"question mark spans a separator", "/a?c", "/a/c", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := matchArchivePath(tc.pattern, tc.path)
			if err != nil {
				t.Fatalf("matchArchivePath(%q, %q) errored: %v", tc.pattern, tc.path, err)
			}
			if got != tc.want {
				t.Errorf("matchArchivePath(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}

	t.Run("malformed pattern reports an error", func(t *testing.T) {
		if _, err := matchArchivePath("[", "/home/m/x"); err == nil {
			t.Error("matchArchivePath with an unterminated character class should error")
		}
	})
}

func TestParseSize(t *testing.T) {
	validCases := []struct {
		name  string
		input string
		want  int64
	}{
		{"bytes", "100B", 100},
		{"kilobytes", "1KB", 1024},
		{"megabytes", "5MB", 5242880},
		{"gigabytes", "2GB", 2147483648},
		{"terabytes", "1TB", 1099511627776},
		{"case insensitive mb lower", "100mb", 5242880 / 5 * 100},
		{"case insensitive kb lower", "1kb", 1024},
		{"case insensitive mixed", "5Mb", 5242880},
	}

	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSize(tc.input)
			if err != nil {
				t.Fatalf("parseSize(%q) returned unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("parseSize(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}

	// Verify case insensitivity produces identical results.
	t.Run("case insensitive mb matches MB", func(t *testing.T) {
		lower, err1 := parseSize("100mb")
		upper, err2 := parseSize("100MB")
		if err1 != nil || err2 != nil {
			t.Fatalf("unexpected error: lower=%v, upper=%v", err1, err2)
		}
		if lower != upper {
			t.Errorf("parseSize(\"100mb\") = %d, parseSize(\"100MB\") = %d; want equal", lower, upper)
		}
	})

	t.Run("case insensitive kb matches KB", func(t *testing.T) {
		lower, err1 := parseSize("1kb")
		upper, err2 := parseSize("1KB")
		if err1 != nil || err2 != nil {
			t.Fatalf("unexpected error: lower=%v, upper=%v", err1, err2)
		}
		if lower != upper {
			t.Errorf("parseSize(\"1kb\") = %d, parseSize(\"1KB\") = %d; want equal", lower, upper)
		}
	})

	errorCases := []struct {
		name  string
		input string
	}{
		{"empty string", ""},
		{"no suffix", "100"},
		{"zero value", "0MB"},
		{"no number", "MB"},
		{"unknown suffix", "100PB"},
		{"not a number", "abc"},
		{"negative value", "-5MB"},
	}

	for _, tc := range errorCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSize(tc.input)
			if err == nil {
				t.Errorf("parseSize(%q) = %d, want error", tc.input, got)
			}
		})
	}
}
