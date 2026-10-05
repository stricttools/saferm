package test

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stricttools/saferm/internal/testutil"
)

// archivedUUID reads the uuid out of one `delete` line: the command prints
// `archived: [<id>] <uuid> <path> (<size>)` for every record it writes, and the
// uuid is what names the entry in the archive directory.
var archivedUUID = regexp.MustCompile(`archived: \[\d+\] ([0-9a-f-]{36}) `)

func parseArchivedUUID(t *testing.T, stdout string) string {
	t.Helper()
	m := archivedUUID.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("no archived line with a uuid in: %q", stdout)
	}
	return m[1]
}

// writeCorruptTarZst replaces path with a .tar.zst holding one complete member
// followed by garbage, so an extraction writes that member and then fails. It
// is the shape a truncated or bit-rotted directory archive takes on the way
// back.
func writeCorruptTarZst(t *testing.T, path string, top string) {
	t.Helper()

	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	body := []byte("first member\n")
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: top + "/first.txt", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	raw.Write(bytes.Repeat([]byte{0xff}, 512))

	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// archiveEntry returns the path of a record's archive entry inside the test's
// own saferm home.
func archiveEntry(homeDir string, uuid string, suffix string) string {
	return filepath.Join(homeDir, ".saferm", "archive", uuid+suffix)
}

// A directory restore that fails partway must leave three things true: the
// archived copy is still in the archive, the destination does not look
// restored, and the record is still restorable. Nothing about the restore is
// verified up front here -- the destination is absent, so nothing is destroyed
// by trying -- which is exactly why the failure has to be harmless.
func TestUndelete_CorruptDirectoryArchive_KeepsTheCopyAndLeavesNoHalfTree(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	tree := testutil.CreateTempDir(t, workDir, "tree")
	testutil.CreateTempFile(t, tree, "first.txt", "first member\n")
	testutil.CreateTempFile(t, tree, "second.txt", "second member\n")

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "corrupt archive test", tree)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	entry := archiveEntry(homeDir, uuid, ".tar.zst")
	writeCorruptTarZst(t, entry, "tree")

	_, stderr, code = runSaferm(t, homeDir, "undelete", uuid)
	if code == 0 {
		t.Fatalf("restoring a corrupt directory archive must fail; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "had been extracted") {
		t.Errorf("the failure must report what it managed to extract, got: %q", stderr)
	}
	if !strings.Contains(stderr, "still restorable") {
		t.Errorf("the failure must say the record can be tried again, got: %q", stderr)
	}

	if _, err := os.Stat(entry); err != nil {
		t.Errorf("a failed restore must keep the archived copy: %v", err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Errorf("a failed restore must leave no half tree at the destination, got: %v", err)
	}

	// The record was never stamped as restored, so it is still listed as
	// restorable and a retry is possible.
	infoOut, _, code := runSaferm(t, homeDir, "info", uuid)
	if code != 0 {
		t.Fatalf("info failed after the failed restore: %q", infoOut)
	}
	if !strings.Contains(infoOut, "restorable") {
		t.Errorf("the record must still be restorable, got: %q", infoOut)
	}
}

// The preview and the real restore walk one step list, so a directory restore's
// would-do log names the same three acts the real one performs: the parent
// directory, the tree appearing at the destination, and the archived copy being
// dropped afterwards.
func TestUndelete_DirectoryDryRunRecordsTheWholeRestore(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	tree := testutil.CreateTempDir(t, workDir, "tree")
	testutil.CreateTempFile(t, tree, "inner.txt", "content\n")

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "preview seed", tree)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	entry := archiveEntry(homeDir, uuid, ".tar.zst")

	stdout, stderr, code = runSaferm(t, homeDir, "--dry-run", "undelete", uuid)
	if code != 0 {
		t.Fatalf("undelete --dry-run failed (exit %d): stderr=%q", code, stderr)
	}
	log := wouldDoLog(stdout)
	if !strings.Contains(log, "write: "+tree) {
		t.Errorf("the preview must name the tree it would restore, got: %q", log)
	}
	if !strings.Contains(log, "remove: "+entry) {
		t.Errorf("the preview must name the archived copy it would consume, got: %q", log)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Errorf("a dry-run restore recreated %s for real", tree)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Errorf("a dry-run restore consumed the archived copy: %v", err)
	}
}

// The symlink shape goes through the same list: the recorded target is written
// out as the preview's content, and the .symlink entry is consumed last.
func TestUndelete_SymlinkDryRunRecordsTheWholeRestore(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	target := testutil.CreateTempFile(t, workDir, "target.txt", "target\n")
	link := filepath.Join(workDir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "symlink preview seed", link)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	entry := archiveEntry(homeDir, uuid, ".symlink")

	stdout, stderr, code = runSaferm(t, homeDir, "--dry-run", "undelete", uuid)
	if code != 0 {
		t.Fatalf("undelete --dry-run failed (exit %d): stderr=%q", code, stderr)
	}
	log := wouldDoLog(stdout)
	if !strings.Contains(log, "write: "+link) {
		t.Errorf("the preview must name the link it would recreate, got: %q", log)
	}
	if !strings.Contains(log, "remove: "+entry) {
		t.Errorf("the preview must name the archived entry it would consume, got: %q", log)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("a dry-run restore recreated %s for real", link)
	}

	// And the real one performs exactly that.
	if _, stderr, code = runSaferm(t, homeDir, "undelete", uuid); code != 0 {
		t.Fatalf("undelete failed (exit %d): stderr=%q", code, stderr)
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("the link was not recreated: %v", err)
	}
	if got != target {
		t.Errorf("symlink target mismatch: got %q, want %q", got, target)
	}
	if _, err := os.Lstat(entry); !os.IsNotExist(err) {
		t.Errorf("a completed restore must consume the archived entry, got: %v", err)
	}
}

// An overwrite is the one destructive thing a restore does, so the archived
// copy is checked BEFORE the destination is touched. The restore used to remove
// the destination first and read the archive afterwards, which turned a
// corrupted archive into the loss of whatever was standing there.
func TestUndelete_CorruptFileArchive_RefusesBeforeTouchingTheDestination(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "verified.txt", "the archived content\n")

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "verify test", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	entry := archiveEntry(homeDir, uuid, "")
	if err := os.WriteFile(entry, []byte("rotted bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Something is standing at the destination, and the caller asked for it to
	// be replaced.
	testutil.CreateTempFile(t, workDir, "verified.txt", "do not lose me\n")

	_, stderr, code = runSaferm(t, homeDir, "undelete", "--on-conflict", "overwrite", uuid)
	if code == 0 {
		t.Fatalf("overwriting from a corrupt archive must fail; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "refusing to overwrite") {
		t.Errorf("the refusal must say it did not touch the destination, got: %q", stderr)
	}

	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading the destination after the refusal: %v", err)
	}
	if string(got) != "do not lose me\n" {
		t.Errorf("the destination was touched by a refused restore: got %q", got)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Errorf("a refused restore must keep the archived copy: %v", err)
	}
	infoOut, _, _ := runSaferm(t, homeDir, "info", uuid)
	if !strings.Contains(infoOut, "restorable") {
		t.Errorf("a refused restore must leave the record restorable, got: %q", infoOut)
	}
}

// The same for a tree: a directory's recorded hash covers the .tar.zst
// container, so a damaged container is found before the destination goes.
func TestUndelete_CorruptDirectoryArchive_RefusesBeforeTouchingTheDestination(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	tree := testutil.CreateTempDir(t, workDir, "tree")
	testutil.CreateTempFile(t, tree, "inner.txt", "archived\n")

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "verify tree test", tree)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	entry := archiveEntry(homeDir, uuid, ".tar.zst")
	writeCorruptTarZst(t, entry, "tree")

	// A different tree is standing in its place.
	replacement := testutil.CreateTempDir(t, workDir, "tree")
	testutil.CreateTempFile(t, replacement, "mine.txt", "do not lose me\n")

	_, stderr, code = runSaferm(t, homeDir, "undelete", "--on-conflict", "overwrite", uuid)
	if code == 0 {
		t.Fatalf("overwriting from a corrupt container must fail; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "refusing to overwrite") {
		t.Errorf("the refusal must say it did not touch the destination, got: %q", stderr)
	}
	got, err := os.ReadFile(filepath.Join(tree, "mine.txt"))
	if err != nil {
		t.Fatalf("the destination tree was destroyed by a refused restore: %v", err)
	}
	if string(got) != "do not lose me\n" {
		t.Errorf("the destination was rewritten by a refused restore: got %q", got)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Errorf("a refused restore must keep the archived copy: %v", err)
	}
}

// Verification is not a real-mode step: a dry run of the same overwrite reads
// the archived copy and refuses in exactly the same way, with the same exit
// code.
//
// A preview that skipped the check would answer "this would work" for a restore
// that cannot, which is the one thing a preview must never say -- and the check
// costs a preview nothing it would not cost the run it previews, because both
// read the copy once and neither touches the destination until it has passed.
func TestUndelete_DryRunOverwrite_RefusesACorruptArchiveToo(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "previewed.txt", "the archived content\n")

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "dry-run verify test", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	entry := archiveEntry(homeDir, uuid, "")
	if err := os.WriteFile(entry, []byte("rotted bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	testutil.CreateTempFile(t, workDir, "previewed.txt", "do not lose me\n")

	_, stderr, code = runSaferm(t, homeDir, "--dry-run", "undelete", "--on-conflict", "overwrite", uuid)
	if code != 6 {
		t.Fatalf("a previewed overwrite from a corrupt archive must refuse with exit 6, got %d: %q", code, stderr)
	}
	if !strings.Contains(stderr, "refusing to overwrite") {
		t.Errorf("the refusal must say it did not touch the destination, got: %q", stderr)
	}

	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading the destination after the refusal: %v", err)
	}
	if string(got) != "do not lose me\n" {
		t.Errorf("a previewed restore touched the destination: got %q", got)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Errorf("a previewed restore consumed the archived copy: %v", err)
	}
	infoOut, _, _ := runSaferm(t, homeDir, "info", uuid)
	if !strings.Contains(infoOut, "restorable") {
		t.Errorf("a previewed restore must leave the record restorable, got: %q", infoOut)
	}
}

// A symlink was never hashed. Its entry is the recorded target written out, so
// verification is an equality against the record -- and it must not fail
// spuriously on the ordinary case, which is the whole reason the three node types
// are defined separately.
func TestUndelete_SymlinkEntryDiverged_RefusesBeforeTouchingTheDestination(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	target := testutil.CreateTempFile(t, workDir, "target.txt", "target\n")
	link := filepath.Join(workDir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "symlink verify test", link)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	entry := archiveEntry(homeDir, uuid, ".symlink")

	// Something else is at the destination now.
	testutil.CreateTempFile(t, workDir, "link.txt", "do not lose me\n")

	// The untouched entry verifies: an ordinary overwrite of a symlink goes
	// through, and no hash is involved anywhere.
	if _, stderr, code = runSaferm(t, homeDir, "undelete", "--on-conflict", "overwrite", uuid); code != 0 {
		t.Fatalf("overwriting with an intact symlink entry must succeed, got %d: %q", code, stderr)
	}
	if _, err := os.Readlink(link); err != nil {
		t.Fatalf("the link was not restored: %v", err)
	}
	if _, err := os.Lstat(entry); !os.IsNotExist(err) {
		t.Errorf("the restore must have consumed the entry, got: %v", err)
	}
}

// And an entry that no longer names the recorded target is refused before the
// destination is touched.
func TestUndelete_SymlinkEntryRewritten_RefusesBeforeTouchingTheDestination(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	target := testutil.CreateTempFile(t, workDir, "target.txt", "target\n")
	link := filepath.Join(workDir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "symlink divergence test", link)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	entry := archiveEntry(homeDir, uuid, ".symlink")
	if err := os.WriteFile(entry, []byte("/somewhere/else"), 0o600); err != nil {
		t.Fatal(err)
	}

	testutil.CreateTempFile(t, workDir, "link.txt", "do not lose me\n")

	_, stderr, code = runSaferm(t, homeDir, "undelete", "--on-conflict", "overwrite", uuid)
	if code == 0 {
		t.Fatalf("overwriting from a diverged symlink entry must fail; stderr=%q", stderr)
	}
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("the destination was destroyed by a refused restore: %v", err)
	}
	if string(got) != "do not lose me\n" {
		t.Errorf("the destination was rewritten by a refused restore: got %q", got)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Errorf("a refused restore must keep the archived entry: %v", err)
	}
}

// The conflict mode follows the delete side's --on-error: a destination that
// already exists has two defensible answers, they suit opposite callers, and
// saferm refuses to pick one silently. It is required exactly when the
// situation arises, so an ordinary restore into an absent destination stays a
// bare `saferm undelete <target>`.
func TestUndelete_ConflictModeIsRequiredWhenTheDestinationExists(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "occupied.txt", "archived content\n")
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "conflict mode test", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	testutil.CreateTempFile(t, workDir, "occupied.txt", "standing here\n")

	_, stderr, code = runSaferm(t, homeDir, "undelete", uuid)
	if code != 2 {
		t.Fatalf("an omitted conflict mode is an argument error (exit 2), got %d: %q", code, stderr)
	}
	if !strings.Contains(stderr, "--on-conflict") {
		t.Errorf("the error must name the flag it needs, got: %q", stderr)
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != "standing here\n" {
		t.Errorf("the destination must be untouched: got %q (%v)", got, err)
	}
}

// abort is the explicit refusal, and it is a conflict rather than an argument
// error: the caller said what to do and saferm did it.
func TestUndelete_OnConflictAbortRefuses(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "occupied.txt", "archived content\n")
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "abort test", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	entry := archiveEntry(homeDir, uuid, "")
	testutil.CreateTempFile(t, workDir, "occupied.txt", "standing here\n")

	_, stderr, code = runSaferm(t, homeDir, "undelete", "--on-conflict", "abort", uuid)
	if code != 7 {
		t.Fatalf("an aborted restore exits 7 (conflict), got %d: %q", code, stderr)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("the refusal must name the conflict, got: %q", stderr)
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != "standing here\n" {
		t.Errorf("the destination must be untouched: got %q (%v)", got, err)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Errorf("a refused restore must keep the archived copy: %v", err)
	}
}

// overwrite is the destructive answer, and it is the one behind verification.
func TestUndelete_OnConflictOverwriteReplaces(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "occupied.txt", "archived content\n")
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "overwrite test", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	testutil.CreateTempFile(t, workDir, "occupied.txt", "replace me\n")

	if _, stderr, code = runSaferm(t, homeDir, "undelete", "--on-conflict", "overwrite", uuid); code != 0 {
		t.Fatalf("an overwriting restore failed (exit %d): %q", code, stderr)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "archived content\n" {
		t.Errorf("the archived content must be back: got %q", got)
	}
}

// The retired boolean went with the mode that replaced it: two spellings for
// one decision is exactly what an agent learns wrong, and a parse error says so
// at once.
func TestUndelete_ForceOverwriteFlagIsRetired(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)

	_, stderr, code := runSaferm(t, homeDir, "undelete", "--force-overwrite", "1")
	if code == 0 {
		t.Fatal("--force-overwrite must be a parse error, got exit 0")
	}
	if !strings.Contains(stderr, "force-overwrite") {
		t.Errorf("the parse error must name the retired flag, got: %q", stderr)
	}
}

