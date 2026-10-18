package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Data by reference (RFC EP P2b): a Crypto call may read its input from, and
// write its output to, a volume file, a Memory key or a Document chunk, so the
// bytes never pass through the model.
//
// The authority is exactly the agent's own. Each store is reached through the
// same internal seams its tool uses (the volume resolver and fence, Memory's
// scope check and every write check, the Document tool itself), never around
// them and never through a tool's text output, which would truncate a file or
// re-encode bytes. Those seams sit below the per-run dispatcher that enforces
// `tools:`, so the agent must also hold the matching tool, checked here; the
// tool-use hooks of Read, Write, Memory and Document do not fire on this path.

type refKind string

const (
	refVolume   refKind = "volume"
	refMemory   refKind = "memory"
	refDocument refKind = "document"
)

// dataRef is a parsed `from` / `to` reference.
type dataRef struct {
	raw     string
	kind    refKind
	volume  string // volume: "" for the agent's default volume
	path    string // volume: the path inside it
	scope   string // memory, document: agent | user | tenant
	key     string // memory: the key
	chunkID string // document: the chunk id
}

var refVolumeNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// parseDataRef checks a reference's syntax only:
//
//	volume:<path>            a file on the agent's default volume
//	volume:<name>:<path>     a file on the named volume
//	memory:<scope>/<key>     a Memory key
//	document:<scope>/<id>    a Document chunk's body
//
// For volume, a leading segment that is a valid volume name and contains no
// "/" before a ":" is the volume; so volume:./a:b names a file "a:b".
func parseDataRef(s string) (dataRef, error) {
	r := dataRef{raw: s}
	kind, rest, ok := strings.Cut(s, ":")
	if !ok || rest == "" {
		return r, errors.New("a reference is volume:<path>, memory:<scope>/<key> or document:<scope>/<chunk_id>")
	}
	switch refKind(kind) {
	case refVolume:
		r.kind = refVolume
		if name, p, ok := strings.Cut(rest, ":"); ok && refVolumeNameRe.MatchString(name) {
			r.volume, rest = name, p
		}
		if rest == "" {
			return r, errors.New("volume reference has no path")
		}
		r.path = rest
	case refMemory, refDocument:
		r.kind = refKind(kind)
		scope, id, ok := strings.Cut(rest, "/")
		if !ok || id == "" {
			return r, fmt.Errorf("%s reference must be %s:<scope>/<%s>", kind, kind, map[string]string{"memory": "key", "document": "chunk_id"}[kind])
		}
		switch scope {
		case "agent", "user", "tenant":
		default:
			return r, fmt.Errorf("%s reference scope must be agent, user or tenant", kind)
		}
		r.scope = scope
		if r.kind == refMemory {
			r.key = id
		} else {
			r.chunkID = id
		}
	default:
		return r, fmt.Errorf("unknown reference kind %q (use volume, memory or document)", kind)
	}
	return r, nil
}

// refTool is the tool an agent must hold to read (write=false) or write a
// reference of this kind.
func (r dataRef) refTool(write bool) string {
	switch r.kind {
	case refVolume:
		if write {
			return "Write"
		}
		return "Read"
	case refMemory:
		return "Memory"
	}
	return "Document"
}

// checkRefAllowed refuses a reference the caller may not use: on the operator
// API (no agent, so no stores of an agent's); when this run does not hold the
// matching tool; or when that tool's pre-hooks deny (or would rewrite) the
// equivalent call.
//
// The tool must be in the run's EFFECTIVE tool list — what this run may call,
// after any narrowing the run's caller applied — never the agent definition's
// raw patterns, which can be wider: a run narrowed to tools [Crypto] must not
// write files because its definition grants Write.
func checkRefAllowed(ctx context.Context, r dataRef, write bool) (tools.Result, bool) {
	if tools.IsSubstrateOperator(ctx) && tools.RunID(ctx) == "" {
		return errPermission("from and to are not available outside a run", "Pass the data inline."), false
	}
	need := r.refTool(write)
	if !slices.Contains(tools.AgentTools(ctx), need) {
		return errPermission(fmt.Sprintf("%s needs the %s tool, which this run does not have", r.raw, need),
			"Pass the data inline, or ask an operator to grant the tool."), false
	}
	if res, ok := tools.HookPreCheck(ctx, need, r.hookInput(write)); !ok {
		return classified(fmt.Sprintf("%s: refused by a %s hook: %s", r.raw, need, res.Text), tools.CategoryPermission, false,
			"Pass the data inline, or ask the operator about the hook."), false
	}
	return tools.Result{}, true
}

