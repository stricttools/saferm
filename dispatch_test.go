package main

import (
	"testing"

	"github.com/stricttools/saferm/internal/archive"
)

// Every kind dispatch in the command package has a case for each of
// [archive.Kinds] and refuses any other kind as a hard error. The archive
// package holds its own dispatches to the same test.
func TestEveryKindDispatchHandlesEveryKind(t *testing.T) {
	const bogus archive.Kind = "bogus"
	for _, k := range append(archive.Kinds(), bogus) {
		known := k != bogus
		tmp := t.TempDir()

		rp := &archive.RestorePlan{UUID: "u", ArchiveDir: tmp, Dest: tmp + "/dest", Kind: k, Entry: tmp + "/entry"}
		if _, err := restoreSteps(rp, false); (err == nil) != known {
			t.Errorf("restoreSteps(%q): %v", k, err)
		}

		plan := &archive.Plan{Source: tmp, ArchiveDir: tmp, UUID: "u", Kind: k, Dest: tmp + "/entry"}
		if _, err := previewEntryContent(plan); (err == nil) != known {
			t.Errorf("previewEntryContent(%q): %v", k, err)
		}

		panicked := func() (p bool) {
			defer func() { p = recover() != nil }()
			kindIndicator(k)
			return false
		}()
		if panicked == known {
			t.Errorf("kindIndicator(%q) panicked: %v", k, panicked)
		}
	}
}

// The machine payloads' kind enums and the capability features are generated
// from archive.Kinds, so every kind reaches every surface.
func TestKindEnumsAndFeaturesCoverEveryKind(t *testing.T) {
	enum := map[interface{}]bool{}
	for _, v := range kindEnum() {
		enum[v] = true
	}
	have := map[string]bool{}
	for _, f := range features {
		have[f] = true
	}
	for _, k := range archive.Kinds() {
		if !enum[string(k)] {
			t.Errorf("the kind enum lacks %q", k)
		}
		if !have[kindFeaturePrefix+string(k)] {
			t.Errorf("capabilities lacks %s%s", kindFeaturePrefix, k)
		}
	}
	if len(enum) != len(archive.Kinds()) {
		t.Errorf("the kind enum has %d values, archive.Kinds has %d", len(enum), len(archive.Kinds()))
	}
}
