package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/help"
)

// walkerProseOps are the Document ops kb/walker's prose sends the model to call.
var walkerProseOps = []string{"search", "lineage", "place", "measure", "query_documents",
	"get_chunk", "get_document", "documents_summary", "related", "get_annotation"}

var quotedArg = regexp.MustCompile(`"([a-z_]+)"`)

// TestKnowledgeBaseBundle_WalkerProseNamesOnlyArgumentsItsOpsTake reads every
// sentence of kb/walker's prompt of the shape `<op> … with "<arg>" … "<arg>"`
// and checks each quoted Document argument against what that op's help article
// lists — the list the dispatcher's argument check refuses by. The rendered call
// samples are checked by the help corpus; the prose that tells the model which
// argument to ADD is not, and a move that adds an argument its op does not take
// is either refused (a sibling op documents it) or silently dropped. Measured:
// the CHANGED move told the walker to search with "created_after", which only
// query_documents and list_statements take.
func TestKnowledgeBaseBundle_WalkerProseNamesOnlyArgumentsItsOpsTake(t *testing.T) {
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatal(err)
	}
	takes := map[string]map[string]bool{}
	known := map[string]bool{} // every argument some Document op documents
	for _, op := range walkerProseOps {
		tp, ok := set.Get("Document/" + op)
		if !ok {
			t.Fatalf("no help article Document/%s", op)
		}
		args, ok := tp.Arguments()
		if !ok {
			t.Fatalf("Document/%s has no Arguments section", op)
		}
		takes[op] = map[string]bool{"op": true}
		for _, a := range args {
			takes[op][a] = true
			known[a] = true
		}
	}
	prompt := knowledgeBaseConfig(t).Agents["kb/walker"].SystemPrompt
	// The prose part only: the rendered help samples are checked elsewhere.
	if i := strings.Index(prompt, "HOW TO CALL THE TOOLS."); i > 0 {
		prompt = prompt[:i]
	}
	flat := strings.Join(strings.Fields(prompt), " ")
	checked := 0
	var bad []string
	for _, op := range walkerProseOps {
		re := regexp.MustCompile(`\b` + op + `\b([^.;]*?)\bwith\b([^.;]*)`)
		for _, m := range re.FindAllStringSubmatch(flat, -1) {
			for _, q := range quotedArg.FindAllStringSubmatch(m[2], -1) {
				arg := q[1]
				if !known[arg] {
					continue // a value ("share", "tenant") or another tool's name
				}
				checked++
				if !takes[op][arg] {
					bad = append(bad, op+" with \""+arg+"\" in: "+m[0])
				}
			}
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("kb/walker prose sends %s", b)
	}
	if checked < 5 {
		t.Fatalf("only %d op/argument pairs found in the prose; the pattern no longer reads it", checked)
	}
}
