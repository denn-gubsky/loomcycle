package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/calc"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Data by reference for Calc (RFC GY P3b): `from` reads named data from a
// volume file, a Memory key or a Document chunk, and `to` writes the result
// to one, so a large table never passes through the model.
//
// Calc adds no reach of its own. A reference goes through the seam Crypto's
// from/to use (checkRefGrant, then refStores): the run must hold the matching
// tool (Read for a volume read, Write for a volume write, Memory, Document) in
// its EFFECTIVE tools, that tool's pre-hooks are asked (via "Calc"), and each
// store applies its own checks — the volume bindings, fence and read-only
// flag, Memory's scope grant and set, the Document tool's scope and revision.
// What is read is handed to the evaluator as data, never as expression text.

// calcRefInstead is the alternative a refused reference offers.
const calcRefInstead = "Pass the data inline in vars"

// calcNoRun refuses from and to outside a run, in the shape and words of the
// transports' own guard (calcRefuseDataByReference in the HTTP connector), so
// a path that reaches the tool without that guard is refused the same way.
func calcNoRun() tools.Result {
	return calcRefusal(&calc.Error{Code: "bad_request", Pos: -1,
		Detail: "data by reference needs a run: `from` and `to` are refused outside one",
		Hint:   "pass the data inline in `vars`, or call Calc from inside a run, where `from` and `to` reach the agent's stores"})
}

// calcRefs is what prepareRefs resolved: the data to bind and, with `to`,
// where the result goes and in which format.
type calcRefs struct {
	data        map[string]calc.Data
	to          *dataRef
	writeFormat string
}

// prepareRefs judges every reference before anything is read — `to` first,
// so a call that cannot store its result reads nothing — then reads each
// `from` within what is left of calc.MaxRefBytes.
func (c *Calc) prepareRefs(ctx context.Context, in calcInput) (calcRefs, tools.Result, bool) {
	var out calcRefs
	// Off a run there is no agent whose stores these would be. The transports
	// refuse first; this holds even when one does not.
	if tools.IsOffRunPlane(ctx) || tools.RunID(ctx) == "" {
		return out, calcNoRun(), false
	}
	if len(in.From) > calc.MaxRefSources {
		return out, calcRefusal(&calc.Error{Code: "bad_request", Pos: -1,
			Detail: fmt.Sprintf("from names %d sources; the limit is %d", len(in.From), calc.MaxRefSources),
			Hint:   "read fewer sources per call, or combine them into one file"}), false
	}
	stores := refStores{Memory: c.Memory, Docs: c.Docs}
	if in.To != nil {
		r, err := parseDataRef(*in.To)
		if err != nil {
			return out, calcRefusal(&calc.Error{Code: "bad_request", Pos: -1, Detail: "to: " + err.Error()}), false
		}
		format, cerr := calcWriteFormat(r, in.Format)
		if cerr != nil {
			return out, calcRefusal(cerr), false
		}
		if res, ok := checkRefGrant(ctx, r, true, c.Name(), calcRefInstead); !ok {
			return out, res, false
		}
		out.to, out.writeFormat = &r, format
	}
	names := make([]string, 0, len(in.From))
	for name := range in.From {
		names = append(names, name)
	}
	sort.Strings(names)
	refs := make([]dataRef, len(names))
	for i, name := range names {
		r, err := parseDataRef(in.From[name])
		if err != nil {
			return out, calcRefusal(&calc.Error{Code: "bad_request", Pos: -1, Detail: "from " + name + ": " + err.Error()}), false
		}
		if res, ok := checkRefGrant(ctx, r, false, c.Name(), calcRefInstead); !ok {
			return out, res, false
		}
		refs[i] = r
	}
	left := calc.MaxRefBytes
	for i, name := range names {
		// readRef refuses more than left bytes, checking a volume file's
		// size before reading it, so the cap holds over every source.
		b, res, ok := stores.readRef(ctx, refs[i], left)
		if !ok {
			return out, res, false
		}
		left -= len(b)
		if out.data == nil {
			out.data = map[string]calc.Data{}
		}
		out.data[name] = calc.Data{Bytes: b, Format: calcReadFormat(refs[i], b)}
	}
	return out, tools.Result{}, true
}

