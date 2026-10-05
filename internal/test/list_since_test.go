package test

import (
	"strings"
	"testing"
	"time"

	"github.com/stricttools/saferm/internal/archive"
	"github.com/stricttools/saferm/internal/db"
	"github.com/stricttools/saferm/internal/testutil"
)

// backdate writes a record deleted at a chosen moment straight into the test
// archive's database: no command can archive something in the past. The
// timestamp is written in a zone other than the local one, so a comparison of
// deleted_at as text across zones would be caught.
func backdate(t *testing.T, homeDir, path string, at time.Time) {
	t.Helper()
	d := openArchive(t, homeDir)
	if _, err := d.Insert(&db.DeletionRecord{
		UUID: "backdated" + path, OriginalPath: path, OriginalName: path[strings.LastIndex(path, "/")+1:],
		Size: 1, Hash: "h", Kind: archive.KindFile, DeletedAt: at.In(time.FixedZone("west", -11*3600)), Description: "backdated",
	}); err != nil {
		t.Fatalf("inserting a backdated record: %v", err)
	}
}

// --since keeps what was deleted within the duration, in the syntax
// purge --older-than takes, and combines with --path and --limit.
func TestList_Since(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	archiveMany(t, homeDir, 3)
	now := time.Now()
	backdate(t, homeDir, "/x/old", now.Add(-72*time.Hour))
	backdate(t, homeDir, "/x/mid-aged", now.Add(-10*time.Hour))

	stdout, stderr, code := runSaferm(t, homeDir, "list", "--since", "1d")
	if code != 0 {
		t.Fatalf("list --since 1d failed (exit %d): %q", code, stderr)
	}
	if n := len(parseAllIDs(t, stdout)); n != 4 || strings.Contains(stdout, "/x/old") || !strings.Contains(stdout, "/x/mid-aged") {
		t.Errorf("--since 1d shows %d entries, want the 3 fresh ones and /x/mid-aged:\n%s", n, stdout)
	}

	stdout, _, _ = runSaferm(t, homeDir, "list", "--since", "1d", "--path", "/x/*")
	if ids := parseAllIDs(t, stdout); len(ids) != 1 || !strings.Contains(stdout, "/x/mid-aged") {
		t.Errorf("--since 1d --path /x/* shows %v, want only /x/mid-aged:\n%s", ids, stdout)
	}

	stdout, _, _ = runSaferm(t, homeDir, "list", "--since", "1d", "--limit", "2")
	if n := len(parseAllIDs(t, stdout)); n != 2 || !strings.Contains(stdout, "showing 2 of 4;") {
		t.Errorf("--since 1d --limit 2 shows %d entries:\n%s", n, stdout)
	}

	if doc := listPayload(t, homeDir, "--since", "4d"); doc.Total != 5 {
		t.Errorf("--since 4d: total %d, want all 5", doc.Total)
	}
	if doc := listPayload(t, homeDir, "--since", "1h", "--path", "/x/*"); doc.Total != 0 || len(doc.Rows) != 0 {
		t.Errorf("--since 1h --path /x/*: total %d with %d rows, want nothing", doc.Total, len(doc.Rows))
	}
}

// A duration purge --older-than would refuse is refused here too, as a usage
// error, before anything is read.
func TestList_SinceRejectsAMalformedDuration(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	for _, bad := range []string{"3x", "d", "0d", ""} {
		_, stderr, code := runSaferm(t, homeDir, "list", "--since", bad)
		if code != 2 {
			t.Errorf("list --since %q: exit %d, want 2 (stderr=%q)", bad, code, stderr)
		}
	}
}

// The footer names --since among the ways to see what the limit hid.
func TestList_FooterNamesSince(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	archiveMany(t, homeDir, 4)
	stdout, _, _ := runSaferm(t, homeDir, "list", "--limit", "1")
	if !strings.Contains(stdout, "showing 1 of 4; pass --limit N, --since, or --path to see others") {
		t.Errorf("the footer should name --limit, --since, and --path:\n%s", stdout)
	}
}
