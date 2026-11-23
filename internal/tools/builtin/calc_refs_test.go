package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/calc"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// calcRefCtx is a run's ctx for Calc references: a run id, a bound default
// volume at dir and the effective tools Calc plus toolNames.
func calcRefCtx(ctx context.Context, dir string, toolNames ...string) context.Context {
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Volumes = tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{Name: "work", Root: dir, Default: true}}}
		rp.Tools = append([]string{"Calc"}, toolNames...)
	})
	return tools.WithRunID(ctx, "run_calc")
}

func calcBase() context.Context {
	return tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"})
}

func calcExec(t *testing.T, c *Calc, ctx context.Context, body string) (map[string]any, tools.Result) {
	t.Helper()
	res, err := c.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute hard error: %v", err)
	}
	var out map[string]any
	if !res.IsError {
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("result is not JSON: %s", res.Text)
		}
	}
	return out, res
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const calcSales = "region,amount\nEU,120.10\nUS,300\nEU,79.90\n"

// A table read from a volume is computed on, and a result written to one is
// on disk byte for byte while the tool result carries none of it.
func TestCalc_FromAndToAVolumeKeepTheDataOutOfTheResult(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "sales.csv", calcSales)
	c := &Calc{}
	ctx := calcRefCtx(calcBase(), dir, "Read", "Write")

	out, res := calcExec(t, c, ctx, `{"expr":"sum(t.amount)","from":{"t":"volume:sales.csv"}}`)
	mustOK(t, "from", res)
	if out["text"] != "500" || out["exact"] != true {
		t.Fatalf("sum of the file = %v", out)
	}

	out, res = calcExec(t, c, ctx, `{"expr":"groupby(t, \"region\", \"total\", g -> sum(g.amount))","from":{"t":"volume:work:sales.csv"},"to":"volume:summary.csv"}`)
	mustOK(t, "to", res)
	got, _ := os.ReadFile(filepath.Join(dir, "summary.csv"))
	if string(got) != "region,total\nEU,200\nUS,300\n" {
		t.Fatalf("written file = %q", got)
	}
	for _, leak := range []string{"EU", "US", "200", "300", "region", "total"} {
		if strings.Contains(res.Text, leak) {
			t.Errorf("the result carries the content (%q): %s", leak, res.Text)
		}
	}
	if out["to"] != "volume:summary.csv" || out["format"] != "csv" || out["type"] != "table" || out["bytes"] != float64(len(got)) {
		t.Errorf("to result = %v", out)
	}
	if s, _ := out["shape"].([]any); len(s) != 2 || s[0] != float64(2) || s[1] != float64(2) {
		t.Errorf("shape = %v", out["shape"])
	}
	for _, k := range []string{"result", "assigned", "fraction"} {
		if _, ok := out[k]; ok {
			t.Errorf("the result has %s", k)
		}
	}
	// The written file reads back.
	out, res = calcExec(t, c, ctx, `{"expr":"sum(t.total)","from":{"t":"volume:summary.csv"}}`)
	mustOK(t, "read back", res)
	if out["text"] != "500" {
		t.Errorf("read back = %v", out["text"])
	}
}

