package extract

import (
	"bytes"
	"context"
	"testing"
	"time"
)

const teamsVTT = "\xef\xbb\xbfWEBVTT\n\nNOTE exported by Teams\n\n" +
	"0c1d/12-0\n00:00:04.120 --> 00:00:06.000\n<v Іван Петренко>Добрий день, колеги.</v>\n\n" +
	"0c1d/13-0\n00:00:06.000 --> 00:00:09.500\n<v Іван Петренко>Почнемо засідання правління.</v>\n\n" +
	"0c1d/14-0\n00:00:10.000 --> 00:00:12.000\n<v.loud Olena **Kovalenko**>Згодна &amp; готова.</v>\n\n" +
	"0c1d/15-0\n00:10:02.000 --> 00:10:05.000\n<v Іван Петренко># Пункт другий\n<c.yellow>бюджет</c></v>\n"

// TestVTT_SpeakerTagsBecomeLabels — a Teams transcript reads with no engine:
// each voice becomes a speaker label, one speaker's consecutive cues join, a
// cue's start is kept, the ten-minute mark starts a section, and nothing in a
// cue (a heading mark, emphasis in a name, a class span) becomes markup.
func TestVTT_SpeakerTagsBecomeLabels(t *testing.T) {
	s, rep := run(t, Captions{}, []byte(teamsVTT), Options{Name: "meeting.vtt"})
	md := s.md.String()
	if rep.Format != "vtt" || rep.Chars == 0 {
		t.Errorf("report = %+v", rep)
	}
	want := "# meeting\n\n" +
		"## [00:00:00 – 00:10:00]\n\n" +
		"**Іван Петренко** [00:00:04] Добрий день, колеги. Почнемо засідання правління.\n\n" +
		"**Olena Kovalenko** [00:00:10] Згодна & готова.\n\n" +
		"## [00:10:00 – 00:10:05]\n\n" +
		"**Іван Петренко** [00:10:02] \\# Пункт другий бюджет"
	if md != want {
		t.Errorf("markdown:\n%s\nwant:\n%s", md, want)
	}
}

// TestSRT_CuesBecomeTimedParagraphs — SubRip's index lines and comma
// fractions are read; tags are dropped; a block without a timing line is not
// a cue.
func TestSRT_CuesBecomeTimedParagraphs(t *testing.T) {
	srt := "1\r\n00:00:01,000 --> 00:00:02,500\r\n<i>Hello</i>\r\nthere\r\n\r\n2\r\n00:00:40,000 --> 00:00:41,000\r\n- Bye\r\n\r\njunk without timing\r\n"
	s, rep := run(t, Captions{}, []byte(srt), Options{Name: "film.srt"})
	want := "# film\n\n## [00:00:00 – 00:00:41]\n\n[00:00:01] Hello there\n\n[00:00:40] \\- Bye"
	if rep.Format != "srt" || s.md.String() != want {
		t.Errorf("format %q markdown:\n%s", rep.Format, s.md.String())
	}
	if _, err := (Captions{}).Extract(context.Background(), bytes.NewReader([]byte("WEBVTT\n\nNOTE only\n")), 17, &memSink{}, Options{}); !IsRefused(err) {
		t.Errorf("a caption file without cues: err = %v, want a refusal", err)
	}
}

// TestCaptions_PickedForVTTAndSRT — the registry hands a .vtt or .srt file
// (sniffed as text) to the caption reader, not the plain-text one.
func TestCaptions_PickedForVTTAndSRT(t *testing.T) {
	for _, c := range []struct{ mt, name string }{{"text/vtt", "a.vtt"}, {"application/x-subrip", "a.srt"}, {"", "b.srt"}} {
		e, ok := Default().For(c.mt, c.name)
		if _, isCaptions := e.(Captions); !ok || !isCaptions {
			t.Errorf("%s %s: got %T", c.mt, c.name, e)
		}
	}
}

// TestWriteTranscript_EscapesAndSections — the shared renderer: speaker
// labels keep only name characters, text cannot open markup, and without a
// section length there is one section to the recording's end.
func TestWriteTranscript_EscapesAndSections(t *testing.T) {
	md := WriteTranscript([]TranscriptCue{
		{Start: 3 * time.Second, Speaker: "Speaker 1", Text: "<script>x</script>\nline"},
		{Start: 70 * time.Second, Speaker: "**](evil)", Text: "> quoted"},
	}, TranscriptOptions{Title: "call.mp4", Meta: "stt engine=openai-audio", Total: 90 * time.Second})
	want := "# call.mp4\n<!-- loom: stt engine=openai-audio -->\n\n## [00:00:00 – 00:01:30]\n\n" +
		"**Speaker 1** \\<script>x\\</script> line\n\n**evil** \\> quoted"
	if md != want {
		t.Errorf("markdown:\n%s\nwant:\n%s", md, want)
	}
}
