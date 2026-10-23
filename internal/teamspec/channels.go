package teamspec

import "strings"

// ChannelPrefix is the reserved prefix a team's own channels are stored
// under: local channel n of team T keeps its messages and cursors as
// "_team/T/n". Nothing else may declare a channel under it or address one by
// that spelling — the operator's config, runtime ChannelDefs and the Channel
// tool all refuse it — so the one way to such a channel is from inside the
// team, and deleting the team can drop everything under its prefix.
const ChannelPrefix = "_team/"

// IsChannelName reports whether name is under the reserved team prefix.
func IsChannelName(name string) bool {
	return strings.HasPrefix(name, ChannelPrefix)
}
