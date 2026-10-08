package builtin

import (
	"errors"
	"reflect"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// localBodyKeys types the bodies a team declares under `local`, for the key
// check: teamgraph holds them as raw JSON because their types live here. An
// agent's body is the overlay AgentDef takes, whose decoder drops a key it
// does not know, so an unknown key is reported. A channel's and a webhook's
// are decoded strictly already, each naming what it refuses and why, so only
// what those decoders let through is reported: a repeated key, and a key
// matched by ignoring its case.
var localBodyKeys = teamgraph.LocalBodyKeys{
	"local.agents":   {Type: reflect.TypeOf(mergedDef{})},
	"local.channels": {Type: reflect.TypeOf(connector.ChannelCreateRequest{}), UnknownRefusedElsewhere: true},
	"local.webhooks": {Type: reflect.TypeOf(localWebhookBody{}), UnknownRefusedElsewhere: true},
}

// overlayKeysError is buildDefinition's refusal of an overlay whose keys the
// decoder would not read as written. It carries every such key, for verify to
// report each at its path; as an error it is the first, which is what a
// create or a fork refuses with.
type overlayKeysError struct{ issues []*teamgraph.Issue }

func (e *overlayKeysError) Error() string { return e.issues[0].Msg }

// overlayIssues turns a buildDefinition error into the issues verify reports:
// one per offending key when the overlay's keys were refused, else the one
// refusal with no path.
func overlayIssues(err error) []teamIssue {
	var keys *overlayKeysError
	if errors.As(err, &keys) {
		return graphIssues(keys.issues)
	}
	return []teamIssue{refused(teamIssueOverlayInvalid, "", err.Error())}
}