// hookInput is the call the reference stands for, in the referenced tool's
// own input shape, for its hooks to judge. The data itself is not in it: a
// hook sees where, not what. "via" says which tool is asking.
func (r dataRef) hookInput(write bool) json.RawMessage {
	var in map[string]any
	switch r.kind {
	case refVolume:
		in = map[string]any{"path": r.path}
		if r.volume != "" {
			in["volume"] = r.volume
		}
	case refMemory:
		op := "get"
		if write {
			op = "set"
		}
		in = map[string]any{"op": op, "scope": r.scope, "key": r.key}
	default:
		op := "get_chunk"
		if write {
			op = "update_chunk"
		}
		in = map[string]any{"op": op, "scope": r.scope, "id": r.chunkID}
	}
	in["via"] = CryptoToolName
	b, _ := json.Marshal(in)
	return b
}

// readRef reads at most max bytes from r; more than max is refused, never
// truncated.
func (c *Crypto) readRef(ctx context.Context, r dataRef, max int) ([]byte, tools.Result, bool) {
	tooBig := func() tools.Result {
		return errValidation(fmt.Sprintf("%s holds more than %d bytes", r.raw, max), "Split the data, or ask the operator to raise LOOMCYCLE_CRYPTO_MAX_BYTES.")
	}
	switch r.kind {
	case refVolume:
		root, fence, err := effectiveRoot(ctx, r.volume, false)
		if err != nil {
			return nil, errPermission(err.Error(), "Name a volume this agent has bound."), false
		}
		resolved, err := resolveInsideRoot(root, r.path)
		if err == nil {
			err = outsideFence(fence, resolved)
		}
		if err != nil {
			return nil, errPermission(err.Error(), ""), false
		}
		f, err := os.Open(resolved)
		if err != nil {
			return nil, errNotFound(fmt.Sprintf("cannot open %s", r.raw), "Check the path."), false
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return nil, errValidation(fmt.Sprintf("%s is not a regular file", r.raw), ""), false
		}
		if info.Size() > int64(max) {
			return nil, tooBig(), false
		}
		b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
		if err != nil {
			return nil, errTransient(fmt.Sprintf("reading %s failed", r.raw), "Try again."), false
		}
		if len(b) > max {
			return nil, tooBig(), false
		}
		return b, tools.Result{}, true

	case refMemory:
		m := c.Memory
		if m == nil {
			return nil, errBusiness("memory references are not available here", ""), false
		}
		scope, scopeID, err := m.resolveScope(ctx, r.scope)
		if err != nil {
			return nil, errPermission(err.Error(), "Use a scope in this agent's memory_scopes."), false
		}
		entry, err := m.backend(ctx).Get(ctx, scope, scopeID, r.key)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return nil, errNotFound(fmt.Sprintf("%s is not set", r.raw), ""), false
			}
			return nil, errFrom(fmt.Sprintf("reading %s failed", r.raw), err), false
		}
		// A JSON string is its text; any other JSON value is read as written.
		b := []byte(entry.Value)
		var s string
		if json.Unmarshal(entry.Value, &s) == nil {
			b = []byte(s)
		}
		if len(b) > max {
			return nil, tooBig(), false
		}
		return b, tools.Result{}, true

	default: // document
		body, _, res, ok := c.readChunk(ctx, r)
		if !ok {
			return nil, res, false
		}
		if len(body) > max {
			return nil, tooBig(), false
		}
		return []byte(body), tools.Result{}, true
	}
}

