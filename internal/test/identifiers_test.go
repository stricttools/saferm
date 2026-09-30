package test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// The uuid is saferm's durable handle: the numeric id is an autoincrement
// counter of one database, while the uuid names the archived entry itself and
// is what a caller can hold on to. Until now `delete` printed neither -- a
// caller had to run `list` afterwards and guess which row was its own -- so
// these tests pin that every archived path is named on stdout with BOTH
// identifiers, one line per record, in a shape a machine caller can split.

// archivedLine matches the per-record line `delete` prints:
//
//	archived: [12] 6f1c0e2a-... /abs/path (4 B)
var archivedLine = regexp.MustCompile(`^archived: \[(\d+)\] ([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}) (\S+) \(([^)]+)\)$`)

// parseArchivedLines returns the (id, uuid, path) triples `delete` reported.
func parseArchivedLines(t *testing.T, stdout string) [][3]string {
	t.Helper()
	var out [][3]string
	for _, line := range strings.Split(stdout, "\n") {
		m := archivedLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		out = append(out, [3]string{m[1], m[2], m[3]})
	}
	return out
}

func TestDelete_PrintsIDAndUUIDForEveryArchivedPath(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()

	first := testutil.CreateTempFile(t, work, "first.txt", "first\n")
	second := testutil.CreateTempFile(t, work, "second.txt", "second\n")

	stdout, stderr, code := runSaferm(t, home, "delete", "--on-error", "abort", "--description", "durable handle", first, second)
	if code != 0 {
		t.Fatalf("delete failed (%d): %s", code, stderr)
	}

	lines := parseArchivedLines(t, stdout)
	if len(lines) != 2 {
		t.Fatalf("expected one identifier line per archived path, got %d in:\n%s", len(lines), stdout)
	}
	if lines[0][2] != first || lines[1][2] != second {
		t.Errorf("identifier lines name the wrong paths: %v (wanted %s then %s)", lines, first, second)
	}
	if lines[0][1] == lines[1][1] {
		t.Errorf("two records reported the same uuid: %v", lines)
	}

	// The printed pair must be the record's own: info by the printed id has to
	// report the printed uuid.
	for _, l := range lines {
		infoOut, stderr, code := runSaferm(t, home, "info", l[0])
		if code != 0 {
			t.Fatalf("info %s failed (%d): %s", l[0], code, stderr)
		}
		if !strings.Contains(infoOut, l[1]) {
			t.Errorf("info %s does not report the uuid delete printed (%s):\n%s", l[0], l[1], infoOut)
		}
	}
}

// The identifiers print in the ordinary (non-verbose) mode too: they are the
// command's result, not progress chatter. --quiet still silences them, which
// TestQuietSuppressesDeleteSummary pins from the other side.
func TestDelete_PrintsIdentifiersUnderVerboseToo(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "verbose.txt", "content\n")

	stdout, stderr, code := runSaferm(t, home, "--verbose", "delete", "--on-error", "abort", "--description", "verbose handle", target)
	if code != 0 {
		t.Fatalf("verbose delete failed (%d): %s", code, stderr)
	}
	if got := parseArchivedLines(t, stdout); len(got) != 1 {
		t.Fatalf("expected one identifier line under --verbose, got %d in:\n%s", len(got), stdout)
	}
}
