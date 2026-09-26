package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Every env-denylist pattern the documentation shows a reader must compile with
// Go's regexp, because a reader copies it into config.toml verbatim. Go's
// regexp is RE2: no lookahead, no backreferences. The docs once carried
// "(?i)key(?!BOARD)", which RE2 rejects -- and until an uncompilable pattern
// became a hard error, copying it silently switched the `key` redaction off.
//
// This test extracts the patterns from the docs SOURCE (stricttools/docs/*.md,
// the
// templates selfdoc renders README.md and CLAUDE.md from), so a future doc edit
// that introduces an illegal pattern fails here rather than in a user's config.

// docPattern is one pattern literal found in the documentation.
type docPattern struct {
	pattern string
	file    string
	line    int
}

// excludeListStart matches the opening of a TOML `exclude_env_patterns` list.
var excludeListStart = regexp.MustCompile(`^\s*exclude_env_patterns\s*=\s*\[`)

// quotedString matches a double-quoted TOML string.
var quotedString = regexp.MustCompile(`"([^"]*)"`)

// bulletedRegexp matches a markdown list item whose whole content is one
// inline-code span opening with a regexp group flag -- the `(?i)` convention
// every documented denylist pattern follows. It catches patterns documented as
// a bulleted list rather than inside a TOML list literal.
//
// It is deliberately restricted to list items: running prose says things like
// "lookahead (`(?!...)`) is unavailable", which is a description of syntax
// rather than a pattern anyone would copy.
var bulletedRegexp = regexp.MustCompile("^\\s*[-*]\\s+`(\\(\\?[^`]*)`\\s*$")

// collectDocPatterns walks docsDir and returns every denylist pattern the
// documentation shows.
func collectDocPatterns(t *testing.T, docsDir string) []docPattern {
	t.Helper()

	var found []docPattern

	entries, err := os.ReadDir(docsDir)
	if err != nil {
		t.Fatalf("reading %s: %v", docsDir, err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(docsDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}

		inExcludeList := false
		for i, line := range strings.Split(string(data), "\n") {
			lineNo := i + 1

			if excludeListStart.MatchString(line) {
				inExcludeList = true
			}
			if inExcludeList {
				for _, m := range quotedString.FindAllStringSubmatch(line, -1) {
					found = append(found, docPattern{pattern: m[1], file: path, line: lineNo})
				}
				if strings.Contains(line, "]") {
					inExcludeList = false
				}
				continue
			}

			if m := bulletedRegexp.FindStringSubmatch(line); m != nil {
				found = append(found, docPattern{pattern: m[1], file: path, line: lineNo})
			}
		}
	}

	return found
}

func TestDocumentedDenylistPatternsCompile(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	docsDir := filepath.Join(filepath.Dir(thisFile), "stricttools", "docs")

	patterns := collectDocPatterns(t, docsDir)

	// A restructure that hides the patterns from the extractor would make this
	// test pass vacuously, so require that it still finds them -- both the TOML
	// list literal in the readme template and the bulleted list in the
	// configuration page.
	const minimumExpected = 5
	if len(patterns) < minimumExpected {
		t.Fatalf("found only %d documented denylist patterns under %s; the extractor has lost sight of them", len(patterns), docsDir)
	}

	sources := make(map[string]map[string]bool)
	for _, p := range patterns {
		if sources[p.pattern] == nil {
			sources[p.pattern] = map[string]bool{}
		}
		sources[p.pattern][filepath.Base(p.file)] = true
	}
	for pattern, wantFile := range map[string]string{
		"(?i)token": "_README.md",
		"(?i)key":   "configuration.md",
	} {
		if !sources[pattern][wantFile] {
			t.Fatalf("expected to extract %q from the docs source %s; the extractor found it in %v",
				pattern, wantFile, sources[pattern])
		}
	}

	for _, p := range patterns {
		if _, err := regexp.Compile(p.pattern); err != nil {
			t.Errorf("%s:%d documents the pattern %q, which Go's regexp rejects: %v",
				p.file, p.line, p.pattern, err)
		}
	}
}
