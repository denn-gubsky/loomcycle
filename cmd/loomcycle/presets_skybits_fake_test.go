package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// This file is the test double the skybits-sync bundle runs against: a fake
// Skybits MCP server (one tools.Tool per mcp__skybits__* tool, answering in the
// exact envelope loomcycle's MCP client produces), a fake File tool whose
// import goes through the REAL Document import_md, a fake Agent tool that runs
// the shipped skybits/import body through the real loop and code-js provider
// (and a scripted skybits/structure), and the real Document, Path and Memory
// tools on sqlite with SQL Memory. Every item, title and tag is invented.

// skyItem is one item in the fake account.
type skyItem struct {
	ID, Kind, Title, CreatedAt, UpdatedAt string
	Tags                                  []string
	File                                  *skyFile
	Template, Incoming, Trashed           bool
	Rev                                   int
	// Body is the item's text as the export or extract renders it (Markdown,
	// without the title heading).
	Body string
	// TooLarge makes File download refuse the item.
	TooLarge bool
	// Cells is a sheet tab's populated cell count in the outline.
	Cells int
}

type skyFile struct {
	Filename, ContentType string
	Size                  int64
	Version               int
}

type skyCall struct {
	Tool string
	Args map[string]any
}

type skyJob struct {
	ID, ItemID, Format string
	Polls              int
}

// skyFake is the account, the run inbox and the scripted children.
type skyFake struct {
	t  *testing.T
	mu sync.Mutex

	items []*skyItem
	calls []skyCall

	// faults[tool] is a queue of error texts returned by the next calls of
	// tool, one per call ("" lets a call through).
	faults map[string][]string
	// pendingPolls is how many get_job polls answer "running" before done.
	pendingPolls int
	// shortBy makes the documents listing return fewer rows than total_count.
	shortBy int
	// emptyText makes the listing's text block empty, so the envelope starts
	// with "server-reported details: ".
	emptyText bool
	// zipExport makes exports come back as an export.zip URL.
	zipExport bool
	jobs      map[string]*skyJob
	jobSeq    int

	// files is the inbox: ref → content; removed counts File rm per ref.
	files   map[string]string
	removed map[string]int
	// ocr: "ok" reads images, "off" refuses OCR. stt: "ok", "off", "convert".
	ocr, stt string
	// extractFault fails File extract with this text.
	extractFault string
	// importRefusesHeadings makes File import refuse any headings.
	importRefusesHeadings bool

	// structure answers skybits/structure: given the untrusted payload, the
	// reply object (marshalled as `structured`) or a raw string (final_text).
	structure func(payload string) any
	// structurePayloads records what each structurer call was given.
	structurePayloads []string

	childSeq int
	cfg      *config.Config
	docs     *builtin.Document
	mem      *builtin.Memory
	paths    *builtin.Path
}

func newSkyFake(t *testing.T) *skyFake {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir(), StatementTimeoutMS: 30000, MaxRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	return &skyFake{
		t: t, faults: map[string][]string{}, jobs: map[string]*skyJob{},
		files: map[string]string{}, removed: map[string]int{}, ocr: "ok", stt: "ok",
		cfg:   skybitsConfig(t),
		docs:  &builtin.Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()},
		mem:   &builtin.Memory{Store: st, SqlMem: mgr},
		paths: &builtin.Path{Store: st},
	}
}

func (f *skyFake) item(id string) *skyItem {
	for _, it := range f.items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

func (f *skyFake) record(tool string, raw json.RawMessage) map[string]any {
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	f.mu.Lock()
	f.calls = append(f.calls, skyCall{Tool: tool, Args: in})
	f.mu.Unlock()
	return in
}

func (f *skyFake) fault(tool string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.faults[tool]
	if len(q) == 0 {
		return ""
	}
	f.faults[tool] = q[1:]
	return q[0]
}

// callsTo returns the recorded calls of tool (and of op, when op is not "").
func (f *skyFake) callsTo(tool, op string) []skyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []skyCall
	for _, c := range f.calls {
		if c.Tool == tool && (op == "" || c.Args["op"] == op) {
			out = append(out, c)
		}
	}
	return out
}

