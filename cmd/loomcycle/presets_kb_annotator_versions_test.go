package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// kb/annotator's prompt version is per annotation level (RFC GM P4.1): a later
// prompt fix bumps one level without making every other level's annotations
// stale. Every existing level started at 2; the folder level is at 3 since RFC GM
// P6 (typical documents, sibling folders and statuses in its request), which
// rewrites each folder annotation once and nothing else.

// annShippedVersions is the shipped CONFIG line the tests rewrite to raise one
// level. A test that cannot find it fails rather than testing the shipped body.
const annShippedVersions = "prompt_versions: { branch: 2, document: 2, path: 3, scheme: 1, concept: 1, class: 1 },"

func annotatorWithVersions(t *testing.T, branch, document, path int) string {
	t.Helper()
	code := knowledgeBaseConfig(t).Agents["kb/annotator"].Code
	if strings.Count(code, annShippedVersions) != 1 {
		t.Fatalf("kb/annotator's CONFIG has no single %q", annShippedVersions)
	}
	return strings.Replace(code, annShippedVersions,
		fmt.Sprintf("prompt_versions: { branch: %d, document: %d, path: %d, scheme: 1, concept: 1, class: 1 },", branch, document, path), 1)
}

// annVersionCall is one annotation_inputs or set_annotation call the annotator made.
type annVersionCall struct {
	op, level string
	version   float64
}

// annRecDoc is the real Document tool, recording the prompt version each
// annotation_inputs and set_annotation call sends.
type annRecDoc struct {
	*builtin.Document
	mu    sync.Mutex
	calls []annVersionCall
}

func (r *annRecDoc) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	if op, _ := in["op"].(string); op == "annotation_inputs" || op == "set_annotation" {
		level, _ := in["level"].(string)
		v, ok := in["prompt_version"].(float64)
		if !ok {
			v = -1 // absent: the server would read it as 0
		}
		r.mu.Lock()
		r.calls = append(r.calls, annVersionCall{op: op, level: level, version: v})
		r.mu.Unlock()
	}
	return r.Document.Execute(ctx, raw)
}

// annStableReply answers the same text for the same part every time, so a
// rewrite changes no hash above the part it rewrote.
func annStableReply(p string) string {
	title := ""
	for _, line := range strings.Split(p, "\n") {
		if v, ok := strings.CutPrefix(line, "TITLE: "); ok {
			title = v
			break
		}
	}
	b, _ := json.Marshal(map[string]string{"brief": annFakeBrief(p, title), "abstract": "Abstract of " + title + ".", "language": "en"})
	return string(b)
}

// annVersionsByLevel is the set of prompt versions the tenant's annotations
// carry, per level.
func annVersionsByLevel(t *testing.T, d *builtin.Document, ctx context.Context) map[string]map[int]int {
	t.Helper()
	out := map[string]map[int]int{}
	for _, a := range realDoc(t, d, ctx, `{"op":"list_annotations","scope":"tenant"}`)["annotations"].([]any) {
		rec := a.(map[string]any)
		level := rec["level"].(string)
		if out[level] == nil {
			out[level] = map[int]int{}
		}
		out[level][int(rec["prompt_version"].(float64))]++
	}
	return out
}

func annStale(rep map[string]any) map[string]any {
	s, _ := rep["stale"].(map[string]any)
	return s
}

