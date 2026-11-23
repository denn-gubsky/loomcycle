// Package kernel runs a workspace's persistent interpreters (RFC GW §5.7):
// one small driver program of our own per language, started in the
// workspace through the worker's engine, spoken to in JSON lines on its
// stdin and stdout. It is the worker's half: the core only sends run_cell
// and reset_kernel ops.
//
// What a driver sends is untrusted. A cell runs inside the driver's process
// and can write to the protocol stream itself, so every frame is read with
// a bounded reader and taken only as the current cell's own output.
package kernel

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// The kernels a workspace offers.
const (
	Python = "python"
	Node   = "node"
)

// Valid reports whether kind is a kernel this build runs.
func Valid(kind string) bool { return kind == Python || kind == Node }

//go:embed drivers/python_driver.py
var pythonDriver []byte

//go:embed drivers/node_driver.js
var nodeDriver []byte

// ImageDriversDir is where the workspace session image (Dockerfile.workspace)
// carries the two drivers: the paths a container engine's session sees.
const ImageDriversDir = "/opt/loomcycle/drivers"

// Drivers are the paths of the two driver files, as the engine's programs
// see them.
type Drivers struct {
	Python string
	Node   string
}

// WriteDrivers writes the embedded drivers into dir/<digest>/ and returns
// their paths. The folder is named by the drivers' digest, so a worker of
// another build never runs this build's drivers, and a file someone changed
// is written again. It is for the host engine, whose programs see the
// worker's disk; an image carries its own copies at a path its engine
// knows (RFC GW P3).
func WriteDrivers(dir string) (Drivers, error) {
	h := sha256.New()
	h.Write(pythonDriver)
	h.Write(nodeDriver)
	sub := filepath.Join(dir, hex.EncodeToString(h.Sum(nil))[:16])
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return Drivers{}, fmt.Errorf("making the drivers' folder: %w", err)
	}
	d := Drivers{Python: filepath.Join(sub, "python_driver.py"), Node: filepath.Join(sub, "node_driver.js")}
	for path, body := range map[string][]byte{d.Python: pythonDriver, d.Node: nodeDriver} {
		if same(path, body) {
			continue
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, body, 0o600); err != nil {
			return Drivers{}, fmt.Errorf("writing a driver: %w", err)
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return Drivers{}, fmt.Errorf("placing a driver: %w", err)
		}
	}
	return d, nil
}

func same(path string, body []byte) bool {
	// #nosec G304 -- path is the worker's own drivers folder joined with a fixed file name (WriteDrivers), never input from an op
	b, err := os.ReadFile(path)
	return err == nil && string(b) == string(body)
}
