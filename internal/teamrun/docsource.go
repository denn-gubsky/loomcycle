package teamrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// A Starter whose source is a DOCUMENT: the wave's items are the document's
// top-level sections, read once when the wave dispatches.
//
// Like the channel path, this file does not know what a document is. The read
// goes through the DocumentReader seam, injected the way ChannelIO is, so the
// store, the scope fold and the Path tree stay behind it.

// DocumentSection is one work item: a top-level section of the document with
// its whole subtree rendered as Markdown. Its JSON is the payload a member
// receives in the reserved data slot, so the field names are the contract a
// definition's `binds` and prompt are written against.
type DocumentSection struct {
	DocumentID string `json:"document_id"`
	ChunkID    string `json:"chunk_id"`
	Index      int    `json:"index"`
	Title      string `json:"title"`
	Markdown   string `json:"markdown"`
}

// DocumentReader is everything a document-source Starter needs from the
// Document substrate: one read.
type DocumentReader interface {
	// Sections returns the direct children of the root of the document at
	// path, in document order, read under ctx's identity in scope ("user" or
	// "tenant"). A document the identity cannot see is an error, never an
	// empty list: an empty list is a document with no sections.
	Sections(ctx context.Context, scope, path string) ([]DocumentSection, error)
}

// WithDocuments wires the document-source Starter's reader.
func WithDocuments(dr DocumentReader) RunnerOption {
	return func(r *agentRunner) { r.documents = dr }
}

// runDocumentStarter reads the document, turns each section into a wave item
// and dispatches the wave exactly as a channel Starter does. Nothing is acked:
// a document has no cursor.
func (r *agentRunner) runDocumentStarter(ctx context.Context, st teamgraph.State, task *Task) (Outcome, error) {
	h := st.Handler
	if r.documents == nil {
		return Outcome{}, fmt.Errorf("state %q reads a document but no document reader is wired", st.ID)
	}
	// The sink is still a channel. Checked before the read so a walk that
	// cannot publish its results never spawns the runs that produce them.
	if h.Sink != nil && r.channels == nil {
		return Outcome{}, fmt.Errorf("state %q publishes to a sink but no channel executor is wired", st.ID)
	}
	width := 0
	if h.Fanout.Per == teamgraph.FanoutPerChunk {
		var err error
		if width, err = r.waveWidth(st); err != nil {
			return Outcome{}, err
		}
	}
	scope := h.Source.Scope
	if scope == "" {
		scope = "user"
	}

	// Read ONCE, here. The items are a snapshot: an edit to the document while
	// the wave runs does not change what was dispatched, and a later visit to
	// this state reads the document again.
	sections, err := r.documents.Sections(ctx, scope, h.Source.Path)
	if err != nil {
		return Outcome{}, fmt.Errorf("state %q read document %q: %w", st.ID, h.Source.Path, err)
	}
	if len(sections) == 0 {
		// The same posture as an empty channel: a walk that proceeded on an
		// empty wave would hand the next state an answer nobody produced.
		return Outcome{}, fmt.Errorf("state %q: document %q has no sections — its work items are the top-level sections", st.ID, h.Source.Path)
	}
	// MORE than the ceiling fails the walk rather than dispatching the first
	// `max`. A channel Starter can truncate because the unread messages wait
	// on the channel for the next wave; a document has no cursor, so the
	// sections past the ceiling would be dropped with nothing to say so.
	if width > 0 && len(sections) > width {
		return Outcome{}, fmt.Errorf("state %q: document %q has %d sections, more than fanout.max=%d — "+
			"raise max or split the document; a document has no cursor, so dispatching the first %d would silently drop the rest",
			st.ID, h.Source.Path, len(sections), width, width)
	}

	msgs := make([]ChannelMessage, 0, len(sections))
	for i, sec := range sections {
		// The index is the item's place in THIS wave, set here rather than
		// trusted from the reader.
		sec.Index = i
		payload, err := sectionPayload(sec)
		if err != nil {
			return Outcome{}, fmt.Errorf("state %q section %d payload: %w", st.ID, i, err)
		}
		msgs = append(msgs, ChannelMessage{ID: sec.ChunkID, Payload: payload})
	}

	results, waveErr := r.runWave(ctx, st, task, msgs)
	if waveErr != nil {
		return Outcome{}, fmt.Errorf("state %q wave: %w", st.ID, waveErr)
	}
	envelope, err := resultsEnvelope(results)
	if err != nil {
		return Outcome{}, err
	}
	return r.captured(st, task, Outcome{Output: envelope})
}

// sectionPayload encodes a section without HTML escaping. The markdown is read
// by a model, and `<`, `>` and `&` arriving as <… make a spec harder to
// read for no gain: the payload lands in a data slot, not in HTML.
func sectionPayload(sec DocumentSection) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(sec); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
