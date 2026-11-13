package main

import (
	"github.com/denn-gubsky/loomcycle/internal/cli"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// assembleConfigLayers is the server's call of the one layer assembly
// (cli.AssembleConfigStack): boot and the runtime config reload build their
// stack through it, and `tokens`, `audit` and the introspection commands
// build theirs through the same function. It returns an error and never exits,
// so a reload can reject a broken assembly and keep the running config.
func assembleConfigLayers(presetFlags, cfgPaths []string, verbose bool) ([]config.Layer, error) {
	stack, err := cli.AssembleConfigStack(presetFlags, cfgPaths, verbose)
	if err != nil {
		return nil, err
	}
	return stack.Layers, nil
}
