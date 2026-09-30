package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stricttools/saferm/internal/testutil"
)

// The machine surface is what a program -- an agent, a launcher, a release tool
// -- reads instead of parsing saferm's tables. It is the framework's envelope
// with saferm's own payload inside it, and these tests hold both halves: that
// stdout carries exactly one document, and that each verb's payload says what
// the human output says.

// envelope is the framework's machine-mode document, declared here as a
// consumer sees it rather than imported: the point of the surface is that
// something outside this repository can parse it from the bytes alone.
type envelope struct {
	InterfaceVersion int             `json:"interface_version"`
	App              string          `json:"app"`
	AppVersion       string          `json:"app_version"`
	Command          *string         `json:"command"`
	ExitCode         int             `json:"exit_code"`
	Payload          json.RawMessage `json:"payload"`
	DryRun           bool            `json:"dry_run"`
	// Always null here: `writes` names the properties an update command wrote,
	// and saferm declares no update command. It is mirrored anyway, because a
	// consumer parsing this envelope sees the member and this struct is the
	// consumer's own view of the document.
	Writes       json.RawMessage          `json:"writes"`
	Preview      []map[string]interface{} `json:"preview"`
	PreviewError *map[string]interface{}  `json:"preview_error"`
	Diagnostics  []struct {
		Level   string `json:"level"`
		Message string `json:"message"`
	} `json:"diagnostics"`
}

// runSafermJSON runs saferm in machine mode and parses the one document stdout
// is allowed to carry. A second document, or a table printed beside the
// envelope, fails here -- which is the property the whole surface rests on.
func runSafermJSON(t *testing.T, homeDir string, args ...string) (envelope, string, int) {
	t.Helper()

	stdout, stderr, code := runSaferm(t, homeDir, append([]string{"--json"}, args...)...)

	trimmed := strings.TrimSuffix(stdout, "\n")
	if strings.Contains(trimmed, "\n") {
		t.Fatalf("machine mode must leave exactly one document on stdout, got:\n%s", stdout)
	}
	var env envelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		t.Fatalf("stdout is not one envelope (%v): %q", err, stdout)
	}
	if env.InterfaceVersion != 2 {
		t.Errorf("interface_version = %d, want 2", env.InterfaceVersion)
	}
	// saferm declares no update command, so the version-2 member that names an
	// update's write set is null on every one of saferm's envelopes.
	if s := string(env.Writes); s != "null" {
		t.Errorf("writes = %s, want null", s)
	}
	if env.App != "saferm" {
		t.Errorf("app = %q, want saferm", env.App)
	}
	if env.ExitCode != code {
		t.Errorf("the envelope reports exit_code %d, the process exited %d", env.ExitCode, code)
	}
	return env, stderr, code
}

// Every verb's human output goes through the context writers, so machine mode
// carries it in the envelope's diagnostics instead of printing it beside the
// envelope. A table on stdout next to the document is the one thing a consumer
// cannot recover from, so it is asserted for every verb that prints one --
// including `purge`, which has no payload and is outside the machine surface.
func TestMachineMode_StdoutCarriesOnlyTheEnvelope(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "one.txt", "content\n")
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "machine mode test", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"list", []string{"list"}},
		{"info", []string{"info", uuid}},
		{"purge preview", []string{"--dry-run", "purge", "--all"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, stderr, code := runSafermJSON(t, homeDir, tc.args...)
			if code != 0 {
				t.Fatalf("%v failed (exit %d): %q", tc.args, code, stderr)
			}
			// The text the run would have printed is not lost: it rides the
			// envelope.
			var joined strings.Builder
			for _, d := range env.Diagnostics {
				joined.WriteString(d.Message)
			}
			if !strings.Contains(joined.String(), "one.txt") {
				t.Errorf("the envelope's diagnostics must carry what the run printed, got: %q", joined.String())
			}
		})
	}
}

// deletePayloadDoc is what `delete` answers with, as a consumer parses it: what
// the invocation archived, and what it could not.
type deletePayloadDoc struct {
	GroupID  string `json:"group_id"`
	Archived []struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
		Path string `json:"path"`
		Size int64  `json:"size"`
	} `json:"archived"`
	Failed []struct {
		Path  string `json:"path"`
		Error string `json:"error"`
	} `json:"failed"`
}

