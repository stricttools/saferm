package meta

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/stricttools/saferm/internal/trace"
)

// Metadata holds contextual information captured at deletion time.
type Metadata struct {
	Env       map[string]string `json:"env,omitempty"`
	GitBranch string            `json:"git_branch,omitempty"`
	GitHEAD   string            `json:"git_head,omitempty"`
	GitRoot   string            `json:"git_root,omitempty"`
	PPID      int               `json:"ppid,omitempty"`
	ParentCmd string            `json:"parent_cmd,omitempty"`
	Custom    map[string]string `json:"custom,omitempty"`

	// Trace is the process ancestry resolved from the strictcli trace store at
	// capture time: the flattened chain of invocations that led to this
	// deletion, their identifiers, and anything malformed the resolution met.
	// Nil when nothing claimed the invocation, which is not an anomaly.
	//
	// The chain is embedded rather than referenced so the record stays
	// self-contained: pruning the store cannot orphan it.
	Trace *trace.Capture `json:"trace,omitempty"`
}

// Collect gathers metadata from the current environment.
//
// Best-effort per collector -- git context, parent-process details and the
// process ancestry read from the trace store degrade to empty values, or to a
// recorded anomaly, rather than failing. The one thing it refuses to work around
// is an exclude pattern that does not compile: that is the caller asking for a
// redaction, and continuing without it would write the very variables the
// caller meant to keep out. See collectEnv.
func Collect(excludePatterns []string, customMeta map[string]string) (*Metadata, error) {
	m := &Metadata{}

	var err error
	m.Env, err = collectEnv(excludePatterns)
	if err != nil {
		return nil, err
	}
	m.GitBranch, m.GitHEAD, m.GitRoot = collectGitContext()
	m.PPID, m.ParentCmd = collectParentProcess()
	m.Trace = trace.Collect()

	if len(customMeta) > 0 {
		m.Custom = make(map[string]string, len(customMeta))
		for k, v := range customMeta {
			m.Custom[k] = v
		}
	}

	return m, nil
}

// collectEnv reads all environment variables and filters out those whose
// names match any of the exclude patterns.
//
// A pattern that does not compile is a hard error, reported before a single
// variable is read. These patterns are the redaction control: skipping a broken
// one leaves the caller believing a class of variables is being kept out of the
// archive while it is being written into it. Go's regexp is RE2, so the common
// way to reach this is a pattern borrowed from a lookahead-capable flavour.
func collectEnv(excludePatterns []string) (map[string]string, error) {
	compiled := make([]*regexp.Regexp, 0, len(excludePatterns))
	for _, pat := range excludePatterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("exclude-env-patterns entry %q does not compile: %w "+
				"(Go uses RE2, which has no lookahead or backreferences)", pat, err)
		}
		compiled = append(compiled, re)
	}

	env := os.Environ()
	result := make(map[string]string, len(env))
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		excluded := false
		for _, re := range compiled {
			if re.MatchString(name) {
				excluded = true
				break
			}
		}
		if !excluded {
			result[name] = value
		}
	}
	return result, nil
}

// collectGitContext retrieves git branch, HEAD SHA, and repo root.
// Returns empty strings if not in a git repository.
func collectGitContext() (branch, head, root string) {
	branch = runGitCmd("rev-parse", "--abbrev-ref", "HEAD")
	head = runGitCmd("rev-parse", "HEAD")
	root = runGitCmd("rev-parse", "--show-toplevel")
	return
}

// runGitCmd executes a git command and returns trimmed stdout, or empty string on failure.
func runGitCmd(args ...string) string {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// collectParentProcess returns the PPID and the parent's command line.
func collectParentProcess() (ppid int, cmdline string) {
	ppid = os.Getppid()
	cmdline = readParentCmdline(ppid)
	return
}
