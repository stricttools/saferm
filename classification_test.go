package main

import (
	"sort"
	"strings"
	"testing"

	"github.com/smm-h/strictcli/go/strictcli"
	"github.com/stricttools/testisolation/go/hygiene"
)

// classification pins every command's strictcli effect classification and its
// `consequential` declaration. The table is the specification: changing a row
// here is the deliberate edit that a reclassification requires, and registering
// a command without adding its row fails the test.
//
// Reasoning, per the strictcli effects contract (§1: `read_only` means no
// user-visible or consequential mutation; §8.1: `consequential` means the act
// is worth interrupting someone for):
//
//   - delete -- mutating, NOT consequential. It moves a file out of the working
//     tree, which is a real mutation, but the file lands in the archive and
//     `undelete` brings it back. Recoverable by construction, so it runs bare:
//     `saferm delete --description "why" <files>` is the complete invocation
//     from a script or an agent.
//   - undelete -- mutating, NOT consequential. It is delete's inverse. It
//     writes to the restoration path but refuses to clobber an existing file
//     without an explicit --on-conflict overwrite, so nothing is lost by
//     running it.
//   - list -- read_only. It queries the database and prints a table. A
//     read_only command cannot be consequential at all.
//   - purge -- mutating AND consequential. The one saferm operation with no way
//     back: it destroys the archived content, and after it nothing in the tool
//     can recover the file. That is saferm's whole purpose inverted, which is
//     precisely what the confirm protocol exists to interrupt.
//   - info -- read_only. It reads one record and prints its metadata.
//   - usage -- read_only. It reads the database and stats the archive's
//     entries to report how much disk the archive takes; it changes nothing.
//   - reclassify-records -- mutating, NOT consequential. It rewrites archive
//     entries that older saferm versions left in a form their records
//     contradict, and updates those records; the content each entry holds is
//     carried into the new entry before the old one goes, so nothing is lost.
//   - capabilities -- read_only. It reads nothing at all: the answer is a
//     declaration compiled into the binary. It is the probe a program runs
//     before it has decided to use saferm, so read_only is not merely true of
//     it -- the framework's own enforcement is what guarantees a probe cannot
//     create saferm's state directory on a machine that never ran saferm.
//
// The `config` group is registered by the framework (WithConfig), not by
// saferm, and its five commands are pinned here for the same reason as the
// rest: they are part of the CLI surface saferm ships, and a framework upgrade
// that reclassifies one of them changes what saferm's users get.
var classification = map[string]struct {
	effect        string
	consequential bool
}{
	"delete":             {strictcli.EffectMutating, false},
	"undelete":           {strictcli.EffectMutating, false},
	"list":               {strictcli.EffectReadOnly, false},
	"purge":              {strictcli.EffectMutating, true},
	"info":               {strictcli.EffectReadOnly, false},
	"usage":              {strictcli.EffectReadOnly, false},
	"capabilities":       {strictcli.EffectReadOnly, false},
	"reclassify-records": {strictcli.EffectMutating, false},
	"config.show":        {strictcli.EffectReadOnly, false},
	"config.set":         {strictcli.EffectMutating, false},
	"config.path":        {strictcli.EffectReadOnly, false},
	"config.edit":        {strictcli.EffectMutating, false},
	"config.init":        {strictcli.EffectMutating, false},
}

// collectCommands flattens the app's command tree into dotted paths.
func collectCommands(app *strictcli.App) map[string]*strictcli.Command {
	out := map[string]*strictcli.Command{}
	for name, cmd := range app.Commands() {
		out[name] = cmd
	}
	var walk func(prefix string, g *strictcli.Group)
	walk = func(prefix string, g *strictcli.Group) {
		for name, cmd := range g.Commands {
			out[prefix+name] = cmd
		}
		for name, sub := range g.Groups {
			walk(prefix+name+".", sub)
		}
	}
	for name, g := range app.Groups() {
		walk(name+".", g)
	}
	return out
}

