package builtin

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// teamDefOpArguments reads, from this package's source, which arguments each
// TeamDef op takes: the teamDefInput fields its exec function selects, and
// those of every function it hands its input to, by JSON name. It is derived
// from the code that runs, so it cannot drift from it; it fails rather than
// guess when the input is used in a way it cannot follow (copied, stored).
func teamDefOpArguments(t *testing.T) map[string]map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	jsonName := map[string]string{}
	typ := reflect.TypeOf(teamDefInput{})
	for i := 0; i < typ.NumField(); i++ {
		jsonName[typ.Field(i).Name] = strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
	}
	fields := map[string]map[string]bool{} // function → JSON names it reads
	calls := map[string][]string{}         // function → functions it hands its input to
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				param := ""
				for _, p := range fn.Type.Params.List {
					if id, ok := p.Type.(*ast.Ident); ok && id.Name == "teamDefInput" && len(p.Names) == 1 {
						param = p.Names[0].Name
					}
				}
				if param == "" {
					continue
				}
				name := fn.Name.Name
				fields[name] = map[string]bool{}
				uses, followed := 0, 0
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.Ident:
						if n.Name == param {
							uses++
						}
					case *ast.SelectorExpr:
						if x, ok := n.X.(*ast.Ident); ok && x.Name == param {
							followed++
							fields[name][jsonName[n.Sel.Name]] = true
						}
					case *ast.CallExpr:
						for _, a := range n.Args {
							if id, ok := a.(*ast.Ident); ok && id.Name == param {
								followed++
								switch f := n.Fun.(type) {
								case *ast.SelectorExpr:
									calls[name] = append(calls[name], f.Sel.Name)
								case *ast.Ident:
									calls[name] = append(calls[name], f.Name)
								}
							}
						}
					}
					return true
				})
				if uses != followed {
					t.Fatalf("%s uses its teamDefInput in a way this derivation does not follow (%d uses, %d followed)", name, uses, followed)
				}
			}
		}
	}
	out := map[string]map[string]bool{}
	for _, op := range tools.SchemaEnum((&TeamDef{}).InputSchema(), "op") {
		exec := "exec"
		for _, part := range strings.Split(op, "_") {
			exec += strings.ToUpper(part[:1]) + part[1:]
		}
		if _, ok := fields[exec]; !ok {
			t.Fatalf("op %q: no function %s(ctx, in teamDefInput) found", op, exec)
		}
		args := map[string]bool{}
		seen := map[string]bool{}
		var walk func(string)
		walk = func(fn string) {
			if seen[fn] {
				return
			}
			seen[fn] = true
			for a := range fields[fn] {
				if a != "op" {
					args[a] = true
				}
			}
			for _, c := range calls[fn] {
				walk(c)
			}
		}
		walk(exec)
		out[op] = args
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Every TeamDef op has an article listing exactly the arguments the op reads,
// and the real dispatcher, over the real corpus, accepts a call to each op
// carrying every one of them. The dispatcher refuses an argument an op's
// article does not list when a sibling op's article does, so an argument
// missing from its op's article is a legitimate call refused; one listed but
// never read is an argument the op silently drops. Every schema argument is
// some op's.
func TestTeamDefHelp_EveryOpsArticleListsExactlyTheArgumentsItReads(t *testing.T) {
	set := bundledHelp(t)
	reads := teamDefOpArguments(t)
	d := tools.NewDispatcher([]tools.Tool{&TeamDef{}, &Context{Help: set}})
	if _, ok := set.ToolArticle("TeamDef"); !ok {
		t.Fatal("TeamDef has no tool article, so the dispatcher checks none of its arguments")
	}
	used := map[string]bool{}
	for op, args := range reads {
		topic, ok := set.Get("TeamDef/" + op)
		if !ok {
			t.Errorf("op %s has no article", op)
			continue
		}
		listed, ok := topic.Arguments()
		if !ok {
			t.Errorf("TeamDef/%s has no ## Arguments section", op)
			continue
		}
		documented := map[string]bool{}
		for _, a := range listed {
			documented[a] = true
		}
		if got, want := sortedKeys(documented), sortedKeys(args); !reflect.DeepEqual(got, want) {
			t.Errorf("TeamDef/%s lists %v; the op reads %v", op, got, want)
		}
		call := map[string]any{"op": op}
		for a := range args {
			call[a] = nil
			used[a] = true
		}
		in, _ := json.Marshal(call)
		if msg, refused := d.ArgumentRefusal("TeamDef", in); refused {
			t.Errorf("the dispatcher refuses a %s call with every argument the op reads: %s", op, msg)
		}
	}
	for _, p := range sortedKeys(schemaPropertyNames(t, &TeamDef{})) {
		if p != "op" && !used[p] {
			t.Errorf("schema argument %q is read by no op", p)
		}
	}
	if len(reads["run"]) == 0 || len(reads["poll"]) == 0 {
		t.Fatalf("derived %v; the derivation is asserting nothing", reads)
	}
}
