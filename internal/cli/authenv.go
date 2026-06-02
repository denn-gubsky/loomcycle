package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// authEnvFileName is the operator-owned env file that `loomcycle init
// --with-token` writes (mode 0600) next to loomcycle.yaml. Auto-loading it
// is what lets a brew-installed operator get an authenticated runtime with
// zero shell-rc edits — the difference between "source this every shell"
// and "it just works". The server (cmd/loomcycle) and `loomcycle doctor`
// both call LoadAuthEnv so their view of the token is identical.
const authEnvFileName = "auth.env"

// LoadAuthEnv reads <dir-of-configPath>/auth.env into the process
// environment and returns the file path + the number of variables it set.
//
// Two deliberate guarantees:
//
//   - Real env wins. A variable is set ONLY if it is currently unset, so an
//     explicit shell `export LOOMCYCLE_AUTH_TOKEN=…` always overrides the
//     file. The file is a fallback, never a silent shadow.
//   - Narrow scope. Only the operator-owned config directory is consulted,
//     and only a boring `KEY=VALUE` / `export KEY=VALUE` shape is parsed
//     (blank lines and `#` comments ignored). No globbing, no nesting.
//
// An absent file is the common case, not an error (most deployments set the
// token via the real environment). Parsing is lenient — a malformed line is
// skipped, never fatal, so a hand-edited file can't wedge startup.
func LoadAuthEnv(configPath string) (path string, n int, err error) {
	path = filepath.Join(filepath.Dir(configPath), authEnvFileName)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, 0, nil
		}
		return path, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		// Strip one layer of matching surrounding quotes if present.
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if k == "" || os.Getenv(k) != "" {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return path, n, err
		}
		n++
	}
	return path, n, sc.Err()
}