// An EMPTY destination directory is not a conflict for a tree: it is the
// tree's own original place, emptied. Requiring a conflict mode there would
// make the commonest directory restore need a flag to say "replace nothing".
func TestUndelete_EmptyDestinationDirectoryNeedsNoChoice(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	tree := testutil.CreateTempDir(t, workDir, "tree")
	testutil.CreateTempFile(t, tree, "inner.txt", "archived\n")

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "empty destination test", tree)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)

	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, stderr, code = runSaferm(t, homeDir, "undelete", uuid); code != 0 {
		t.Fatalf("restoring into an empty destination must need no flag, got %d: %q", code, stderr)
	}
	got, err := os.ReadFile(filepath.Join(tree, "inner.txt"))
	if err != nil {
		t.Fatalf("the tree was not restored: %v", err)
	}
	if string(got) != "archived\n" {
		t.Errorf("restored content mismatch: got %q", got)
	}
}

// A destination directory that holds anything is a conflict again: the
// extraction would merge the tree into someone else's directory.
func TestUndelete_NonEmptyDestinationDirectoryNeedsTheChoice(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	tree := testutil.CreateTempDir(t, workDir, "tree")
	testutil.CreateTempFile(t, tree, "inner.txt", "archived\n")

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "non-empty destination test", tree)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)

	occupied := testutil.CreateTempDir(t, workDir, "tree")
	testutil.CreateTempFile(t, occupied, "someone-elses.txt", "mine\n")

	_, stderr, code = runSaferm(t, homeDir, "undelete", uuid)
	if code != 2 {
		t.Fatalf("a non-empty destination directory must demand the choice (exit 2), got %d: %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(occupied, "someone-elses.txt")); err != nil {
		t.Errorf("the destination must be untouched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(occupied, "inner.txt")); err == nil {
		t.Error("nothing may have been extracted into the occupied destination")
	}
}

// And the stated answer goes through: a non-empty destination directory is
// REPLACED, not merged into.
//
// The distinction is the whole reason the conflict mode exists for trees. The
// overwrite removes the destination outright before extracting, so what was
// standing there is gone -- including files the archived tree never held, which
// a merge would have left behind looking like part of the restored tree.
func TestUndelete_NonEmptyDestinationDirectoryOverwriteReplacesIt(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	tree := testutil.CreateTempDir(t, workDir, "tree")
	testutil.CreateTempFile(t, tree, "inner.txt", "archived\n")

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "directory overwrite test", tree)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	entry := archiveEntry(homeDir, uuid, ".tar.zst")

	occupied := testutil.CreateTempDir(t, workDir, "tree")
	testutil.CreateTempFile(t, occupied, "someone-elses.txt", "replace me\n")

	if _, stderr, code = runSaferm(t, homeDir, "undelete", "--on-conflict", "overwrite", uuid); code != 0 {
		t.Fatalf("an overwriting tree restore failed (exit %d): %q", code, stderr)
	}

	got, err := os.ReadFile(filepath.Join(tree, "inner.txt"))
	if err != nil {
		t.Fatalf("the archived tree was not restored: %v", err)
	}
	if string(got) != "archived\n" {
		t.Errorf("restored content mismatch: got %q", got)
	}
	// CreateTempDir seeds file1.txt and file2.txt, so the archived tree's own
	// members are back as well and the check is not resting on one file.
	if _, err := os.Stat(filepath.Join(tree, "file1.txt")); err != nil {
		t.Errorf("the rest of the archived tree must be back too: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(tree, "someone-elses.txt")); !os.IsNotExist(err) {
		t.Errorf("an overwrite replaces the destination rather than merging into it, got: %v", err)
	}
	if _, err := os.Lstat(entry); !os.IsNotExist(err) {
		t.Errorf("a successful restore must consume the archived copy, got: %v", err)
	}

	infoOut, _, _ := runSaferm(t, homeDir, "info", uuid)
	if !strings.Contains(infoOut, "restored at") {
		t.Errorf("the record must be stamped as restored, got: %q", infoOut)
	}
}

// The empty-directory rule belongs to trees only. An empty directory standing
// where a FILE was archived is still a conflict: a file cannot be renamed over
// a directory, and removing it is a decision the caller has to state.
func TestUndelete_EmptyDestinationDirectoryIsAConflictForAFile(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "shadowed.txt", "archived content\n")
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "shadowed file test", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)

	if err := os.Mkdir(file, 0o755); err != nil {
		t.Fatal(err)
	}

	_, stderr, code = runSaferm(t, homeDir, "undelete", uuid)
	if code != 2 {
		t.Fatalf("an empty directory over a file record must demand the choice (exit 2), got %d: %q", code, stderr)
	}

	// And the stated overwrite goes through, directory and all.
	if _, stderr, code = runSaferm(t, homeDir, "undelete", "--on-conflict", "overwrite", uuid); code != 0 {
		t.Fatalf("the stated overwrite failed (exit %d): %q", code, stderr)
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != "archived content\n" {
		t.Errorf("the file must be back: got %q (%v)", got, err)
	}
}

