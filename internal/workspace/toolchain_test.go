package workspace

import (
	"strings"
	"testing"
)

func TestValidToolchainName_AcceptsOnlyThePattern(t *testing.T) {
	for _, name := range []string{"python", "node", "go", "cpp", "c", "go1.26", "clang++", "g++", "dotnet-8", "x_y", "a" + strings.Repeat("b", 31)} {
		if !ValidToolchainName(name) {
			t.Errorf("%q refused", name)
		}
	}
	for _, name := range []string{"", "Go", "1go", "-go", ".go", "+c", "go/bin", "/usr/bin/go", "go bin", "go;rm", "go$x", "go`x`", "go\n", "gö", "a" + strings.Repeat("b", 32)} {
		if ValidToolchainName(name) {
			t.Errorf("%q accepted", name)
		}
	}
}
