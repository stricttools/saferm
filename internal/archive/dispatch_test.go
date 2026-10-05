package archive

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// bogusNodeType is a NodeType outside [NodeTypes], which every dispatch must refuse.
const bogusNodeType NodeType = "bogus"

// panics runs fn and reports whether it panicked.
func panics(fn func()) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	fn()
	return false
}

// Every dispatch on a node type has a case for each of [NodeTypes] and refuses any
// other node type as a hard error. A node type added to the list without a case in one of
// these switches fails here instead of falling through to another node type's
// handling.
func TestEveryNodeTypeDispatchHandlesEveryNodeType(t *testing.T) {
	for _, k := range append(NodeTypes(), bogusNodeType) {
		known := k != bogusNodeType
		tmp := t.TempDir()
		src := filepath.Join(tmp, "src")
		dest := filepath.Join(tmp, "entry")
		for _, path := range []string{src, dest} {
			if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		identity, err := os.Lstat(src)
		if err != nil {
			t.Fatal(err)
		}

		check := func(dispatch string, err error) {
			t.Helper()
			if got := errors.Is(err, ErrUnknownNodeType); got == known {
				t.Errorf("%s(%q): unknown-node-type error is %v (err: %v)", dispatch, k, got, err)
			}
		}

		plan := &Plan{Source: src, ArchiveDir: tmp, UUID: NewUUID(), NodeType: k, Dest: dest, identity: identity}
		_, err = Execute(plan)
		check("Execute", err)

		plan = &Plan{Source: src, ArchiveDir: tmp, UUID: NewUUID(), NodeType: k, Dest: dest, identity: identity}
		check("verifySource", verifySource(plan))

		rp := &RestorePlan{UUID: "u", ArchiveDir: tmp, Dest: filepath.Join(tmp, "dest"), NodeType: k, Entry: dest}
		check("EntryPresent", EntryPresent(rp))
		check("VerifyEntry", VerifyEntry(rp, "h"))

		if got := panics(func() { EntryPath(tmp, "u", k) }); got == known {
			t.Errorf("EntryPath(%q) panicked: %v", k, got)
		}
		if got := panics(func() { IsSpecialFileType(k) }); got == known {
			t.Errorf("IsSpecialFileType(%q) panicked: %v", k, got)
		}
	}
}

func TestParseNodeTypeAcceptsEveryNodeTypeAndNothingElse(t *testing.T) {
	for _, k := range NodeTypes() {
		if got, err := ParseNodeType(string(k)); err != nil || got != k {
			t.Errorf("ParseNodeType(%q) = %q, %v", k, got, err)
		}
	}
	if _, err := ParseNodeType(string(bogusNodeType)); !errors.Is(err, ErrUnknownNodeType) {
		t.Errorf("ParseNodeType(bogus): %v", err)
	}
}
