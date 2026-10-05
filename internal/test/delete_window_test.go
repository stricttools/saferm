package test

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stricttools/saferm/internal/testutil"
)

// What `delete` does when the world changes under it mid-archival.
//
// The archival is two calls with the database insert between them, and the
// insert is not instantaneous: SQLite's busy_timeout is 5s and saferm retries
// five times on top of it, so a contended archive holds that window open for up
// to ~25 real seconds with the source path live the whole time. These tests
// open the window on purpose -- a held write lock stretches the insert, and the
// archive entry appearing is the signal that the window is open -- and change
// the path while `delete` is inside it.
//
// The archive-package tests cover the same two races at the unit level and in
// microseconds. These exist because the decision about the RECORD is the
// command's, not the archive's: which failures discard the entry, which leave
// it, and what the caller is told about the row that is already committed.

// inFlightDelete is a `saferm delete` stopped inside its window.
type inFlightDelete struct {
	entry   string // the archive entry Execute wrote, by name
	release func()
	done    chan deleteOutcome
	t       *testing.T
}

type deleteOutcome struct {
	stdout string
	stderr string
	code   int
}

// startDeleteInWindow runs `saferm delete` against a held write lock and
// returns once the archive entry exists, which is the proof that the archival
// wrote its entry and is now retrying the insert. Everything the caller does
// next happens inside the window.
func startDeleteInWindow(t *testing.T, home string, args ...string) *inFlightDelete {
	t.Helper()

	before := make(map[string]bool)
	for _, name := range archiveEntries(t, home) {
		before[name] = true
	}

	release := holdArchiveWriteLock(t, home)

	done := make(chan deleteOutcome, 1)
	go func() {
		stdout, stderr, code := runSaferm(t, home, args...)
		done <- deleteOutcome{stdout, stderr, code}
	}()

	var entry string
	deadline := time.Now().Add(20 * time.Second)
	for entry == "" {
		for _, name := range archiveEntries(t, home) {
			if !before[name] {
				entry = name
				break
			}
		}
		if entry != "" {
			break
		}
		if time.Now().After(deadline) {
			release()
			<-done
			t.Fatal("the archival never wrote an entry, so the window never opened")
		}
		time.Sleep(10 * time.Millisecond)
	}

	return &inFlightDelete{entry: entry, release: release, done: done, t: t}
}

// startDeleteInsideTheInsert is startDeleteInWindow's stronger signal, for the
// tests that cannot act on the entry the moment it appears.
//
// A directory's .tar.zst is CREATED empty and filled afterwards, and Execute
// reads it back to hash it once it is closed. A test that removed it as soon as
// the name existed would be racing the compression rather than the insert, and
// would provoke "hashing archive: no such file" instead of the window it means
// to open. So this one waits for the process to REPORT its first contention
// retry (`--verbose` prints one on stderr), which cannot happen until Execute
// has returned and the insert is in flight. The wait costs one busy_timeout --
// five seconds -- which is why the cheap signal stays the default.
func startDeleteInsideTheInsert(t *testing.T, home string, args ...string) *inFlightDelete {
	t.Helper()

	before := make(map[string]bool)
	for _, name := range archiveEntries(t, home) {
		before[name] = true
	}

	release := holdArchiveWriteLock(t, home)

	cmd := exec.Command(safermBinary, append([]string{"--verbose"}, args...)...)
	// Same environment hygiene as runSafermEnv: SAFERM_* and STRICTCLI_* are
	// stripped from the inherited environment so a developer's overrides and an
	// inherited trace ancestry cannot reach the process under test.
	env := filterEnv(filterEnv(os.Environ(), "SAFERM_"), "STRICTCLI_")
	cmd.Env = append(env, "SAFERM_HOME="+filepath.Join(home, ".saferm"))

	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		release()
		t.Fatalf("piping the delete's stderr: %v", err)
	}
	if err := cmd.Start(); err != nil {
		release()
		t.Fatalf("starting the delete: %v", err)
	}

	// The retry notice, read as it is printed. Everything the process says is
	// kept, so the outcome carries the whole of stderr as the buffered runs do.
	var mu sync.Mutex
	var errBuf strings.Builder
	retrying := make(chan struct{})
	go func() {
		var once sync.Once
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			line := scanner.Text()
			mu.Lock()
			errBuf.WriteString(line + "\n")
			mu.Unlock()
			if strings.Contains(line, "database is locked by another process") {
				once.Do(func() { close(retrying) })
			}
		}
	}()

	done := make(chan deleteOutcome, 1)
	go func() {
		waitErr := cmd.Wait()
		code := 0
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else if waitErr != nil {
			code = -1
		}
		mu.Lock()
		stderr := errBuf.String()
		mu.Unlock()
		done <- deleteOutcome{outBuf.String(), stderr, code}
	}()

	select {
	case <-retrying:
	case out := <-done:
		release()
		t.Fatalf("the delete finished without ever reaching its insert: stderr=%s", out.stderr)
	case <-time.After(60 * time.Second):
		release()
		<-done
		t.Fatal("the delete never reported a contention retry, so the window never opened")
	}

	var entry string
	for _, name := range archiveEntries(t, home) {
		if !before[name] {
			entry = name
			break
		}
	}
	if entry == "" {
		release()
		<-done
		t.Fatal("the insert is in flight but no archive entry was written")
	}

	return &inFlightDelete{entry: entry, release: release, done: done, t: t}
}