// TestKBAnnotator_SendsEachLevelsShippedVersion pins the inputs hash: sections and
// documents ask annotation_inputs and write with version 2, as the one global
// version did, so the server marks none of their records stale on upgrade;
// folders with 3 (RFC GM P6).
func TestKBAnnotator_SendsEachLevelsShippedVersion(t *testing.T) {
	d, author, driver := annRealDocument(t)
	a := seedAnnDoc(t, d, author, "Widget guide", "/kb/widgets/guide")
	rec := &annRecDoc{Document: d}
	f := &annFake{reply: annReplyFromTitle}
	shipped := knowledgeBaseConfig(t).Agents["kb/annotator"].Code
	rep := runAnnotatorWith(t, rec, shipped, driver, f, fmt.Sprintf(`{"mode":"document","document_id":%q,"write":"annotate"}`, a.doc))
	if annCount(rep, "writes") != 6 {
		t.Fatalf("report = %v, want 6 writes", rep)
	}
	seen := map[string]bool{}
	for _, c := range rec.calls {
		want := 2.0
		if c.level == "path" {
			want = 3
		}
		if c.version != want {
			t.Errorf("%s at level %s sent prompt_version %v, want %v", c.op, c.level, c.version, want)
		}
		seen[c.op+"/"+c.level] = true
	}
	for _, op := range []string{"annotation_inputs", "set_annotation"} {
		for _, level := range []string{"branch", "document", "path"} {
			if !seen[op+"/"+level] {
				t.Errorf("no %s call at level %s; calls %v", op, level, rec.calls)
			}
		}
	}
	want := map[string]any{"branch": float64(2), "document": float64(2), "path": float64(3), "scheme": float64(1), "concept": float64(1), "class": float64(1)}
	if got, _ := rep["prompt_versions"].(map[string]any); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("report prompt_versions = %v, want %v", rep["prompt_versions"], want)
	}
}

// TestKBAnnotator_VersionOneRecordsAreStaleVersionTwoAreCurrent: records of the
// old prompt, still on deployments, are rewritten at every level; the records
// that rewrite leaves are current.
func TestKBAnnotator_VersionOneRecordsAreStaleVersionTwoAreCurrent(t *testing.T) {
	d, author, driver := annRealDocument(t)
	a := seedAnnDoc(t, d, author, "Widget guide", "/kb/widgets/guide")
	// Version-1 records of every part, bottom-up, written as the old body did.
	writeV1 := func(level, target string) {
		in := realDoc(t, d, driver, fmt.Sprintf(`{"op":"annotation_inputs","scope":"tenant","level":%q,%s,"prompt_version":1}`, level, target))
		realDoc(t, d, driver, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","level":%q,%s,"prompt_version":1,"inputs_sha256":%q,"brief":"Old brief of a part","abstract":"Written by the old prompt.","language":"en"}`,
			level, target, in["inputs_sha256"]))
	}
	writeV1("branch", fmt.Sprintf(`"id":%q`, a.setup))
	writeV1("branch", fmt.Sprintf(`"id":%q`, a.notes))
	writeV1("document", fmt.Sprintf(`"document_id":%q`, a.doc))
	for _, folder := range []string{"/kb/widgets", "/kb", "/"} {
		writeV1("path", fmt.Sprintf(`"path":%q`, folder))
	}
	if got := annVersionsByLevel(t, d, author); fmt.Sprint(got) != "map[branch:map[1:2] document:map[1:1] path:map[1:3]]" {
		t.Fatalf("seeded records = %v", got)
	}

	f := &annFake{reply: annStableReply}
	in := fmt.Sprintf(`{"mode":"document","document_id":%q}`, a.doc)
	rep := runAnnotator(t, d, driver, f, in)
	if s := annStale(rep); annCount(rep, "would_call") != 6 || s["branch"] != float64(2) || s["document"] != float64(1) || s["path"] != float64(3) {
		t.Fatalf("report over version-1 records = %v, want every part stale", rep)
	}
	rep = runAnnotator(t, d, driver, f, fmt.Sprintf(`{"mode":"document","document_id":%q,"write":"annotate"}`, a.doc))
	if annCount(rep, "writes") != 6 {
		t.Fatalf("annotate over version-1 records = %v, want 6 writes", rep)
	}
	if got := annVersionsByLevel(t, d, author); fmt.Sprint(got) != "map[branch:map[2:2] document:map[2:1] path:map[3:3]]" {
		t.Fatalf("records after the rewrite = %v, want the shipped versions", got)
	}
	rep = runAnnotator(t, d, driver, f, in)
	if annCount(rep, "would_call") != 0 {
		t.Fatalf("report over the rewritten records = %v, want nothing stale", rep)
	}
	rep = runAnnotator(t, d, driver, f, `{"mode":"path","path":"/","recursive":true}`)
	if annCount(rep, "would_call") != 0 {
		t.Fatalf("folder report over the rewritten records = %v, want nothing stale", rep)
	}
}