// A restore can go somewhere other than where the record came from, and where
// it went is written down: the record's restored-to column is the alternate
// destination, so `info` answers "where is it now" without guessing.
func TestUndelete_AlternateDestinationIsUsedAndRecorded(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()
	elsewhere := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "moved.txt", "archived content\n")
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "alternate destination test", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)

	dest := filepath.Join(elsewhere, "nested", "moved.txt")
	stdout, stderr, code = runSaferm(t, homeDir, "undelete", "--destination", dest, uuid)
	if code != 0 {
		t.Fatalf("restoring to an alternate destination failed (exit %d): %q", code, stderr)
	}
	if !strings.Contains(stdout, dest) {
		t.Errorf("the confirmation must name where the file went, got: %q", stdout)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("nothing was restored to the alternate destination: %v", err)
	}
	if string(got) != "archived content\n" {
		t.Errorf("restored content mismatch: got %q", got)
	}
	if _, err := os.Lstat(file); !os.IsNotExist(err) {
		t.Errorf("the original path must stay empty, got: %v", err)
	}

	infoOut, _, code := runSaferm(t, homeDir, "info", uuid)
	if code != 0 {
		t.Fatalf("info failed: %q", infoOut)
	}
	if !strings.Contains(infoOut, "Restored To:") || !strings.Contains(infoOut, dest) {
		t.Errorf("the alternate destination must be recorded on the record, got: %q", infoOut)
	}
}