// writeRef writes data to r with the checks the store's own tool applies.
// Memory and Document hold text, so bytes that are not valid UTF-8 are
// refused there.
func (c *Crypto) writeRef(ctx context.Context, r dataRef, data []byte) (tools.Result, bool) {
	if r.kind != refVolume && !utf8.Valid(data) {
		return errValidation(fmt.Sprintf("%s holds text, and this is binary data", r.raw),
			"Write it to a volume instead, or decrypt without to and ask for output_encoding base64."), false
	}
	switch r.kind {
	case refVolume:
		root, fence, err := effectiveRoot(ctx, r.volume, true)
		if err != nil {
			return errPermission(err.Error(), "Name a read-write volume this agent has bound."), false
		}
		target, err := resolveParentInsideRoot(root, r.path)
		if err == nil {
			err = outsideFence(fence, target)
		}
		if err != nil {
			return errPermission(err.Error(), ""), false
		}
		// As the Write tool: a temp file beside the target, then an atomic
		// rename, so a reader never sees half a file. The file keeps the temp
		// file's 0600 mode, as Write's does.
		tmp, err := os.CreateTemp(filepath.Dir(target), ".loomcycle-crypto-*")
		if err != nil {
			return errTransient(fmt.Sprintf("writing %s failed", r.raw), "Try again."), false
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			return errTransient(fmt.Sprintf("writing %s failed", r.raw), "Try again."), false
		}
		if err := tmp.Close(); err != nil {
			return errTransient(fmt.Sprintf("writing %s failed", r.raw), "Try again."), false
		}
		if err := ctx.Err(); err != nil {
			return errTransient("the call was cancelled", ""), false
		}
		if err := os.Rename(tmpName, target); err != nil {
			return errTransient(fmt.Sprintf("writing %s failed", r.raw), "Try again."), false
		}
		return tools.Result{}, true

	case refMemory:
		m := c.Memory
		if m == nil {
			return errBusiness("memory references are not available here", ""), false
		}
		// A chunk body lives in Memory under this prefix; writing it here would
		// replace a Document chunk without its revision check.
		if strings.HasPrefix(r.key, chunkBodyKeyPrefix) {
			return errValidation(fmt.Sprintf("%s is a document chunk's body", r.raw), "Use document:<scope>/<chunk_id>."), false
		}
		scope, scopeID, err := m.resolveScope(ctx, r.scope)
		if err != nil {
			return errPermission(err.Error(), "Use a scope in this agent's memory_scopes."), false
		}
		value, err := json.Marshal(string(data))
		if err != nil {
			return errTransient("encoding the value failed", ""), false
		}
		// execSet is Memory's own set: every check it makes (value cap,
		// reserved keys, core blocks, quota) applies.
		res, err := m.execSet(ctx, scope, scopeID, memoryInput{Op: "set", Key: r.key, Value: value})
		if err != nil {
			return errFrom(fmt.Sprintf("writing %s failed", r.raw), err), false
		}
		if res.IsError {
			return res, false
		}
		return tools.Result{}, true

	default: // document
		_, rev, res, ok := c.readChunk(ctx, r)
		if !ok {
			return res, false
		}
		in, _ := json.Marshal(map[string]any{"op": "update_chunk", "scope": r.scope, "id": r.chunkID, "revision": rev, "body": string(data)})
		out, err := c.Docs.Execute(ctx, in)
		if err != nil {
			return errFrom(fmt.Sprintf("writing %s failed", r.raw), err), false
		}
		if out.IsError {
			return out, false
		}
		return tools.Result{}, true
	}
}

// readChunk reads a chunk's body and revision through the Document tool, so
// its scope checks apply; an unknown chunk is not-found, never an empty body.
func (c *Crypto) readChunk(ctx context.Context, r dataRef) (string, int, tools.Result, bool) {
	if c.Docs == nil {
		return "", 0, errBusiness("document references are not available here", ""), false
	}
	in, _ := json.Marshal(map[string]any{"op": "get_chunk", "scope": r.scope, "id": r.chunkID})
	out, err := c.Docs.Execute(ctx, in)
	if err != nil {
		return "", 0, errFrom(fmt.Sprintf("reading %s failed", r.raw), err), false
	}
	if out.IsError {
		return "", 0, out, false
	}
	var chunk struct {
		Body     string `json:"body"`
		Revision int    `json:"revision"`
	}
	if err := json.Unmarshal([]byte(out.Text), &chunk); err != nil {
		return "", 0, errTransient(fmt.Sprintf("reading %s failed", r.raw), ""), false
	}
	return chunk.Body, chunk.Revision, tools.Result{}, true
}

