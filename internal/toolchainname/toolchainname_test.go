package toolchainname

import (
	"strings"
	"testing"
)

func TestValid_AcceptsOnlyThePattern(t *testing.T) {
	for _, name := range []string{"python", "node", "go", "cpp", "c", "go1.26", "clang++", "g++", "dotnet-8", "x_y", "a" + strings.Repeat("b", 31)} {
		if !Valid(name) {
			t.Errorf("%q refused", name)
		}
	}
	for _, name := range []string{"", "Go", "1go", "-go", ".go", "+c", "go/bin", "/usr/bin/go", "go bin", "go;rm", "go$x", "go`x`", "go\n", "gö", "a" + strings.Repeat("b", 32)} {
		if Valid(name) {
			t.Errorf("%q accepted", name)
		}
	}
}