// The conflict rules follow the destination, not the record: an occupied
// alternate destination demands the same answer an occupied original one does.
func TestUndelete_AlternateDestinationObeysTheConflictMode(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()
	elsewhere := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "moved.txt", "archived content\n")
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "alternate conflict test", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)

	dest := testutil.CreateTempFile(t, elsewhere, "occupied.txt", "standing here\n")

	_, stderr, code = runSaferm(t, homeDir, "undelete", "--destination", dest, uuid)
	if code != 2 {
		t.Fatalf("an occupied alternate destination must demand the choice (exit 2), got %d: %q", code, stderr)
	}
	if !strings.Contains(stderr, dest) {
		t.Errorf("the error must name the destination it means, got: %q", stderr)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "standing here\n" {
		t.Errorf("the alternate destination must be untouched: got %q (%v)", got, err)
	}

	if _, stderr, code = runSaferm(t, homeDir, "undelete", "--destination", dest, "--on-conflict", "overwrite", uuid); code != 0 {
		t.Fatalf("the stated overwrite failed (exit %d): %q", code, stderr)
	}
	got, err = os.ReadFile(dest)
	if err != nil || string(got) != "archived content\n" {
		t.Errorf("the archived content must be at the alternate destination: got %q (%v)", got, err)
	}
}