func TestCommandClassificationIsPinned(t *testing.T) {
	hygiene.Isolate(t, hygiene.Preserve(hygiene.GoPath, hygiene.GoModCache, hygiene.GoCache))

	cmds := collectCommands(newApp())

	var names []string
	for name := range cmds {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		want, ok := classification[name]
		if !ok {
			t.Errorf("command %q is registered but has no pinned classification; add a row to classification with the reasoning", name)
			continue
		}
		if cmds[name].Effect != want.effect {
			t.Errorf("command %q: effect = %q, pinned %q", name, cmds[name].Effect, want.effect)
		}
		if cmds[name].Consequential != want.consequential {
			t.Errorf("command %q: consequential = %v, pinned %v", name, cmds[name].Consequential, want.consequential)
		}
	}
	for name := range classification {
		if _, ok := cmds[name]; !ok {
			t.Errorf("classification pins %q but no such command is registered", name)
		}
	}
}

// machineSurface pins which of saferm's own commands answer a machine, and
// therefore what a consumer may parse. Membership IS the surface: a command
// with a declared payload schema publishes that schema through --dump-schema
// and through the MCP tool descriptor, and one without it answers --json with a
// null payload.
//
//   - delete, undelete, list, info -- the four consumer verbs. Each declares a
//     payload schema and supplies its value unconditionally.
//   - usage -- the disk-use report, whose figures a program reads as bytes
//     rather than as the table's rounded sizes.
//   - capabilities -- the probe itself, which is only useful to a machine.
//   - reclassify-records -- the records it changed (or under --dry-run would),
//     so a program can tell which of its handles now name another kind.
//   - purge -- deliberately OUTSIDE. It is the one irreversible operation and
//     the one that asks for consent; nothing should be driving it from a
//     parsed document, and a payload would be the first step toward something
//     that does.
//
// The framework's own `config` commands are not saferm's to declare and are
// excluded from this pin -- `config show` carries a framework-owned schema.
var machineSurface = map[string]bool{
	"delete":             true,
	"undelete":           true,
	"list":               true,
	"info":               true,
	"usage":              true,
	"capabilities":       true,
	"reclassify-records": true,
	"purge":              false,
}

func TestMachineSurfaceMembershipIsPinned(t *testing.T) {
	hygiene.Isolate(t, hygiene.Preserve(hygiene.GoPath, hygiene.GoModCache, hygiene.GoCache))

	cmds := collectCommands(newApp())
	for name, wantSchema := range machineSurface {
		cmd, ok := cmds[name]
		if !ok {
			t.Errorf("the machine surface pins %q but no such command is registered", name)
			continue
		}
		if got := cmd.PayloadSchema != nil; got != wantSchema {
			t.Errorf("command %q declares a payload schema = %v, pinned %v", name, got, wantSchema)
		}
	}
	for name := range cmds {
		if _, pinned := machineSurface[name]; pinned || strings.HasPrefix(name, "config.") {
			continue
		}
		t.Errorf("command %q is registered but the machine surface does not say whether it answers a machine", name)
	}
}

// TestEveryCommandSupportsDryRun pins the other half of the effects
// declaration. saferm previews every command honestly: `delete`, `undelete` and
// `purge` mint their mutations on the effects handle so a dry run renders a
// would-do log naming each path, and `purge --dry-run` additionally renders the
// table of what it would destroy. Nothing here is unrepresentable ahead of
// time, so no command declares WithDryRunUnsupported -- and declaring it would
// be illegal on the two read_only commands anyway.
func TestEveryCommandSupportsDryRun(t *testing.T) {
	hygiene.Isolate(t, hygiene.Preserve(hygiene.GoPath, hygiene.GoModCache, hygiene.GoCache))

	for name, cmd := range collectCommands(newApp()) {
		if !cmd.DryRunSupported {
			t.Errorf("command %q declares --dry-run unsupported; saferm previews everything", name)
		}
	}
}

// TestNoReservedGlobalFlagNames guards the framework's reserved quartet at the
// app level. Command-level flags are covered implicitly: strictcli panics at
// registration for a reserved name anywhere, and newApp() registers everything.
func TestNoReservedGlobalFlagNames(t *testing.T) {
	hygiene.Isolate(t, hygiene.Preserve(hygiene.GoPath, hygiene.GoModCache, hygiene.GoCache))

	reserved := map[string]bool{
		"dry-run":               true,
		"approve-consequential": true,
		"quiet":                 true,
		"verbose":               true,
	}
	for _, f := range newApp().GlobalFlags() {
		if reserved[f.Name] {
			t.Errorf("global flag %q is reserved by the framework", f.Name)
		}
	}
}