// finish closes the window and waits for the command to end.
func (d *inFlightDelete) finish() deleteOutcome {
	d.t.Helper()
	d.release()
	select {
	case out := <-d.done:
		return out
	case <-time.After(90 * time.Second):
		d.t.Fatal("the delete never finished after the write lock was released")
		return deleteOutcome{}
	}
}

// A write through the original path while the insert is in flight rewrites the
// archived bytes with it, because the entry is a hard link to that same inode.
// The row that is already committed records the hash the file had BEFORE the
// write, and nothing can make it true again. Removing the source would leave
// that lie in the archive permanently; instead the entry is discarded, the file
// stays exactly where it is with the content it now has, and the caller is told
// the row names nothing.
func TestDelete_AFileWrittenThroughDuringTheInsertIsNotRemoved(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedArchive(t, home, work, "seed.txt")

	target := testutil.CreateTempFile(t, work, "edited.txt", "the bytes that were hashed\n")
	inFlight := startDeleteInWindow(t, home, "delete", "--on-error", "abort", "--description", "written through mid-archival", target)

	const rewritten = "rewritten while the insert was still retrying\n"
	if err := os.WriteFile(target, []byte(rewritten), 0644); err != nil {
		t.Fatalf("rewriting the source inside the window: %v", err)
	}
	out := inFlight.finish()

	if out.code == 0 {
		t.Fatalf("a delete whose archived bytes changed under it must not exit 0; stderr=%s", out.stderr)
	}
	if !strings.Contains(out.stderr, "was written to while it was being archived") {
		t.Errorf("the failure must say what happened, got: %s", out.stderr)
	}
	if !strings.Contains(out.stderr, "names nothing") {
		t.Errorf("the failure must say the committed row now names no archived copy, got: %s", out.stderr)
	}

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the source was removed although the record does not describe it: %v", err)
	}
	if string(content) != rewritten {
		t.Errorf("the source's content changed: %q", content)
	}

	// The entry is gone: keeping it would leave the archive holding a blob
	// whose row records a different hash, and a second name for a file the
	// caller is still writing to.
	if _, err := os.Stat(filepath.Join(home, ".saferm", "archive", inFlight.entry)); !os.IsNotExist(err) {
		t.Errorf("the archive entry that no longer matches its record was kept (err=%v)", err)
	}
}

