package worker

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/toolchainname"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

func TestParseToolchains_ReadsCatalogNamesAndNamedBinaries(t *testing.T) {
	got, err := ParseToolchains(" python , cpp,go1.26=go, tool=a|b ,")
	if err != nil {
		t.Fatal(err)
	}
	want := []engine.Toolchain{
		{Name: "python", Binaries: []string{"python3", "python"}},
		{Name: "cpp", Binaries: []string{"c++", "g++", "clang++"}},
		{Name: "go1.26", Binaries: []string{"go"}},
		{Name: "tool", Binaries: []string{"a", "b"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// Unset (or only separators) is nil: the worker probes for the default.
func TestParseToolchains_EmptyIsNil(t *testing.T) {
	for _, v := range []string{"", " ", ",,"} {
		if got, err := ParseToolchains(v); err != nil || got != nil {
			t.Errorf("%q: got %v, %v; want nil, nil", v, got, err)
		}
	}
}

// A malformed entry stops the worker at its start, with an error naming it.
func TestParseToolchains_RefusesAMalformedEntryNamingIt(t *testing.T) {
	var over []string
	for i := 0; i <= toolchainname.Max; i++ {
		over = append(over, "t"+strconv.Itoa(i)+"=x")
	}
	for v, mention := range map[string]string{
		"python,Go":             "Go",
		"go;rm=go":              "go;rm",
		"go=/usr/local/bin/go":  "/usr/local/bin/go",
		"go=go|":                "go=go|",
		"go=":                   "go=",
		"zig":                   "zig",
		"python,python":         "python",
		"cpp=g++ -O2":           "g++ -O2",
		strings.Join(over, ","): "at most",
	} {
		_, err := ParseToolchains(v)
		if err == nil {
			t.Errorf("%q accepted", v)
			continue
		}
		if !strings.Contains(err.Error(), mention) {
			t.Errorf("%q: err %q does not name %q", v, err, mention)
		}
	}
}

// The catalog's own entries pass the rules a declared one must.
func TestToolchainCatalog_EveryEntryPassesTheRules(t *testing.T) {
	var all []engine.Toolchain
	for name, bins := range toolchainCatalog {
		all = append(all, engine.Toolchain{Name: name, Binaries: bins})
	}
	if err := engine.CheckToolchains(all); err != nil {
		t.Fatal(err)
	}
}

// fakePath makes a PATH of one directory holding empty executables named
// bins, so a probe sees only them.
func fakePath(t *testing.T, bins ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, b := range bins {
		if err := os.WriteFile(filepath.Join(dir, b), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestDetectToolchains_FindsTheFirstBinaryOfEachOnPath(t *testing.T) {
	fakePath(t, "python", "clang++", "cargo")
	tcs, err := ParseToolchains("python,node,cpp,rust,go")
	if err != nil {
		t.Fatal(err)
	}
	if got := DetectToolchains(tcs); !slices.Equal(got, []string{"python", "cpp", "rust"}) {
		t.Fatalf("found %v, want [python cpp rust]", got)
	}
}

// A declared toolchain the probe does not find is left out, never claimed,
// and the worker warns naming it (RFC HH D2).
func TestNewEngine_AMissingDeclaredToolchainIsOmittedAndWarned(t *testing.T) {
	fakePath(t, "go")
	tcs, err := ParseToolchains("go,cpp")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	opt := Options{Engine: workspace.EngineHost, ToolchainProbes: tcs}
	if _, err := newEngine(context.Background(), &opt, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(opt.Toolchains, []string{"go"}) {
		t.Fatalf("toolchains = %v, want [go]", opt.Toolchains)
	}
	if !strings.Contains(logs.String(), "toolchain=cpp") || strings.Contains(logs.String(), "toolchain=go ") {
		t.Fatalf("logs do not warn of cpp alone:\n%s", logs.String())
	}
}

// Unset, a worker probes for python and node, as before RFC HH, and warns
// of neither when it lacks them: the operator declared nothing.
func TestNewEngine_WithNoDeclaredListReportsPythonAndNodeSilently(t *testing.T) {
	fakePath(t, "python3", "node", "go")
	var logs bytes.Buffer
	opt := Options{Engine: workspace.EngineHost}
	if _, err := newEngine(context.Background(), &opt, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(opt.Toolchains, []string{"python", "node"}) {
		t.Fatalf("toolchains = %v, want [python node]", opt.Toolchains)
	}
	fakePath(t, "python3")
	opt = Options{Engine: workspace.EngineHost}
	logs.Reset()
	if _, err := newEngine(context.Background(), &opt, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(opt.Toolchains, []string{"python"}) || strings.Contains(logs.String(), "toolchain=") {
		t.Fatalf("toolchains = %v, logs:\n%s", opt.Toolchains, logs.String())
	}
}

// A list that would break the probe stops the worker before it attaches.
func TestRun_RefusesAMalformedToolchainList(t *testing.T) {
	err := Run(context.Background(), Options{Token: "t", Target: "127.0.0.1:1", Root: t.TempDir(),
		ToolchainProbes: []engine.Toolchain{{Name: "go", Binaries: []string{"/bin/go"}}}})
	if err == nil || !strings.Contains(err.Error(), "/bin/go") {
		t.Fatalf("err = %v, want a refusal naming the binary", err)
	}
}
