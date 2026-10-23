// Command modules prints the Go modules this repository builds from, for the
// shell scripts and Makefile targets that must visit each one (RFC FE). The
// set is the root module plus every module the root go.mod replaces with a
// directory inside the tree; see scripts/ci/gomodules.
//
// Usage, from the repository root:
//
//	modules                 one module directory per line, root (".") first
//	modules -group PKG...   one line per module that owns any of the import
//	                        paths: its directory, then those paths, in the
//	                        order given; modules in the order above
//
// -group is how a package list is run: `go test` must run a nested module's
// packages from that module's directory, where its own go.mod and go.sum
// apply. An import path no in-tree module owns is an error.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/denn-gubsky/loomcycle/scripts/ci/gomodules"
)

func main() {
	group := flag.Bool("group", false, "group the import paths given as arguments by owning module")
	flag.Parse()

	mods, err := gomodules.InTree(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "modules:", err)
		os.Exit(1)
	}
	if !*group {
		if flag.NArg() > 0 {
			fmt.Fprintln(os.Stderr, "modules: arguments are import paths for -group")
			os.Exit(2)
		}
		for _, m := range mods {
			fmt.Println(m.Dir)
		}
		return
	}

	lines, err := groupByModule(mods, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "modules:", err)
		os.Exit(1)
	}
	for _, l := range lines {
		fmt.Println(l)
	}
}

// groupByModule returns "DIR PKG..." for each module owning any of pkgs.
func groupByModule(mods []gomodules.Module, pkgs []string) ([]string, error) {
	byDir := map[string][]string{}
	for _, p := range pkgs {
		m, ok := gomodules.Owner(mods, p)
		if !ok {
			return nil, fmt.Errorf("%s is in no in-tree module (want an import path under %s)", p, mods[0].Path)
		}
		byDir[m.Dir] = append(byDir[m.Dir], p)
	}
	var lines []string
	for _, m := range mods {
		if ps := byDir[m.Dir]; len(ps) > 0 {
			lines = append(lines, m.Dir+" "+strings.Join(ps, " "))
		}
	}
	return lines, nil
}
