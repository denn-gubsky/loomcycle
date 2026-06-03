package codejs

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/dop251/goja"
)

// Validate parse-compiles an inline code-js body and returns its content
// hash (sha256 hex of the source bytes). It is the authorship-time check
// the AgentDef substrate runs on a `code_body` overlay so a syntax error
// is rejected at create/fork — mirroring the boot-time validateCodeAgents
// pass for filesystem agents — rather than surfacing at first fire.
//
// It is the single source of truth for the compile flags: the same
// goja.Compile(name, src, strict=false) the compiler uses at run time
// (compiler.load / loadSource), so a body that Validates here is exactly
// the body that will compile there. Pure: no Provider, no filesystem, no
// store — which keeps the tools/builtin → providers/codejs import edge
// one-directional and cycle-free.
func Validate(src string) (hash string, err error) {
	if _, err := goja.Compile("code_body", src, false); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:]), nil
}
