package codejs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dop251/goja"
)

// compiled is one agent's parsed JS program plus the content hash of its
// source. The hash is the provider.code_hash OTEL attribute (Decision 9) and
// the AgentDef lineage field — it is what lets an operator answer "which code
// version produced this run" without versioning the JS files separately.
type compiled struct {
	prog *goja.Program
	hash string // sha256 hex of the index.js source bytes
}

// compiler resolves, reads, hashes, and parse-compiles a code-agent's JS —
// either an inline body (substrate code_body, RFC J) or the filesystem
// agent_code/<name>/index.js fallback. goja.Compile parses without executing,
// so a syntactically broken source is caught here (at AgentDef load via Cache,
// and again — cached — at first Call) rather than at first scheduled fire.
//
// The cache is keyed by the source's CONTENT HASH, not the agent name. This is
// what makes inline code correct under versioning: a new AgentDef version
// shipping new JS under the same name produces a different hash → compiles
// fresh, never serving the prior version's program. (As a side effect the old
// by-name "edit-without-restart-not-picked-up" sharp edge no longer applies to
// distinct bytes; identical bytes still hit cache by design.)
type compiler struct {
	root string

	mu sync.RWMutex
	// cache is the inline path's program cache, keyed by sha256 hex of the
	// source bytes — version-correct (a new code_body under the same name is
	// a new key, never serving the prior program).
	cache map[string]*compiled
	// fsCache is the filesystem path's program cache, keyed by AGENT NAME.
	// A code-agent run is a SEQUENCE of Provider.Call turns (replay model);
	// keying the FS path by name lets turns 2..N skip both the os.ReadFile
	// and the re-hash that an unconditional content-hash lookup would repeat
	// every turn. Bounded by the agent roster (not by authorship churn).
	// Editing index.js needs a process restart — the documented sharp edge.
	fsCache map[string]*compiled
}

func newCompiler(root string) *compiler {
	return &compiler{
		root:    root,
		cache:   make(map[string]*compiled),
		fsCache: make(map[string]*compiled),
	}
}

// agentFile is the resolved path of an agent's entrypoint. Exposed (not just
// inlined) so the load error names the exact path the operator must create.
func (c *compiler) agentFile(name string) string {
	return filepath.Join(c.root, name, "index.js")
}

// load returns the compiled program for an agent, reading + parsing on the
// first request and serving the cache thereafter. Errors are fail-loud:
//   - the file is missing  → "code-agent <name>: no index.js at <path>"
//   - the file won't parse  → the goja SyntaxError, wrapped with the path
//
// Both are surfaced at AgentDef load time (so a broken code-agent fails the
// load, not the first fire) AND defended again here for the direct-Call path.
func (c *compiler) load(name string) (*compiled, error) {
	// By-name cache check FIRST so replay turns 2..N skip the disk read +
	// hash entirely (the per-turn regression a content-hash-only lookup
	// would introduce). Only valid names are ever stored, so an invalid
	// name simply misses and falls through to the checks below.
	c.mu.RLock()
	if got, ok := c.fsCache[name]; ok {
		c.mu.RUnlock()
		return got, nil
	}
	c.mu.RUnlock()

	if name == "" {
		return nil, fmt.Errorf("code-agent: empty agent name (no RunMeta on ctx?)")
	}
	// Host-side containment floor (correct depth — rejects regardless of how
	// the name reached us). The agent name becomes a path segment under
	// CodeRoot; without this a name like "../../etc/cron.d/x" would
	// filepath.Join-collapse out of CodeRoot and read/compile an arbitrary
	// index.js anywhere on disk. The static .md loader applies the same check
	// (internal/agents/loader.go:199); the AgentDef substrate create/fork path
	// also validates the name, but this floor holds even if a caller forgets.
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return nil, fmt.Errorf("code-agent %q: invalid agent name (must not contain a path separator or be \".\"/\"..\")", name)
	}
	path := c.agentFile(name)
	src, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Hint at the inline path: a fork of a filesystem code-agent
			// lands under a new name whose index.js does not exist —
			// supply a code_body in the fork overlay instead.
			return nil, fmt.Errorf("code-agent %q: no index.js at %s (a substrate-authored or forked code agent must supply an inline code_body)", name, path)
		}
		return nil, fmt.Errorf("code-agent %q: reading %s: %w", name, path, err)
	}
	got, err := c.compileCached(name, string(src))
	if err != nil {
		return nil, err
	}
	// Memoize by name so subsequent turns skip the read+hash above.
	c.mu.Lock()
	if existing, ok := c.fsCache[name]; ok {
		c.mu.Unlock()
		return existing, nil
	}
	c.fsCache[name] = got
	c.mu.Unlock()
	return got, nil
}

// loadSource compiles an inline code-js body (substrate code_body), caching
// by its content hash. name is used only for error messages — the cache key
// is the hash, so two agents with byte-identical bodies share one compiled
// program and a re-registered identical body is a no-op.
//
// Unlike the filesystem path this re-hashes the body on every turn rather than
// caching by name: a code_body is versioned (a new AgentDef version ships new
// bytes under the same name), so a by-name cache would serve a stale program.
// The per-turn sha256 of a small body is cheap CPU (no I/O); a per-run resolve
// that hashes once is the deferred optimization (see the review's altitude note).
func (c *compiler) loadSource(name, src string) (*compiled, error) {
	if src == "" {
		return nil, fmt.Errorf("code-agent %q: empty inline code_body", name)
	}
	return c.compileCached(name, src)
}

// compileCached is the shared hash → cache → goja.Compile core for both the
// filesystem and inline paths. Keyed by sha256 of the source bytes.
func (c *compiler) compileCached(name, src string) (*compiled, error) {
	sum := sha256.Sum256([]byte(src))
	key := hex.EncodeToString(sum[:])

	c.mu.RLock()
	if got, ok := c.cache[key]; ok {
		c.mu.RUnlock()
		return got, nil
	}
	c.mu.RUnlock()

	// goja.Compile parses without executing — strict mode off matches ES5.1
	// authoring; the sandbox (sandbox.go) removes eval/Function regardless.
	prog, err := goja.Compile(name, src, false)
	if err != nil {
		return nil, fmt.Errorf("code-agent %q: parse: %w", name, err)
	}
	got := &compiled{prog: prog, hash: key}

	c.mu.Lock()
	// Re-check under the write lock: a concurrent loader may have won.
	if existing, ok := c.cache[key]; ok {
		c.mu.Unlock()
		return existing, nil
	}
	c.cache[key] = got
	c.mu.Unlock()
	return got, nil
}