// A reference is refused, with nothing read or written and no content in the
// refusal, when the run lacks the tool, the path leaves the volume, the
// volume is read-only or unbound, or the reference is of a kind Calc does
// not take.
func TestCalc_ReferencesAreRefusedOutsideTheAgentsAuthority(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, outside, "secret.csv", "a\n424242\n")
	dir := t.TempDir()
	writeFile(t, dir, "a.csv", "a\n1\n")
	if err := os.Symlink(filepath.Join(outside, "secret.csv"), filepath.Join(dir, "link.csv")); err != nil {
		t.Fatal(err)
	}
	base := calcBase()
	full := calcRefCtx(base, dir, "Read", "Write", "Memory", "Document")
	readOnly := tools.UpdateRunPolicy(full, func(rp *tools.RunPolicy) {
		rp.Volumes = tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{Name: "work", Root: dir, Default: true, ReadOnly: true}}}
	})
	// The definition's patterns grant Read and Write; the run was narrowed
	// to Calc. The effective list decides.
	narrowed := tools.UpdateRunPolicy(calcRefCtx(base, dir), func(rp *tools.RunPolicy) { rp.ToolPatterns = []string{"Calc", "Read", "Write"} })
	c := &Calc{}
	for name, tc := range map[string]struct {
		ctx  context.Context
		body string
		cat  tools.ErrorCategory
	}{
		"no Read tool":        {calcRefCtx(base, dir, "Write"), `{"expr":"t","from":{"t":"volume:a.csv"}}`, tools.CategoryPermission},
		"no Write tool":       {calcRefCtx(base, dir, "Read"), `{"expr":"1","to":"volume:b.csv"}`, tools.CategoryPermission},
		"narrowed run":        {narrowed, `{"expr":"1","to":"volume:b.json"}`, tools.CategoryPermission},
		"no tool list":        {tools.UpdateRunPolicy(full, func(rp *tools.RunPolicy) { rp.Tools = nil }), `{"expr":"t","from":{"t":"volume:a.csv"}}`, tools.CategoryPermission},
		"dot-dot escape":      {full, `{"expr":"t","from":{"t":"volume:../` + filepath.Base(outside) + `/secret.csv"}}`, tools.CategoryPermission},
		"absolute path":       {full, `{"expr":"t","from":{"t":"volume:` + filepath.Join(outside, "secret.csv") + `"}}`, tools.CategoryPermission},
		"symlink escape":      {full, `{"expr":"t","from":{"t":"volume:link.csv"}}`, tools.CategoryPermission},
		"write escape":        {full, `{"expr":"1","to":"volume:../` + filepath.Base(outside) + `/b.json"}`, tools.CategoryPermission},
		"read-only volume":    {readOnly, `{"expr":"1","to":"volume:b.json"}`, tools.CategoryPermission},
		"unbound volume":      {full, `{"expr":"t","from":{"t":"volume:other:a.csv"}}`, tools.CategoryPermission},
		"workspace":           {full, `{"expr":"t","from":{"t":"workspace:w:a.csv"}}`, tools.CategoryValidation},
		"memory without tool": {calcRefCtx(base, dir, "Read"), `{"expr":"t","from":{"t":"memory:user/k"}}`, tools.CategoryPermission},
		"doc without tool":    {calcRefCtx(base, dir, "Read"), `{"expr":"t","from":{"t":"document:user/c1"}}`, tools.CategoryPermission},
		"unknown kind":        {full, `{"expr":"t","from":{"t":"s3:bucket/x"}}`, tools.CategoryValidation},
	} {
		_, res := calcExec(t, c, tc.ctx, tc.body)
		if !res.IsError || res.Error == nil || res.Error.Category != tc.cat {
			t.Errorf("%s: %+v, want a %s refusal", name, res, tc.cat)
		}
		if strings.Contains(res.Text, "424242") {
			t.Errorf("%s: the refusal carries the outside file's content", name)
		}
	}
	for _, f := range []string{filepath.Join(dir, "b.csv"), filepath.Join(dir, "b.json"), filepath.Join(outside, "b.json")} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("a refused call wrote %s", f)
		}
	}
}

// A `to` that cannot be written is refused before any `from` is read.
func TestCalc_AnUnwritableToReadsNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.csv", "a\n1\n")
	var asked []string
	ctx := tools.WithHookPreCheck(calcRefCtx(calcBase(), dir, "Read"), func(_ context.Context, name string, _ json.RawMessage) (tools.Result, bool) {
		asked = append(asked, name)
		return tools.Result{}, true
	})
	if _, res := calcExec(t, &Calc{}, ctx, `{"expr":"t","from":{"t":"volume:a.csv"},"to":"volume:b.csv"}`); !res.IsError {
		t.Fatal("a to without Write was accepted")
	}
	if len(asked) != 0 {
		t.Errorf("the from was judged (%v) before the to was refused", asked)
	}
}

