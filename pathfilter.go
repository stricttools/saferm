package main

import (
	"strings"
	"time"

	"github.com/stricttools/saferm/internal/db"
)

// pathGlobMeta holds the bytes at which a --path pattern stops being literal:
// the three filepath.Match wildcards and the escape character. Stopping at the
// escape too keeps the literal prefix conservative -- `\*` pins down a star,
// but the prefix never has to know that.
const pathGlobMeta = `*?[\`

// validatePathPattern reports a malformed --path pattern before anything is
// read, whatever the archive holds.
//
// Matching the pattern against the empty path is not enough on its own:
// filepath.Match returns at the first chunk that fails to match, so a
// malformed chunk after a star -- `/a/*/[b` -- is never examined. What it does
// examine completely is a single chunk, even after the chunk has failed to
// match. So the pattern is cut at its top-level stars, the way filepath.Match
// cuts it, and every chunk goes through matchArchivePath -- still the one
// matcher, now shown every part of the pattern.
func validatePathPattern(pattern string) error {
	for _, chunk := range patternChunks(pattern) {
		if _, err := matchArchivePath(chunk, ""); err != nil {
			return err
		}
	}
	return nil
}

// patternChunks cuts a pattern into the star-free chunks filepath.Match
// matches one at a time (its scanChunk, outside Windows): a star inside a
// character class, or escaped by a backslash, belongs to its chunk.
func patternChunks(pattern string) []string {
	var chunks []string
	for len(pattern) > 0 {
		pattern = strings.TrimLeft(pattern, "*")
		inClass := false
		i := 0
	scan:
		for ; i < len(pattern); i++ {
			switch pattern[i] {
			case '\\':
				if i+1 < len(pattern) {
					i++
				}
			case '[':
				inClass = true
			case ']':
				inClass = false
			case '*':
				if !inClass {
					break scan
				}
			}
		}
		chunks = append(chunks, pattern[:i])
		pattern = pattern[i:]
	}
	return chunks
}

// pathPatternRange returns the range of original paths a --path pattern can
// match: every match starts with the pattern's literal prefix, so every match
// lies in [lower, upper). bounded is false when no string is above every path
// carrying the prefix -- an empty prefix, or one made only of 0xff bytes --
// and the range then runs from lower to the end.
func pathPatternRange(pattern string) (lower, upper string, bounded bool) {
	lower = pattern
	if i := strings.IndexAny(pattern, pathGlobMeta); i >= 0 {
		lower = pattern[:i]
	}
	upper, bounded = prefixUpperBound(lower)
	return lower, upper, bounded
}

// prefixUpperBound returns the smallest string greater than every string that
// starts with prefix: the prefix with its last byte below 0xff incremented and
// everything after that byte dropped. It reports false when there is no such
// byte.
func prefixUpperBound(prefix string) (string, bool) {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}

// selectListRecords returns the records `list` shows, newest first: every
// record (or every live one), narrowed to those whose original path matches
// pathGlob when it is not empty, and to those deleted at or after since when
// since is not nil. pathGlob must already have passed validatePathPattern.
//
// A pattern is answered in two reads rather than one read of every row. The
// first takes (id, original_path) for the pattern's literal-prefix range out
// of the path index alone, and matchArchivePath -- the one matcher --
// decides which of those match. The second fetches only the matching rows, by
// id. SQL's GLOB is never used: its `*` stops at nothing either, but its
// character classes and escapes are not filepath.Match's, and two matchers are
// two answers.
func selectListRecords(database *db.DB, pathGlob string, includeAll bool, since *time.Time) ([]*db.DeletionRecord, error) {
	if pathGlob == "" {
		if since != nil {
			return database.QueryDeletedSince(*since, includeAll)
		}
		return database.QueryAll(includeAll)
	}
	lower, upper, bounded := pathPatternRange(pathGlob)
	candidates, err := database.QueryPathRange(lower, upper, bounded)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, c := range candidates {
		matched, err := matchArchivePath(pathGlob, c.Path)
		if err != nil {
			return nil, err
		}
		if matched {
			ids = append(ids, c.ID)
		}
	}
	records, err := database.QueryByIDs(ids, includeAll)
	if err != nil {
		return nil, err
	}
	if since != nil {
		records = db.DeletedSince(records, *since)
	}
	return records, nil
}