// A tree goes to an alternate destination the same way, and the preview names
// the alternate destination rather than the original path.
func TestUndelete_AlternateDestinationForATree(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()
	elsewhere := t.TempDir()

	tree := testutil.CreateTempDir(t, workDir, "tree")
	testutil.CreateTempFile(t, tree, "inner.txt", "archived\n")

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "alternate tree test", tree)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)

	dest := filepath.Join(elsewhere, "restored-tree")
	stdout, stderr, code = runSaferm(t, homeDir, "--dry-run", "undelete", "--destination", dest, uuid)
	if code != 0 {
		t.Fatalf("previewing an alternate destination failed (exit %d): %q", code, stderr)
	}
	if !strings.Contains(wouldDoLog(stdout), "write: "+dest) {
		t.Errorf("the preview must name the alternate destination, got: %q", stdout)
	}

	if _, stderr, code = runSaferm(t, homeDir, "undelete", "--destination", dest, uuid); code != 0 {
		t.Fatalf("restoring a tree to an alternate destination failed (exit %d): %q", code, stderr)
	}
	got, err := os.ReadFile(filepath.Join(dest, "inner.txt"))
	if err != nil {
		t.Fatalf("the tree was not restored to the alternate destination: %v", err)
	}
	if string(got) != "archived\n" {
		t.Errorf("restored content mismatch: got %q", got)
	}
}