// A reference is judged by the referenced tool's pre-hooks, asked via Calc
// with where and never what: a deny refuses it before anything is read or
// written.
func TestCalc_AReferenceIsRefusedByTheReferencedToolsHooks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "in.csv", "a\n1\n")
	var asked []string
	ctx := tools.WithHookPreCheck(calcRefCtx(calcBase(), dir, "Read", "Write"), func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		asked = append(asked, name+" "+string(input))
		if strings.Contains(string(input), "denied") {
			return tools.Result{Text: "not that file", IsError: true}, false
		}
		return tools.Result{}, true
	})
	c := &Calc{}
	_, res := calcExec(t, c, ctx, `{"expr":"\"TOP-SECRET\"","to":"volume:denied.json"}`)
	if !res.IsError || res.Error.Category != tools.CategoryPermission || !strings.Contains(res.Text, "not that file") {
		t.Fatalf("denied write = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "denied.json")); err == nil {
		t.Error("a hook-denied write happened")
	}
	if len(asked) != 1 || !strings.HasPrefix(asked[0], "Write ") || !strings.Contains(asked[0], `"via":"Calc"`) || strings.Contains(asked[0], "TOP-SECRET") {
		t.Errorf("the hook was asked %v", asked)
	}
	writeFile(t, dir, "denied.csv", "a\n7\n")
	asked = nil
	if _, res := calcExec(t, c, ctx, `{"expr":"t","from":{"t":"volume:denied.csv"}}`); !res.IsError || res.Error.Category != tools.CategoryPermission || strings.Contains(res.Text, "7") {
		t.Errorf("denied read = %+v", res)
	}
	asked = nil
	if out, res := calcExec(t, c, ctx, `{"expr":"sum(t.a)","from":{"t":"volume:in.csv"}}`); res.IsError || out["text"] != "1" || len(asked) != 1 || !strings.HasPrefix(asked[0], "Read ") {
		t.Errorf("allowed read = %+v, asked %v", res, asked)
	}
}

// Outside a run `from` and `to` are refused by the tool itself, whatever the
// key's case, even when the run's tools and a volume are in the ctx.
func TestCalc_DataByReferenceIsRefusedWithoutARun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.csv", "a\n1\n")
	inRun := calcRefCtx(calcBase(), dir, "Read", "Write")
	noRunID := tools.UpdateRunPolicy(calcBase(), func(rp *tools.RunPolicy) {
		rp.Volumes = tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{Name: "work", Root: dir, Default: true}}}
		rp.Tools = []string{"Calc", "Read", "Write"}
	})
	offRun := tools.WithOffRunVolumes(tools.WithCallerTier(noRunID, tools.TierOperatorPlane))
	c := &Calc{}
	for name, ctx := range map[string]context.Context{"no run id": noRunID, "operator plane": offRun} {
		for _, body := range []string{
			`{"expr":"t","from":{"t":"volume:a.csv"}}`,
			`{"expr":"1","to":"volume:b.json"}`,
			`{"expr":"t","FROM":{"t":"volume:a.csv"}}`,
			`{"expr":"t","from":{}}`,
		} {
			_, res := calcExec(t, c, ctx, body)
			if !res.IsError || res.Error.Category != tools.CategoryValidation || !strings.Contains(res.Text, "needs a run") {
				t.Errorf("%s %s: %+v, want the no-run refusal", name, body, res)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "b.json")); err == nil {
		t.Error("an off-run call wrote b.json")
	}
	if _, res := calcExec(t, c, inRun, `{"expr":"len(t)","from":{"t":"volume:a.csv"}}`); res.IsError {
		t.Errorf("in a run: %s", res.Text)
	}
}

