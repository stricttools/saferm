package test

import (
	"os"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/testutil"
)

// The uuid is the durable handle, so every verb that takes an identifier has to
// accept it: `undelete`, `info` and `purge`. These tests pin that, and pin the
// order in which an identifier argument is read -- uuid, then numeric id, then
// path -- by looking at which vocabulary the failure speaks when nothing
// resolves.

// deleteOne archives one file and returns the id and uuid `delete` printed.
func deleteOne(t *testing.T, home, path string) (id, uuid string) {
	t.Helper()
	stdout, stderr, code := runSaferm(t, home, "delete", "--on-error", "abort", "--description", "resolution test", path)
	if code != 0 {
		t.Fatalf("delete failed (%d): %s", code, stderr)
	}
	lines := parseArchivedLines(t, stdout)
	if len(lines) != 1 {
		t.Fatalf("expected one identifier line, got %d in:\n%s", len(lines), stdout)
	}
	return lines[0][0], lines[0][1]
}

func TestUndelete_ByUUID(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "by-uuid.txt", "restore me\n")

	_, uuid := deleteOne(t, home, target)

	if _, stderr, code := runSaferm(t, home, "undelete", uuid); code != 0 {
		t.Fatalf("undelete by uuid failed (%d): %s", code, stderr)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("restored file missing: %v", err)
	}
	if string(content) != "restore me\n" {
		t.Errorf("restored content is %q", content)
	}
}

func TestInfo_ByUUID(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "info-uuid.txt", "content\n")

	id, uuid := deleteOne(t, home, target)

	stdout, stderr, code := runSaferm(t, home, "info", uuid)
	if code != 0 {
		t.Fatalf("info by uuid failed (%d): %s", code, stderr)
	}
	if !strings.Contains(stdout, "ID:            "+id) {
		t.Errorf("info by uuid reported a different record:\n%s", stdout)
	}
}

func TestPurge_ByUUID(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	target := testutil.CreateTempFile(t, work, "purge-uuid.txt", "content\n")

	id, uuid := deleteOne(t, home, target)

	if _, stderr, code := runSaferm(t, home, "--approve-consequential", "purge", uuid); code != 0 {
		t.Fatalf("purge by uuid failed (%d): %s", code, stderr)
	}

	stdout, stderr, code := runSaferm(t, home, "info", id)
	if code != 0 {
		t.Fatalf("info after purge failed (%d): %s", code, stderr)
	}
	if !strings.Contains(stdout, "Purged At:") {
		t.Errorf("the record purged by uuid is not marked purged:\n%s", stdout)
	}
}

// A well-formed uuid that resolves to nothing must be reported as a uuid, not
// as a path -- that is the disambiguation order, observed from the failure
// side. Same for an all-digit argument.
func TestResolution_OrderIsUUIDThenIDThenPath(t *testing.T) {
	home := testutil.SetupTestEnv(t)

	cases := []struct {
		name   string
		target string
		want   string
	}{
		{"uuid", "6f1c0e2a-0000-4000-8000-000000000000", "UUID"},
		{"id", "987654", "ID"},
		{"path", "/tmp/definitely-not-archived-by-saferm", "path"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, stderr, code := runSaferm(t, home, "undelete", c.target)
			if code == 0 {
				t.Fatalf("undelete %q should have failed", c.target)
			}
			if !strings.Contains(stderr, c.want) {
				t.Errorf("undelete %q was not read as a %s: %q", c.target, c.want, stderr)
			}
		})
	}
}

// `info` and `purge` never accepted paths and still do not; the refusal has to
// name what they do accept rather than calling a path an invalid ID.
func TestResolution_InfoAndPurgeRefusePathsNamingTheAcceptedForms(t *testing.T) {
	home := testutil.SetupTestEnv(t)

	for _, args := range [][]string{
		{"info", "/tmp/some/path"},
		{"--approve-consequential", "purge", "/tmp/some/path"},
	} {
		_, stderr, code := runSaferm(t, home, args...)
		if code == 0 {
			t.Fatalf("%v should have failed", args)
		}
		if !strings.Contains(stderr, "UUID") || !strings.Contains(stderr, "ID") {
			t.Errorf("%v must name the accepted identifier forms, got: %q", args, stderr)
		}
	}
}