// refExt is the lower-cased extension of a volume path or a Memory key, the
// one name a reference carries that can say what the data is.
func refExt(r dataRef) string {
	switch r.kind {
	case refVolume:
		return strings.ToLower(path.Ext(r.path))
	case refMemory:
		return strings.ToLower(path.Ext(r.key))
	}
	return ""
}

var calcExtFormat = map[string]string{
	".csv": calc.FormatCSV, ".tsv": calc.FormatTSV, ".tab": calc.FormatTSV,
	".json": calc.FormatJSON, ".md": calc.FormatMarkdown, ".markdown": calc.FormatMarkdown,
}

// calcReadFormat is how a source is read: a Document chunk is Markdown (its
// first table); otherwise the name's extension decides, and without one the
// first bytes do (calc.SniffFormat).
func calcReadFormat(r dataRef, b []byte) string {
	if r.kind == refDocument {
		return calc.FormatMarkdown
	}
	if f, ok := calcExtFormat[refExt(r)]; ok {
		return f
	}
	return calc.SniffFormat(b)
}

// calcWriteFormat is how the result is written to r: `format` when given,
// else the name's extension, else Markdown for a Document chunk and JSON
// otherwise. A `format` the name contradicts (csv to report.json) is refused
// rather than writing a file whose name lies about it.
func calcWriteFormat(r dataRef, format string) (string, *calc.Error) {
	byName, named := calcExtFormat[refExt(r)]
	if named && byName == calc.FormatTSV {
		return "", &calc.Error{Code: "bad_request", Pos: -1, Detail: "to: " + r.raw + " names a TSV file, and Calc writes csv, json or markdown",
			Hint: "name the file .csv, .json or .md"}
	}
	switch {
	case format != "" && named && format != byName:
		return "", &calc.Error{Code: "bad_request", Pos: -1, Detail: fmt.Sprintf("to: format is %s, and %s names a %s file", format, r.raw, byName),
			Hint: "drop format, or give the file the extension of the format"}
	case format != "":
		return format, nil
	case named:
		return byName, nil
	case r.kind == refDocument:
		return calc.FormatMarkdown, nil
	}
	return calc.FormatJSON, nil
}

// writeResult stores the rendered result at the `to` reference and returns
// what the model sees instead of it: where, in which format, the type, shape
// and size — never the content.
func (c *Calc) writeResult(ctx context.Context, refs calcRefs, resp calc.Response) tools.Result {
	stores := refStores{Memory: c.Memory, Docs: c.Docs}
	if res, ok := stores.writeRef(ctx, *refs.to, resp.Written, ".loomcycle-calc-"); !ok {
		return res
	}
	out := map[string]any{
		"to": refs.to.raw, "format": refs.writeFormat, "type": resp.Type, "exact": resp.Exact,
		"bytes": len(resp.Written), "text": calcWroteText(refs, resp),
	}
	if len(resp.Shape) > 0 {
		out["shape"] = resp.Shape
	}
	body, _ := json.Marshal(out)
	return tools.Result{Text: string(body)}
}

// calcWroteText is the one line a model reads after a write.
func calcWroteText(refs calcRefs, resp calc.Response) string {
	what := "a " + resp.Type
	switch {
	case resp.Type == "table" && len(resp.Shape) == 2:
		what = fmt.Sprintf("a table of %d rows and %d columns", resp.Shape[0], resp.Shape[1])
	case len(resp.Shape) == 2:
		what = fmt.Sprintf("a %d × %d matrix", resp.Shape[0], resp.Shape[1])
	case len(resp.Shape) == 1:
		what = fmt.Sprintf("a list of %d", resp.Shape[0])
	}
	return fmt.Sprintf("wrote %s, %d bytes of %s, to %s", what, len(resp.Written), refs.writeFormat, refs.to.raw)
}
