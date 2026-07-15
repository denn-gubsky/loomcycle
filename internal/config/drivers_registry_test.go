package config

// RFC BF P2a — validate() now checks each `providers:` entry's driver against the
// providers driver registry (providers.RegisteredDrivers / DriverDialects), which
// is populated by each driver package's init() self-registration. The server
// binary gets that via blank imports in cmd/loomcycle; the config test binary gets
// it here so validate()'s driver/dialect checks exercise the real registered
// factories (e.g. TestValidate_Providers with driver: anthropic/ollama/openai).
// None of these packages import internal/config, so there is no import cycle
// (verified: go list -deps of each driver excludes internal/config).
import (
	_ "github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/deepseek"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/gemini"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/mock"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/ollama"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/openai"
)