// TestKBAnnotator_RaisingOneLevelsVersionRewritesOnlyThatLevel: a later prompt
// fix for one level leaves the other levels' annotations current.
func TestKBAnnotator_RaisingOneLevelsVersionRewritesOnlyThatLevel(t *testing.T) {
	for _, tc := range []struct {
		level                  string
		branch, document, path int
		pathMode               bool // a path pass over the tree, else a document pass
		calls                  int
		want                   string // the records' versions afterwards
	}{
		{"branch", 3, 2, 2, false, 2,
			"map[branch:map[3:2] document:map[2:1] path:map[2:3]]"},
		{"document", 2, 3, 2, false, 1,
			"map[branch:map[2:2] document:map[3:1] path:map[2:3]]"},
		{"path", 2, 2, 3, true, 3,
			"map[branch:map[2:2] document:map[2:1] path:map[3:3]]"},
	} {
		t.Run(tc.level, func(t *testing.T) {
			d, author, driver := annRealDocument(t)
			a := seedAnnDoc(t, d, author, "Widget guide", "/kb/widgets/guide")
			f := &annFake{reply: annStableReply}
			// From every level at 2, the body before RFC GM P6.
			runAnnotatorWith(t, d, annotatorWithVersions(t, 2, 2, 2), driver, f, fmt.Sprintf(`{"mode":"document","document_id":%q,"write":"annotate"}`, a.doc))
			if got := annVersionsByLevel(t, d, author); fmt.Sprint(got) != "map[branch:map[2:2] document:map[2:1] path:map[2:3]]" {
				t.Fatalf("records after the first pass = %v", got)
			}

			raised := annotatorWithVersions(t, tc.branch, tc.document, tc.path)
			input := fmt.Sprintf(`{"mode":"document","document_id":%q,"write":"annotate"}`, a.doc)
			if tc.pathMode {
				input = `{"mode":"path","path":"/","recursive":true,"write":"annotate"}`
			}
			before := len(f.prompts)
			rep := runAnnotatorWith(t, d, raised, driver, f, input)
			if n := len(f.prompts) - before; n != tc.calls || annCount(rep, "writes") != tc.calls {
				t.Fatalf("raising %s: %d calls, report %v; want %d", tc.level, n, rep, tc.calls)
			}
			for lvl, n := range annStale(rep) {
				if lvl != tc.level && n != float64(0) {
					t.Errorf("raising %s found %v %s annotations stale", tc.level, n, lvl)
				}
			}
			if got := annVersionsByLevel(t, d, author); fmt.Sprint(got) != tc.want {
				t.Fatalf("records after raising %s = %v, want %s", tc.level, got, tc.want)
			}
			// The raised body finds its own records current.
			before = len(f.prompts)
			runAnnotatorWith(t, d, raised, driver, f, input)
			if n := len(f.prompts) - before; n != 0 {
				t.Fatalf("a second pass at the raised version made %d calls", n)
			}
		})
	}
}

// annRequestBrief answers with a brief and abstract that differ whenever the
// request does, as a model's would: a sibling's rewrite always gives it a new brief.
func annRequestBrief(prompt string) string {
	title := ""
	for _, line := range strings.Split(prompt, "\n") {
		if v, ok := strings.CutPrefix(line, "TITLE: "); ok {
			title = v
			break
		}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(prompt))
	b, _ := json.Marshal(map[string]string{"brief": fmt.Sprintf("%s, from request %08x", annFakeBrief(prompt, title), h.Sum32()),
		"abstract": fmt.Sprintf("Abstract of %s, from request %08x.", title, h.Sum32()), "language": "en"})
	return string(b)
}