// envelope is what loomcycle's MCP client hands a code agent: the text block,
// then the structured content after a fixed marker.
func envelope(text string, structured any) string {
	b, _ := json.Marshal(structured)
	if text == "" {
		return "server-reported details: " + string(b)
	}
	return text + "\n\nserver-reported details: " + string(b)
}

// ─── Skybits tools ───────────────────────────────────────────────────

type skyTool struct {
	f    *skyFake
	name string
}

func (s *skyTool) Name() string                 { return "mcp__skybits__" + s.name }
func (s *skyTool) Description() string          { return "skybits " + s.name + " (test double)" }
func (s *skyTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (s *skyTool) Execute(_ context.Context, raw json.RawMessage) (tools.Result, error) {
	f := s.f
	in := f.record(s.Name(), raw)
	if e := f.fault(s.name); e != "" {
		return tools.Result{IsError: true, Text: e}, nil
	}
	switch s.name {
	case "find_documents":
		return f.findDocuments(in), nil
	case "list_tags":
		return tools.Result{Text: envelope("tags", map[string]any{"tags": []any{}})}, nil
	case "list_jobs":
		f.mu.Lock()
		n := len(f.jobs)
		f.mu.Unlock()
		return tools.Result{Text: envelope(fmt.Sprintf("%d jobs", n), map[string]any{"jobs": []any{}, "limit": in["limit"]})}, nil
	case "start_export":
		id, _ := in["doc_urls"].(string)
		it := f.item(id)
		if it == nil {
			return tools.Result{IsError: true, Text: "no such document " + id}, nil
		}
		f.mu.Lock()
		f.jobSeq++
		j := &skyJob{ID: fmt.Sprintf("job-%d", f.jobSeq), ItemID: id, Format: fmt.Sprint(in["export_formats"])}
		f.jobs[j.ID] = j
		f.mu.Unlock()
		return tools.Result{Text: envelope("export queued", map[string]any{"id": j.ID, "status": "pending"})}, nil
	case "get_job":
		id, _ := in["job_id"].(string)
		f.mu.Lock()
		j := f.jobs[id]
		if j == nil {
			f.mu.Unlock()
			return tools.Result{IsError: true, Text: "no such job"}, nil
		}
		j.Polls++
		done := j.Polls > f.pendingPolls
		f.mu.Unlock()
		if !done {
			return tools.Result{Text: envelope("running", map[string]any{"id": id, "status": "running"})}, nil
		}
		name := j.ItemID + "." + j.Format
		if f.zipExport {
			name = "export.zip"
		}
		return tools.Result{Text: envelope("completed", map[string]any{"id": id, "status": "completed",
			"download_url": "https://exports.s3.example.test/" + j.ItemID + "/" + name + "?X-Amz-Signature=secret-sig&X-Amz-Credential=cred"})}, nil
	case "get_file_url":
		id, _ := in["doc_url"].(string)
		it := f.item(id)
		if it == nil || it.File == nil {
			return tools.Result{IsError: true, Text: "not a file"}, nil
		}
		// The text form: the URL on the first line.
		return tools.Result{Text: "https://files.s3.example.test/" + id + "/" + it.File.Filename + "?X-Amz-Signature=secret-sig\nexpires in 15 minutes"}, nil
	case "outline_document":
		id, _ := in["doc_url"].(string)
		it := f.item(id)
		if it == nil {
			return tools.Result{IsError: true, Text: "no such document"}, nil
		}
		return tools.Result{Text: fmt.Sprintf("rev %d\n## Data — A1:H40 · %d cells · header row\n", it.Rev, it.Cells)}, nil
	case "document_history":
		id, _ := in["doc_url"].(string)
		it := f.item(id)
		if it == nil {
			return tools.Result{IsError: true, Text: "no such document"}, nil
		}
		return tools.Result{Text: envelope("history", map[string]any{"revisions": []any{map[string]any{"revision": it.Rev}}})}, nil
	}
	return tools.Result{IsError: true, Text: "unexpected tool " + s.name}, nil
}

func (f *skyFake) findDocuments(in map[string]any) tools.Result {
	loc, _ := in["location"].(string)
	if loc == "" {
		loc = "documents"
	}
	f.mu.Lock()
	var rows []*skyItem
	for _, it := range f.items {
		if it.Trashed == (loc == "trash") {
			rows = append(rows, it)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt < rows[j].CreatedAt })
	total := len(rows)
	if loc == "documents" && f.shortBy > 0 {
		rows = rows[:len(rows)-f.shortBy]
	}
	emptyText := f.emptyText
	f.mu.Unlock()
	start := 0
	if c, _ := in["cursor"].(string); c != "" {
		start, _ = strconv.Atoi(strings.TrimPrefix(c, "c"))
	}
	limit := 100
	if l, ok := in["limit"].(float64); ok {
		limit = int(l)
	}
	end := min(start+limit, len(rows))
	docs := []any{}
	for _, it := range rows[start:end] {
		d := map[string]any{"id": it.ID, "kind": it.Kind, "title": it.Title, "url": "https://skybits.ai/documents/" + it.ID,
			"created_at": it.CreatedAt, "updated_at": it.UpdatedAt, "is_template": it.Template}
		tags := []any{}
		for _, tg := range it.Tags {
			tags = append(tags, map[string]any{"name": tg, "source": "user"})
		}
		d["tags"] = tags
		if it.File != nil {
			d["file"] = map[string]any{"filename": it.File.Filename, "content_type": it.File.ContentType, "size_bytes": it.File.Size, "version": it.File.Version}
		}
		if it.Incoming {
			d["incoming_status"] = "pending"
			d["title"] = ""
		}
		docs = append(docs, d)
	}
	out := map[string]any{"documents": docs, "total_count": total}
	if end < len(rows) {
		out["next_cursor"] = "c" + strconv.Itoa(end)
	}
	text := fmt.Sprintf("%d documents", len(docs))
	if emptyText {
		text = ""
	}
	return tools.Result{Text: envelope(text, out)}
}

// ─── File ────────────────────────────────────────────────────────────

type skyFakeFile struct{ f *skyFake }

func (x *skyFakeFile) Name() string                 { return "File" }
func (x *skyFakeFile) Description() string          { return "file (test double)" }
func (x *skyFakeFile) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

var skyURLItem = regexp.MustCompile(`^https://[a-z.]+\.s3\.example\.test/([^/]+)/`)

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func okJSONText(v any) tools.Result { b, _ := json.Marshal(v); return tools.Result{Text: string(b)} }

func (x *skyFakeFile) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	f := x.f
	in := f.record("File", raw)
	op, _ := in["op"].(string)
	p, _ := in["path"].(string)
	switch op {
	case "download":
		u, _ := in["url"].(string)
		m := skyURLItem.FindStringSubmatch(u)
		if m == nil {
			return tools.Result{IsError: true, Text: "download: unexpected url"}, nil
		}
		it := f.item(m[1])
		if it == nil {
			return tools.Result{IsError: true, Text: "download: HTTP 404"}, nil
		}
		if it.TooLarge {
			return tools.Result{IsError: true, Text: "download: the file is larger than max_bytes (100663296): nothing was kept"}, nil
		}
		to, _ := in["to"].(string)
		content := it.Body
		f.mu.Lock()
		f.files[to] = content
		f.mu.Unlock()
		return okJSONText(map[string]any{"url": strings.Split(u, "?")[0], "status": 200, "ref": to, "size": len(content), "sha256": sha(content)}), nil
	case "extract":
		if f.extractFault != "" {
			return tools.Result{IsError: true, Text: f.extractFault}, nil
		}
		f.mu.Lock()
		content, ok := f.files[p]
		f.mu.Unlock()
		if !ok {
			return tools.Result{IsError: true, Text: "extract: no such file " + p}, nil
		}
		base := strings.TrimSuffix(path.Base(p), path.Ext(p))
		ext := strings.TrimPrefix(path.Ext(p), ".")
		out := map[string]any{"format": ext}
		md := "# " + base + "\n\n" + content
		if ext == "png" || ext == "jpg" {
			if f.ocr != "ok" {
				return tools.Result{IsError: true, Text: "extract: the OCR model cannot read this file: OCR is not set up"}, nil
			}
			md = "# " + base + "\n\n<!-- loom: ocr -->\n" + content
			out["ocr"] = map[string]any{"status": "ok", "pages": 1}
		}
		ref := "volume:inbox:extracted/" + base + ".md"
		f.mu.Lock()
		f.files[ref] = md
		f.mu.Unlock()
		out["ref"] = ref
		out["chars"] = len(md)
		out["excerpt"] = fmt.Sprintf("<file_content path=%q truncated=\"false\">\n%s\n</file_content>", ref, md)
		return okJSONText(out), nil
	case "transcribe":
		switch f.stt {
		case "off":
			return tools.Result{IsError: true, Text: "transcribe: speech-to-text is not set up here"}, nil
		case "convert":
			return tools.Result{IsError: true, Text: "transcribe: the recording cannot be sent whole and no media converter is set up"}, nil
		}
		if in["dry_run"] == true {
			return okJSONText(map[string]any{"dry_run": true, "est_minutes": 3.5}), nil
		}
		ref := "volume:inbox:extracted/" + strings.TrimSuffix(path.Base(p), path.Ext(p)) + ".md"
		f.mu.Lock()
		f.files[ref] = "# Recording\n\n## 00:00\n\n**Speaker** [00:00:01] hello there from the recording\n"
		f.mu.Unlock()
		return okJSONText(map[string]any{"ref": ref, "minutes": 3.5}), nil
	case "rm":
		f.mu.Lock()
		delete(f.files, p)
		f.removed[p]++
		f.mu.Unlock()
		return okJSONText(map[string]any{"ok": true}), nil
	case "import":
		return f.fileImport(ctx, in)
	}
	return tools.Result{IsError: true, Text: "File: unexpected op " + op}, nil
}

