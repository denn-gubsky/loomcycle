// Package unitgen is the model that writes Document derived search units (RFC DM
// Design C): a description, claims and questions about one chunk. It holds the
// prompts and the reading of the model's answer; which chunks get units, and
// storing them, is the Document tool's generation pass.
package unitgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

const (
	defaultTimeout       = 120 * time.Second
	defaultContextTokens = 16384
	defaultMaxOutput     = 2000
	// keyEnvName is the credential a generator answers to when its endpoint is
	// overridden without a named key — see providerbuild.ServiceProvider.
	keyEnvName = "LOOMCYCLE_UNIT_GENERATOR_API_KEY"
	// Each list is asked for as "3 to 6"; a model that writes more is trimmed so
	// one chunk cannot flood the shared search pool.
	maxPerList = 6
	attempts   = 3
)

// The prompts measured on QASPER (bench/docs/contextual/contextualize.py),
// generalised from "paper" to "document": the probe corpus was scientific papers,
// and a policy, a manual or an FAQ is not one. The structure, the counts and the
// "own terms" instruction are unchanged.
const describePrompt = `Document: %s
Section: %s
<section>
%s
</section>
Describe in one or two sentences what this section states. Name the specific terms, entities, numbers and findings it contains, in the document's own terms. Answer only with the description.`

const unitsPrompt = `Document: %s
Section: %s
<section>
%s
</section>
From this section write:
1. "claims": 3 to 6 atomic claims. Each is one self-contained sentence stating ONE specific fact from the section — a term, entity, number, rule or finding — in the document's own terms, understandable without the section.
2. "questions": 3 to 6 questions that this section answers, phrased as a reader of the document would ask them.
If the section is very short, write fewer. Answer only with JSON: {"claims": [...], "questions": [...]}`

// unitsSchema asks the provider for exactly the answer's shape where it can
// enforce one (Ollama `format`, OpenAI structured outputs); a driver that cannot
// sends nothing for it, and the reply is parsed the same way either way.
var unitsSchema = json.RawMessage(`{"type":"object","properties":{"claims":{"type":"array","items":{"type":"string"}},"questions":{"type":"array","items":{"type":"string"}}},"required":["claims","questions"]}`)

// Generator is one configured unit generator. Safe for concurrent use.
type Generator struct {
	provider      providers.Provider
	model         string
	timeout       time.Duration
	effort        string
	contextTokens int
	maxOutput     int
}

// Build constructs the generator declared in cfg.Memory.UnitGenerator, or returns
// nil when none is declared. A declared one that cannot be built is an error.
func Build(cfg *config.Config) (*Generator, error) {
	gc := cfg.Memory.UnitGenerator
	if !gc.Configured() {
		return nil, nil
	}
	p, _, model, err := providerbuild.ServiceProvider(cfg, "memory.unit_generator", providerbuild.ServiceEndpoint{
		Provider: gc.Provider, Model: gc.Model, BaseURL: gc.BaseURL, APIKeyEnv: gc.APIKeyEnv,
	}, keyEnvName)
	if err != nil {
		return nil, err
	}
	gc.Model = model
	return New(p, gc), nil
}

// New wraps a constructed provider; Build is the production path.
func New(p providers.Provider, gc config.UnitGeneratorConfig) *Generator {
	g := &Generator{
		provider: p, model: gc.Model, effort: gc.Effort,
		timeout:       time.Duration(gc.TimeoutMs) * time.Millisecond,
		contextTokens: gc.ContextTokens, maxOutput: gc.MaxOutputTokens,
	}
	if g.timeout <= 0 {
		g.timeout = defaultTimeout
	}
	if g.contextTokens <= 0 {
		g.contextTokens = defaultContextTokens
	}
	if g.maxOutput <= 0 {
		g.maxOutput = defaultMaxOutput
	}
	return g
}

// ModelID is recorded on every unit the generator writes.
func (g *Generator) ModelID() string { return g.model }