// The path can be replaced outright while the insert is in flight, and a
// removal by name would then destroy a file nothing archived. Both the original
// (in the archive, under the committed row) and the replacement (at the path)
// survive.
func TestDelete_AFileReplacedDuringTheInsertIsNotRemoved(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedArchive(t, home, work, "seed.txt")

	target := testutil.CreateTempFile(t, work, "swapped.txt", "the archived original\n")
	inFlight := startDeleteInWindow(t, home, "delete", "--on-error", "abort", "--description", "replaced mid-archival", target)

	const replacement = "a completely different file at the same path\n"
	if err := os.Remove(target); err != nil {
		t.Fatalf("removing the source inside the window: %v", err)
	}
	if err := os.WriteFile(target, []byte(replacement), 0644); err != nil {
		t.Fatalf("recreating the path inside the window: %v", err)
	}
	out := inFlight.finish()

	if out.code == 0 {
		t.Fatalf("a delete whose path was replaced under it must not exit 0; stderr=%s", out.stderr)
	}
	if !strings.Contains(out.stderr, "no longer names the file that was archived") {
		t.Errorf("the failure must name the replacement, got: %s", out.stderr)
	}
	if !strings.Contains(out.stderr, "now holds something else") {
		t.Errorf("the failure must state what the path holds now, got: %s", out.stderr)
	}
	if !recordIdentifiers.MatchString(out.stderr) {
		t.Errorf("the failure must name the record that holds the original, got: %s", out.stderr)
	}

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the replacement was destroyed by a removal that went by name: %v", err)
	}
	if string(content) != replacement {
		t.Errorf("the replacement's content changed: %q", content)
	}

	// The record is truthful -- the entry holds exactly the bytes it was
	// written for -- so it is kept.
	archived, err := os.ReadFile(filepath.Join(home, ".saferm", "archive", inFlight.entry))
	if err != nil {
		t.Fatalf("the archive entry for the original is gone: %v", err)
	}
	if string(archived) != "the archived original\n" {
		t.Errorf("the archive entry holds %q, not the original", archived)
	}
}

// The other thing that can happen inside the window, and the one the identity
// checks never asked about: the ARCHIVED COPY can go. The row is inserted
// before the source is removed, so a concurrent `saferm purge --all` can select
// that row and destroy its blob legitimately while the archival is still
// waiting on its own insert. Removing the source then would leave no copy of
// the content anywhere. Every node type is covered, because every node type's entry is a
// file some other process can remove.

// windowStarter is either of the two ways to stop a delete inside its window.
type windowStarter func(t *testing.T, home string, args ...string) *inFlightDelete

// entryVanishes runs a delete of target, removes the archive entry from inside
// the window, and returns what the command said about it.
func entryVanishes(t *testing.T, home string, start windowStarter, args ...string) (deleteOutcome, string) {
	t.Helper()

	inFlight := start(t, home, args...)
	entryPath := filepath.Join(home, ".saferm", "archive", inFlight.entry)
	if err := os.Remove(entryPath); err != nil {
		t.Fatalf("removing the archive entry inside the window: %v", err)
	}
	return inFlight.finish(), entryPath
}

// assertEntryVanishedReport pins what the caller is told: the entry that is
// gone, by name, and the fact that the committed row now names nothing.
func assertEntryVanishedReport(t *testing.T, out deleteOutcome, entryPath string) {
	t.Helper()

	if out.code == 0 {
		t.Fatalf("a delete whose archived copy vanished must not exit 0; stderr=%s", out.stderr)
	}
	if !strings.Contains(out.stderr, entryPath) {
		t.Errorf("the failure must name the entry that is gone (%s), got: %s", entryPath, out.stderr)
	}
	if !strings.Contains(out.stderr, "names nothing") {
		t.Errorf("the failure must say the committed row names no archived copy, got: %s", out.stderr)
	}
	if !recordIdentifiers.MatchString(out.stderr) {
		t.Errorf("the failure must name the record it committed, got: %s", out.stderr)
	}
}

func TestDelete_AFileWhoseEntryVanishesDuringTheInsertIsNotRemoved(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedArchive(t, home, work, "seed.txt")

	target := testutil.CreateTempFile(t, work, "lonely.txt", "the only copy\n")
	out, entryPath := entryVanishes(t, home, startDeleteInWindow, "delete", "--on-error", "abort", "--description", "entry purged mid-archival", target)
	assertEntryVanishedReport(t, out, entryPath)

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the source was destroyed with no archived copy of it: %v", err)
	}
	if string(content) != "the only copy\n" {
		t.Errorf("the source's content changed: %q", content)
	}
}