var mdHeading = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+|$)(.*)$`)

// fileImport stands in for File op=import: the headings applied to the file
// as written, the title as the root, then the REAL Document import_md (with
// the source as provenance), then the tags.
func (f *skyFake) fileImport(ctx context.Context, in map[string]any) (tools.Result, error) {
	p, _ := in["path"].(string)
	f.mu.Lock()
	md, ok := f.files[p]
	f.mu.Unlock()
	if !ok {
		return tools.Result{IsError: true, Text: "import: no such file " + p}, nil
	}
	lines := strings.Split(md, "\n")
	if hs, _ := in["headings"].([]any); len(hs) > 0 {
		if f.importRefusesHeadings {
			return tools.Result{IsError: true, Text: "import: headings: line 3 is blank. Nothing was imported."}, nil
		}
		for _, h := range hs {
			e := h.(map[string]any)
			n := int(e["line"].(float64))
			lvl := int(e["level"].(float64))
			if n < 1 || n > len(lines) {
				return tools.Result{IsError: true, Text: "import: headings: line outside the file. Nothing was imported."}, nil
			}
			cur := mdHeading.FindStringSubmatch(lines[n-1])
			switch e["as"] {
			case "heading":
				if cur != nil || strings.TrimSpace(lines[n-1]) == "" {
					return tools.Result{IsError: true, Text: "import: headings: line is already a heading. Nothing was imported."}, nil
				}
				lines[n-1] = strings.Repeat("#", lvl) + " " + strings.TrimSpace(lines[n-1])
			case "level":
				if cur == nil || len(cur[1]) == lvl {
					return tools.Result{IsError: true, Text: "import: headings: a re-level to the level it has. Nothing was imported."}, nil
				}
				lines[n-1] = strings.Repeat("#", lvl) + " " + cur[2]
			default:
				return tools.Result{IsError: true, Text: "import: headings: unexpected as"}, nil
			}
		}
	}
	if title, _ := in["title"].(string); title != "" {
		if mdHeading.MatchString(lines[0]) {
			lines[0] = "# " + title
		} else {
			lines = append([]string{"# " + title, ""}, lines...)
		}
	}
	call := map[string]any{"op": "import_md", "scope": in["scope"], "markdown": strings.Join(lines, "\n"), "path": in["doc_path"]}
	for _, k := range []string{"reimport", "work_key"} {
		if v, ok := in[k]; ok {
			call[k] = v
		}
	}
	if src, _ := in["source"].(map[string]any); src != nil {
		for k, v := range map[string]string{"url": "source_url", "kind": "source_kind", "title": "source_title", "revision": "source_revision", "updated_at": "source_updated_at"} {
			if s, ok := src[k]; ok {
				call[v] = s
			}
		}
	}
	b, _ := json.Marshal(call)
	res, err := f.docs.Execute(ctx, b)
	if err != nil || res.IsError {
		return res, err
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	if tags, _ := in["tags"].([]any); len(tags) > 0 {
		tb, _ := json.Marshal(map[string]any{"op": "add_tags", "scope": in["scope"], "document_id": out["document_id"], "tags": append(tags, "untrusted")})
		if tr, err := f.docs.Execute(ctx, tb); err != nil || tr.IsError {
			return tools.Result{IsError: true, Text: "import: tags: " + tr.Text}, err
		}
	}
	out["provenance"] = map[string]any{"captured_at": "2026-11-07T08:00:00Z", "run_id": tools.RunID(ctx)}
	return okJSONText(out), nil
}

// ─── Agent: the import children and the structurer ───────────────────

type skyFakeAgent struct {
	f *skyFake
}

func (a *skyFakeAgent) Name() string                 { return "Agent" }
func (a *skyFakeAgent) Description() string          { return "agent (test double)" }
func (a *skyFakeAgent) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (a *skyFakeAgent) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	f := a.f
	in := f.record("Agent", raw)
	if in["op"] == "parallel_spawn" {
		spawns, _ := in["spawns"].([]any)
		if len(spawns) > 3 {
			return tools.Result{IsError: true, Text: "parallel_spawn: more than 3 spawns"}, nil
		}
		rows := []any{}
		for i, s := range spawns {
			e := s.(map[string]any)
			if e["name"] != "skybits/import" {
				rows = append(rows, map[string]any{"index": i, "ok": false, "error": "unexpected child " + fmt.Sprint(e["name"])})
				continue
			}
			runID, text, err := f.runImport(ctx, e["prompt"].(string))
			if err != nil {
				rows = append(rows, map[string]any{"index": i, "agent": "skybits/import", "ok": false, "error": err.Error(), "run_id": runID})
				continue
			}
			rows = append(rows, map[string]any{"index": i, "agent": "skybits/import", "ok": true, "run_id": runID,
				"result": map[string]any{"run_id": runID, "status": "completed", "final_text": text}})
		}
		return okJSONText(map[string]any{"results": rows}), nil
	}
	if in["name"] != "skybits/structure" {
		return tools.Result{IsError: true, Text: "spawn: unexpected child " + fmt.Sprint(in["name"])}, nil
	}
	payload := ""
	if us, _ := in["untrusted"].([]any); len(us) > 0 {
		payload, _ = us[0].(map[string]any)["text"].(string)
	}
	f.mu.Lock()
	f.structurePayloads = append(f.structurePayloads, payload)
	f.mu.Unlock()
	if f.structure == nil {
		return okJSONText(map[string]any{"status": "completed", "structured": map[string]any{"headings": []any{}, "notes": "none"}}), nil
	}
	switch r := f.structure(payload).(type) {
	case string:
		return okJSONText(map[string]any{"status": "completed", "final_text": r}), nil
	default:
		return okJSONText(map[string]any{"status": "completed", "structured": r}), nil
	}
}

// skyCtx is the run context of agent name: its tenant identity and the policy
// the runtime derives from its definition plus the deployment's tenant grants.
func (f *skyFake) skyCtx(name, runID string) context.Context {
	a := f.cfg.Agents[name]
	ctx := tools.WithAgentName(context.Background(), name)
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a-" + runID, UserID: "operator", TenantID: "tnt"})
	ctx = tools.WithRunID(ctx, runID)
	return tools.WithRunPolicy(ctx, tools.RunPolicy{
		Tools:            a.Tools,
		StatementMethods: a.StatementMethods,
		Memory:           tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
		SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
	})
}

// toolsFor is the tool set agent name holds: exactly its ceiling, so a body
// that calls anything else fails with "not defined".
func (f *skyFake) toolsFor(name string) []tools.Tool {
	pool := map[string]tools.Tool{
		"Document": f.docs, "Memory": f.mem, "Path": f.paths, "File": &skyFakeFile{f: f}, "Agent": &skyFakeAgent{f: f},
		"HTTP": &skyNoTool{name: "HTTP"},
	}
	var out []tools.Tool
	for _, n := range f.cfg.Agents[name].Tools {
		if strings.HasPrefix(n, "mcp__skybits__") {
			out = append(out, &skyTool{f: f, name: strings.TrimPrefix(n, "mcp__skybits__")})
			continue
		}
		tl, ok := pool[n]
		if !ok {
			f.t.Fatalf("no test tool for %s", n)
		}
		out = append(out, tl)
	}
	return out
}

// skyNoTool is a granted tool the body must never call directly (HTTP: File
// download goes through it in the runtime).
type skyNoTool struct{ name string }

func (n *skyNoTool) Name() string                 { return n.name }
func (n *skyNoTool) Description() string          { return "unused" }
func (n *skyNoTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (n *skyNoTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{IsError: true, Text: n.name + " must not be called directly"}, nil
}

func (f *skyFake) runAgent(ctx context.Context, name, input string) (string, error) {
	set := f.toolsFor(name)
	res, err := loop.Run(ctx, loop.RunOptions{
		Provider:   codejs.New(codejs.Config{CodeRoot: f.t.TempDir(), RunTimeout: codejs.DefaultRunTimeout}),
		Model:      "code-js",
		AgentName:  name,
		CodeBody:   f.cfg.Agents[name].Code,
		Tools:      set,
		Dispatcher: tools.NewDispatcher(set),
		Segments:   []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: input}}}},
	})
	if err != nil {
		return "", err
	}
	return res.FinalText, nil
}

func (f *skyFake) runImport(_ context.Context, prompt string) (string, string, error) {
	f.mu.Lock()
	f.childSeq++
	runID := fmt.Sprintf("run-import-%d", f.childSeq)
	f.mu.Unlock()
	text, err := f.runAgent(f.skyCtx("skybits/import", runID), "skybits/import", prompt)
	return runID, text, err
}

// sync runs skybits/sync with input and returns its report.
func (f *skyFake) sync(input string) map[string]any {
	f.t.Helper()
	text, err := f.runAgent(f.skyCtx("skybits/sync", "run-sync"), "skybits/sync", input)
	if err != nil {
		f.t.Fatalf("skybits/sync: %v", err)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(text), &rep); err != nil {
		f.t.Fatalf("report: %v\n%s", err, text)
	}
	f.t.Logf("report: %s", text)
	return rep
}

// syncErr runs skybits/sync expecting the run to fail.
func (f *skyFake) syncErr(input string) error {
	f.t.Helper()
	_, err := f.runAgent(f.skyCtx("skybits/sync", "run-sync"), "skybits/sync", input)
	return err
}

// ─── Store reads ─────────────────────────────────────────────────────

func (f *skyFake) doc(body string) (map[string]any, bool) {
	f.t.Helper()
	ctx := f.skyCtx("skybits/sync", "run-test")
	res, err := f.docs.Execute(ctx, json.RawMessage(body))
	if err != nil {
		f.t.Fatalf("Document %s: %v", body, err)
	}
	if res.IsError {
		return map[string]any{"error": res.Text}, false
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	return out, true
}

func (f *skyFake) mustDoc(body string) map[string]any {
	f.t.Helper()
	out, ok := f.doc(body)
	if !ok {
		f.t.Fatalf("Document %s: %v", body, out["error"])
	}
	return out
}

func (f *skyFake) sql(op, stmt string, args ...any) map[string]any {
	f.t.Helper()
	ctx := f.skyCtx("skybits/sync", "run-test")
	b, _ := json.Marshal(map[string]any{"op": op, "scope": "tenant", "statement": stmt, "args": args})
	res, err := f.mem.Execute(ctx, b)
	if err != nil || res.IsError {
		f.t.Fatalf("Memory %s %s: %v %s", op, stmt, err, res.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	return out
}

// manifestRow reads one manifest row as column → value.
func (f *skyFake) manifestRow(id string) map[string]any {
	f.t.Helper()
	r := f.sql("sql_query", "SELECT item_id, status, reason, document_id, primary_path, paths_json, revision, updated_at, tags_json, source_status, fail_count FROM skybits_manifest WHERE item_id = $1", id)
	rows, _ := r["rows"].([]any)
	if len(rows) == 0 {
		return nil
	}
	cols := []string{"item_id", "status", "reason", "document_id", "primary_path", "paths_json", "revision", "updated_at", "tags_json", "source_status", "fail_count"}
	out := map[string]any{}
	for i, v := range rows[0].([]any) {
		out[cols[i]] = v
	}
	return out
}
