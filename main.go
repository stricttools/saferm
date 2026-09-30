// Command saferm: The `rm` replacement that is safe to hand to your AI Agents: deleted files can be restored, know exactly which session deleted a file, when, and why, with the reason being a required flag.
//
// Every archived path is recorded in a SQLite database alongside the reason
// the caller gave for the deletion, the environment it ran in, the git state
// of the working tree and the process that invoked it. The archive is read
// back with list, info and undelete, and emptied with purge.
package main

import (
	"github.com/smm-h/strictcli/go/strictcli"
)

// newApp builds the fully-registered saferm app. It is separate from main so
// tests can construct the same app and assert over its registration (see
// classification_test.go); main does nothing but run it.
func newApp() *strictcli.App {
	app := strictcli.NewApp("saferm", Version, "The `rm` replacement that is safe to hand to your AI Agents: deleted files can be restored, know exactly which session deleted a file, when, and why, with the reason being a required flag",
		strictcli.WithEnvPrefix("SAFERM"),
		strictcli.WithInfraRoot("SAFERM_HOME", "~/.saferm"),
		strictcli.WithConfig(),
		strictcli.WithConfigPathRelativeToRoot("SAFERM_HOME", "config.toml"),
		strictcli.WithConfigFormat("toml"),
	)

	// --verbose, --quiet, --dry-run and --approve-consequential are owned by
	// the CLI framework: they are pre-scanned out of argv wherever they appear
	// and delivered on the Context, so registering them here is a hard error.
	app.GlobalFlag(strictcli.StringFlag("archive-dir", "Path to the archive directory",
		strictcli.Default(strictcli.RelativeToRoot("SAFERM_HOME", "archive")),
		strictcli.ConflictMode("error")))
	app.GlobalFlag(strictcli.StringFlag("db-path", "Path to the SQLite database",
		strictcli.Default(strictcli.RelativeToRoot("SAFERM_HOME", "db", "saferm.db")),
		strictcli.ConflictMode("error")))
	// exclude-env-patterns is intentionally left at cli-wins (no ConflictMode) and
	// gets no ConfigField: it is a list, and ConfigField supports only scalars.
	app.GlobalFlag(strictcli.StringFlag("exclude-env-patterns", "Regex patterns for env vars to exclude from metadata",
		strictcli.Repeatable(), strictcli.Unique(true),
		strictcli.Default([]interface{}{"(?i)token", "(?i)secret", "(?i)password", "(?i)key", "(?i)credential"})))

	// ConfigFields for the two scalar config-managed flags. Each names an existing
	// flag param, so it renders once (as a "-- help" annotation on the flag line in
	// `config show`) and feeds unknown-key validation. The flags carry defaults, so
	// these fields declare NO default (one-absent-OK), avoiding duplication of the
	// runtime-computed SAFERM_HOME-derived paths. The help text records the
	// infrastructure-vs-config boundary.
	app.ConfigField("archive_dir", strictcli.ConfigFieldHelp(
		"Directory where deleted files are archived. Default derives from SAFERM_HOME "+
			"(an infrastructure override: unlike config values, SAFERM_HOME is NOT suppressed by --hermetic)."))
	app.ConfigField("db_path", strictcli.ConfigFieldHelp(
		"Path to the SQLite database. Default derives from SAFERM_HOME "+
			"(an infrastructure override: unlike config values, SAFERM_HOME is NOT suppressed by --hermetic)."))

	registerDeleteCmd(app)
	registerUndeleteCmd(app)
	registerListCmd(app)
	registerPurgeCmd(app)
	registerInfoCmd(app)
	registerUsageCmd(app)
	registerCapabilitiesCmd(app)

	return app
}

func main() {
	newApp().Run()
}
