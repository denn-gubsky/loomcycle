package synth

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/speech"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
)

const rate = 8000 // 16-bit mono

// tenthPerChar is the fake engine's speaking rate: 0.1 s of audio a character.
func tenthPerChar(text string) time.Duration {
	return time.Duration(utf8.RuneCountInString(text)) * 100 * time.Millisecond
}

// wav is a 16-bit mono PCM WAV lasting d.
func wav(d time.Duration) []byte {
	data := make([]byte, int(d.Seconds()*rate)*2)
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

// engine is a speech provider that answers each piece with silence lasting a
// tenth of a second per character; failOn makes that call (1-based) fail.
type engine struct {
	providers.Provider
	failOn int

	mu   sync.Mutex
	sent []providers.SpeechRequest
}

func (e *engine) ID() string { return "speech-local" }
func (e *engine) SynthesizeSpeech(_ context.Context, req providers.SpeechRequest) (providers.SpeechResponse, error) {
	e.mu.Lock()
	e.sent = append(e.sent, req)
	n := len(e.sent)
	e.mu.Unlock()
	if n == e.failOn {
		return providers.SpeechResponse{}, errors.New("openai-speech: 503 busy")
	}
	return providers.SpeechResponse{Audio: wav(tenthPerChar(req.Input)), ContentType: "audio/wav", CredentialSource: "operator"}, nil
}

type ledger struct {
	mu      sync.Mutex
	charges []transcribe.Charge
}

func (l *ledger) Charge(_ context.Context, c transcribe.Charge) (transcribe.Price, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.charges = append(l.charges, c)
	return transcribe.Price{Cost: float64(c.InputChars) / 1e6 * 15, Currency: "USD", Priced: true}, nil
}
func (l *ledger) Estimate(string, string, int, int) transcribe.Price       { return transcribe.Price{} }
func (l *ledger) EstimateSeconds(string, string, float64) transcribe.Price { return transcribe.Price{} }

func speechConfig() *config.SpeechConfig {
	return (&config.SpeechConfig{
		Engine:          config.SpeechEngineOpenAISpeech,
		DefaultLanguage: "uk",
		Voices: map[string]config.SpeechVoice{
			"uk": {Model: "tts-uk", Voice: "ukrainian_tts"},
			"en": {Model: "phonon", Voice: "Hadley", Alternates: []string{"Zoey"}},
		},
	}).Resolved()
}

func deps(cfg *config.SpeechConfig, e *engine, l *ledger) Deps {
	return Deps{
		Config: cfg,
		Resolve: func(_ context.Context, alias string) (string, string, error) {
			return "speech-local", "model-of-" + alias, nil
		},
		Provider: func(string) (providers.Provider, error) { return e, nil },
		Ledger:   l,
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-3 }

func TestPrepare_PicksTheVoiceByScriptAndKeepsToTheList(t *testing.T) {
	cfg := speechConfig()
	caps := OperatorCaps(cfg)
	for _, c := range []struct {
		req         Request
		lang, voice string
		err         error
	}{
		{req: Request{Text: "Привіт, це перевірка голосу."}, lang: "uk", voice: "ukrainian_tts"},
		{req: Request{Text: "Hello, this is a voice check."}, lang: "uk", voice: "ukrainian_tts"}, // Latin is never guessed
		{req: Request{Text: "Hello.", Language: "EN"}, lang: "en", voice: "Hadley"},
		{req: Request{Text: "Hello.", Language: "en", Voice: "Zoey"}, lang: "en", voice: "Zoey"},
		{req: Request{Text: "Hello.", Language: "en", Voice: "Bob"}, err: ErrVoice},
		{req: Request{Text: "Hallo.", Language: "de"}, err: ErrLanguage},
		{req: Request{Text: "<!-- only a comment -->"}, err: ErrEmpty},
	} {
		job, err := Prepare(cfg, caps, c.req)
		if c.err != nil {
			if !errors.Is(err, c.err) {
				t.Errorf("%+v: err = %v, want %v", c.req, err, c.err)
			}
			continue
		}
		if err != nil || job.Language != c.lang || job.Voice != c.voice {
			t.Errorf("%+v: job = %+v, %v; want %s/%s", c.req, job, err, c.lang, c.voice)
		}
	}
}

func TestPrepare_EncodedFormatsNeedTheEncoderThatMakesThem(t *testing.T) {
	plain := speechConfig()
	withOgg := speechConfig()
	withOgg.Encode = &config.HelperCommand{Command: "ffmpeg -i {in} -c:a libopus {out}"}
	withOgg.Format = config.SpeechFormatOgg
	for _, c := range []struct {
		cfg    *config.SpeechConfig
		format string
		want   string
		err    error
	}{
		{plain, "", "wav", nil},
		{plain, "wav", "wav", nil},
		{plain, "mp3", "", ErrEncodeNotConfigured},
		{plain, "ogg", "", ErrEncodeNotConfigured},
		{withOgg, "", "ogg", nil},
		{withOgg, "opus", "ogg", nil},
		{withOgg, "mp3", "", ErrEncodeNotConfigured},
		{withOgg, "flac", "", ErrFormat},
	} {
		job, err := Prepare(c.cfg, OperatorCaps(c.cfg), Request{Text: "Привіт.", Format: c.format})
		if c.err != nil {
			if !errors.Is(err, c.err) {
				t.Errorf("encode=%v format %q: err = %v, want %v", c.cfg.Encode != nil, c.format, err, c.err)
			}
			continue
		}
		if err != nil || job.Format != c.want {
			t.Errorf("encode=%v format %q: got %q, %v; want %q", c.cfg.Encode != nil, c.format, job.Format, err, c.want)
		}
	}
	if got := Formats(withOgg); len(got) != 2 || got[1] != "ogg" {
		t.Errorf("Formats = %v, want [wav ogg]", got)
	}
	if got := Formats(plain); len(got) != 1 {
		t.Errorf("Formats without an encoder = %v, want [wav]", got)
	}
}

// Two paragraphs cut into three pieces: each piece is one request and one
// ledger row, one pause goes between the paragraphs and none inside one, and
// the budget ends holding what was spent.
func TestRun_SplitsJoinsAndChargesEachPiece(t *testing.T) {
	cfg := speechConfig()
	caps := OperatorCaps(cfg)
	caps.MaxCharsPerCall = 30
	text := "# Звіт\n\nПерше речення тут. Друге речення тут.\n\nТретій абзац."
	job, err := Prepare(cfg, caps, Request{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	e, l := &engine{}, &ledger{}
	d := deps(cfg, e, l)
	slots := 0
	d.Slot = func(context.Context) (func(), error) { slots++; return func() {}, nil }
	b := speech.NewBudget(caps)
	res, err := Run(context.Background(), d, job, []*speech.Budget{b})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.sent) != len(job.Pieces) || len(l.charges) != len(job.Pieces) || slots != len(job.Pieces) || len(job.Pieces) < 3 {
		t.Fatalf("pieces=%d requests=%d charges=%d slots=%d", len(job.Pieces), len(e.sent), len(l.charges), slots)
	}
	speechSeconds, paragraphs := 0.0, 0
	for i, c := range l.charges {
		sent := e.sent[i]
		if sent.Format != "wav" || sent.Voice != "ukrainian_tts" || sent.Model != "model-of-tts-uk" {
			t.Errorf("request %d = %+v", i, sent)
		}
		want := tenthPerChar(sent.Input).Seconds()
		if c.Purpose != transcribe.PurposeTTS || c.InputChars != int64(utf8.RuneCountInString(sent.Input)) || !near(c.AudioSeconds, want) || c.Provider != "speech-local" {
			t.Errorf("charge %d = %+v, want tts, %d chars, %.1f s", i, c, utf8.RuneCountInString(sent.Input), want)
		}
		speechSeconds += c.AudioSeconds
		if job.Pieces[i].ParagraphEnd && i < len(job.Pieces)-1 {
			paragraphs++
		}
	}
	if paragraphs == 0 || paragraphs == len(job.Pieces)-1 {
		t.Fatalf("want pieces both inside and at the end of a paragraph, got %+v", job.Pieces)
	}
	if want := speechSeconds + float64(paragraphs)*ParagraphGap.Seconds(); !near(res.Seconds, want) {
		t.Errorf("audio lasts %.3f s, want %.3f (speech plus one pause per paragraph end)", res.Seconds, want)
	}
	if res.ContentType != "audio/wav" || res.Chars != job.Chars || !res.Priced || res.Language != "uk" {
		t.Errorf("result = %+v", res)
	}
	if b.Chars() != job.Chars || !near(b.Seconds(), speechSeconds) {
		t.Errorf("budget holds %d chars / %.3f s, want %d / %.3f", b.Chars(), b.Seconds(), job.Chars, speechSeconds)
	}
}

// A failed request returns no audio; the pieces already spoken stay charged
// and counted, and the rest of the reservation is given back.
func TestRun_FailedPieceReturnsNoAudioAndGivesBackTheRest(t *testing.T) {
	cfg := speechConfig()
	caps := OperatorCaps(cfg)
	caps.MaxCharsPerCall = 20
	job, err := Prepare(cfg, caps, Request{Text: "Перше речення тут. Друге речення тут. Третє речення тут."})
	if err != nil || len(job.Pieces) < 3 {
		t.Fatalf("job = %+v, %v", job, err)
	}
	e, l := &engine{failOn: 2}, &ledger{}
	b := speech.NewBudget(caps)
	res, err := Run(context.Background(), deps(cfg, e, l), job, []*speech.Budget{b})
	if err == nil || res.Audio != nil {
		t.Fatalf("err = %v, audio = %d bytes; want an error and no audio", err, len(res.Audio))
	}
	first := utf8.RuneCountInString(job.Pieces[0].Text)
	if len(e.sent) != 2 || len(l.charges) != 1 || b.Chars() != first || !near(b.Seconds(), tenthPerChar(job.Pieces[0].Text).Seconds()) {
		t.Errorf("requests=%d charges=%d budget=%d chars / %.2f s; want 2, 1, %d", len(e.sent), len(l.charges), b.Chars(), b.Seconds(), first)
	}
}

func TestRun_RefusedReservationSendsNothing(t *testing.T) {
	cfg := speechConfig()
	caps := OperatorCaps(cfg)
	job, err := Prepare(cfg, caps, Request{Text: "Привіт, це перевірка голосу."})
	if err != nil {
		t.Fatal(err)
	}
	roomy := speech.NewBudget(caps)
	tight := speech.NewBudget(speech.Caps{MaxCharsPerRun: 5, MaxSecondsPerRun: 60})
	e := &engine{}
	_, err = Run(context.Background(), deps(cfg, e, &ledger{}), job, []*speech.Budget{roomy, tight})
	if !errors.Is(err, speech.ErrCapExceeded) || len(e.sent) != 0 || roomy.Chars() != 0 || roomy.Seconds() != 0 {
		t.Fatalf("err=%v requests=%d roomy=%d/%v; want a cap refusal, nothing sent, nothing held", err, len(e.sent), roomy.Chars(), roomy.Seconds())
	}
}

// The encoder is the operator's command, run on the joined WAV in a scratch
// folder that is removed afterwards. cp stands in for ffmpeg.
func TestRun_EncodesWithTheOperatorHelper(t *testing.T) {
	cfg := speechConfig()
	cfg.Encode = &config.HelperCommand{Command: "cp {in} {out}"}
	cfg.Format = config.SpeechFormatOgg
	job, err := Prepare(cfg, OperatorCaps(cfg), Request{Text: "Привіт."})
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	d := deps(cfg, &engine{}, &ledger{})
	d.Scratch = func(context.Context) (string, error) { return scratch, nil }
	res, err := Run(context.Background(), d, job, []*speech.Budget{speech.NewBudget(OperatorCaps(cfg))})
	if err != nil {
		t.Fatal(err)
	}
	if res.ContentType != "audio/ogg" || res.Format != "ogg" || !strings.HasPrefix(string(res.Audio), "RIFF") {
		t.Errorf("result = %s %s, %d bytes", res.ContentType, res.Format, len(res.Audio))
	}
	if left, _ := os.ReadDir(scratch); len(left) != 0 {
		t.Errorf("scratch folder left behind: %v", left)
	}
}
