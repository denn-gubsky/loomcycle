package config

import (
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/help/builtin"
	"github.com/denn-gubsky/loomcycle/internal/promptinject"
)

// ValidateHelpPlaceholders refuses a system prompt whose {{help:<topic>}}
// placeholders (RFC GL P4) cannot render exactly as written: a malformed
// placeholder, a topic the help corpus embedded in this binary does not hold,
// a topic with no call examples, or more examples in total than
// promptinject.HelpMaxBytes allows. Each message names the placeholder.
//
// Config load runs it over every static agent, and AgentDef create/fork over a
// runtime agent's prompt, so the set of topics a prompt may name is the
// binary's own — closed, like the {{tool:…}} allowlist — and an accepted
// prompt renders whole at every run start. Rendering here is the same pure
// read of embedded files prompt assembly does; it reads no store.
func ValidateHelpPlaceholders(prompt string) error {
	if bad := promptinject.MalformedHelpRefs(prompt); len(bad) > 0 {
		return fmt.Errorf("{{help:%s}} in system prompt is not a help topic name "+
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
				return fmt.Errorf("{{help:%s}} in system prompt names no help topic built into this server "+
					"(Context op=help lists them; an operation is <Tool>/<op>, e.g. Document/search)", topic)
			case errors.Is(err, builtin.ErrNoExamples):
				return fmt.Errorf("{{help:%s}} in system prompt names a help topic with no call examples to render", topic)
			case err != nil:
				return fmt.Errorf("{{help:%s}} in system prompt: %w", topic, err)
			}
			n = len(body)
			size[topic] = n
		}
		total += n
	}
	if total > promptinject.HelpMaxBytes {
		return fmt.Errorf("{{help:...}} placeholders in system prompt render %d bytes of examples, over the %d-byte budget; name fewer topics",
			total, promptinject.HelpMaxBytes)
	}
	return nil
}