// A file over the byte cap is refused, never truncated; the cap holds over
// every source together.
func TestCalc_FromRefusesDataOverTheByteCap(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "huge.csv"))
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: a terabyte on paper, nothing on disk.
	if err := f.Truncate(1 << 40); err != nil {
		t.Skipf("sparse file: %v", err)
	}
	f.Close()
	ctx := calcRefCtx(calcBase(), dir, "Read")
	c := &Calc{}
	if _, res := calcExec(t, c, ctx, `{"expr":"1","from":{"t":"volume:huge.csv"}}`); !res.IsError || !strings.Contains(res.Text, "more than") {
		t.Errorf("a terabyte file = %+v", res)
	}
	half := strings.Repeat("1\n", calc.MaxRefBytes/4+8)
	writeFile(t, dir, "a.csv", "a\n"+half)
	writeFile(t, dir, "b.csv", "b\n"+half)
	if _, res := calcExec(t, c, ctx, `{"expr":"1","from":{"a":"volume:a.csv","b":"volume:b.csv"}}`); !res.IsError || !strings.Contains(res.Text, "more than") {
		t.Errorf("two files past the cap together = %+v", res)
	}
	many := map[string]string{}
	for i := 0; i <= calc.MaxRefSources; i++ {
		many["t"+strings.Repeat("x", i)] = "volume:a.csv"
	}
	body, _ := json.Marshal(map[string]any{"expr": "1", "from": many})
	if _, res := calcExec(t, c, ctx, string(body)); !res.IsError || !strings.Contains(res.Text, "sources") {
		t.Errorf("%d sources = %+v", len(many), res)
	}
}

// Memory references go through Memory's scope grant and the caller's own
// identity: another user's or another tenant's key is not reachable.
func TestCalc_MemoryReferencesStayInTheCallersScope(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := &Calc{Memory: &Memory{Store: s, MaxValueBytes: 65536, DefaultQuotaBytes: 1 << 20}}
	as := func(user, tenant string, scopes ...string) context.Context {
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a", UserID: user, TenantID: tenant})
		ctx = calcRefCtx(ctx, t.TempDir(), "Memory")
		return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Memory = tools.MemoryPolicyValue{AllowedScopes: scopes} })
	}
	u1 := as("u1", "tnt", "user", "tenant")
	out, res := calcExec(t, c, u1, `{"expr":"csv(\"a\\n1\\n2\")","to":"memory:user/nums.csv"}`)
	mustOK(t, "write user memory", res)
	if out["format"] != "csv" || strings.Contains(res.Text, "a\\n1") {
		t.Fatalf("to memory = %s", res.Text)
	}
	_, res = calcExec(t, c, u1, `{"expr":"[7, 8]","to":"memory:tenant/shared"}`)
	mustOK(t, "write tenant memory", res)
	out, res = calcExec(t, c, u1, `{"expr":"sum(t.a) + sum(s)","from":{"t":"memory:user/nums.csv","s":"memory:tenant/shared"}}`)
	mustOK(t, "read back", res)
	if out["text"] != "18" {
		t.Fatalf("read back = %v", out["text"])
	}
	for name, tc := range map[string]struct {
		ctx context.Context
		ref string
	}{
		"another user":      {as("u2", "tnt", "user", "tenant"), "memory:user/nums.csv"},
		"another tenant":    {as("u1", "other", "user", "tenant"), "memory:tenant/shared"},
		"scope not granted": {as("u1", "tnt", "user"), "memory:tenant/shared"},
	} {
		_, res := calcExec(t, c, tc.ctx, `{"expr":"t","from":{"t":"`+tc.ref+`"}}`)
		if !res.IsError || strings.Contains(res.Text, "7") {
			t.Errorf("%s: %+v, want a refusal without the data", name, res)
		}
	}
	if _, res := calcExec(t, c, u1, `{"expr":"1","to":"memory:user/doc.chunk:abc"}`); !res.IsError {
		t.Error("a chunk body key was written through memory")
	}
}

