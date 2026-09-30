package main

import (
	"strings"

	"github.com/smm-h/strictcli/go/strictcli"
)

// The features saferm ships, named one by one.
//
// This is the negotiation surface: a program that drives saferm asks what this
// binary can do, and treats a missing feature exactly as it treats saferm not
// being installed at all -- one code path for both. Names, never numbers: a
// locally built saferm reports a Go pseudo-version no semver parser accepts, so
// a version comparison rejects a perfectly good install, and a released version
// says nothing about what a build actually carries.
//
// Adding a name is how a new capability becomes negotiable. Removing one is a
// breaking change to every caller that asks for it, so a feature is named here
// only once it is shipped and covered.
const (
	// featureGitIndexSwitches: both halves of the round trip can be told to
	// leave the git index alone -- `delete --no-update-git-index` and
	// `undelete --no-update-git-index`. A programmatic caller archiving someone
	// else's tree wants no index side effects at all.
	featureGitIndexSwitches = "git-index-switches"

	// featureGroupID: every delete invocation stamps one group identifier on
	// every record it writes, so a batch stays recoverable as a batch. It is on
	// `delete`'s payload and on `info`'s.
	featureGroupID = "group-id"

	// featureMachinePayloads: `delete`, `undelete`, `list` and `info` each
	// declare a payload schema and answer under --json with the framework's
	// envelope carrying it. `purge` deliberately does not.
	featureMachinePayloads = "machine-payloads"

	// featureOnConflictModes: `undelete --on-conflict overwrite|abort`, required
	// exactly when the destination is occupied, with no default.
	featureOnConflictModes = "on-conflict-modes"

	// featureOnErrorModes: `delete --on-error abort|continue`, mandatory, with
	// no default.
	featureOnErrorModes = "on-error-modes"

	// featureRestoreDestination: `undelete --destination <path>` restores
	// somewhere other than the recorded original, and writes where the content
	// went to the record.
	featureRestoreDestination = "restore-destination"

	// featureTraceOrigin: which tool ran a deletion is derived from the
	// strictcli process trace store and recorded on the row, and `info`'s
	// payload carries it. There is no flag that declares an origin.
	featureTraceOrigin = "trace-origin"

	// featureUsageReport: `usage` reports what the archive and its database
	// occupy on disk, broken down by age and by original directory, and
	// answers --json with the figures in bytes.
	featureUsageReport = "usage-report"

	// featureUUIDHandles: every record has a uuid that `info`, `undelete` and
	// `purge` all accept, and `delete` hands back. It is the handle that
	// survives -- the numeric id is one database's counter.
	featureUUIDHandles = "uuid-handles"
)

// features is the list the verb answers with, in a fixed order so two runs of
// the same binary produce the same document.
var features = []string{
	featureGitIndexSwitches,
	featureGroupID,
	featureMachinePayloads,
	featureOnConflictModes,
	featureOnErrorModes,
	featureRestoreDestination,
	featureTraceOrigin,
	featureUsageReport,
	featureUUIDHandles,
}

// capabilitiesPayloadSchema declares the probe's answer: the feature names, and
// nothing else. No version member: putting one here would invite exactly the
// comparison this verb exists to replace.
var capabilitiesPayloadSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"features": map[string]interface{}{
			"type":  "array",
			"items": map[string]interface{}{"type": "string"},
		},
	},
	"required":             []interface{}{"features"},
	"additionalProperties": false,
}

// capabilitiesPayload is the answer's shape.
type capabilitiesPayload struct {
	Features []string `json:"features"`
}

func registerCapabilitiesCmd(app *strictcli.App) {
	app.Command("capabilities", "Name the features this saferm ships, for a program deciding how to drive it", handleCapabilities,
		strictcli.WithEffect(strictcli.EffectReadOnly),
		strictcli.PayloadSchema(capabilitiesPayloadSchema),
	)
}

// handleCapabilities answers from the declaration above and reads nothing: no
// database, no archive directory, no configuration that could make the answer
// depend on the machine. A consumer probes before it has decided to use saferm
// at all, so the probe must work where saferm has never run -- and must not
// create saferm's state directory in order to answer.
func handleCapabilities(ctx *strictcli.Context, kwargs map[string]interface{}) strictcli.Outcome {
	ctx.Payload(capabilitiesPayload{Features: features})
	emit(ctx, "%s\n", strings.Join(features, "\n"))
	return strictcli.Exit(ExitSuccess)
}
