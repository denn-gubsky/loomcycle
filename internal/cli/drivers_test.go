package cli

// RFC BF P3 — loadLayeredConfig now prepends the embedded default-providers layer
// (the sole source of the built-ins after the hardcoded floor was deleted), so
// LoadLayers validates each declared entry's driver against the providers driver
// registry (providers.RegisteredDrivers), populated by each driver package's
// init() self-registration. The real loomcycle binary gets that via blank imports
// in cmd/loomcycle; the CLI test binary gets it here so validate()/loadLayeredConfig
// exercise the same registered factories the server does. None of these packages
// import internal/cli, so there is no import cycle.
import (
	_ "github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/deepseek"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/gemini"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/mock"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/ollama"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/openai"
)
