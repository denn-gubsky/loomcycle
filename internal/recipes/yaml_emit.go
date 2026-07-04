package recipes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AppendOptions controls the behaviour of `mcp-registry append-to-config`.
type AppendOptions struct {
	// Force allows overwriting an existing mcp_servers.<name> entry.
	// Without it, the operation refuses on collision.
	Force bool

	// EnvAllowlist is the operator's currently-allowlisted env-var
	// prefixes (typically the `LOOMCYCLE_*` set). When non-nil,
	// AppendToConfig refuses to write a recipe whose
	// _loomcycle.env_vars_required references a name not in the
	// allowlist — and includes the missing names in the error so
	// the operator can update their env.
	EnvAllowlist map[string]bool
}

// AppendToConfig appends the recipe to the target YAML file's
// `mcp_servers:` block. Preserves operator-authored comments and
// ordering by manipulating the yaml.v3 Node tree directly (the only
// way to round-trip a YAML document without losing comments).
//
// Returns the new file contents (does not write to disk; caller
// decides). Errors fall into three buckets:
//
//   - File-IO / parse errors → returned verbatim. Caller writes a
//     descriptive operator message + exits 2 (config error).
//   - Collision when !opts.Force → typed *ErrEntryExists for the CLI
//     to format with the "use --force" hint.
//   - Env-allowlist mismatch → typed *ErrMissingEnvVars naming the
//     unallowlisted vars so the operator gets actionable feedback.
func AppendToConfig(rec *Recipe, targetPath string, opts AppendOptions) ([]byte, error) {
	if rec == nil {
		return nil, fmt.Errorf("nil recipe")
	}

	// Read the target. Missing file = create-fresh shape (rare;
	// operators usually `loomcycle init` first), so we treat absence
	// as "empty document" rather than an error.
	var source []byte
	if data, err := os.ReadFile(targetPath); err == nil {
		source = data
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", targetPath, err)
	}

	// Env-allowlist gate. The recipe declares which env vars must be
	// in the operator's LOOMCYCLE_* allowlist for the substitution
	// chain to resolve at runtime. Missing names produce a typed
	// error before we touch the file.
	if opts.EnvAllowlist != nil && rec.Loomcycle != nil {
		var missing []string
		for _, name := range rec.Loomcycle.EnvVarsRequired {
			if !opts.EnvAllowlist[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return nil, &ErrMissingEnvVars{Names: missing}
		}
	}

	// Parse the source. Empty file → fresh document with just a
	// `mcp_servers:` block; non-empty → preserve everything.
	var doc yaml.Node
	if len(bytes.TrimSpace(source)) == 0 {
		// Initialise a minimal document: top-level mapping with
		// mcp_servers as the sole key. Comments inside the recipe
		// (none in this path) still propagate from the entry node.
		doc = yaml.Node{
			Kind: yaml.DocumentNode,
			Content: []*yaml.Node{
				{Kind: yaml.MappingNode, Content: nil},
			},
		}
	} else {
		if err := yaml.Unmarshal(source, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", targetPath, err)
		}
		if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s: top-level must be a YAML mapping (got %v)", targetPath, doc.Content[0].Kind)
		}
	}
	root := doc.Content[0]

	// Find the existing mcp_servers entry, or create one.
	var mcpServersValue *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "mcp_servers" {
			mcpServersValue = root.Content[i+1]
			break
		}
	}
	if mcpServersValue == nil {
		// Append a fresh `mcp_servers:` mapping at the end of the
		// document. Preserves all surrounding comments + ordering.
		mcpServersKey := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mcp_servers"}
		mcpServersValue = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, mcpServersKey, mcpServersValue)
	}
	if mcpServersValue.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: mcp_servers must be a mapping (got %v)", targetPath, mcpServersValue.Kind)
	}

	// Check for an existing entry with this name.
	for i := 0; i+1 < len(mcpServersValue.Content); i += 2 {
		if mcpServersValue.Content[i].Value == rec.Name {
			if !opts.Force {
				return nil, &ErrEntryExists{Name: rec.Name, Path: targetPath}
			}
			// Force: replace the value node in place. Keep the key
			// node (preserves any operator comment associated with it).
			entryNode, err := recipeToYAMLValueNode(rec)
			if err != nil {
				return nil, err
			}
			mcpServersValue.Content[i+1] = entryNode
			return marshalDoc(&doc)
		}
	}

	// Append a fresh entry.
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: rec.Name}
	if rec.Loomcycle != nil && rec.Loomcycle.Description != "" {
		keyNode.HeadComment = "# " + rec.Loomcycle.Description
	}
	entryNode, err := recipeToYAMLValueNode(rec)
	if err != nil {
		return nil, err
	}
	mcpServersValue.Content = append(mcpServersValue.Content, keyNode, entryNode)
	return marshalDoc(&doc)
}

