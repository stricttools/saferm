package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stricttools/saferm/internal/archive"
	"github.com/stricttools/saferm/internal/db"
	"github.com/stricttools/saferm/internal/testutil"
)

func TestPathPatternRange(t *testing.T) {
	cases := []struct {
		pattern string
		lower   string
		upper   string
		bounded bool
	}{
		{"/home/m/*", "/home/m/", "/home/m0", true},
		// An escaped star is still where the literal part stops: the prefix
		// is conservative, never longer than what the pattern pins down.
		{`/a/\*b`, "/a/", "/a0", true},
		{"/a/[bc]/x", "/a/", "/a0", true},
		{"/a/?x", "/a/", "/a0", true},
		// No wildcard: the whole pattern is the prefix.
		{"/a/b.txt", "/a/b.txt", "/a/b.txu", true},
		// A trailing 0xff byte cannot be incremented; the bound drops it and
		// increments the byte before.
		{"/a\xff", "/a\xff", "/b", true},
		{"/a\xff*", "/a\xff", "/b", true},
		// Nothing but 0xff bytes: no string is above every path they start.
		{"\xff\xff", "\xff\xff", "", false},
		// A leading wildcard pins nothing down.
		{"*/build/*", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		lower, upper, bounded := pathPatternRange(c.pattern)
		if lower != c.lower || upper != c.upper || bounded != c.bounded {
			t.Errorf("pathPatternRange(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.pattern, lower, upper, bounded, c.lower, c.upper, c.bounded)
		}
	}
}

func TestValidatePathPattern(t *testing.T) {
	for _, bad := range []string{"[", "/a/[", `/a/\`, "*[", "/a/*/[b"} {
		if err := validatePathPattern(bad); err == nil {
			t.Errorf("validatePathPattern(%q) accepted a malformed pattern", bad)
		}
	}
	// A star inside a character class, or escaped, does not cut the pattern:
	// "/a/[*[]x" is one valid chunk whose class holds a star and a bracket.
	for _, good := range []string{"/a/*", `/a/\*`, "*", "**", "/a/[bc]", "/a/?", "/a/[*[]x", `/a/\*/[b]`} {
		if err := validatePathPattern(good); err != nil {
			t.Errorf("validatePathPattern(%q) = %v, want nil", good, err)
		}
	}
}

// oldListFilter is how `list --path` selected records before it read the path
// index: every record, filtered in Go with matchArchivePath.
func oldListFilter(t *testing.T, database *db.DB, pattern string, includeAll bool) []int64 {
	t.Helper()
	records, err := database.QueryAll(includeAll)
	if err != nil {
		t.Fatalf("QueryAll failed: %v", err)
	}
	var ids []int64
	for _, rec := range records {
		ok, err := matchArchivePath(pattern, rec.OriginalPath)
		if err != nil {
			t.Fatalf("matchArchivePath(%q) failed: %v", pattern, err)
		}
		if ok {
			ids = append(ids, rec.ID)
		}
	}
	return ids
}

// The index-driven --path selection must choose the same records, in the
// same order, as reading everything and filtering in Go did.
func TestListPathSelectionMatchesTheInGoFilter(t *testing.T) {
	testutil.Isolate(t)
	database, err := db.Open(filepath.Join(t.TempDir(), "saferm.db"), nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	paths := []string{
		"/home/m/Projects/a/main.go",
		"/home/m/Projects/a/build/out.o",
		"/home/m/Projects/b/build/deep/x.o",
		"/home/m/Projects",
		"/home/m/Projects0/z",
		"/home/m/Projectsx",
		"/home/m/notes.txt",
		"/home/n/file",
		"/tmp/build/a",
		"/tmp/build",
		"/tmp/a*b/star",
		"/tmp/a[b]/bracket",
		"/tmp/a\\b/backslash",
		"/tmp/q?/question",
		"/tmp/\xff/high",
		"/tmp/\xff\xff",
		"/tmp/caf\xc3\xa9/accent",
		"/tmp/space dir/f",
		"/",
		"/a",
		"/b",
	}
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	for round := 0; round < 3; round++ {
		for i, p := range paths {
			// Some records share a timestamp, so the tie-break is compared too.
			ts := base.Add(time.Duration((round*len(paths)+i)/2) * time.Minute)
			id, err := database.Insert(&db.DeletionRecord{
				UUID: fmt.Sprintf("uuid-eq-%d", n), OriginalPath: p, OriginalName: filepath.Base(p),
				Size: 1, Hash: "h", Kind: archive.KindFile, DeletedAt: ts, Description: "equivalence",
			})
			if err != nil {
				t.Fatalf("Insert %q failed: %v", p, err)
			}
			switch n % 5 {
			case 1:
				if err := database.MarkRestored(id, p); err != nil {
					t.Fatalf("MarkRestored failed: %v", err)
				}
			case 3:
				if err := database.MarkPurged(id); err != nil {
					t.Fatalf("MarkPurged failed: %v", err)
				}
			}
			n++
		}
	}

	patterns := []string{
		"*", "/*", "/home/m/Projects/*", "/home/m/Projects*", "/home/m/Projects",
		"*/build/*", "*build*", "/tmp/*", "/tmp/a\\*b/*", "/tmp/a\\[b]/*", "/tmp/a[*]b/*",
		"/tmp/a\\\\b/*", "/tmp/q\\?/*", "/tmp/q?/*", "/tmp/\xff*", "/tmp/\xff\xff", "/tmp/caf?/*",
		"/tmp/caf\xc3\xa9/*", "/tmp/space dir/?", "/?", "/[ab]", "/home/[mn]/*", "/home/[!m]/*",
		"/nothing/*", "", "/home/m/Projects/a/main.go",
	}
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		for _, includeAll := range []bool{false, true} {
			want := oldListFilter(t, database, pattern, includeAll)
			records, err := selectListRecords(database, pattern, includeAll, nil)
			if err != nil {
				t.Fatalf("selectListRecords(%q, %v) failed: %v", pattern, includeAll, err)
			}
			var got []int64
			for _, rec := range records {
				got = append(got, rec.ID)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("pattern %q includeAll=%v:\n got %v\nwant %v", pattern, includeAll, got, want)
			}
		}
	}
}
