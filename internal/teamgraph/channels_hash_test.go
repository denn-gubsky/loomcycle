package teamgraph

import "testing"

// Definition.Channels is AUTHORITY, so unlike Colors and Layout it must be
// inside the content hash: a verify against a recorded hash that passed while
// the workflow had quietly gained a channel would be worse than no verify.
func TestSign_ChannelsAreContent(t *testing.T) {
	base := Definition{
		Entry:       "a",
		States:      []State{{ID: "a", Handler: Handler{Kind: HandlerTerminal}}},
		Transitions: []Transition{},
	}
	bare := Sign("t", base)

	withACL := base
	withACL.Channels = &TeamChannels{Subscribe: []string{"pr-events"}}
	if Sign("t", withACL) == bare {
		t.Errorf("adding a channel ACL did not change the hash — authority must be content")
	}

	wider := base
	wider.Channels = &TeamChannels{Subscribe: []string{"pr-events", "secrets"}}
	if Sign("t", wider) == Sign("t", withACL) {
		t.Errorf("widening the ACL did not change the hash")
	}

	// And the presentation fields still do NOT change it.
	withLayout := base
	withLayout.Layout = &Layout{Nodes: map[string]NodePos{"a": {X: 1, Y: 2}}}
	if Sign("t", withLayout) != bare {
		t.Errorf("layout changed the hash; it is presentation")
	}
}

// A definition that omits Channels must hash byte-identically to one written
// before the field existed, or every recorded content_sha256 in every
// deployment becomes wrong on upgrade. The field is last and omitempty for
// exactly this reason; this pins it.
func TestSign_OmittedChannelsKeepsTheRecordedHash(t *testing.T) {
	// The SAME definition the pre-Channels byte-stability test pins, so this
	// fails the moment Channels stops being omitempty-last.
	d := Definition{Entry: "a", States: []State{{ID: "a", Handler: Handler{Kind: HandlerTerminal}}}}

	const before = "sha256:4db3ad92f133dda9bd89bcf2d6dc59daa1039e8de15ed16e2d4e60924506a0a0"
	if got := Sign("t", d); got != before {
		t.Errorf("hash of a Channels-less definition = %s, want the pre-Channels %s — "+
			"every recorded hash in every deployment depends on this", got, before)
	}
}
