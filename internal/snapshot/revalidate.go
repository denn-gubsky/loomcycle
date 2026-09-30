package snapshot

import (
	"encoding/json"
	"fmt"
)

// Re-validation of the def sections snapshots carried before restore-side
// validation existed: agent, skill, team, hook, MCP server and channel defs.
//
// A restore bypasses the tools that author these rows, so without it a
// snapshot written somewhere else — or edited by hand — could land a
// definition this host would never let an author create: an MCP server on a
// host outside this host's allowlist, a hook whose headers smuggle a line
// break, a body that does not even decode. Each row therefore goes through the
// authoring validator the call site injects (RestoreOptions.Validators). A row
// it refuses is not written; the rest of the section lands, and the pointer
// pass refuses an active pointer at the refused def, since it is not here.
//
// These sections differ from the ones added with restore-side validation in
// one way, by the owner's decision: with no validator wired they restore as
// they always have, plus one section-level warning. A library caller of
// Restore that wires no validators must not suddenly lose definitions it used
// to get back. The newer sections (validRestoredBody) skip instead: they were
// never restored raw. Both production call sites wire every validator.

// existingSectionValidator returns section's injected validator. When none is
// wired and the section carries rows, one warning says the section was
// restored without re-validation.
func existingSectionValidator(opts RestoreOptions, section string, rows int, result *RestoreResult) func(json.RawMessage) error {
	validate := opts.Validators[section]
	if validate == nil && rows > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%s: %d row(s) restored without re-validation: no %s validator is wired on this restore", section, rows, section))
	}
	return validate
}

// refusedByValidation reports whether validate refuses the body of the row
// named where. A refused row is counted in refused and named in a warning,
// with the validator's reason; a nil validate refuses nothing.
func refusedByValidation(validate func(json.RawMessage) error, where string, body json.RawMessage, refused *int, result *RestoreResult) bool {
	if validate == nil {
		return false
	}
	if err := validate(body); err != nil {
		*refused++
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%s: not restored: it fails the validation an author would face on this host: %v", where, err))
		return true
	}
	return false
}
