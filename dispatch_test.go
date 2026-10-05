package main

import (
	"testing"

	"github.com/stricttools/saferm/internal/archive"
)

// Every node type dispatch in the command package has a case for each of
// [archive.NodeTypes] and refuses any other node type as a hard error. The archive
// package holds its own dispatches to the same test.
func TestEveryNodeTypeDispatchHandlesEveryNodeType(t *testing.T) {
	const bogus archive.NodeType = "bogus"
	for _, k := range append(archive.NodeTypes(), bogus) {
		known := k != bogus
		tmp := t.TempDir()

		rp := &archive.RestorePlan{UUID: "u", ArchiveDir: tmp, Dest: tmp + "/dest", NodeType: k, Entry: tmp + "/entry"}
		if _, err := restoreSteps(rp, false); (err == nil) != known {
			t.Errorf("restoreSteps(%q): %v", k, err)
		}

		plan := &archive.Plan{Source: tmp, ArchiveDir: tmp, UUID: "u", NodeType: k, Dest: tmp + "/entry"}
		if _, err := previewEntryContent(plan); (err == nil) != known {
			t.Errorf("previewEntryContent(%q): %v", k, err)
		}

		panicked := func() (p bool) {
			defer func() { p = recover() != nil }()
			nodeTypeMarker(k)
			return false
		}()
		if panicked == known {
			t.Errorf("nodeTypeMarker(%q) panicked: %v", k, panicked)
		}
	}
}

// The machine payloads' node type enums and the capability features are generated
// from archive.NodeTypes, so every node type reaches every surface.
func TestNodeTypeEnumsAndFeaturesCoverEveryNodeType(t *testing.T) {
	enum := map[interface{}]bool{}
	for _, v := range nodeTypeEnum() {
		enum[v] = true
	}
	have := map[string]bool{}
	for _, f := range features {
		have[f] = true
	}
	for _, k := range archive.NodeTypes() {
		if !enum[string(k)] {
			t.Errorf("the node type enum lacks %q", k)
		}
		if !have[nodeTypeFeaturePrefix+string(k)] {
			t.Errorf("capabilities lacks %s%s", nodeTypeFeaturePrefix, k)
		}
	}
	if len(enum) != len(archive.NodeTypes()) {
		t.Errorf("the node type enum has %d values, archive.NodeTypes has %d", len(enum), len(archive.NodeTypes()))
	}
}