// recipeToYAMLValueNode builds the YAML mapping for one recipe's
// `mcp_servers.<name>:` block. Order is operator-friendly:
//
//	transport, command, args, env, url, headers, pool_size, tools
//
// The transport key is always written explicitly (yaml expects it; the
// JSON recipe's `_loomcycle.transport` field is inferred at validate
// time but the yaml form is the canonical source for boot loading).
//
// Per-field error handling: each field that fails to decode from the
// stored JSON RawMessage emits an error mentioning the field name, so
// the operator can locate the bad input.
func recipeToYAMLValueNode(rec *Recipe) (*yaml.Node, error) {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}

	// transport (always first; explicit).
	transport := rec.HasTransport()
	if transport == "" {
		return nil, fmt.Errorf("recipe %q has neither command (stdio) nor url (http)", rec.Name)
	}
	addStrField(out, "transport", transport)

	// stdio fields.
	if len(rec.Command) > 0 {
		if err := addRawField(out, "command", rec.Command); err != nil {
			return nil, fmt.Errorf("recipe %q field command: %w", rec.Name, err)
		}
	}
	if len(rec.Args) > 0 {
		if err := addRawField(out, "args", rec.Args); err != nil {
			return nil, fmt.Errorf("recipe %q field args: %w", rec.Name, err)
		}
	}
	if len(rec.Env) > 0 {
		if err := addRawField(out, "env", rec.Env); err != nil {
			return nil, fmt.Errorf("recipe %q field env: %w", rec.Name, err)
		}
	}
	// http fields.
	if len(rec.URL) > 0 {
		if err := addRawField(out, "url", rec.URL); err != nil {
			return nil, fmt.Errorf("recipe %q field url: %w", rec.Name, err)
		}
	}
	if len(rec.Headers) > 0 {
		if err := addRawField(out, "headers", rec.Headers); err != nil {
			return nil, fmt.Errorf("recipe %q field headers: %w", rec.Name, err)
		}
	}
	// pool_size from _loomcycle metadata.
	if rec.Loomcycle != nil && rec.Loomcycle.PoolSize > 0 {
		addIntField(out, "pool_size", rec.Loomcycle.PoolSize)
	}
	return out, nil
}

func addStrField(parent *yaml.Node, key, value string) {
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func addIntField(parent *yaml.Node, key string, value int) {
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", value)},
	)
}

// addRawField unmarshals a JSON RawMessage into a Go value, then
// marshals it as yaml so the resulting yaml.Node reflects the JSON
// content. Used for the structurally-richer fields (args, env,
// headers) where direct string scalar construction won't work.
func addRawField(parent *yaml.Node, key string, raw json.RawMessage) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	valueNode := &yaml.Node{}
	if err := valueNode.Encode(value); err != nil {
		return err
	}
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		valueNode,
	)
	return nil
}

// marshalDoc serialises the modified document with 2-space indent —
// matches the convention loomcycle uses elsewhere (e.g. snapshot
// restore yaml + cfg yaml in init).
func marshalDoc(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ErrEntryExists is the typed error AppendToConfig returns on
// collision when !opts.Force. The CLI catches it to format a
// helpful "use --force" message.
type ErrEntryExists struct {
	Name string
	Path string
}

func (e *ErrEntryExists) Error() string {
	return fmt.Sprintf("mcp_servers.%s already exists in %s (use --force to overwrite)", e.Name, e.Path)
}

// ErrMissingEnvVars is the typed error AppendToConfig returns when
// the recipe references env vars not in the operator's allowlist.
// The CLI catches it to emit an "add these to your env" diff.
type ErrMissingEnvVars struct {
	Names []string
}

func (e *ErrMissingEnvVars) Error() string {
	return fmt.Sprintf("recipe requires env vars not in LOOMCYCLE_* allowlist: %s", strings.Join(e.Names, ", "))
}
