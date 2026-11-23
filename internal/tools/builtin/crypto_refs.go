package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
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
	// refWorkspace is a file or folder in a workspace pinned to the calling
	// run (RFC GW §5.1). Only the File tool reaches one; every other user of
	// a reference refuses it (checkRefGrant).
	refWorkspace refKind = "workspace"
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
	// workspace is the workspace's name (workspace kind); path is then
	// relative to its root, as written ("" is the root).
	workspace string
}

var refVolumeNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// parseDataRef checks a reference's syntax only:
//
//	volume:<path>            a file on the agent's default volume
//	volume:<name>:<path>     a file on the named volume
//	memory:<scope>/<key>     a Memory key
//	document:<scope>/<id>    a Document chunk's body
//	workspace:<name>:<path>  a file or folder in the run's workspace <name>
//
// For volume, a leading segment that is a valid volume name and contains no
// "/" before a ":" is the volume; so volume:./a:b names a file "a:b".
func parseDataRef(s string) (dataRef, error) {
	r := dataRef{raw: s}
	kind, rest, ok := strings.Cut(s, ":")
	if !ok || rest == "" {
		return r, errors.New("a reference is volume:<path>, memory:<scope>/<key>, document:<scope>/<chunk_id> or workspace:<name>:<path>")
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
	case refWorkspace:
		return parseWorkspaceRef(r, rest)
	default:
		return r, fmt.Errorf("unknown reference kind %q (use volume, memory, document or workspace)", kind)
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
	return checkRefGrant(ctx, r, write, CryptoToolName, "Pass the data inline")
}

// checkRefGrant is checkRefAllowed for any tool that reaches a store by
// reference: via names the asking tool (its hooks see it), and instead is
// the alternative the refusal offers.
func checkRefGrant(ctx context.Context, r dataRef, write bool, via, instead string) (tools.Result, bool) {
	// A workspace is reached through its own grant and hooks (file_ref.go),
	// and only by the File ops and the Crypto from/to that take one, which
	// resolve it themselves; anything else that parsed one is refused here
	// rather than judged as some other tool's reference.
	if r.kind == refWorkspace {
		return errValidation(fmt.Sprintf("%s: %s does not take a workspace reference here", r.raw, via),
			instead+", or copy the file into the inbox first with File op=copy."), false
	}
	// Off a run no agent definition chose the volumes, so a reference is
	// refused unless the transport itself authorized the volume it names
	// (the File twin's keep and render target). Memory and Document
	// references stay refused.
	if tools.IsOffRunPlane(ctx) && !(r.kind == refVolume && tools.OffRunVolumes(ctx)) {
		return errPermission("from and to are not available outside a run", instead+"."), false
	}
	need := r.refTool(write)
	if !slices.Contains(tools.PolicyOf(ctx).Tools, need) {
		return errPermission(fmt.Sprintf("%s needs the %s tool, which this run does not have", r.raw, need),
			instead+", or ask an operator to grant the tool."), false
	}
	if res, ok := tools.HookPreCheck(ctx, need, r.hookInput(write, via)); !ok {
		return classified(fmt.Sprintf("%s: refused by a %s hook: %s", r.raw, need, res.Text), tools.CategoryPermission, false,
			instead+", or ask the operator about the hook."), false
	}
	return tools.Result{}, true
}

// hookInput is the call the reference stands for, in the referenced tool's
// own input shape, for its hooks to judge. The data itself is not in it: a
// hook sees where, not what. "via" says which tool is asking.
func (r dataRef) hookInput(write bool, via string) json.RawMessage {
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
	in["via"] = via
	b, _ := json.Marshal(in)
	return b
}

// refStores reaches the stores a volume:, memory: or document: reference
// names, through each store's own checks: the run's volume bindings and fence,
// Memory's scope check and set, and the Document tool itself. Every tool that
// takes data by reference (Crypto, Calc) reads and writes through it, after
// checkRefGrant has judged the reference. A nil Memory or Docs refuses that
// kind of reference.
type refStores struct {
	Memory *Memory
	Docs   *Document
}

// stores is the Crypto tool's view of refStores, read at call time because
// its Memory and Docs are bound after construction.
func (c *Crypto) stores() refStores { return refStores{Memory: c.Memory, Docs: c.Docs} }

// readRef reads at most max bytes from r; more than max is refused, never
// truncated. A volume file's size is checked before it is read.
func (c refStores) readRef(ctx context.Context, r dataRef, max int) ([]byte, tools.Result, bool) {
	tooBig := func() tools.Result {
		return errValidation(fmt.Sprintf("%s holds more than %d bytes", r.raw, max), "Split the data.")
	}
	switch r.kind {
	case refVolume:
		root, fence, err := effectiveRoot(ctx, r.volume, false)
		if err != nil {
			return nil, errPermission(err.Error(), "Name a volume this agent has bound."), false
		}
		resolved, err := fsops.ResolveInsideRoot(root, r.path)
		if err == nil {
			err = fsops.OutsideFence(fence, resolved)
		}
		if err != nil {
			return nil, errPermission(err.Error(), ""), false
		}
		// Open through the volume's os.Root, as Read does (RFC ES #1).
		vfs, err := fsops.OpenVolumeFS(root, fence)
		if err != nil {
			return nil, errPermission(err.Error(), ""), false
		}
		defer vfs.Close()
		rel, err := vfs.Rel(resolved)
		if err != nil {
			return nil, errPermission(err.Error(), ""), false
		}
		f, err := vfs.Open(rel)
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
		if m == nil || m.Store == nil { // as Memory.Execute: no store, no Memory
			return nil, errBusiness("memory references are not available here", ""), false
		}
		scope, scopeID, err := m.resolveScope(ctx, r.scope)
		if err != nil {
			return nil, errPermission(err.Error(), "Use a scope in this agent's memory_scopes."), false
		}
		// Memory's own get, with its document read rules: a row the caller
		// may not read is refused, or reads as missing, as op=get answers.
		entry, found, res, ok := m.gatedGet(ctx, scope, scopeID, r.key)
		if !ok {
			return nil, res, false
		}
		if !found {
			return nil, errNotFound(fmt.Sprintf("%s is not set", r.raw), ""), false
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
	if r.kind == refWorkspace {
		return c.writeWorkspaceRef(ctx, r, data)
	}
	if r.kind != refVolume && !utf8.Valid(data) {
		return errValidation(fmt.Sprintf("%s holds text, and this is binary data", r.raw),
			"Write it to a volume instead, or decrypt without to and ask for output_encoding base64."), false
	}
	return c.stores().writeRef(ctx, r, data, ".loomcycle-crypto-")
}

// writeRef writes data to a volume, memory or document reference; the temp
// file of a volume write is named with tmpPrefix. Text stores refuse bytes
// that are not valid UTF-8.
func (c refStores) writeRef(ctx context.Context, r dataRef, data []byte, tmpPrefix string) (tools.Result, bool) {
	if r.kind != refVolume && !utf8.Valid(data) {
		return errValidation(fmt.Sprintf("%s holds text, and this is binary data", r.raw), "Write it to a volume instead."), false
	}
	switch r.kind {
	case refVolume:
		root, fence, err := effectiveRoot(ctx, r.volume, true)
		if err != nil {
			return errPermission(err.Error(), "Name a read-write volume this agent has bound."), false
		}
		target, err := fsops.ResolveParentInsideRoot(root, r.path)
		if err == nil {
			err = fsops.OutsideFence(fence, target)
		}
		if err != nil {
			return errPermission(err.Error(), ""), false
		}
		// As the Write tool: a temp file beside the target, then an atomic
		// rename, so a reader never sees half a file — both through the
		// volume's os.Root (RFC ES #1). The file keeps the temp file's 0600
		// mode, as Write's does.
		vfs, err := fsops.OpenVolumeFS(root, fence)
		if err != nil {
			return errPermission(err.Error(), ""), false
		}
		defer vfs.Close()
		rel, err := vfs.Rel(target)
		if err != nil {
			return errPermission(err.Error(), ""), false
		}
		dir, err := vfs.OpenDir(filepath.Dir(rel))
		if err != nil {
			return errTransient(fmt.Sprintf("writing %s failed", r.raw), "Try again."), false
		}
		defer dir.Close()
		tmp, tmpName, err := fsops.CreateTemp(dir, tmpPrefix)
		if err != nil {
			return errTransient(fmt.Sprintf("writing %s failed", r.raw), "Try again."), false
		}
		defer dir.Remove(tmpName)
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
		if err := dir.Rename(tmpName, filepath.Base(rel)); err != nil {
			return errTransient(fmt.Sprintf("writing %s failed", r.raw), "Try again."), false
		}
		return tools.Result{}, true

	case refMemory:
		m := c.Memory
		if m == nil || m.Store == nil { // as Memory.Execute: no store, no Memory
			return errBusiness("memory references are not available here", ""), false
		}
		// A chunk body lives in Memory under this prefix; writing it here would
		// replace a Document chunk without its revision check.
		if strings.HasPrefix(r.key, chunkBodyKeyPrefix) {
			return errValidation(fmt.Sprintf("%s is a document chunk's body", r.raw), "Use document:<scope>/<chunk_id>."), false
		}
		// As Memory op=set: a scope this run may only read is not written.
		if res, refused := memoryReadOnlyScope(ctx, "set", r.scope); refused {
			return res, false
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
func (c refStores) readChunk(ctx context.Context, r dataRef) (string, int, tools.Result, bool) {
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
		if r.kind == refWorkspace {
			if _, _, res, ok := c.cryptoWorkspaceLoc(ctx, r, true); !ok {
				return in, res, false
			}
		} else if res, ok := checkRefAllowed(ctx, r, true); !ok {
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
	if r.kind != refWorkspace {
		if res, ok := checkRefAllowed(ctx, r, false); !ok {
			return in, res, false
		}
	}
	limit := c.MaxBytes
	if limit <= 0 {
		limit = defaultCryptoMaxBytes
	}
	if field == "ciphertext" {
		limit = limit/3*4 + 1024 // a ciphertext is text: base64 or an envelope
	}
	var b []byte
	var res tools.Result
	if r.kind == refWorkspace {
		// The workspace grant and hook are judged here (RFC GW P2 §3.4).
		b, res, ok = c.readWorkspaceRef(ctx, r, limit)
	} else {
		b, res, ok = c.stores().readRef(ctx, r, limit)
	}
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