func deletePayloadOf(t *testing.T, env envelope) deletePayloadDoc {
	t.Helper()
	var payload deletePayloadDoc
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("delete's payload does not parse (%v): %s", err, env.Payload)
	}
	return payload
}

// `delete`'s payload is the identifier lines it already prints, in a form a
// consumer does not have to parse out of prose: one entry per archived path,
// carrying both identifiers, the path, and the size. The group identifier the
// invocation stamps on every record it writes is on the payload too, because
// nothing on the human stream ever named it.
func TestMachineSurface_DeleteNamesEveryRecordItWrote(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	first := testutil.CreateTempFile(t, workDir, "first.txt", "first\n")
	second := testutil.CreateTempFile(t, workDir, "second.txt", "second content\n")

	env, stderr, code := runSafermJSON(t, homeDir,
		"delete", "--on-error", "abort", "--description", "delete payload test", first, second)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): %q", code, stderr)
	}

	payload := deletePayloadOf(t, env)

	// A run that failed nothing says so with an empty list rather than with a
	// missing member: a consumer reads the same two arrays on every answer.
	if payload.Failed == nil {
		t.Errorf("the failure list is always present, empty where nothing failed, got: %s", env.Payload)
	}
	if len(payload.Failed) != 0 {
		t.Errorf("nothing failed, so the failure list is empty, got: %s", env.Payload)
	}

	if len(payload.Archived) != 2 {
		t.Fatalf("both paths were archived, so both belong on the payload, got: %s", env.Payload)
	}
	if payload.GroupID == "" {
		t.Error("the payload must carry the group identifier the invocation stamped on its records")
	}
	if payload.Archived[0].Path != first || payload.Archived[1].Path != second {
		t.Errorf("the payload must name the paths in the order they were archived, got: %s", env.Payload)
	}
	if payload.Archived[0].Size != int64(len("first\n")) {
		t.Errorf("size = %d, want %d", payload.Archived[0].Size, len("first\n"))
	}
	for _, rec := range payload.Archived {
		if len(rec.UUID) != 36 {
			t.Errorf("each entry carries the durable handle, got %q", rec.UUID)
		}
		if rec.ID <= 0 {
			t.Errorf("each entry carries the database id, got %d", rec.ID)
		}
	}

	// `info` accepts what the payload handed back, which is what makes the
	// identifiers usable rather than merely present.
	if _, _, code := runSaferm(t, homeDir, "info", payload.Archived[0].UUID); code != 0 {
		t.Errorf("the payload's uuid must resolve, got exit %d", code)
	}
}

// Under `--on-error continue` the failures are the half of the answer the
// archived list cannot carry: a consumer that only reads `archived` has to diff
// its own argument list against it to learn what did not make it, and the
// reason lives nowhere but stderr prose. The payload names both -- the path and
// the message saferm printed about it.
func TestMachineSurface_DeleteNamesEveryPathThatFailed(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	first := testutil.CreateTempFile(t, workDir, "first.txt", "one\n")
	missing := filepath.Join(workDir, "does-not-exist.txt")
	third := testutil.CreateTempFile(t, workDir, "third.txt", "three\n")

	env, stderr, code := runSafermJSON(t, homeDir, "delete", "--on-error", "continue",
		"--description", "failure payload test", first, missing, third)
	if code == 0 {
		t.Fatalf("a failing path must fail the command; payload=%s", env.Payload)
	}

	payload := deletePayloadOf(t, env)
	if len(payload.Archived) != 2 {
		t.Fatalf("continue mode archived both surviving paths, so both belong on the payload, got: %s", env.Payload)
	}
	if payload.Archived[0].Path != first || payload.Archived[1].Path != third {
		t.Errorf("archived = %v, want %s and %s", payload.Archived, first, third)
	}
	if len(payload.Failed) != 1 {
		t.Fatalf("one path failed, so the failure list holds one entry, got: %s", env.Payload)
	}
	if payload.Failed[0].Path != missing {
		t.Errorf("the failure names %q, want %q", payload.Failed[0].Path, missing)
	}
	if payload.Failed[0].Error == "" {
		t.Errorf("the failure carries why it failed, got: %s", env.Payload)
	}
	// The message is the same one the human stream carries, not a second
	// vocabulary a consumer would have to learn separately.
	if !strings.Contains(stderr, payload.Failed[0].Error) {
		t.Errorf("the payload's message %q is not what stderr said: %q", payload.Failed[0].Error, stderr)
	}
}