// TestKBAnnotator_FolderContextVersionRewritesEachFolderOnce — over records the
// body before RFC GM P6 wrote, the shipped body finds only the folders stale,
// rewrites each exactly once with its typical documents, sibling folders and
// statuses in the request, and then finds nothing stale: sibling folders rewritten
// in turn, each with a new brief, do not make each other stale again.
func TestKBAnnotator_FolderContextVersionRewritesEachFolderOnce(t *testing.T) {
	d, author, driver := annRealDocument(t)
	a := seedAnnDoc(t, d, author, "Widget guide", "/kb/widgets/guide")
	b := seedAnnDoc(t, d, author, "Gadget manual", "/kb/gadgets/manual")
	f := &annFake{reply: annRequestBrief}
	old := annotatorWithVersions(t, 2, 2, 2)
	for _, doc := range []string{a.doc, b.doc} {
		runAnnotatorWith(t, d, old, driver, f, fmt.Sprintf(`{"mode":"document","document_id":%q,"write":"annotate"}`, doc))
	}
	const before = "map[branch:map[2:4] document:map[2:2] path:map[2:4]]"
	if got := annVersionsByLevel(t, d, author); fmt.Sprint(got) != before {
		t.Fatalf("records of the old body = %v, want %s", got, before)
	}

	// Sections and documents stay current under the shipped body.
	for _, doc := range []string{a.doc, b.doc} {
		rep := runAnnotator(t, d, driver, f, fmt.Sprintf(`{"mode":"document","document_id":%q}`, doc))
		if s := annStale(rep); s["branch"] != float64(0) || s["document"] != float64(0) {
			t.Fatalf("shipped report over %s = %v, want no section or document stale", doc, rep)
		}
	}
	tree := `{"mode":"path","path":"/","recursive":true}`
	rep := runAnnotator(t, d, driver, f, tree)
	if annCount(rep, "would_call") != 4 {
		t.Fatalf("shipped folder report = %v, want the 4 folders stale", rep)
	}
	n0 := len(f.prompts)
	rep = runAnnotator(t, d, driver, f, `{"mode":"path","path":"/","recursive":true,"write":"annotate"}`)
	if n := len(f.prompts) - n0; n != 4 || annCount(rep, "writes") != 4 {
		t.Fatalf("shipped folder pass: %d calls, report %v; want 4", n, rep)
	}
	sawSiblings := false
	for _, p := range f.prompts[n0:] {
		if !strings.Contains(p, "LEVEL: a folder of documents") || !strings.Contains(p, "\nSTATUS: ") {
			t.Errorf("folder request without a STATUS line:\n%s", p)
		}
		if !strings.Contains(p, "MATERIAL — typical documents below it:") {
			t.Errorf("folder request without typical documents:\n%s", p)
		}
		if strings.Contains(p, "TITLE: widgets") && strings.Contains(p, "MATERIAL — the folders beside it, to tell it apart from:") &&
			strings.Contains(p, "gadgets/ — brief: Brief of the folder gadgets") {
			sawSiblings = true
		}
	}
	if !sawSiblings {
		t.Errorf("the widgets folder's request does not name its sibling gadgets with its brief")
	}
	const after = "map[branch:map[2:4] document:map[2:2] path:map[3:4]]"
	if got := annVersionsByLevel(t, d, author); fmt.Sprint(got) != after {
		t.Fatalf("records after the folder pass = %v, want %s", got, after)
	}
	n0 = len(f.prompts)
	runAnnotator(t, d, driver, f, `{"mode":"path","path":"/","recursive":true,"write":"annotate"}`)
	for _, doc := range []string{a.doc, b.doc} {
		runAnnotator(t, d, driver, f, fmt.Sprintf(`{"mode":"document","document_id":%q,"write":"annotate"}`, doc))
	}
	if n := len(f.prompts) - n0; n != 0 {
		t.Fatalf("a second pass made %d calls, want none", n)
	}
}
