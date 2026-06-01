package codejs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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

// compiler resolves, reads, hashes, and parse-compiles an agent's
// agent_code/<name>/index.js, caching the result by agent name. goja.Compile
// parses without executing, so a syntactically broken file is caught here
// (at AgentDef load via Cache, and again — cached — at first Call) rather
// than at first scheduled fire.
//
// The cache is keyed by agent name only. Editing a code-agent's JS without a
// process restart is NOT picked up (the compiled program is cached for the
// process lifetime) — operators evolve code-agent JS through the normal
// version-control + restart / AgentDef-fork path (RFC J "no agent rewrites
// its own JS" sharp edge). This matches the static-skill bundling posture.
type compiler struct {
	root string

	mu    sync.RWMutex
	cache map[string]*compiled
}

func newCompiler(root string) *compiler {
	return &compiler{root: root, cache: make(map[string]*compiled)}
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
	c.mu.RLock()
	if got, ok := c.cache[name]; ok {
		c.mu.RUnlock()
		return got, nil
	}
	c.mu.RUnlock()

	if name == "" {
		return nil, fmt.Errorf("code-agent: empty agent name (no RunMeta on ctx?)")
	}
	path := c.agentFile(name)
	src, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("code-agent %q: no index.js at %s", name, path)
		}
		return nil, fmt.Errorf("code-agent %q: reading %s: %w", name, path, err)
	}
	sum := sha256.Sum256(src)
	// goja.Compile parses without executing — strict mode off matches ES5.1
	// authoring; the sandbox (sandbox.go) removes eval/Function regardless.
	prog, err := goja.Compile(path, string(src), false)
	if err != nil {
		return nil, fmt.Errorf("code-agent %q: parse %s: %w", name, path, err)
	}
	got := &compiled{prog: prog, hash: hex.EncodeToString(sum[:])}

	c.mu.Lock()
	// Re-check under the write lock: a concurrent loader may have won.
	if existing, ok := c.cache[name]; ok {
		c.mu.Unlock()
		return existing, nil
	}
	c.cache[name] = got
	c.mu.Unlock()
	return got, nil
}
