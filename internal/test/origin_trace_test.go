package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stricttools/saferm/internal/db"
	"github.com/stricttools/saferm/internal/meta"
	"github.com/stricttools/saferm/internal/testutil"
	"github.com/stricttools/saferm/internal/trace"
)

// The origin columns are DERIVED: saferm reads STRICTCLI_TRACE_PARENT and
// resolves the entry from the shared trace store. There is no --origin flag on
// any caller, nothing is auto-filled from a marker variable, and history is not
// backfilled. Null means no tool claimed the deletion.
//
// These tests simulate a traced caller the way a real one appears to saferm:
// the variable is set in the child's environment and the store holds
// spec-conformant partition files under the child's HOME. Nothing here needs a
// framework that writes the store, which is what lets the columns ship before
// callers upgrade.

const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// mintID encodes a 26-character identifier under the strict ULID profile with
// the given millisecond in its first 48 bits.
func mintID(ms int64, tail string) string {
	if len(tail) != 16 {
		panic("tail must be 16 characters")
	}
	head := make([]byte, 10)
	for i := 9; i >= 0; i-- {
		head[i] = crockfordAlphabet[ms&0x1F]
		ms >>= 5
	}
	return string(head) + tail
}

// traceEntryLine renders one conforming store entry, with all thirteen keys.
func traceEntryLine(id, parent, app, version string) string {
	parentJSON := "null"
	if parent != "" {
		parentJSON = `"` + parent + `"`
	}
	return `{"id":"` + id + `","parent_id":` + parentJSON + `,"app":"` + app +
		`","version":"` + version + `","command":"release.run","dry_run":false,` +
		`"machine_mode":false,"quiet":false,"verbose":false,"approve_consequential":true,` +
		`"effect":"mutating","pid":4242,"spawned_at":"2026-08-13T04:17:52.913Z"}` + "\n"
}