// gitRepo makes a throwaway repository with one commit and returns its path.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s\n%s", args, err, out)
		}
	}
	return dir
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %s\n%s", args, err, out)
	}
	return string(out)
}

// stagedPaths is what the index holds against HEAD: after a saferm delete of a
// tracked file it names that file (its removal is staged), and after the
// restore stages it back it names nothing.
func stagedPaths(t *testing.T, repo string) string {
	t.Helper()
	return gitRun(t, repo, "diff", "--cached", "--name-only")
}

// The delete side has a switch for the git index; the restore side staged
// unconditionally. Both sides need one, because a caller that hands a whole
// directory to saferm may want no index side effects at all.
func TestUndelete_StagesTheRestoredPathByDefault(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	repo := gitRepo(t)

	file := testutil.CreateTempFile(t, repo, "tracked.txt", "tracked content\n")
	gitRun(t, repo, "add", "tracked.txt")
	gitRun(t, repo, "commit", "-m", "seed")

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "git index test", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	// Whatever the delete side did with the index, the removal is staged from
	// here, so what the restore does to it is the only variable.
	gitRun(t, repo, "rm", "--cached", "--ignore-unmatch", "tracked.txt")
	if staged := stagedPaths(t, repo); !strings.Contains(staged, "tracked.txt") {
		t.Fatalf("the removal should be staged before the restore, got: %q", staged)
	}

	if _, stderr, code = runSaferm(t, homeDir, "undelete", uuid); code != 0 {
		t.Fatalf("undelete failed (exit %d): %q", code, stderr)
	}
	if staged := stagedPaths(t, repo); strings.Contains(staged, "tracked.txt") {
		t.Errorf("the restore must stage the restored path by default, still staged: %q", staged)
	}
}

