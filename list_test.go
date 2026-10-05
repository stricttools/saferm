package main

import (
	"testing"

	"github.com/stricttools/saferm/internal/archive"
)

func TestGroupThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 25570: "25,570", 1234567: "1,234,567"} {
		if got := groupThousands(n); got != want {
			t.Errorf("groupThousands(%d) = %q, want %q", n, got, want)
		}
	}
}

// A special file's marker in the list table is its node type word, the word the
// payloads' node_type member carries.
func TestNodeTypeMarker_NamesASpecialFileByItsNodeType(t *testing.T) {
	for _, nodeType := range archive.NodeTypes() {
		if !archive.IsSpecialFileType(nodeType) {
			continue
		}
		if got, want := nodeTypeMarker(nodeType), " ["+string(nodeType)+"]"; got != want {
			t.Errorf("nodeTypeMarker(%q) = %q, want %q", nodeType, got, want)
		}
	}
}
