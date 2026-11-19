package config

import (
	"strings"
	"testing"
)

// TestTranscriptionConfig_DjVuHelpersAreCheckedAtLoad: a DjVu helper is
// refused at load the way the runner would refuse it (RFC GV P3): a key the
// block does not take, a command the runner cannot parse, a placeholder its
// job does not fill or one it needs and lacks, inspect without text, and
// a page rasterizer (RFC GV P4) missing a placeholder its job fills or
// naming one it does not.
func TestTranscriptionConfig_DjVuHelpersAreCheckedAtLoad(t *testing.T) {
	const text = `text: { command: "djvutxt --detail=page --page={pages} {in}" }`
	for _, c := range []struct {
		name, block string
		want        []string
	}{
		{"unknown key", "djvu: { txt: { command: x } }", []string{"transcription.ocr.djvu", `unknown key "txt"`}},
		{"unknown helper key", "djvu: { text: { cmd: x } }", []string{"transcription.ocr.djvu.text", `unknown key "cmd"`}},
		{"rasterize unknown key", "djvu: { " + text + ", rasterize: { cmd: x } }", []string{"transcription.ocr.djvu.rasterize", `unknown key "cmd"`}},
		{"rasterize without page", "djvu: { " + text + ", rasterize: { command: \"ddjvu -format=tiff -size={size} {in} {out}\" } }", []string{"transcription.ocr.djvu.rasterize.command", "{page}"}},
		{"rasterize without size", "djvu: { " + text + ", rasterize: { command: \"ddjvu -format=tiff -page={page} {in} {out}\" } }", []string{"transcription.ocr.djvu.rasterize.command", "{size}"}},
		{"rasterize without out", "djvu: { " + text + ", rasterize: { command: \"ddjvu -format=tiff -page={page} -size={size} {in}\" } }", []string{"transcription.ocr.djvu.rasterize.command", "{out}"}},
		{"rasterize pages", "djvu: { " + text + ", rasterize: { command: \"ddjvu -page={pages} -size={size} {in} {out}\" } }", []string{"transcription.ocr.djvu.rasterize.command", "unknown placeholder {pages}"}},
		{"rasterize shell syntax", "djvu: { " + text + ", rasterize: { command: \"ddjvu -page={page} -size={size} {in} {out} > /tmp/x\" } }", []string{"transcription.ocr.djvu.rasterize.command"}},
		{"no text", "djvu: { inspect: { command: \"djvused -e {script} {in}\" } }", []string{"transcription.ocr.djvu.text is required"}},
		{"text empty", "djvu: { text: { command: \"  \" } }", []string{"transcription.ocr.djvu.text.command is required"}},
		{"text without pages", "djvu: { text: { command: \"djvutxt --detail=page {in}\" } }", []string{"transcription.ocr.djvu.text.command", "{pages}"}},
		{"text without in", "djvu: { text: { command: \"djvutxt --page={pages} book.djvu\" } }", []string{"transcription.ocr.djvu.text.command", "{in}"}},
		{"text unknown placeholder", "djvu: { text: { command: \"djvutxt --page={pages} {in} {out}\" } }", []string{"transcription.ocr.djvu.text.command", "unknown placeholder {out}"}},
		{"text shell syntax", "djvu: { text: { command: \"djvutxt --page={pages} {in} | head\" } }", []string{"transcription.ocr.djvu.text.command"}},
		{"text program a placeholder", "djvu: { text: { command: \"{in} --page={pages}\" } }", []string{"transcription.ocr.djvu.text.command", "program"}},
		{"inspect without script", "djvu: { " + text + ", inspect: { command: \"djvused -e print-meta {in}\" } }", []string{"transcription.ocr.djvu.inspect.command", "{script}"}},
		{"inspect pages", "djvu: { " + text + ", inspect: { command: \"djvused -e {script} {in} {pages}\" } }", []string{"transcription.ocr.djvu.inspect.command", "unknown placeholder {pages}"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadTranscription(t, "transcription:\n  ocr: { engine: off, "+c.block+" }\n")
			if err == nil {
				t.Fatal("the config loaded; want a refusal")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %q, want it to contain %q", err, w)
				}
			}
		})
	}

	cfg, err := LoadLayers(Layer{Name: "op", Data: []byte(transcriptionBase + `
transcription:
  ocr:
    engine: off
    djvu:
      text: { command: "djvutxt --detail=page --page={pages} {in}" }
      inspect: { command: "djvused -e {script} {in}" }
      rasterize: { command: "ddjvu -format=tiff -page={page} -size={size} {in} {out}" }
`)})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	d := cfg.Transcription.OCR.DjVu
	if d == nil || d.Text == nil || d.Inspect == nil || d.Inspect.Command != "djvused -e {script} {in}" {
		t.Fatalf("djvu = %+v", d)
	}
	if d.Rasterize == nil || d.Rasterize.Command != "ddjvu -format=tiff -page={page} -size={size} {in} {out}" {
		t.Fatalf("rasterize = %+v", d.Rasterize)
	}
	if again := cfg.Transcription.Resolved().OCR.DjVu; again == d || again.Text == d.Text || again.Rasterize == d.Rasterize {
		t.Error("Resolved shares the DjVu helpers with the stored config")
	}
}
