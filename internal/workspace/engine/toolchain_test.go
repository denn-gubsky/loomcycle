package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestToolchainScript_IsOneLinePerToolchainFromTheList(t *testing.T) {
	got, err := ToolchainScript([]Toolchain{
		{Name: "python", Binaries: []string{"python3", "python"}},
		{Name: "go", Binaries: []string{"go"}},
		{Name: "cpp", Binaries: []string{"c++", "g++", "clang++"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "command -v python3 >/dev/null 2>&1 || command -v python >/dev/null 2>&1 && echo python\n" +
		"command -v go >/dev/null 2>&1 && echo go\n" +
		"command -v c++ >/dev/null 2>&1 || command -v g++ >/dev/null 2>&1 || command -v clang++ >/dev/null 2>&1 && echo cpp\n" +
		"true\n"
	if got != want {
		t.Fatalf("script =\n%s\nwant\n%s", got, want)
	}
}

// The script is never built from an entry CheckToolchains refuses: nothing
// typed by the operator reaches the shell unchecked.
func TestToolchainScript_RefusesWhatCheckToolchainsRefuses(t *testing.T) {
	many := make([]Toolchain, 33)
	for i := range many {
		many[i] = Toolchain{Name: "t" + strings.Repeat("x", i%30) + string(rune('a'+i%26)), Binaries: []string{"x"}}
	}
	nineBins := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	for name, tcs := range map[string][]Toolchain{
		"bad name":          {{Name: "Go", Binaries: []string{"go"}}},
		"shell in name":     {{Name: "go;rm", Binaries: []string{"go"}}},
		"binary with path":  {{Name: "go", Binaries: []string{"/usr/bin/go"}}},
		"shell in binary":   {{Name: "go", Binaries: []string{"go$(id)"}}},
		"no binary":         {{Name: "go"}},
		"too many binaries": {{Name: "go", Binaries: nineBins}},
		"listed twice":      {{Name: "go", Binaries: []string{"go"}}, {Name: "go", Binaries: []string{"go"}}},
		"over the cap":      many,
	} {
		if s, err := ToolchainScript(tcs); err == nil {
			t.Errorf("%s: built %q", name, s)
		}
	}
}

func TestFoundToolchains_TakesOnlyNamesOnTheList(t *testing.T) {
	tcs := []Toolchain{{Name: "python", Binaries: []string{"python3"}}, {Name: "node", Binaries: []string{"node"}}, {Name: "go", Binaries: []string{"go"}}}
	out := []byte("Welcome to the image!\nrust\nnode\ncpp python\nnode\n")
	if got := FoundToolchains(out, tcs); !slices.Equal(got, []string{"python", "node"}) {
		t.Fatalf("found = %v, want [python node]", got)
	}
}

// The generated script, run by a real sh against a PATH of fake programs,
// prints exactly the toolchains whose binaries are there: the first one
// found of several counts, and a missing one prints nothing.
func TestToolchainScript_PrintsTheToolchainsAShellFinds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	for _, bin := range []string{"g++", "python", "go"} {
		if err := os.WriteFile(filepath.Join(dir, bin), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tcs := []Toolchain{
		{Name: "python", Binaries: []string{"python3", "python"}},
		{Name: "node", Binaries: []string{"node"}},
		{Name: "cpp", Binaries: []string{"c++", "g++", "clang++"}},
		{Name: "go", Binaries: []string{"go"}},
	}
	script, err := ToolchainScript(tcs)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=" + dir}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("script failed: %v", err)
	}
	if got := FoundToolchains(out, tcs); !slices.Equal(got, []string{"python", "cpp", "go"}) {
		t.Fatalf("found = %v (output %q), want [python cpp go]", got, out)
	}
}
