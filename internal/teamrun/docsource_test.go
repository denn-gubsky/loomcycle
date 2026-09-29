package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// fakeDocuments is a DocumentReader over an in-memory section list that a test
// may rewrite mid-wave, recording every read it serves.
type fakeDocuments struct {
	mu       sync.Mutex
	sections []DocumentSection
	err      error
	reads    []string // "scope path", one per call
}

func (f *fakeDocuments) Sections(_ context.Context, scope, path string) ([]DocumentSection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, scope+" "+path)
	if f.err != nil {
		return nil, f.err
	}
	return append([]DocumentSection(nil), f.sections...), nil
}

func (f *fakeDocuments) set(secs []DocumentSection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sections = secs
}

func threeSections() []DocumentSection {
	out := make([]DocumentSection, 0, 3)
	for i, title := range []string{"Goals", "Risks", "Plan"} {
		out = append(out, DocumentSection{
			DocumentID: "doc1", ChunkID: "c" + title, Index: 99 - i, // the runner owns the index
			Title: title, Markdown: "## " + title + "\n\nBody of " + title + ".",
		})
	}
	return out
}

func docStarterState(per string, max int) teamgraph.State {
	return teamgraph.State{ID: "wave", Handler: teamgraph.Handler{
		Kind:   teamgraph.HandlerStarter,
		Source: &teamgraph.StarterSource{Kind: teamgraph.SourceDocument, Path: "/specs/acme"},
		Fanout: &teamgraph.StarterFanout{Agent: "reviewer", Per: per, Max: max},
		Sink:   &teamgraph.StarterSink{Channel: "verdicts"},
		Prompt: &teamgraph.StarterPrompt{Input: "Review:\n" + StarterMessageSlot},
	}}
}

// docSpawn captures every prompt the wave dispatched, in call order.
type docSpawn struct {
	mu      sync.Mutex
	prompts []Prompt
	onCall  func(n int)
}

func (s *docSpawn) spawn() SpawnFunc {
	return textSpawn(func(_ context.Context, _ string, p Prompt, _ string) (string, error) {
		s.mu.Lock()
		s.prompts = append(s.prompts, p)
		n := len(s.prompts)
		s.mu.Unlock()
		if s.onCall != nil {
			s.onCall(n)
		}
		return "ok", nil
	})
}

func (s *docSpawn) payloads(t *testing.T, slot string) []DocumentSection {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DocumentSection, 0, len(s.prompts))
	for _, p := range s.prompts {
		var sec DocumentSection
		if err := json.Unmarshal([]byte(p.DataSlots[slot]), &sec); err != nil {
			t.Fatalf("data slot %s is not a section payload: %q", slot, p.DataSlots[slot])
		}
		out = append(out, sec)
	}
	return out
}

// A three-section document is a wave of three, each run holding ITS section in
// the data slot, one sink message each, and nothing read from or acked on a
// channel.
func TestDocumentStarter_DispatchesOneRunPerSection(t *testing.T) {
	docs := &fakeDocuments{sections: threeSections()}
	ch := &fakeChannels{}
	rec := &docSpawn{}
	r := starterRunner(ch, rec.spawn())
	r.documents = docs

	out, err := r.RunHandler(context.Background(), docStarterState(teamgraph.FanoutPerChunk, 5), &Task{Input: "go", WalkID: "wlk_t"})
	if err != nil {
		t.Fatalf("starter: %v", err)
	}
	if !strings.HasPrefix(out.Output, `{"results":`) {
		t.Errorf("output = %q, want the results envelope", out.Output)
	}
	got := rec.payloads(t, StarterMessageSlot)
	if len(got) != 3 {
		t.Fatalf("dispatched %d runs, want one per section (3)", len(got))
	}
	seen := map[int]DocumentSection{}
	for _, sec := range got {
		seen[sec.Index] = sec
	}
	for i, title := range []string{"Goals", "Risks", "Plan"} {
		sec, ok := seen[i]
		if !ok {
			t.Fatalf("no run received index %d; got %+v", i, got)
		}
		if sec.Title != title || sec.ChunkID != "c"+title || sec.DocumentID != "doc1" ||
			sec.Markdown != "## "+title+"\n\nBody of "+title+"." {
			t.Errorf("index %d payload = %+v, want section %q", i, sec, title)
		}
	}
	if sinks := ch.sinks(t); len(sinks) != 3 {
		t.Errorf("published %d sink messages, want 3", len(sinks))
	}
	if len(ch.acked) != 0 {
		t.Errorf("acked %v — a document has no cursor", ch.acked)
	}
	if len(docs.reads) != 1 || docs.reads[0] != "user /specs/acme" {
		t.Errorf("reads = %v, want one read of /specs/acme in the default user scope", docs.reads)
	}
}