// The same list survives an abort: the batch stopped at the failing path, and
// the payload names what it archived above it AND what stopped it. The exit
// code says a failure happened; only the payload says which path it was.
func TestMachineSurface_DeleteAbortNamesTheFailureThatStoppedIt(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	first := testutil.CreateTempFile(t, workDir, "first.txt", "one\n")
	missing := filepath.Join(workDir, "does-not-exist.txt")
	third := testutil.CreateTempFile(t, workDir, "third.txt", "three\n")

	env, stderr, code := runSafermJSON(t, homeDir, "delete", "--on-error", "abort",
		"--description", "abort payload test", first, missing, third)
	if code == 0 {
		t.Fatalf("a failing path must fail the command; stderr=%q", stderr)
	}

	payload := deletePayloadOf(t, env)
	if len(payload.Archived) != 1 || payload.Archived[0].Path != first {
		t.Fatalf("the abort names everything archived above the failure, got: %s", env.Payload)
	}
	if len(payload.Failed) != 1 || payload.Failed[0].Path != missing {
		t.Fatalf("the abort names the path that stopped it, got: %s", env.Payload)
	}
	// Nothing after the failure was attempted, so nothing after it is claimed
	// on either list.
	if _, err := os.Lstat(third); err != nil {
		t.Errorf("abort mode archived %s after the failure: %v", third, err)
	}
}

// A previewed delete produces no records, so it claims none: the envelope's
// dry_run flag and its preview say what would happen, and the payload does not
// invent identifiers for rows that were never written.
func TestMachineSurface_DeletePreviewClaimsNoRecords(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "previewed.txt", "content\n")

	env, stderr, code := runSafermJSON(t, homeDir,
		"--dry-run", "delete", "--on-error", "abort", "--description", "delete preview payload", file)
	if code != 0 {
		t.Fatalf("previewed delete failed (exit %d): %q", code, stderr)
	}
	if !env.DryRun {
		t.Error("the envelope must report the run as a preview")
	}
	if len(env.Preview) == 0 {
		t.Error("the envelope's preview must name what the delete would do")
	}

	var payload struct {
		GroupID  string        `json:"group_id"`
		Archived []interface{} `json:"archived"`
	}
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("delete's payload does not parse (%v): %s", err, env.Payload)
	}
	if len(payload.Archived) != 0 {
		t.Errorf("a preview writes no records, so it claims none, got: %s", env.Payload)
	}
	if _, err := os.Lstat(file); err != nil {
		t.Errorf("a previewed delete archived %s for real: %v", file, err)
	}
}

// undeletePayload is what `undelete` answers with: which record was restored,
// and where the content actually went.
type undeletePayload struct {
	ID           int64  `json:"id"`
	UUID         string `json:"uuid"`
	OriginalPath string `json:"original_path"`
	RestoredTo   string `json:"restored_to"`
	Kind         string `json:"kind"`
	Overwrote    bool   `json:"overwrote"`
}

// `undelete`'s payload answers "what went where", which the human line says in
// prose and an alternate destination makes non-obvious: the record came from
// one path and the content is now at another.
func TestMachineSurface_UndeleteNamesWhatWentWhere(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()
	elsewhere := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "restored.txt", "archived content\n")
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "undelete payload test", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	dest := filepath.Join(elsewhere, "moved.txt")

	env, stderr, code := runSafermJSON(t, homeDir, "undelete", "--destination", dest, uuid)
	if code != 0 {
		t.Fatalf("undelete failed (exit %d): %q", code, stderr)
	}

	var payload undeletePayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("undelete's payload does not parse (%v): %s", err, env.Payload)
	}
	if payload.UUID != uuid {
		t.Errorf("uuid = %q, want %q", payload.UUID, uuid)
	}
	if payload.OriginalPath != file {
		t.Errorf("original_path = %q, want %q", payload.OriginalPath, file)
	}
	if payload.RestoredTo != dest {
		t.Errorf("restored_to = %q, want %q", payload.RestoredTo, dest)
	}
	if payload.Kind != "file" {
		t.Errorf("kind = %q, want file", payload.Kind)
	}
	if payload.Overwrote {
		t.Error("nothing was standing at the destination, so nothing was overwritten")
	}
	if payload.ID <= 0 {
		t.Errorf("id = %d, want the record's database id", payload.ID)
	}
}

