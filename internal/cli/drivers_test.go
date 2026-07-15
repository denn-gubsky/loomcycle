package cli

// The production loomcycle binary registers every provider driver via blank
// imports in cmd/loomcycle/main.go, so `loomcycle validate|agents|doctor` see
// the full driver set when loadLayeredConfig prepends the embedded
// default-providers layer (RFC BF). The internal/cli TEST binary does not link
// cmd/loomcycle, so it must register the same drivers itself — otherwise the
// default-providers layer fails driver validation with "unknown driver". These
// blank imports mirror the default-providers.yaml driver set exactly.
import (
	_ "github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/deepseek"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/gemini"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/mock"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/ollama"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/openai"
)