// Document references read a chunk's first Markdown table and write a
// Markdown table through the Document tool; another user's chunk is not
// reachable.
func TestCalc_DocumentReferencesGoThroughTheDocumentTool(t *testing.T) {
	d, dctx, _ := documentFixture(t)
	docID, rootID := canvasCreateDoc(t, d, dctx, "report")
	chunkID := canvasCreateChunk(t, d, dctx, docID, rootID, "figures", "Quarterly figures.\n\n| item | cost |\n| --- | --- |\n| rent | 1200.50 |\n| power | 99.50 |\n")
	outID := canvasCreateChunk(t, d, dctx, docID, rootID, "totals", "to be replaced")
	c := &Calc{Docs: d}
	ctx := calcRefCtx(dctx, t.TempDir(), "Document")

	out, res := calcExec(t, c, ctx, `{"expr":"sum(t.cost)","from":{"t":"document:user/`+chunkID+`"}}`)
	mustOK(t, "read chunk", res)
	if out["text"] != "1300" {
		t.Fatalf("sum of the chunk's table = %v", out["text"])
	}
	_, res = calcExec(t, c, ctx, `{"expr":"totals(t)","from":{"t":"document:user/`+chunkID+`"},"to":"document:user/`+outID+`"}`)
	mustOK(t, "write chunk", res)
	if strings.Contains(res.Text, "1300") || strings.Contains(res.Text, "rent") {
		t.Errorf("the result carries the content: %s", res.Text)
	}
	got, gres := docExec(t, d, dctx, `{"op":"get_chunk","scope":"user","id":"`+outID+`"}`)
	mustOK(t, "get_chunk", gres)
	if body := got["body"].(string); body != "| item | cost |\n| --- | --- |\n| rent | 1200.5 |\n| power | 99.5 |\n| total | 1300 |\n" {
		t.Errorf("written chunk = %q", body)
	}

	other := tools.WithRunIdentity(dctx, tools.RunIdentityValue{AgentID: "a", UserID: "u2", TenantID: "tnt"})
	if _, res := calcExec(t, c, calcRefCtx(other, t.TempDir(), "Document"), `{"expr":"t","from":{"t":"document:user/`+chunkID+`"}}`); !res.IsError || strings.Contains(res.Text, "1200") {
		t.Errorf("another user's chunk = %+v", res)
	}
	otherTenant := tools.WithRunIdentity(dctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "other"})
	if _, res := calcExec(t, c, calcRefCtx(otherTenant, t.TempDir(), "Document"), `{"expr":"t","from":{"t":"document:user/`+chunkID+`"}}`); !res.IsError || strings.Contains(res.Text, "1200") {
		t.Errorf("another tenant's chunk = %+v", res)
	}
}

// The write format is `format`, else the name's extension, else Markdown for
// a chunk and JSON otherwise; a format the name contradicts is refused.
func TestCalc_TheWriteFormatFollowsFormatOrTheName(t *testing.T) {
	dir := t.TempDir()
	ctx := calcRefCtx(calcBase(), dir, "Write")
	c := &Calc{}
	const tbl = `csv(\"a,b\\n1,2\")`
	for body, want := range map[string]string{
		`{"expr":"` + tbl + `","to":"volume:x.md"}`:                     "| a | b |\n| --- | --- |\n| 1 | 2 |\n",
		`{"expr":"` + tbl + `","to":"volume:x.out"}`:                    `{"columns":["a","b"],"rows":[[1,2]]}`,
		`{"expr":"` + tbl + `","to":"volume:x.out","format":"csv"}`:     "a,b\n1,2\n",
		`{"expr":"` + tbl + `","to":"volume:x.json","format":"json"}`:   `{"columns":["a","b"],"rows":[[1,2]]}`,
		`{"expr":"0.1 + 0.2","to":"volume:x.json"}`:                     "0.3",
		`{"expr":"` + tbl + `","to":"volume:sub/x.csv","format":"csv"}`: "",
	} {
		_, res := calcExec(t, c, ctx, body)
		if want == "" { // a missing parent folder is the store's refusal
			if !res.IsError {
				t.Errorf("%s: written into a missing folder", body)
			}
			continue
		}
		mustOK(t, body, res)
		var to string
		_ = json.Unmarshal([]byte(`"`+strings.Split(strings.Split(body, `"to":"volume:`)[1], `"`)[0]+`"`), &to)
		got, _ := os.ReadFile(filepath.Join(dir, to))
		if string(got) != want {
			t.Errorf("%s wrote %q, want %q", body, got, want)
		}
	}
	for _, body := range []string{
		`{"expr":"` + tbl + `","to":"volume:x.json","format":"csv"}`,
		`{"expr":"` + tbl + `","to":"volume:x.tsv"}`,
		`{"expr":"[1, 2]","to":"volume:x.csv"}`,
	} {
		if _, res := calcExec(t, c, ctx, body); !res.IsError || res.Error.Category != tools.CategoryValidation {
			t.Errorf("%s: %+v, want a validation refusal", body, res)
		}
	}
}