// The same answer in preview form: a previewed restore says where the content
// would go and restores nothing. The envelope's dry_run flag is what tells the
// two apart, which is why the payload does not need a second word for it.
func TestMachineSurface_UndeletePreviewNamesTheDestination(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	tree := testutil.CreateTempDir(t, workDir, "tree")
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "undelete preview payload", tree)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)

	env, stderr, code := runSafermJSON(t, homeDir, "--dry-run", "undelete", uuid)
	if code != 0 {
		t.Fatalf("previewed undelete failed (exit %d): %q", code, stderr)
	}
	if !env.DryRun {
		t.Error("the envelope must report the run as a preview")
	}

	var payload undeletePayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("undelete's payload does not parse (%v): %s", err, env.Payload)
	}
	if payload.RestoredTo != tree {
		t.Errorf("restored_to = %q, want %q", payload.RestoredTo, tree)
	}
	if payload.Kind != "directory" {
		t.Errorf("kind = %q, want directory", payload.Kind)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Errorf("a previewed restore recreated %s for real: %v", tree, err)
	}
}

// An overwriting restore says so on the payload: the destination held something
// else and the restore destroyed it, which is the one thing a consumer cannot
// read back off the filesystem afterwards.
func TestMachineSurface_UndeleteReportsAnOverwrite(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "occupied.txt", "archived content\n")
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "overwrite payload test", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)
	testutil.CreateTempFile(t, workDir, "occupied.txt", "replace me\n")

	env, stderr, code := runSafermJSON(t, homeDir, "undelete", "--on-conflict", "overwrite", uuid)
	if code != 0 {
		t.Fatalf("overwriting undelete failed (exit %d): %q", code, stderr)
	}
	var payload undeletePayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("undelete's payload does not parse (%v): %s", err, env.Payload)
	}
	if !payload.Overwrote {
		t.Errorf("the payload must record that the restore replaced something, got: %s", env.Payload)
	}
}