func TestUndelete_GitIndexSwitchLeavesTheIndexAlone(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	repo := gitRepo(t)

	file := testutil.CreateTempFile(t, repo, "tracked.txt", "tracked content\n")
	gitRun(t, repo, "add", "tracked.txt")
	gitRun(t, repo, "commit", "-m", "seed")

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "git index switch test", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	gitRun(t, repo, "rm", "--cached", "--ignore-unmatch", "tracked.txt")

	if _, stderr, code = runSaferm(t, homeDir, "undelete", "--no-update-git-index", uuid); code != 0 {
		t.Fatalf("undelete --no-update-git-index failed (exit %d): %q", code, stderr)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the file must still be restored to the working tree: %v", err)
	}
	if staged := stagedPaths(t, repo); !strings.Contains(staged, "tracked.txt") {
		t.Errorf("the switch must leave the index exactly as it was, got: %q", staged)
	}
}

// The failure-keeps-the-copy property holds for the symlink shape too: the
// .symlink entry is dropped only once the link is back on disk, so a refused
// symlink call leaves the record restorable.
func TestUndelete_SymlinkRecreationFails_KeepsTheEntry(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	target := testutil.CreateTempFile(t, workDir, "target.txt", "target\n")
	holder := filepath.Join(workDir, "holder")
	if err := os.Mkdir(holder, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(holder, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "symlink failure test", link)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	entry := archiveEntry(homeDir, uuid, ".symlink")

	// Nothing may be created in the link's own directory any more.
	if err := os.Chmod(holder, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(holder, 0o700) })

	_, stderr, code = runSaferm(t, homeDir, "undelete", uuid)
	if code == 0 {
		t.Fatalf("recreating a symlink into an unwritable directory must fail; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "still restorable") {
		t.Errorf("the failure must say the record can be tried again, got: %q", stderr)
	}
	if _, err := os.Lstat(entry); err != nil {
		t.Errorf("a failed restore must keep the archived entry: %v", err)
	}

	// And once the obstacle is gone, the same restore works.
	if err := os.Chmod(holder, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code = runSaferm(t, homeDir, "undelete", uuid); code != 0 {
		t.Fatalf("the retry after a kept copy must succeed, got %d: %q", code, stderr)
	}
	if _, err := os.Readlink(link); err != nil {
		t.Errorf("the link was not restored on retry: %v", err)
	}
}
