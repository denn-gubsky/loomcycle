package config

// The env blocks LoadLayers reads, in its order; see LoadLayers.

import (
	"os"
	"strconv"
)

// applyEnvLoopTuning reads the local Ollama context window and the
// fallback-pin switch.
func applyEnvLoopTuning(cfg *Config) {
	// LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX overrides the ollama-local
	// driver's options.num_ctx. Required when running prompts above
	// ~4k tokens against a local Ollama whose Modelfile doesn't pin
	// num_ctx — see the field doc on Env.OllamaLocalNumCtx.
	if v := os.Getenv("LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.OllamaLocalNumCtx = n
			// The yaml option used to win. Say which one holds now, so a
			// deployment that set both does not change window unannounced.
			if y, ok := providerOptionInt(cfg.Providers["ollama-local"], "num_ctx"); ok && y != int64(n) {
				cfg.Warnings = append(cfg.Warnings, "LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX and providers.ollama-local.options.num_ctx are both set and differ: the environment's value is the one used; remove one of them")
			}
		}
	}
	// LOOMCYCLE_FALLBACK_PIN_AFTER_SUCCESS: when set to "1",
	// suppress provider fallback after the first successful turn.
	// Opt-in.
	cfg.Env.FallbackPinAfterSuccess = os.Getenv("LOOMCYCLE_FALLBACK_PIN_AFTER_SUCCESS") == "1"
}
