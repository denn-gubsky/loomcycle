package http

import (
	"context"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/openaiaudio"
)

// FileSTT builds what the File tool needs to transcribe recordings (RFC FG)
// from the operator's transcription block: the openai-audio engine on this
// server's provider registry, and the usage ledger. nil when speech-to-text is
// off. Built once at boot, as FileOCR is: a change to the block takes a
// restart, while the models: alias, the providers and the pricing table it
// reads stay live.
func (s *Server) FileSTT() (*builtin.FileSTT, error) {
	t := s.cfg().Transcription.Resolved()
	if !t.STTEnabled() {
		return nil, nil
	}
	if t.STT.Engine != config.STTEngineOpenAIAudio {
		return nil, errors.New("transcription.stt: only the openai-audio engine is built")
	}
	engine, err := openaiaudio.New(openaiaudio.Options{
		Config:   *t.STT,
		Resolve:  s.ResolveSTTModel,
		Provider: func(id string) (providers.Provider, error) { return s.providers.Get(id) },
	})
	if err != nil {
		return nil, err
	}
	return &builtin.FileSTT{Config: t, Engine: engine, Ledger: transcriptionLedger{s: s}}, nil
}

// ResolveSTTModel names the provider and concrete model of the operator's
// speech-to-text alias, read from the live configuration. The provider is the
// operator's choice, not a tier's: a run restricted from the operator's key
// is still refused at the driver unless its tenant holds a key of its own.
func (s *Server) ResolveSTTModel(context.Context) (string, string, error) {
	cfg := s.cfg()
	stt := cfg.Transcription.STT
	if stt == nil {
		return "", "", errors.New("speech-to-text is not configured")
	}
	ref, ok := cfg.Models[stt.Model]
	if !ok || ref.Model == "" {
		return "", "", fmt.Errorf("transcription.stt.model %q is not a models: alias naming a model", stt.Model)
	}
	return stt.Provider, ref.Model, nil
}
