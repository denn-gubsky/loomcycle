package http

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The route gate asks exactly the pairs the enforcement table says it asks
// (RFC FY Part E): a pair the table credits to the HTTP routes that no route
// asks would let a rule on it be stored as enforced, and a pair a route asks
// that the table does not list would refuse a rule that holds.
func TestACLEnforcement_HTTPRoutesAskWhatTheTableSays(t *testing.T) {
	asked := map[string]bool{}
	for _, r := range eqRoutes(t) {
		if s, ok := authz.HTTPSurface(r.method, r.path); ok {
			asked[s.Action+" "+s.Type] = true
		}
	}
	assertSiteAsks(t, authz.SiteHTTPRoute, asked)
}

// assertSiteAsks compares the pairs a site really asks with the table's.
func assertSiteAsks(t *testing.T, site authz.Site, asked map[string]bool) {
	t.Helper()
	if len(asked) == 0 {
		t.Fatalf("%s asked nothing: the enumeration is broken", site)
	}
	table := map[string]bool{}
	for _, p := range authz.PairsAskedAt(site) {
		table[p] = true
		if !asked[p] {
			t.Errorf("the enforcement table credits %q to %s, which never asks it", p, site)
		}
	}
	var missing []string
	for p := range asked {
		if !table[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	for _, p := range missing {
		t.Errorf("%s asks %q, which the enforcement table does not credit to it", site, p)
	}
}

// The shared connector operations ask exactly the pairs the table credits to
// them. The census reads every connectorAsk call site in this package's
// source rather than a hand-kept list (RFC GD P1): each site's question is
// derived from its argument — an authz.Surface literal, a channelQuestion of
// a literal op, askChannels
// of a literal verb, or the per-op builder a tool wrapper drives
// (memoryOpQuestion, channelOpQuestions, run over the tool schema's ops). A
// site of any other shape fails here until it is classified.
func TestACLEnforcement_ConnectorOperationsAskWhatTheTableSays(t *testing.T) {
	asked := map[string]bool{}
	add := func(q authz.Surface) { asked[q.Action+" "+q.Type] = true }
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	strLit := func(e ast.Expr) (string, bool) {
		bl, ok := e.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(bl.Value)
		return v, err == nil
	}
	calls := func(n ast.Node, name string, fn func(*ast.CallExpr)) {
		ast.Inspect(n, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
					fn(c)
				}
			}
			return true
		})
	}
	sites := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				calls(fd.Body, "askChannels", func(c *ast.CallExpr) {
					if v, ok := strLit(c.Args[1]); ok {
						add(authz.Surface{Action: v, Type: "channel"})
					} else {
						t.Errorf("%s: askChannels with a verb that is not a literal", fset.Position(c.Pos()))
					}
				})
				calls(fd.Body, "connectorAsk", func(c *ast.CallExpr) {
					sites++
					pos := fset.Position(c.Pos())
					arg := c.Args[1]
					if st, ok := arg.(*ast.StarExpr); ok {
						arg = st.X
					}
					switch fd.Name.Name {
					case "askChannels":
						return // its callers' literal verbs, above
					case "Memory":
						for _, op := range schemaOps(t, (&builtin.Memory{}).InputSchema()) {
							if q := memoryOpQuestion([]byte(`{"op":"` + op + `"}`)); q != nil {
								add(*q)
							}
						}
						return
					case "Channel":
						for _, op := range schemaOps(t, (&builtin.Channel{}).InputSchema()) {
							qs, err := channelOpQuestions([]byte(`{"op":"` + op + `","channel":"c","channels":["c"]}`))
							if err != nil || len(qs) == 0 {
								t.Errorf("Channel op %s asks no question (%v)", op, err)
							}
							for _, q := range qs {
								add(q)
							}
						}
						return
					}
					if cl, ok := arg.(*ast.CompositeLit); ok {
						var q authz.Surface
						for _, el := range cl.Elts {
							kv, ok := el.(*ast.KeyValueExpr)
							if !ok {
								continue
							}
							k, _ := kv.Key.(*ast.Ident)
							v, lit := strLit(kv.Value)
							switch {
							case k == nil:
							case k.Name == "Action" && lit:
								q.Action = v
							case k.Name == "Type" && lit:
								q.Type = v
							}
						}
						if q.Action != "" && q.Type != "" {
							add(q)
							return
						}
					}
					if ce, ok := arg.(*ast.CallExpr); ok {
						if id, ok := ce.Fun.(*ast.Ident); ok && id.Name == "channelQuestion" {
							if op, ok := strLit(ce.Args[0]); ok {
								if q := channelQuestion(op, "c"); q != nil {
									add(*q)
									return
								}
							}
						}
					}
					t.Errorf("%s: connectorAsk in %s asks a question the census cannot derive; classify it here", pos, fd.Name.Name)
				})
			}
		}
	}
	if sites < 5 {
		t.Fatalf("found %d connectorAsk call sites: the source scan is broken", sites)
	}
	if _, err := channelOpQuestions([]byte(`{"op":"no_such_op","channel":"c"}`)); err == nil {
		t.Error("an op the table does not map was let through unasked")
	}
	assertSiteAsks(t, authz.SiteConnector, asked)
}

// schemaOps is the `op` enum of a tool's input schema.
func schemaOps(t *testing.T, schema json.RawMessage) []string {
	t.Helper()
	var s struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil || len(s.Properties.Op.Enum) < 3 {
		t.Fatalf("the schema's op enum could not be read: %v %v", err, s.Properties.Op.Enum)
	}
	return s.Properties.Op.Enum
}
