package builtin

// history_content_search.go — RFC CI P2: search your chats by what was SAID.
//
// `History op=search` matched on the TITLE and nothing else, which made finding a
// past conversation depend on how well the auto-titler happened to name it. The turns
// themselves were retained in full and indexed nowhere. P1 built the index; this is
// the surface that makes it useful to a person rather than to a query for
// `sources: ["traces"]`.
//
// It is the same op with a `match` selector rather than a new one, because "find the
// chat where we discussed X" is one question a user has, not two — and a separate op
// would make the cheap title path something they have to know to prefer.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// contentMatchCandidates caps the trace rows one content search considers.
//
// Generous relative to the chat limit because hits CLUSTER: a conversation about one
// topic contributes many matching turns, and collapsing them to chats costs rows. A
// cap that matched the chat limit would return one chat for a search that should have
// returned five.
const contentMatchCandidates = 60

// chatContentMatch is the evidence for why a chat came back — the turn that matched.
//
// It exists because a content search whose results look exactly like a title search
// gives a reader no way to tell WHY a chat is in the list, and no way to judge whether
// the search understood them.
type chatContentMatch struct {
	SessionID string  `json:"session_id"`
	Text      string  `json:"text"`
	Speaker   string  `json:"speaker"`
	Score     float64 `json:"score"`
}

// searchContent finds chats by the words in their turns.
//
// THE INDEX IS PER-USER BY CONSTRUCTION, and that is what makes this safe to run
// before any authorization check: a trace row lives at (tenant, user, the caller's own
// user id), resolved server-side from the run identity, so a caller can only ever
// search their own turns. Each candidate chat is then loaded through
// loadSessionInScope — the SAME gate `get` uses — rather than by a check written here,
// because a second implementation of "may this caller see this chat" is a second
// chance to get it wrong.
func (h *History) searchContent(ctx context.Context, scope string, in historyInput) (tools.Result, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return errResult("history: search requires a non-empty query"), nil
	}
	if h.Embedder == nil {
		return errResult("history: match=content needs an embedder — none is configured, so " +
			"only the title match is available (memory.embedder in operator config)"), nil
	}
	ident := tools.RunIdentity(ctx)
	if ident.UserID == "" {
		// Not a refusal of the feature so much as of the request: turns are indexed
		// under the person who typed them, so a run with no user has no turns of its
		// own to search.
		return errResult("history: match=content searches the turns YOU typed, and this run " +
			"carries no user_id — use the default title match, or start the run with a user"), nil
	}

	vecs, err := h.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return errResult("history: search: embed: " + err.Error()), nil
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return errResult("history: search: the embedder returned an empty vector"), nil
	}

	hits, err := h.Store.MemoryEmbedSearch(ctx, ident.TenantID, store.MemoryScopeUser, ident.UserID,
		store.MemorySearchFilter{KeyPrefix: store.TraceTurnKeyPrefix}, vecs[0], contentMatchCandidates)
	if err != nil {
		return errResult("history: search: " + err.Error()), nil
	}

	// Collapse turns to chats, keeping the BEST turn per chat as the evidence. A
	// conversation about one topic matches many times, and a result list that repeated
	// the same chat five times would bury the other four answers.
	best := map[string]chatContentMatch{}
	var order []string
	for _, hit := range hits {
		var turn traceTurnRow
		if json.Unmarshal(hit.Value, &turn) != nil || turn.SessionID == "" {
			continue
		}
		if prev, seen := best[turn.SessionID]; seen {
			if hit.Score > prev.Score {
				best[turn.SessionID] = chatContentMatch{
					SessionID: turn.SessionID, Text: turn.Text, Speaker: turn.Speaker, Score: hit.Score}
			}
			continue
		}
		best[turn.SessionID] = chatContentMatch{
			SessionID: turn.SessionID, Text: turn.Text, Speaker: turn.Speaker, Score: hit.Score}
		order = append(order, turn.SessionID)
	}
	sort.SliceStable(order, func(i, j int) bool { return best[order[i]].Score > best[order[j]].Score })

	limit := effectiveListLimit(in.Limit)
	chats := make([]any, 0, limit)
	matches := make([]chatContentMatch, 0, limit)
	for _, sid := range order {
		if len(chats) >= limit {
			break
		}
		// The authorization gate, and a chat this caller may not see is SKIPPED rather
		// than reported: telling them a chat exists but is not theirs is the disclosure
		// the opaque-404 convention exists to avoid. It should not happen — the index
		// is their own — so it is a belt-and-braces skip, not an expected branch.
		sess, err := h.loadSessionInScope(ctx, scope, "search", sid)
		if err != nil {
			continue
		}
		runs, rerr := h.Store.RunsForSession(ctx, sess.ID)
		if rerr != nil {
			continue
		}
		chats = append(chats, sessionMeta(sess, runs))
		matches = append(matches, best[sid])
	}

	return okJSON(map[string]any{
		"scope": scope,
		"match": contentMatchMode,
		"chats": chats,
		// Index-aligned with `chats`: a hit nobody can attribute is a hit nobody can
		// use, so each chat is returned WITH the turn that put it in the list.
		"matched_turns": matches,
		"total":         len(chats),
		"limit":         limit,
	})
}

// traceTurnRow reads back what the indexer stored. Kept here rather than shared with
// the writer: the two live in different packages, and a shared struct would make the
// stored shape part of a wider contract than it needs to be. Fields are matched by
// their JSON names, which is what the writer actually commits to.
type traceTurnRow struct {
	Text      string `json:"text"`
	Speaker   string `json:"speaker"`
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
}

const (
	titleMatchMode   = "title"
	contentMatchMode = "content"
)

// validMatchMode reports whether a `match` value is one this build knows.
//
// REFUSED RATHER THAN DEFAULTED, unlike most optional fields here. A caller asking for
// `content` against a runtime that predates it would otherwise get a title search
// labelled as what they asked for — the same silent-downgrade shape that made a
// dropped `sources` value look like it worked.
func validMatchMode(mode string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", titleMatchMode, contentMatchMode:
		return nil
	default:
		return fmt.Errorf("history: unknown match %q (want %q or %q)", mode, titleMatchMode, contentMatchMode)
	}
}