func TestDelete_ADirectoryWhoseEntryVanishesDuringTheInsertIsNotRemoved(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedArchive(t, home, work, "seed.txt")

	tree := testutil.CreateTempDir(t, work, "tree")
	inner := testutil.CreateTempFile(t, tree, "inner.txt", "the only copy\n")
	out, entryPath := entryVanishes(t, home, startDeleteInsideTheInsert, "delete", "--on-error", "abort", "-r", "--description", "tar purged mid-archival", tree)
	assertEntryVanishedReport(t, out, entryPath)

	content, err := os.ReadFile(inner)
	if err != nil {
		t.Fatalf("the tree was destroyed with no archived copy of it: %v", err)
	}
	if string(content) != "the only copy\n" {
		t.Errorf("the tree's content changed: %q", content)
	}
}

func TestDelete_ASymlinkWhoseEntryVanishesDuringTheInsertIsNotRemoved(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedArchive(t, home, work, "seed.txt")

	link := filepath.Join(work, "link")
	if err := os.Symlink("/etc/hostname", link); err != nil {
		t.Fatalf("creating the symlink: %v", err)
	}
	out, entryPath := entryVanishes(t, home, startDeleteInWindow, "delete", "--on-error", "abort", "--description", "metadata purged mid-archival", link)
	assertEntryVanishedReport(t, out, entryPath)

	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("the symlink was destroyed with no archived copy of it: %v", err)
	}
	if target != "/etc/hostname" {
		t.Errorf("the surviving symlink points at %q", target)
	}
}

// A tree can also GROW while the insert is in flight, and its identity check
// sees none of it: the top-level inode is unchanged, so the recursive removal
// used to proceed and destroy a file that was written after the tar was closed
// and is therefore in no archive at all. The tree is re-walked against what
// went into the tar instead, and the delete refuses -- the whole tree stays,
// the incomplete archive is discarded, and the row that was already committed
// names nothing, exactly as it does for a file written through mid-archival.
func TestDelete_ADirectoryThatGrowsDuringTheInsertIsNotRemoved(t *testing.T) {
	home := testutil.SetupTestEnv(t)
	work := t.TempDir()
	seedArchive(t, home, work, "seed.txt")

	tree := testutil.CreateTempDir(t, work, "tree")
	archived := testutil.CreateTempFile(t, tree, "inner.txt", "was in the tar\n")
	inFlight := startDeleteInsideTheInsert(t, home, "delete", "--on-error", "abort", "-r", "--description", "tree grew mid-archival", tree)

	newcomer := filepath.Join(tree, "written-during-the-insert.txt")
	const unarchived = "written after the tar was closed\n"
	if err := os.WriteFile(newcomer, []byte(unarchived), 0644); err != nil {
		t.Fatalf("writing into the tree inside the window: %v", err)
	}
	out := inFlight.finish()

	if out.code == 0 {
		t.Fatalf("a delete whose tree grew under it must not exit 0; stderr=%s", out.stderr)
	}
	if !strings.Contains(out.stderr, newcomer) {
		t.Errorf("the failure must name the path the archive does not hold (%s), got: %s", newcomer, out.stderr)
	}
	if !strings.Contains(out.stderr, "names nothing") {
		t.Errorf("the failure must say the committed row names no archived copy, got: %s", out.stderr)
	}

	content, err := os.ReadFile(newcomer)
	if err != nil {
		t.Fatalf("the file that was in no archive was destroyed anyway: %v", err)
	}
	if string(content) != unarchived {
		t.Errorf("the new file's content changed: %q", content)
	}
	if _, err := os.Stat(archived); err != nil {
		t.Errorf("the rest of the tree was destroyed: %v", err)
	}

	// The incomplete archive is discarded: keeping it would leave a .tar.zst
	// that is missing part of the tree its row claims to hold.
	if _, err := os.Stat(filepath.Join(home, ".saferm", "archive", inFlight.entry)); !os.IsNotExist(err) {
		t.Errorf("the incomplete archive entry was kept (err=%v)", err)
	}
}

// recordIdentifiers matches the `record [<id>] <uuid>` phrase every
// source-removal failure carries. Both identifiers are in it because both are
// handles the caller can act on, and after this failure the archived copy is
// the only thing that can be named -- the path is not what was archived.
var recordIdentifiers = regexp.MustCompile(`record \[[0-9]+\] [0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
