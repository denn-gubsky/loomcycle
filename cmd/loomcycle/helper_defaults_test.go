package main

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestHelperDefaultsSkipped_NamesEachSettingTheDefaultsMiss: boot warns, by
// setting, for each helper command whose ffmpeg runs behind another program
// or sets its own whitelist, and stays quiet for the rest.
func TestHelperDefaultsSkipped_NamesEachSettingTheDefaultsMiss(t *testing.T) {
	cfg := &config.Config{}
	cfg.Transcription.OCR = &config.OCRConfig{Rasterize: &config.HelperCommand{Command: "pdftoppm -r 150 -png {in} {out_prefix}"}}
	cfg.Transcription.STT = &config.STTConfig{Media: &config.HelperCommand{Command: "nice -n 10 ffmpeg -i {in} -f wav {out}"}}
	cfg.Speech = &config.SpeechConfig{Encode: &config.HelperCommand{Command: "ffmpeg -protocol_whitelist file,http -i {in} {out}"}}
	got := helperDefaultsSkipped(cfg)
	if len(got) != 2 || got[0].key != "transcription.stt.media" || got[1].key != "speech.encode" {
		t.Fatalf("skips = %+v, want stt.media and speech.encode", got)
	}
	cfg.Speech.Encode.Command = "ffmpeg -i {in} -c:a libopus {out}"
	cfg.Transcription.STT.Media.Command = "ffmpeg -nostdin -i {in} -f wav {out}"
	if got := helperDefaultsSkipped(cfg); len(got) != 0 {
		t.Errorf("plain ffmpeg templates reported: %+v", got)
	}
	if got := helperDefaultsSkipped(&config.Config{}); len(got) != 0 {
		t.Errorf("no helpers configured, got %+v", got)
	}
}
