package config

import (
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/help/builtin"
	"github.com/denn-gubsky/loomcycle/internal/promptinject"
)

// ValidateHelpPlaceholders refuses a prompt text whose {{help:<topic>}}
// placeholders (RFC GL P4) cannot render as written: a malformed placeholder,
// a topic the help corpus embedded in this binary does not hold, a topic with
// no call examples, or more examples in total than promptinject.HelpMaxBytes
// allows. Each message names the placeholder; the caller says which text.
//
// It runs where a prompt is AUTHORED — config load for static agents, AgentDef
// create/fork, register_agent, and a team's system_prompt and input
// templates — so the set of topics a prompt may name is the binary's own,
// closed like the {{tool:…}} allowlist, and an accepted text renders whole.
// Never where a stored definition is READ: a topic a later binary dropped then
// renders nothing rather than making the definition unusable.
//
// The budget is per text, because each text is expanded in its own pass with
// its own budget: the agent's system prompt, and separately a team node's
// system and input templates. Each is authored on its own surface, so each is
// judged alone. Rendering here is the same pure read of embedded files prompt
// assembly does; it reads no store.
func ValidateHelpPlaceholders(prompt string) error {
	if bad := promptinject.MalformedHelpRefs(prompt); len(bad) > 0 {
		return fmt.Errorf("{{help:%s}} is not a help topic name "+
			"(want <topic> or <Tool>/<op> as the help index names it, e.g. {{help:Document/search}})", bad[0])
	}
	topics := promptinject.HelpTopics(prompt)
	if len(topics) == 0 {
		return nil
	}
	size := map[string]int{}
	total := 0
	for _, topic := range topics {
		n, seen := size[topic]
		if !seen {
			body, err := builtin.RenderExamples(topic)
			switch {
			case errors.Is(err, builtin.ErrUnknownTopic):
				return fmt.Errorf("{{help:%s}} names no help topic built into this server; only built-in "+
					"topics can be rendered, so one added through the operator's LOOMCYCLE_HELP_ROOT cannot "+
					"(an operation is <Tool>/<op>, e.g. Document/search)", topic)
			case errors.Is(err, builtin.ErrNoExamples):
				return fmt.Errorf("{{help:%s}} names a help topic with no call examples to render", topic)
			case err != nil:
				return fmt.Errorf("{{help:%s}}: %w", topic, err)
			}
			n = len(body)
			size[topic] = n
		}
		total += n
	}
	if total > promptinject.HelpMaxBytes {
		return fmt.Errorf("{{help:...}} placeholders render %d bytes of examples, over the %d-byte budget for one prompt text; name fewer topics",
			total, promptinject.HelpMaxBytes)
	}
	return nil
}
