package tools

import (
	"encoding/json"
	"fmt"
	"sync"
)

// A model that never read a tool's help, failing its calls. Measured on local
// models: small and medium models almost never read an article — 2 help calls
// in 60 runs — despite a line in every tool's description telling them to
// before the first call. A pointer to the help is not followed; what a model
// does read is the failed call's own result. So a run's shape failure on a
// tool whose help it skipped carries that help in the result itself.
//
// Gated twice, because an article is 1–10K characters. It is attached only
// while the run has not read this tool's help — a model that did has the
// article already. And only once per tool and operation: in the append and
// recap loops every result stays in the context, so a second copy would only
// grow it. A stateful run is the exception (SetHelpHintsUntilRead): its model
// sees only its latest observation, so a hint shown once is gone a step later,
// and repeating it there adds nothing to the context.

// HelpContentIndex is implemented by a HelpIndex that can serve an article's
// text. Separate from HelpIndex so an index without it still works and simply
// attaches no hints.
type HelpContentIndex interface {
	// HelpContent returns the text of the article named exactly topic.
	HelpContent(topic string) (string, bool)
	// HelpTopicTool returns the tool a help topic documents, as the model may
	// spell it (Memory.recall, "Memory recall", an alias); "" when it
	// documents no tool.
	HelpTopicTool(topic string) string
}

type helpHints struct {
	mu        sync.Mutex
	enabled   bool
	untilRead bool
	// read holds the tools whose help this run has read.
	read map[string]bool
	// hinted holds the tool+operation pairs that have been given a hint.
	hinted map[string]bool
}

// EnableHelpHints turns on help hints for this dispatcher. A RUN's dispatcher
// only: what the run has read and been shown is what gates a hint, and a
// dispatcher built for one call outside a run has no such history.
func (d *Dispatcher) EnableHelpHints() {
	d.hints.mu.Lock()
	defer d.hints.mu.Unlock()
	d.hints.enabled = true
}

// SetHelpHintsUntilRead makes a hint attach to every qualifying failure until
// the run reads that tool's help, instead of once per tool and operation. For
// the stateful loop, whose model reads only its latest observation.
func (d *Dispatcher) SetHelpHintsUntilRead(on bool) {
	d.hints.mu.Lock()
	defer d.hints.mu.Unlock()
	d.hints.untilRead = on
}

// noteHelpRead records a successful help call, so its tool gets no hint from
// here on.
func (d *Dispatcher) noteHelpRead(name string, input json.RawMessage, res Result) {
	if res.IsError || name != d.helpName {
		return
	}
	idx, ok := d.help.(HelpContentIndex)
	if !ok {
		return
	}
	var in struct {
		Op    string `json:"op"`
		Topic string `json:"topic"`
	}
	if json.Unmarshal(input, &in) != nil || in.Op != "help" || in.Topic == "" {
		return
	}
	tool := idx.HelpTopicTool(in.Topic)
	if tool == "" {
		return
	}
	d.hints.mu.Lock()
	defer d.hints.mu.Unlock()
	if d.hints.read == nil {
		d.hints.read = map[string]bool{}
	}
	d.hints.read[tool] = true
}

// helpHint returns the hint for a failed call of tool name whose call format
// is cf, or "" when there is none to give.
func (d *Dispatcher) helpHint(name string, cf *CallFormat) string {
	idx, ok := d.help.(HelpContentIndex)
	if !ok || cf == nil {
		return ""
	}
	topic := name
	if cf.Op != "" {
		topic = name + "/" + cf.Op
	}
	d.hints.mu.Lock()
	defer d.hints.mu.Unlock()
	if !d.hints.enabled || d.hints.read[name] {
		return ""
	}
	if !d.hints.untilRead && d.hints.hinted[topic] {
		return ""
	}
	content, ok := idx.HelpContent(topic)
	if !ok || content == "" {
		return ""
	}
	if d.hints.hinted == nil {
		d.hints.hinted = map[string]bool{}
	}
	d.hints.hinted[topic] = true
	return fmt.Sprintf("You have not read the help for %s in this run. Its article follows — read it before your next call to %s.\n\n%s",
		topic, name, content)
}