// per=once is one run holding every section as a JSON array.
func TestDocumentStarter_PerOnceHoldsEverySection(t *testing.T) {
	docs := &fakeDocuments{sections: threeSections()}
	rec := &docSpawn{}
	r := starterRunner(&fakeChannels{}, rec.spawn())
	r.documents = docs
	st := docStarterState(teamgraph.FanoutPerOnce, 0)
	st.Handler.Source.Scope = "tenant"
	st.Handler.Prompt.Input = "All:\n" + StarterMessagesSlot

	if _, err := r.RunHandler(context.Background(), st, &Task{Input: "go"}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	if len(rec.prompts) != 1 {
		t.Fatalf("dispatched %d runs, want one", len(rec.prompts))
	}
	var all []DocumentSection
	if err := json.Unmarshal([]byte(rec.prompts[0].DataSlots[StarterMessagesSlot]), &all); err != nil || len(all) != 3 {
		t.Fatalf("messages slot = %q (%v), want all three sections", rec.prompts[0].DataSlots[StarterMessagesSlot], err)
	}
	if docs.reads[0] != "tenant /specs/acme" {
		t.Errorf("read %q, want the declared tenant scope", docs.reads[0])
	}
}

// A document longer than the ceiling FAILS rather than dispatching the first
// `max`: with no cursor, the sections past it would be dropped silently.
func TestDocumentStarter_MoreSectionsThanMaxFailsNamingBoth(t *testing.T) {
	rec := &docSpawn{}
	r := starterRunner(&fakeChannels{}, rec.spawn())
	r.documents = &fakeDocuments{sections: threeSections()}

	_, err := r.RunHandler(context.Background(), docStarterState(teamgraph.FanoutPerChunk, 2), &Task{Input: "go"})
	if err == nil {
		t.Fatal("a 3-section document dispatched under max=2")
	}
	if !strings.Contains(err.Error(), "3 sections") || !strings.Contains(err.Error(), "fanout.max=2") {
		t.Errorf("err = %q, want it to name both 3 and 2", err)
	}
	if len(rec.prompts) != 0 {
		t.Errorf("spawned %d runs before refusing", len(rec.prompts))
	}
}

func TestDocumentStarter_ZeroSectionsFailsTheWalk(t *testing.T) {
	rec := &docSpawn{}
	r := starterRunner(&fakeChannels{}, rec.spawn())
	r.documents = &fakeDocuments{}

	_, err := r.RunHandler(context.Background(), docStarterState(teamgraph.FanoutPerChunk, 4), &Task{Input: "go"})
	if err == nil || !strings.Contains(err.Error(), "has no sections") {
		t.Fatalf("err = %v, want a walk error for an empty document", err)
	}
	if len(rec.prompts) != 0 {
		t.Errorf("spawned %d runs for an empty document", len(rec.prompts))
	}
}

// The items are a snapshot taken at dispatch: a member editing the document
// mid-wave changes nothing about what the rest of the wave received.
func TestDocumentStarter_EditDuringTheWaveDoesNotChangeTheItems(t *testing.T) {
	docs := &fakeDocuments{sections: threeSections()}
	rec := &docSpawn{}
	rec.onCall = func(n int) {
		if n == 1 {
			docs.set([]DocumentSection{{DocumentID: "doc1", ChunkID: "cX", Title: "Rewritten", Markdown: "## Rewritten"}})
		}
	}
	r := starterRunner(&fakeChannels{}, rec.spawn())
	r.documents = docs

	if _, err := r.RunHandler(context.Background(), docStarterState(teamgraph.FanoutPerChunk, 5), &Task{Input: "go"}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	got := rec.payloads(t, StarterMessageSlot)
	if len(got) != 3 {
		t.Fatalf("dispatched %d runs, want the 3 sections read at dispatch", len(got))
	}
	for _, sec := range got {
		if sec.Title == "Rewritten" {
			t.Errorf("a run received the mid-wave edit: %+v", sec)
		}
	}
	if len(docs.reads) != 1 {
		t.Errorf("read the document %d times in one wave, want once", len(docs.reads))
	}
}

// binds project the section payload into ${var.*}, as they project a message.
func TestDocumentStarter_BindsProjectTheSection(t *testing.T) {
	rec := &docSpawn{}
	r := starterRunner(&fakeChannels{}, rec.spawn())
	r.documents = &fakeDocuments{sections: threeSections()[:1]}
	st := docStarterState(teamgraph.FanoutPerChunk, 1)
	st.Handler.Binds = map[string]string{"section": "$.title"}

	task := &Task{Input: "go"}
	if _, err := r.RunHandler(context.Background(), st, task); err != nil {
		t.Fatalf("starter: %v", err)
	}
	if got := rec.prompts[0].Values["var.section"]; got != "Goals" {
		t.Errorf("${var.section} = %q, want the section title", got)
	}
}

func TestDocumentStarter_ReadErrorFailsTheWalk(t *testing.T) {
	rec := &docSpawn{}
	r := starterRunner(&fakeChannels{}, rec.spawn())
	r.documents = &fakeDocuments{err: errors.New("no such path: /specs/acme")}

	_, err := r.RunHandler(context.Background(), docStarterState(teamgraph.FanoutPerChunk, 4), &Task{Input: "go"})
	if err == nil || !strings.Contains(err.Error(), "no such path") || !strings.Contains(err.Error(), `"/specs/acme"`) {
		t.Fatalf("err = %v, want the read failure naming the path", err)
	}
}

// Unwired collaborators are refused at the state, never skipped: a workflow
// whose source never fires looks identical to one whose source is empty.
func TestDocumentStarter_UnwiredReaderOrSinkIsRefused(t *testing.T) {
	r := &agentRunner{spawn: (&docSpawn{}).spawn(), channels: &fakeChannels{}, logf: func(string, ...any) {}}
	if _, err := r.RunHandler(context.Background(), docStarterState(teamgraph.FanoutPerChunk, 4), &Task{}); err == nil ||
		!strings.Contains(err.Error(), "no document reader is wired") {
		t.Errorf("no reader: err = %v", err)
	}
	r = &agentRunner{spawn: (&docSpawn{}).spawn(), documents: &fakeDocuments{sections: threeSections()}, logf: func(string, ...any) {}}
	if _, err := r.RunHandler(context.Background(), docStarterState(teamgraph.FanoutPerChunk, 4), &Task{}); err == nil ||
		!strings.Contains(err.Error(), "no channel executor is wired") {
		t.Errorf("sink without channels: err = %v", err)
	}
	// With no sink, no channel executor is needed.
	st := docStarterState(teamgraph.FanoutPerChunk, 4)
	st.Handler.Sink = nil
	if _, err := r.RunHandler(context.Background(), st, &Task{}); err != nil {
		t.Errorf("sinkless document starter without channels: %v", err)
	}
}

// Markdown reaches the member readable: `<`, `>` and `&` are not escaped.
func TestSectionPayload_KeepsMarkdownReadable(t *testing.T) {
	p, err := sectionPayload(DocumentSection{Markdown: "a < b && c > d"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p), `"a < b && c > d"`) || strings.HasSuffix(string(p), "\n") {
		t.Errorf("payload = %q", p)
	}
}
