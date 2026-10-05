package archive

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// bogusKind is a Kind outside [Kinds], which every dispatch must refuse.
const bogusKind Kind = "bogus"

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

// Every dispatch on a kind has a case for each of [Kinds] and refuses any
// other kind as a hard error. A kind added to the list without a case in one of
// these switches fails here instead of falling through to another kind's
// handling.
func TestEveryKindDispatchHandlesEveryKind(t *testing.T) {
	for _, k := range append(Kinds(), bogusKind) {
		known := k != bogusKind
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
			if got := errors.Is(err, ErrUnknownKind); got == known {
				t.Errorf("%s(%q): unknown-kind error is %v (err: %v)", dispatch, k, got, err)
			}
		}

		plan := &Plan{Source: src, ArchiveDir: tmp, UUID: NewUUID(), Kind: k, Dest: dest, identity: identity}
		_, err = Execute(plan)
		check("Execute", err)

		plan = &Plan{Source: src, ArchiveDir: tmp, UUID: NewUUID(), Kind: k, Dest: dest, identity: identity}
		check("verifySource", verifySource(plan))

		rp := &RestorePlan{UUID: "u", ArchiveDir: tmp, Dest: filepath.Join(tmp, "dest"), Kind: k, Entry: dest}
		check("EntryPresent", EntryPresent(rp))
		check("VerifyEntry", VerifyEntry(rp, "h"))

		if got := panics(func() { EntryPath(tmp, "u", k) }); got == known {
			t.Errorf("EntryPath(%q) panicked: %v", k, got)
		}
		if got := panics(func() { IsNodeKind(k) }); got == known {
			t.Errorf("IsNodeKind(%q) panicked: %v", k, got)
		}
	}
}

func TestParseKindAcceptsEveryKindAndNothingElse(t *testing.T) {
	for _, k := range Kinds() {
		if got, err := ParseKind(string(k)); err != nil || got != k {
			t.Errorf("ParseKind(%q) = %q, %v", k, got, err)
		}
	}
	if _, err := ParseKind(string(bogusKind)); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("ParseKind(bogus): %v", err)
	}
}