// refInputField names the input a `from` stands in for, per op.
var refInputField = map[string]string{
	"encrypt": "plaintext", "decrypt": "ciphertext", "rewrap": "ciphertext",
	"mac": "data", "verify_mac": "data", "sign": "data", "verify": "data", "hash": "data",
}

// readFrom checks `from` and `to` before the op runs, and turns `from` into
// the op's ordinary input, so every path after it is the inline one. A `to` is
// checked here too, so a call that cannot write its result is refused before
// anything is decrypted.
func (c *Crypto) readFrom(ctx context.Context, in cryptoInput) (cryptoInput, tools.Result, bool) {
	if in.To != "" {
		switch in.Op {
		case "encrypt", "decrypt", "rewrap":
		default:
			return in, errValidation(in.Op+": to is only for encrypt, decrypt and rewrap", "Drop to."), false
		}
		r, err := parseDataRef(in.To)
		if err != nil {
			return in, errValidation("to: "+err.Error(), ""), false
		}
		if res, ok := checkRefAllowed(ctx, r, true); !ok {
			return in, res, false
		}
	}
	if in.From == "" {
		return in, tools.Result{}, true
	}
	field, ok := refInputField[in.Op]
	if !ok {
		return in, errValidation(in.Op+": from is not used by this op", "Drop from."), false
	}
	if (field == "plaintext" && in.Plaintext != "") || (field == "ciphertext" && in.Ciphertext != "") || (field == "data" && in.Data != "") {
		return in, errValidation(fmt.Sprintf("%s: give from or %s, not both", in.Op, field), "Drop one of them."), false
	}
	r, err := parseDataRef(in.From)
	if err != nil {
		return in, errValidation("from: "+err.Error(), ""), false
	}
	if res, ok := checkRefAllowed(ctx, r, false); !ok {
		return in, res, false
	}
	limit := c.MaxBytes
	if limit <= 0 {
		limit = defaultCryptoMaxBytes
	}
	if field == "ciphertext" {
		limit = limit/3*4 + 1024 // a ciphertext is text: base64 or an envelope
	}
	b, res, ok := c.readRef(ctx, r, limit)
	if !ok {
		return in, res, false
	}
	switch field {
	case "plaintext":
		in.Plaintext, in.InputEncoding = base64.StdEncoding.EncodeToString(b), "base64"
	case "ciphertext":
		in.Ciphertext = strings.TrimSpace(string(b))
	default:
		in.Data, in.InputEncoding = base64.StdEncoding.EncodeToString(b), "base64"
	}
	return in, tools.Result{}, true
}

// writeTo writes the op's output to `to` and returns the result without it:
// where it went and how many bytes, with everything else the op reported.
func (c *Crypto) writeTo(ctx context.Context, in cryptoInput, res tools.Result) tools.Result {
	r, err := parseDataRef(in.To)
	if err != nil { // checked in readFrom; kept for safety
		return errValidation("to: "+err.Error(), "")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		return errTransient("could not read the result to write it", "")
	}
	var data []byte
	if in.Op == "decrypt" {
		s, _ := out["plaintext"].(string)
		if data, err = base64.StdEncoding.DecodeString(s); err != nil {
			return errTransient("could not read the result to write it", "")
		}
		delete(out, "plaintext")
		delete(out, "encoding")
	} else {
		s, _ := out["ciphertext"].(string)
		data = []byte(s)
		delete(out, "ciphertext")
	}
	if wres, ok := c.writeRef(ctx, r, data); !ok {
		return wres
	}
	out["to"], out["bytes"] = in.To, len(data)
	return cryptoJSON(out)
}
