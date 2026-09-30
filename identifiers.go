package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"

	"github.com/stricttools/saferm/internal/db"
)

// The identifier order, pinned in one place.
//
// Three kinds of string can name an archived record, and they are read in this
// order:
//
//  1. a record UUID -- 36 characters, hyphenated hex in 8-4-4-4-12 groups.
//     Structurally unambiguous: nothing else saferm accepts has that shape.
//  2. a numeric database ID -- all digits, nothing else.
//  3. a path -- everything that is neither of the above.
//
// The order is total and decided by shape alone, so the same argument always
// means the same thing regardless of what happens to exist on disk or in the
// archive. `undelete` accepts all three; `info` and `purge` accept the first
// two and refuse a path outright rather than searching for one.
var (
	uuidShaped   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	digitsShaped = regexp.MustCompile(`^[0-9]+$`)
)

// identifierKind is what an identifier argument turned out to be.
type identifierKind int

const (
	identifierUUID identifierKind = iota
	identifierID
	identifierPath
)

// identifierOrderHelp is the one sentence describing the order, reused verbatim
// in every verb's argument help so the three cannot drift apart.
const identifierOrderHelp = "resolved by shape, in this order: a 36-character hyphenated hex string is a record UUID, an all-digit string is a numeric database ID"

// requireIdentifierShape refuses a path handed to a verb that takes
// identifiers only, and says which two forms it does take.
//
// It exists separately from resolveRecord because the refusal is about the
// argument's shape alone: it holds on a machine with no archive at all, where
// there is no database to resolve anything against, and answering "no record
// for /some/path" there would tell a caller its path was looked up when it
// never could be.
func requireIdentifierShape(target string) int {
	if classifyIdentifier(target) == identifierPath {
		fmt.Fprintf(os.Stderr, "error: %q is neither a record UUID nor a numeric ID (%s; this command does not take paths)\n",
			target, identifierOrderHelp)
		return ExitUsage
	}
	return ExitSuccess
}

// reportNoSuchRecord names an unresolvable identifier in its own vocabulary,
// for the one case resolveRecord cannot cover: a machine with no archive file
// at all, where there is nothing to query and every identifier resolves to
// nothing. A path never reaches it -- requireIdentifierShape has refused it.
func reportNoSuchRecord(target string) {
	if classifyIdentifier(target) == identifierUUID {
		fmt.Fprintf(os.Stderr, "error: no record with UUID %s\n", target)
		return
	}
	fmt.Fprintf(os.Stderr, "error: no record with ID %s\n", target)
}

// classifyIdentifier decides which of the three kinds a target string is.
func classifyIdentifier(target string) identifierKind {
	switch {
	case uuidShaped.MatchString(target):
		return identifierUUID
	case digitsShaped.MatchString(target):
		return identifierID
	default:
		return identifierPath
	}
}

// resolveRecord turns one identifier argument into the record it names,
// reporting its own failures on stderr and returning the exit code they
// deserve. A returned code of ExitSuccess means the record is usable.
//
// allowPath is what separates the verbs: `undelete` restores by original path,
// so a path is a legitimate way to name its target; `info` and `purge` take
// identifiers only, and a path handed to them is a usage error naming the two
// forms they do accept.
func resolveRecord(database *db.DB, target string, allowPath bool) (*db.DeletionRecord, int) {
	switch classifyIdentifier(target) {
	case identifierUUID:
		rec, err := database.QueryByUUID(target)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				fmt.Fprintf(os.Stderr, "error: no record with UUID %s\n", target)
				return nil, ExitFileNotFound
			}
			fmt.Fprintf(os.Stderr, "error: querying database: %s\n", err)
			return nil, dbExit(err)
		}
		return rec, ExitSuccess

	case identifierID:
		// The shape check has already established that every character is a
		// digit, so the only parse failure left is a number too large for the
		// column -- which is a value no id was ever issued for.
		id, parseErr := strconv.ParseInt(target, 10, 64)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "error: no record with ID %s\n", target)
			return nil, ExitFileNotFound
		}
		rec, err := database.QueryByID(id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				fmt.Fprintf(os.Stderr, "error: no record with ID %d\n", id)
				return nil, ExitFileNotFound
			}
			fmt.Fprintf(os.Stderr, "error: querying database: %s\n", err)
			return nil, dbExit(err)
		}
		return rec, ExitSuccess

	default:
		if !allowPath {
			return nil, requireIdentifierShape(target)
		}
		records, err := database.QueryByPath(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: querying database: %s\n", err)
			return nil, dbExit(err)
		}
		if len(records) == 0 {
			fmt.Fprintf(os.Stderr, "error: no archived record found for path %q\n", target)
			return nil, ExitFileNotFound
		}
		if len(records) > 1 {
			fmt.Fprintf(os.Stderr, "Multiple matches found:\n")
			fmt.Fprintf(os.Stderr, "  %-6s %-36s %-40s %-10s %s\n", "ID", "UUID", "Path", "Size", "Deleted")
			for _, r := range records {
				fmt.Fprintf(os.Stderr, "  %-6d %-36s %-40s %-10s %s\n",
					r.ID, r.UUID, r.OriginalPath, humanSize(r.Size), humanAge(r.DeletedAt))
			}
			fmt.Fprintf(os.Stderr, "\nUse saferm undelete <id> or saferm undelete <uuid> to specify.\n")
			return nil, ExitUsage
		}
		return records[0], ExitSuccess
	}
}