// Generate writes the units r asks for: a description (one call) and claims and
// questions (one JSON call), each only when its kind is requested. An answer that
// does not parse is retried; a chunk whose answer never parses fails, and the pass
// reports it rather than store anything.
func (g *Generator) Generate(ctx context.Context, r memory.UnitRequest) ([]memory.GeneratedUnit, error) {
	want := map[string]bool{}
	for _, k := range r.Kinds {
		want[k] = true
	}
	section := r.SectionPath
	if section == "" {
		section = r.DocumentTitle
	}
	var out []memory.GeneratedUnit
	if want["description"] {
		text, err := g.complete(ctx, fmt.Sprintf(describePrompt, r.DocumentTitle, section, r.Text), nil, func(s string) error {
			if strings.TrimSpace(s) == "" {
				return errors.New("an empty description")
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("description: %w", err)
		}
		out = append(out, memory.GeneratedUnit{Kind: memory.UnitDescription, Text: strings.TrimSpace(text)})
	}
	if want["claims"] || want["questions"] {
		var parsed struct {
			Claims    []string `json:"claims"`
			Questions []string `json:"questions"`
		}
		_, err := g.complete(ctx, fmt.Sprintf(unitsPrompt, r.DocumentTitle, section, r.Text),
			&providers.OutputFormat{Name: "units", Schema: unitsSchema}, func(s string) error {
				return json.Unmarshal([]byte(jsonObject(s)), &parsed)
			})
		if err != nil {
			return nil, fmt.Errorf("claims and questions: %w", err)
		}
		if want["claims"] {
			out = append(out, units(memory.UnitClaim, parsed.Claims)...)
		}
		if want["questions"] {
			out = append(out, units(memory.UnitQuestion, parsed.Questions)...)
		}
	}
	return out, nil
}

// complete sends one prompt and returns the reply once accept takes it.
//
// A RETRY IS NOT A REPEAT. At temperature 0 a retry reproduces a degenerate answer
// exactly — measured on LaTeX-heavy sections, where the model repeated one token to
// the output cap — so retries sample at a low temperature instead. (The probe used
// Ollama's repeat_penalty for the same purpose; no driver carries that parameter,
// and any non-zero temperature leaves the loop just as surely.)
func (g *Generator) complete(ctx context.Context, prompt string, format *providers.OutputFormat, accept func(string) error) (string, error) {
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		temp := 0.0
		if attempt > 0 {
			temp = 0.4
		}
		reply, err := g.call(ctx, prompt, format, temp)
		if err == nil {
			if err = accept(reply); err == nil {
				return reply, nil
			}
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return "", lastErr
}

func (g *Generator) call(ctx context.Context, prompt string, format *providers.OutputFormat, temp float64) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	ch, err := g.provider.Call(callCtx, providers.Request{
		Model:            g.model,
		MaxTokens:        g.maxOutput,
		Temperature:      &temp,
		Effort:           g.effort,
		MaxContextTokens: g.contextTokens,
		OutputFormat:     format,
		Messages:         []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: prompt}}}},
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var callErr error
	done := false
	for ev := range ch { // drain to the end: an abandoned channel blocks the driver
		switch ev.Type {
		case providers.EventText:
			b.WriteString(ev.Text)
		case providers.EventDone:
			done = true
		case providers.EventError:
			if callErr == nil {
				callErr = errors.New(ev.Error)
			}
		}
	}
	if callErr == nil && !done {
		// A stream the timeout cut off closes with neither an error nor a done event;
		// its partial text is not an answer.
		if callErr = callCtx.Err(); callErr == nil {
			callErr = errors.New("the reply stream ended before it completed")
		}
	}
	if callErr != nil {
		return "", callErr
	}
	return b.String(), nil
}

// jsonObject cuts the outermost JSON object out of a reply, for a model that wraps
// its JSON in prose or a code fence.
func jsonObject(s string) string {
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return s
}

func units(kind string, texts []string) []memory.GeneratedUnit {
	var out []memory.GeneratedUnit
	seen := map[string]bool{}
	for _, t := range texts {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, memory.GeneratedUnit{Kind: kind, Text: t})
		if len(out) == maxPerList {
			break
		}
	}
	return out
}