// listRow is one row of `list`'s payload: the table's columns, plus the durable
// handle the table has no room for and a timestamp instead of an age.
type listRow struct {
	ID        int64  `json:"id"`
	UUID      string `json:"uuid"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Kind      string `json:"kind"`
	DeletedAt string `json:"deleted_at"`
	Status    string `json:"status"`
}

// `list`'s payload is its rows, beside the total. Two things the table
// cannot carry are on each row: the uuid (the table shows only the numeric id,
// and the uuid is the handle that survives) and an absolute timestamp (the
// Age column is relative prose nothing can compute with).
func TestMachineSurface_ListCarriesTheRows(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "listed.txt", "content\n")
	tree := testutil.CreateTempDir(t, workDir, "listed-tree")

	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "-r", "--description", "list payload test", file, tree)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	_ = stdout

	env, stderr, code := runSafermJSON(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d): %q", code, stderr)
	}

	var doc listDoc
	if err := json.Unmarshal(env.Payload, &doc); err != nil {
		t.Fatalf("list's payload does not parse (%v): %s", err, env.Payload)
	}
	rows := doc.Rows
	if len(rows) != 2 {
		t.Fatalf("two records were archived, so the payload holds two rows, got: %s", env.Payload)
	}

	byPath := map[string]listRow{}
	for _, row := range rows {
		byPath[row.Path] = row
	}
	fileRow, ok := byPath[file]
	if !ok {
		t.Fatalf("the payload must name the archived file, got: %s", env.Payload)
	}
	if fileRow.Kind != "file" {
		t.Errorf("kind = %q, want file", fileRow.Kind)
	}
	if fileRow.Status != "archived" {
		t.Errorf("status = %q, want archived", fileRow.Status)
	}
	if len(fileRow.UUID) != 36 {
		t.Errorf("each row carries the durable handle, got %q", fileRow.UUID)
	}
	if _, err := time.Parse(time.RFC3339, fileRow.DeletedAt); err != nil {
		t.Errorf("deleted_at must be a timestamp, got %q (%v)", fileRow.DeletedAt, err)
	}
	if byPath[tree].Kind != "directory" {
		t.Errorf("the tree's kind = %q, want directory", byPath[tree].Kind)
	}

	// A restored record's row says so, and --all is what shows it at all.
	if _, stderr, code := runSaferm(t, homeDir, "undelete", fileRow.UUID); code != 0 {
		t.Fatalf("undelete failed (exit %d): %q", code, stderr)
	}
	for _, row := range listPayload(t, homeDir, "--all").Rows {
		if row.Path == file && row.Status != "restored" {
			t.Errorf("the restored row's status = %q, want restored", row.Status)
		}
	}
}

// An empty archive answers with an empty list of rows and a total of zero,
// not with null: a consumer iterating the rows must not have to special-case
// "nothing has ever been deleted on this machine".
func TestMachineSurface_ListOfNothingIsAnEmptyArray(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)

	env, stderr, code := runSafermJSON(t, homeDir, "list")
	if code != 0 {
		t.Fatalf("list failed (exit %d): %q", code, stderr)
	}
	if !isEmptyListPayload(env.Payload) {
		t.Errorf("an empty archive's payload must be no rows and a total of 0, got: %s", env.Payload)
	}

	// The same answer where the filter matches nothing.
	env, _, code = runSafermJSON(t, homeDir, "list", "--path", "/nowhere/*")
	if code != 0 {
		t.Fatalf("filtered list failed (exit %d)", code)
	}
	if !isEmptyListPayload(env.Payload) {
		t.Errorf("a filter matching nothing must answer no rows and a total of 0, got: %s", env.Payload)
	}
}

// isEmptyListPayload reports whether a list payload is a total of 0 beside an
// empty rows array -- present and empty, not null.
func isEmptyListPayload(payload json.RawMessage) bool {
	var doc struct {
		Total *int             `json:"total"`
		Rows  *json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil || doc.Total == nil || doc.Rows == nil {
		return false
	}
	return *doc.Total == 0 && strings.TrimSpace(string(*doc.Rows)) == "[]"
}

// infoPayload is the record `info` prints, as a machine reads it. The nullable
// members are pointers on purpose: the difference between "no tool claimed this
// deletion" and "a tool named the empty string" is the whole content of the
// origin columns.
type infoPayload struct {
	ID            int64   `json:"id"`
	UUID          string  `json:"uuid"`
	OriginalPath  string  `json:"original_path"`
	OriginalName  string  `json:"original_name"`
	Size          int64   `json:"size"`
	Hash          string  `json:"hash"`
	Kind          string  `json:"kind"`
	SymlinkTarget *string `json:"symlink_target"`
	DeletedAt     string  `json:"deleted_at"`
	Status        string  `json:"status"`
	Description   string  `json:"description"`
	Command       string  `json:"command"`
	RestoredAt    *string `json:"restored_at"`
	RestoredTo    *string `json:"restored_to"`
	PurgedAt      *string `json:"purged_at"`
	OriginName    *string `json:"origin_name"`
	OriginVersion *string `json:"origin_version"`
	GroupID       *string `json:"group_id"`
}

func infoPayloadOf(t *testing.T, env envelope) infoPayload {
	t.Helper()
	var payload infoPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("info's payload does not parse (%v): %s", err, env.Payload)
	}
	return payload
}

// `info`'s payload is the record, including the three things the human page
// states in prose or not at all: the derived status, the group identifier, and
// where a restored record's content went.
func TestMachineSurface_InfoCarriesTheRecord(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()
	elsewhere := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "inspected.txt", "archived content\n")
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort",
		"--description", "info payload test", "--command", "rm inspected.txt", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)

	env, stderr, code := runSafermJSON(t, homeDir, "info", uuid)
	if code != 0 {
		t.Fatalf("info failed (exit %d): %q", code, stderr)
	}
	payload := infoPayloadOf(t, env)

	if payload.UUID != uuid || payload.OriginalPath != file {
		t.Errorf("the payload must name the record it inspected, got: %s", env.Payload)
	}
	if payload.OriginalName != "inspected.txt" {
		t.Errorf("original_name = %q", payload.OriginalName)
	}
	if payload.Kind != "file" || payload.Hash == "" {
		t.Errorf("kind = %q, hash = %q", payload.Kind, payload.Hash)
	}
	if payload.Status != "restorable" {
		t.Errorf("status = %q, want restorable", payload.Status)
	}
	if payload.Description != "info payload test" || payload.Command != "rm inspected.txt" {
		t.Errorf("description = %q, command = %q", payload.Description, payload.Command)
	}
	if _, err := time.Parse(time.RFC3339, payload.DeletedAt); err != nil {
		t.Errorf("deleted_at must be a timestamp, got %q (%v)", payload.DeletedAt, err)
	}
	if payload.GroupID == nil || *payload.GroupID == "" {
		t.Error("the record carries the group identifier its invocation stamped on it")
	}
	// Nothing traced this deletion, and null is how the payload says so -- not
	// an empty string, which nothing could read as either answer.
	if payload.OriginName != nil || payload.OriginVersion != nil {
		t.Errorf("an untraced deletion claims no origin, got %v / %v", payload.OriginName, payload.OriginVersion)
	}
	if payload.RestoredAt != nil || payload.RestoredTo != nil || payload.PurgedAt != nil {
		t.Errorf("a restorable record has no lifecycle timestamps, got: %s", env.Payload)
	}
	if payload.SymlinkTarget != nil {
		t.Errorf("a file record names no symlink target, got %v", payload.SymlinkTarget)
	}

	// After a restore to somewhere else, the payload says both that it was
	// restored and where the content actually went.
	dest := filepath.Join(elsewhere, "moved.txt")
	if _, stderr, code := runSaferm(t, homeDir, "undelete", "--destination", dest, uuid); code != 0 {
		t.Fatalf("undelete failed (exit %d): %q", code, stderr)
	}
	env, _, _ = runSafermJSON(t, homeDir, "info", uuid)
	payload = infoPayloadOf(t, env)
	if payload.Status != "restored" {
		t.Errorf("status = %q, want restored", payload.Status)
	}
	if payload.RestoredTo == nil || *payload.RestoredTo != dest {
		t.Errorf("restored_to = %v, want %q", payload.RestoredTo, dest)
	}
	if payload.RestoredAt == nil {
		t.Error("a restored record carries the time it was restored")
	}
}

// The status member is a closed set of words, not the human line's prose: a
// consumer branches on it, and "the archived copy is gone though nothing
// restored or purged it" is a sentence, not a state name.
func TestMachineSurface_InfoStatusIsAClosedSet(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "states.txt", "content\n")
	stdout, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "status states", file)
	if code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)

	// The row names nothing: the archived copy went without a restore or a
	// purge, which is a state saferm produces itself.
	if err := os.Remove(archiveEntry(homeDir, uuid, "")); err != nil {
		t.Fatal(err)
	}
	env, _, _ := runSafermJSON(t, homeDir, "info", uuid)
	if got := infoPayloadOf(t, env).Status; got != "entry-missing" {
		t.Errorf("status = %q, want entry-missing", got)
	}

	// And a purged row says purged.
	if _, stderr, code := runSaferm(t, homeDir, "--approve-consequential", "purge", uuid); code != 0 {
		t.Fatalf("purge failed (exit %d): %q", code, stderr)
	}
	env, _, _ = runSafermJSON(t, homeDir, "info", uuid)
	if got := infoPayloadOf(t, env).Status; got != "purged" {
		t.Errorf("status = %q, want purged", got)
	}
}

// A traced deletion's origin reaches the payload, which is the machine-readable
// half of "which tool ran this".
func TestMachineSurface_InfoCarriesTheOrigin(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	parentID := mintID(msTraced, "0000000000000042")
	writeTraceStore(t, homeDir, "2026-08-13T04", traceEntryLine(parentID, "", "claudewheel", "0.42.0"))

	file := testutil.CreateTempFile(t, workDir, "traced.txt", "content\n")
	stdout, stderr, code := runSafermTraced(t, homeDir, parentID,
		"delete", "--on-error", "abort", "--description", "origin payload test", file)
	if code != 0 {
		t.Fatalf("traced delete failed (exit %d): stderr=%q", code, stderr)
	}
	uuid := parseArchivedUUID(t, stdout)

	env, _, _ := runSafermJSON(t, homeDir, "info", uuid)
	payload := infoPayloadOf(t, env)
	if payload.OriginName == nil || *payload.OriginName != "claudewheel" {
		t.Errorf("origin_name = %v, want claudewheel", payload.OriginName)
	}
	if payload.OriginVersion == nil || *payload.OriginVersion != "0.42.0" {
		t.Errorf("origin_version = %v, want 0.42.0", payload.OriginVersion)
	}
}

// The features `capabilities` names, pinned here as a consumer reads them.
//
// A consumer probes the verb and treats a missing verb or a missing feature
// exactly like saferm being absent, so this list is an interface: adding a name
// is how a new feature becomes negotiable, and removing one is a breaking
// change to every caller that asks for it. Nothing here is a version number --
// a locally built saferm reports a Go pseudo-version no semver parser accepts,
// so a version comparison is not a probe anyone can rely on.
var pinnedFeatures = []string{
	"git-index-switches",
	"group-id",
	"list-limit",
	"list-since",
	"machine-payloads",
	"on-conflict-modes",
	"on-error-modes",
	"restore-destination",
	"trace-origin",
	"usage-report",
	"uuid-handles",
}

func TestMachineSurface_CapabilitiesNamesTheFeaturesShipped(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)

	env, stderr, code := runSafermJSON(t, homeDir, "capabilities")
	if code != 0 {
		t.Fatalf("capabilities failed (exit %d): %q", code, stderr)
	}

	var payload struct {
		Features []string `json:"features"`
	}
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("capabilities' payload does not parse (%v): %s", err, env.Payload)
	}
	if len(payload.Features) != len(pinnedFeatures) {
		t.Fatalf("features = %v, want %v", payload.Features, pinnedFeatures)
	}
	for i, want := range pinnedFeatures {
		if payload.Features[i] != want {
			t.Errorf("features[%d] = %q, want %q", i, payload.Features[i], want)
		}
	}
	for _, f := range payload.Features {
		if strings.ContainsAny(f, "0123456789") {
			t.Errorf("a feature name is not a version: %q", f)
		}
	}
}

// The probe must answer on a machine that has never deleted anything, and must
// not create saferm's state directory to do it: a consumer asks what this
// saferm can do before it decides to use it at all.
func TestMachineSurface_CapabilitiesNeedsNoArchive(t *testing.T) {
	homeDir := t.TempDir()
	testutil.Isolate(t)

	stdout, stderr, code := runSaferm(t, homeDir, "capabilities")
	if code != 0 {
		t.Fatalf("capabilities failed on a machine with no archive (exit %d): %q", code, stderr)
	}
	for _, want := range pinnedFeatures {
		if !strings.Contains(stdout, want) {
			t.Errorf("the human output must name %q, got: %q", want, stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(homeDir, ".saferm")); !os.IsNotExist(err) {
		t.Errorf("the probe created saferm's state directory: %v", err)
	}
}

// A machine-mode run under --quiet still emits the complete envelope: the
// document is not written through the writers --quiet suppresses, so quiet has
// no mechanism by which to reach it.
func TestMachineMode_QuietDoesNotReachTheEnvelope(t *testing.T) {
	homeDir := testutil.SetupTestEnv(t)
	workDir := t.TempDir()

	file := testutil.CreateTempFile(t, workDir, "quiet.txt", "content\n")
	if _, stderr, code := runSaferm(t, homeDir, "delete", "--on-error", "abort", "--description", "quiet machine test", file); code != 0 {
		t.Fatalf("delete failed (exit %d): stderr=%q", code, stderr)
	}

	env, stderr, code := runSafermJSON(t, homeDir, "--quiet", "list")
	if code != 0 {
		t.Fatalf("a quiet machine-mode list failed (exit %d): %q", code, stderr)
	}
	if env.Command == nil || *env.Command != "list" {
		t.Errorf("the envelope must name the command it ran, got: %v", env.Command)
	}
}
