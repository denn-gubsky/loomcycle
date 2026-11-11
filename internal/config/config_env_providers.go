package config

// The env blocks LoadLayers reads, in its order; see LoadLayers.

import (
	"os"
	"strconv"
	"time"
)

// applyEnvLoopTuning reads the tool parallelism, Ollama, probe, fallback-pin
// and SSE keepalive knobs.
func applyEnvLoopTuning(cfg *Config) {
	// LOOMCYCLE_TOOL_PARALLELISM overrides the per-iteration
	// tool_call concurrency cap (default 8). Floor 1, ceiling 64
	// — anything beyond 64 would spawn a goroutine storm that
	// outweighs realistic fan-out. Zero / negative values fall
	// through to the default.
	if v := os.Getenv("LOOMCYCLE_TOOL_PARALLELISM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 64 {
				n = 64
			}
			cfg.Env.ToolParallelism = n
		}
	}
	// LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX overrides the ollama-local
	// driver's options.num_ctx. Required when running prompts above
	// ~4k tokens against a local Ollama whose Modelfile doesn't pin
	// num_ctx — see the field doc on Env.OllamaLocalNumCtx.
	if v := os.Getenv("LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.OllamaLocalNumCtx = n
		}
	}
	if v := os.Getenv("LOOMCYCLE_OLLAMA_NUM_CTX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.OllamaNumCtx = n
		}
	}
	// LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU forces the number of model layers
	// Ollama offloads to the GPU on every ollama-local request — see the
	// field doc on Env.OllamaLocalNumGpu. 0/unset = omit (Ollama
	// auto-detects); a literal 0 must NOT be sent (it would force CPU).
	if v := os.Getenv("LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.OllamaLocalNumGpu = n
		}
	}
	// LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS overrides the default 15-min
	// probe cadence. Clamped to a 1-hour ceiling so a typo or
	// misunderstanding can't stretch the recovery window beyond what
	// the design assumed. Sub-minute intervals are also clamped up to
	// 60s — anything tighter risks hammering provider /v1/models
	// endpoints for negligible recovery-time benefit.
	if v := os.Getenv("LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			d := time.Duration(n) * time.Millisecond
			const minProbe = 60 * time.Second
			const maxProbe = 60 * time.Minute
			if d < minProbe {
				d = minProbe
			}
			if d > maxProbe {
				d = maxProbe
			}
			cfg.Env.ResolveProbeInterval = d
		}
	}
	// LOOMCYCLE_FALLBACK_PIN_AFTER_SUCCESS: when set to "1",
	// suppress provider fallback after the first successful turn.
	// Opt-in for v0.8.x; default-on planned for v0.9.x.
	cfg.Env.FallbackPinAfterSuccess = os.Getenv("LOOMCYCLE_FALLBACK_PIN_AFTER_SUCCESS") == "1"
	// LOOMCYCLE_SSE_KEEPALIVE_MS sets the SSE keepalive cadence.
	// Default 20 s; 0 (or any value ≤ 0) disables. Floor 1 s for
	// positive values so a misconfigured tiny value can't busy-loop
	// the writer; ceiling 5 min so a misread (e.g. seconds vs ms)
	// can't effectively disable keepalive in practice.
	//
	// Treating negative values as "disable" (same as 0) matches
	// operator intent for the typical typo case and is consistent
	// with the disable contract on `sse.startKeepalive` itself
	// (interval <= 0 → no goroutine).
	cfg.Env.SSEKeepaliveInterval = 20 * time.Second
	if v := os.Getenv("LOOMCYCLE_SSE_KEEPALIVE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				cfg.Env.SSEKeepaliveInterval = 0
			} else {
				d := time.Duration(n) * time.Millisecond
				const minSSE = 1 * time.Second
				const maxSSE = 5 * time.Minute
				if d < minSSE {
					d = minSSE
				}
				if d > maxSSE {
					d = maxSSE
				}
				cfg.Env.SSEKeepaliveInterval = d
			}
		}
	}
}

// applyEnvProviderTimeouts reads the provider and local-Ollama streaming
// timeouts.
func applyEnvProviderTimeouts(cfg *Config) {
	// Provider streaming timeouts. Defaults match streamhttp.Default*.
	// Negative or zero values are NOT treated as "disable" — there's no
	// safe interpretation of "stream forever" given the agent loop's
	// liveness assumptions. Operators bumping these should pick a real
	// number; bad input falls through to the default.
	cfg.Env.ProviderHeaderTimeout = 60 * time.Second
	if v := os.Getenv("LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.ProviderHeaderTimeout = time.Duration(n) * time.Millisecond
		}
	}
	cfg.Env.ProviderIdleTimeout = 90 * time.Second
	if v := os.Getenv("LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.ProviderIdleTimeout = time.Duration(n) * time.Millisecond
		}
	}

	// Local Ollama gets a MUCH more generous default than the cloud providers:
	// a cold model load + large-context prompt eval genuinely takes minutes to
	// first token — a 27B-class model at a 128K num_ctx on non-datacenter
	// hardware routinely exceeds 5 min just to LOAD (Ollama sends no response
	// headers until the model is resident + generation starts, so the header
	// timeout covers LOAD time). The old 300s default surfaced as "net/http:
	// timeout awaiting response headers" on exactly that setup — loomcycle
	// giving up before a healthy local model had answered. 600s (10 min) is a
	// realistic patient default; still bounded so a genuinely dead server fails
	// over. A very large model / slow box raises
	// LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS further. Scoped to the
	// ollama-local registration; see the field docs on Env.OllamaLocalHeaderTimeout.
	cfg.Env.OllamaLocalHeaderTimeout = 600 * time.Second
	if v := os.Getenv("LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.OllamaLocalHeaderTimeout = time.Duration(n) * time.Millisecond
		}
	}
	cfg.Env.OllamaLocalIdleTimeout = 600 * time.Second
	if v := os.Getenv("LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Env.OllamaLocalIdleTimeout = time.Duration(n) * time.Millisecond
		}
	}
}
