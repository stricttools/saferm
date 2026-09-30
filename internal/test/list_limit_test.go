package test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// archiveMany archives n files in one invocation and returns their paths.
func archiveMany(t *testing.T, homeDir string, n int) []string {
	t.Helper()
	workDir := t.TempDir()
	var files []string
	for i := 0; i < n; i++ {
		files = append(files, testutil.CreateTempFile(t, workDir, fmt.Sprintf("f%03d.txt", i), "x"))
	}
	args := append([]string{"delete", "--on-error", "abort", "--description", "limit test"}, files...)
	if _, stderr, code := runSaferm(t, homeDir, args...); code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	return files
}

// listDoc is `list`'s payload as a consumer parses it.
type listDoc struct {
	Total int       `json:"total"`
	Rows  []listRow `json:"rows"`
}

func listPayload(t *testing.T, homeDir string, args ...string) listDoc {
	t.Helper()
	env, stderr, code := runSafermJSON(t, homeDir, append([]string{"list"}, args...)...)
	if code != 0 {
		t.Fatalf("list %v failed (exit %d): %q", args, code, stderr)
	}
	var doc listDoc
	if err := json.Unmarshal(env.Payload, &doc); err != nil {
		t.Fatalf("list's payload does not parse (%v): %s", err, env.Payload)
	}
	return doc
}

// Bare `list` shows the newest 50 entries and ends by saying how many it
// left out and how to see them; following that advice shows them.
func TestList_BareListShowsTheNewestFiftyAndSaysWhatItHid(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	files := archiveMany(t, homeDir, 55)

	stdout, stderr, code := runSaferm(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d): %q", code, stderr)
	}
	ids := parseAllIDs(t, stdout)
	if len(ids) != 50 {
		t.Fatalf("bare list shows %d entries, want 50:\n%s", len(ids), stdout)
	}
	// Newest first: one invocation stamps one second, so the highest ids lead.
	if ids[0] != "55" || ids[49] != "6" {
		t.Errorf("bare list shows ids %s..%s, want the newest 55..6", ids[0], ids[49])
	}
	if strings.Contains(stdout, filepath.Base(files[0])) {
		t.Errorf("the oldest entry is not among the newest 50:\n%s", stdout)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	footer := "showing 50 of 55; pass --limit N or --path to see others"
	if lines[len(lines)-1] != footer {
		t.Errorf("the last line = %q, want %q", lines[len(lines)-1], footer)
	}

	stdout, _, code = runSaferm(t, homeDir, "list", "--limit", "0")
	if code != 0 {
		t.Fatalf("list --limit 0 failed (exit %d)", code)
	}
	if n := len(parseAllIDs(t, stdout)); n != 55 {
		t.Errorf("--limit 0 shows %d entries, want all 55", n)
	}
	if strings.Contains(stdout, "showing") {
		t.Errorf("nothing is hidden under --limit 0, so nothing says so:\n%s", stdout)
	}

	stdout, _, _ = runSaferm(t, homeDir, "list", "--limit", "3")
	if n := len(parseAllIDs(t, stdout)); n != 3 || !strings.Contains(stdout, "showing 3 of 55;") {
		t.Errorf("--limit 3 shows %d entries:\n%s", n, stdout)
	}
	stdout, _, _ = runSaferm(t, homeDir, "list", "--limit", "55")
	if strings.Contains(stdout, "showing") {
		t.Errorf("a limit that hides nothing says nothing:\n%s", stdout)
	}
}

func TestList_NegativeLimitIsAUsageError(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	_, stderr, code := runSaferm(t, homeDir, "list", "--limit", "-1")
	if code != 2 {
		t.Fatalf("list --limit -1: exit %d, want 2 (stderr=%q)", code, stderr)
	}
}

// The default is declared, so --help shows it rather than leaving it to be
// discovered.
func TestList_LimitDefaultIsDeclared(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	stdout, stderr, code := runSaferm(t, homeDir, "list", "--help")
	if code != 0 {
		t.Fatalf("list --help failed (exit %d): %q", code, stderr)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "--limit") {
			if !strings.Contains(line, "[default: 50]") {
				t.Errorf("the --limit line does not show its default: %q", line)
			}
			return
		}
	}
	t.Errorf("list --help does not mention --limit:\n%s", stdout)
}

// The payload carries the rows shown and the total the selection matched, so
// a machine knows what the limit left out.
func TestMachineSurface_ListCarriesTheTotal(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	archiveMany(t, homeDir, 7)

	doc := listPayload(t, homeDir, "--limit", "3")
	if doc.Total != 7 || len(doc.Rows) != 3 {
		t.Errorf("--limit 3: total %d, %d rows; want total 7, 3 rows", doc.Total, len(doc.Rows))
	}
	doc = listPayload(t, homeDir)
	if doc.Total != 7 || len(doc.Rows) != 7 {
		t.Errorf("bare: total %d, %d rows; want 7 and 7", doc.Total, len(doc.Rows))
	}
}
