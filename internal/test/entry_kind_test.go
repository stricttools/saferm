//go:build !windows

package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stricttools/saferm/internal/archive"
	"github.com/stricttools/saferm/internal/db"
	"github.com/stricttools/saferm/internal/testutil"
)

// recordFileOverEntry writes a record of kind file whose archive entry is
// whatever makeEntry puts at <uuid>: the shape a FIFO deleted by a saferm that
// read it, or a symlink deleted before symlinks were recognized, left behind.
func recordFileOverEntry(t *testing.T, home string, makeEntry func(path string) error) string {
	t.Helper()
	uuid := archive.NewUUID()
	entry := filepath.Join(home, ".saferm", "archive", uuid)
	if err := makeEntry(entry); err != nil {
		t.Fatal(err)
	}
	d := openArchive(t, home)
	if _, err := d.Insert(&db.DeletionRecord{
		UUID: uuid, OriginalPath: filepath.Join(t.TempDir(), "orig"), OriginalName: "orig",
		Hash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		Kind: archive.KindFile, DeletedAt: time.Now(), Description: "contradicted",
	}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	return uuid
}

// An entry whose type contradicts the record's kind is reported as corrupt by
// info, never as restorable, and undelete refuses it without touching it.
func TestInfoAndUndelete_ReportAnEntryThatContradictsItsKindAsCorrupt(t *testing.T) {
	for label, makeEntry := range map[string]func(string) error{
		"a FIFO":    func(p string) error { return syscall.Mkfifo(p, 0o644) },
		"a symlink": func(p string) error { return os.Symlink("../../scripts/hooks/pre-push", p) },
	} {
		t.Run(label, func(t *testing.T) {
			home := testutil.SetupTestEnv(t)
			uuid := recordFileOverEntry(t, home, makeEntry)

			out, stderr, code := runSaferm(t, home, "info", uuid)
			if code != 0 {
				t.Fatalf("info: exit %d: %s", code, stderr)
			}
			status := parseInfoField(t, out, "Status:")
			if status == "restorable" || !strings.Contains(status, "not what this record says") {
				t.Errorf("info Status = %q, want the corrupt-entry line", status)
			}

			env, _, code := runSafermJSON(t, home, "info", uuid)
			if code != 0 {
				t.Fatalf("info --json: exit %d", code)
			}
			var payload struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Status != "entry-corrupt" {
				t.Errorf("payload status = %q, want entry-corrupt", payload.Status)
			}

			_, stderr, code = runSafermWithin(t, specialLimit, home, nil, "undelete", uuid)
			if code != 6 || !strings.Contains(stderr, "not what the record says") {
				t.Errorf("undelete: exit %d, stderr %q; want exit 6 naming the corrupt entry", code, stderr)
			}
			if _, err := os.Lstat(filepath.Join(home, ".saferm", "archive", uuid)); err != nil {
				t.Errorf("the refused entry was touched: %v", err)
			}
		})
	}
}