// writeTraceStore writes one partition into the store under homeDir, at the
// literal path the specification pins.
func writeTraceStore(t *testing.T, homeDir, label, content string) {
	t.Helper()
	store := trace.StoreDir(homeDir)
	if err := os.MkdirAll(store, 0700); err != nil {
		t.Fatalf("creating trace store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store, label+".jsonl"), []byte(content), 0600); err != nil {
		t.Fatalf("writing partition: %v", err)
	}
}

// archivedUUIDs pulls the uuids delete printed, in order.
func archivedUUIDs(t *testing.T, stdout string) []string {
	t.Helper()
	var uuids []string
	for _, line := range parseArchivedLines(t, stdout) {
		uuids = append(uuids, line[1])
	}
	if len(uuids) == 0 {
		t.Fatalf("no archived lines in:\n%s", stdout)
	}
	return uuids
}

// openArchive opens the test archive's database directly, which is the only way
// to read columns no command prints yet.
func openArchive(t *testing.T, homeDir string) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(homeDir, ".saferm", "db", "saferm.db"), nil)
	if err != nil {
		t.Fatalf("opening the archive database: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func recordMetadata(t *testing.T, rec *db.DeletionRecord) meta.Metadata {
	t.Helper()
	var m meta.Metadata
	if err := json.Unmarshal([]byte(rec.Metadata), &m); err != nil {
		t.Fatalf("parsing captured metadata: %v", err)
	}
	return m
}

const msTraced = 1786594672913 // 2026-08-13T04:17:52.913Z

func TestDelete_TracedCallerFillsOriginAndEmbedsChain(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()
	file := testutil.CreateTempFile(t, workDir, "traced.txt", "traced")

	root := mintID(msTraced, "0000000000000001")
	caller := mintID(msTraced, "0000000000000002")
	writeTraceStore(t, homeDir, "2026-08-13T04",
		traceEntryLine(root, "", "claudewheel", "0.20.0")+
			traceEntryLine(caller, root, "rlsbl", "0.61.2"))

	stdout, stderr, code := runSafermTraced(t, homeDir, caller,
		"delete", "--on-error", "abort", "--description", "traced deletion", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}

	d := openArchive(t, homeDir)
	rec, err := d.QueryByUUID(archivedUUIDs(t, stdout)[0])
	if err != nil {
		t.Fatalf("querying the record: %v", err)
	}

	if rec.OriginName == nil || *rec.OriginName != "rlsbl" {
		t.Errorf("origin_name is %v, want the immediate caller's app", rec.OriginName)
	}
	if rec.OriginVersion == nil || *rec.OriginVersion != "0.61.2" {
		t.Errorf("origin_version is %v, want the immediate caller's version", rec.OriginVersion)
	}

	m := recordMetadata(t, rec)
	if m.Trace == nil {
		t.Fatal("the captured metadata holds no trace")
	}
	if m.Trace.ParentID != caller {
		t.Errorf("the captured parent id is %q, want %q", m.Trace.ParentID, caller)
	}
	if len(m.Trace.Chain) != 2 {
		t.Fatalf("the embedded chain holds %d entries, want the caller and its root", len(m.Trace.Chain))
	}
	if m.Trace.Chain[0].App != "rlsbl" || m.Trace.Chain[1].App != "claudewheel" {
		t.Errorf("the embedded chain is %q then %q", m.Trace.Chain[0].App, m.Trace.Chain[1].App)
	}
	if len(m.Trace.ChainIDs) != 2 || m.Trace.ChainIDs[0] != caller || m.Trace.ChainIDs[1] != root {
		t.Errorf("the correlation identifiers are %v", m.Trace.ChainIDs)
	}
	if len(m.Trace.Anomalies) != 0 {
		t.Errorf("a well-formed chain recorded anomalies: %+v", m.Trace.Anomalies)
	}
}

func TestDelete_UntracedCallerLeavesOriginNull(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()
	file := testutil.CreateTempFile(t, workDir, "untraced.txt", "untraced")

	stdout, stderr, code := runSaferm(t, homeDir,
		"delete", "--on-error", "abort", "--description", "untraced deletion", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stdout=%q stderr=%q", code, stdout, stderr)
	}

	d := openArchive(t, homeDir)
	rec, err := d.QueryByUUID(archivedUUIDs(t, stdout)[0])
	if err != nil {
		t.Fatalf("querying the record: %v", err)
	}

	if rec.OriginName != nil || rec.OriginVersion != nil {
		t.Errorf("an untraced deletion claimed an origin: name=%v version=%v", rec.OriginName, rec.OriginVersion)
	}

	// No variable is not an anomaly: it is the ordinary state of every
	// deletion made from a shell, and of every deletion made before callers
	// carried the variable at all.
	m := recordMetadata(t, rec)
	if m.Trace != nil {
		t.Errorf("an untraced deletion captured a trace: %+v", m.Trace)
	}
	if strings.Contains(rec.Metadata, trace.AnomalyDanglingParent) {
		t.Errorf("an untraced deletion recorded a dangling-parent anomaly:\n%s", rec.Metadata)
	}
}

func TestDelete_DanglingParentIsRecordedAndTheDeleteProceeds(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()
	file := testutil.CreateTempFile(t, workDir, "dangling.txt", "dangling")

	// A store that was never written: the identifier is foreign, or the store
	// was pruned. Legal by design.
	foreign := mintID(msTraced, "000000000000DEAD")

	stdout, stderr, code := runSafermTraced(t, homeDir, foreign,
		"delete", "--on-error", "abort", "--description", "dangling deletion", file)
	if code != 0 {
		t.Fatalf("a dangling parent failed the delete (exit %d): stderr=%q", code, stderr)
	}

	d := openArchive(t, homeDir)
	rec, err := d.QueryByUUID(archivedUUIDs(t, stdout)[0])
	if err != nil {
		t.Fatalf("querying the record: %v", err)
	}
	if rec.OriginName != nil {
		t.Errorf("a dangling parent claimed an origin: %v", *rec.OriginName)
	}

	m := recordMetadata(t, rec)
	if m.Trace == nil || len(m.Trace.Anomalies) == 0 {
		t.Fatalf("no anomaly was recorded for a dangling parent: %+v", m.Trace)
	}
	if m.Trace.Anomalies[0].Kind != trace.AnomalyDanglingParent {
		t.Errorf("the anomaly is %q", m.Trace.Anomalies[0].Kind)
	}
	if m.Trace.Anomalies[0].Value != foreign {
		t.Errorf("the anomaly names %q, want the dangling identifier", m.Trace.Anomalies[0].Value)
	}
}

func TestDelete_MalformedTraceVariableNeverBricksTheDelete(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()
	file := testutil.CreateTempFile(t, workDir, "polluted.txt", "polluted")

	const polluted = "definitely not an identifier"
	stdout, stderr, code := runSafermTraced(t, homeDir, polluted,
		"delete", "--on-error", "abort", "--description", "polluted variable", file)
	if code != 0 {
		t.Fatalf("a polluted variable failed the delete (exit %d): stderr=%q", code, stderr)
	}

	d := openArchive(t, homeDir)
	rec, err := d.QueryByUUID(archivedUUIDs(t, stdout)[0])
	if err != nil {
		t.Fatalf("querying the record: %v", err)
	}
	if rec.OriginName != nil || rec.OriginVersion != nil {
		t.Errorf("a polluted variable filled an origin: %v %v", rec.OriginName, rec.OriginVersion)
	}

	m := recordMetadata(t, rec)
	if m.Trace == nil || len(m.Trace.Anomalies) != 1 {
		t.Fatalf("the polluted variable was not recorded: %+v", m.Trace)
	}
	if m.Trace.Anomalies[0].Kind != trace.AnomalyMalformedParentValue ||
		m.Trace.Anomalies[0].Value != polluted {
		t.Errorf("the anomaly is %+v; the value must be recorded verbatim", m.Trace.Anomalies[0])
	}
}

// Every record one invocation writes carries the same group id, and two
// invocations never share one. The id is minted whatever else is true of the
// invocation -- there is nothing to opt into.
func TestDelete_GroupIDIsSharedByOneInvocation(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()
	first := testutil.CreateTempFile(t, workDir, "one.txt", "one")
	second := testutil.CreateTempFile(t, workDir, "two.txt", "two")
	third := testutil.CreateTempFile(t, workDir, "three.txt", "three")

	stdout, stderr, code := runSaferm(t, homeDir,
		"delete", "--on-error", "abort", "--description", "batch deletion", first, second)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	batch := archivedUUIDs(t, stdout)
	if len(batch) != 2 {
		t.Fatalf("archived %d paths, want 2", len(batch))
	}

	d := openArchive(t, homeDir)
	var groupID string
	for _, uuid := range batch {
		rec, err := d.QueryByUUID(uuid)
		if err != nil {
			t.Fatalf("querying %s: %v", uuid, err)
		}
		if rec.GroupID == nil || *rec.GroupID == "" {
			t.Fatalf("record %s carries no group id", uuid)
		}
		if groupID == "" {
			groupID = *rec.GroupID
		} else if *rec.GroupID != groupID {
			t.Errorf("records of one invocation carry different group ids: %q and %q", groupID, *rec.GroupID)
		}
	}

	stdout, stderr, code = runSaferm(t, homeDir,
		"delete", "--on-error", "abort", "--description", "a second invocation", third)
	if code != 0 {
		t.Fatalf("second delete failed (exit %d): stderr=%q", code, stderr)
	}
	rec, err := d.QueryByUUID(archivedUUIDs(t, stdout)[0])
	if err != nil {
		t.Fatalf("querying the second invocation's record: %v", err)
	}
	if rec.GroupID == nil || *rec.GroupID == groupID {
		t.Errorf("a second invocation reused the group id %q", groupID)
	}
}
