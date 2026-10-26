package extract

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Captions reads WebVTT and SubRip caption files — the transcript a Teams
// meeting with transcription on produces — into the transcript shape
// WriteTranscript writes, with no speech engine: each cue's time is kept, a
// WebVTT voice tag (<v Ivan Petrenko>) becomes the speaker, and consecutive
// cues of one speaker join into one paragraph.
type Captions struct{}

func (Captions) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{"text/vtt", "application/x-subrip", "text/srt", "application/srt"}, []string{".vtt", ".srt"})
}

const (
	// captionSection is the length of a transcript section, as for a
	// recording cut into ten-minute pieces.
	captionSection = 10 * time.Minute
	// captionParagraph bounds how long one joined paragraph runs.
	captionParagraph = 30 * time.Second
	// maxCaptionCues bounds the cues read.
	maxCaptionCues = 200000
)

var (
	// cueTiming is a cue's timing line: [hh:]mm:ss.ttt --> [hh:]mm:ss.ttt,
	// with "," as SubRip writes the fraction.
	cueTiming = regexp.MustCompile(`^\s*((?:\d+:)?\d{1,2}:\d{2}[.,]\d{1,3})\s+-->\s+((?:\d+:)?\d{1,2}:\d{2}[.,]\d{1,3})`)
	// voiceTag is a WebVTT voice span's opening tag: <v Name> or <v.class Name>.
	voiceTag = regexp.MustCompile(`<v(?:\.[^\s>]*)?\s+([^>]*)>`)
	// anyTag is any caption markup tag (<i>, </v>, <c.yellow>, <00:01.000>).
	anyTag = regexp.MustCompile(`<[^>]*>`)
	// captionEntities are the character references caption files use.
	captionEntities = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&nbsp;", " ", "&lrm;", "", "&rlm;", "", "&quot;", `"`, "&#39;", "'")
)

func (Captions) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "srt"}
	data, err := readAllBounded(r, size)
	if err != nil {
		return rep, err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return rep, refusedf("the caption file is not UTF-8 text")
	}
	text := normalizeNewlines(string(data))
	if strings.HasPrefix(text, "WEBVTT") {
		rep.Format = "vtt"
	}
	cues, err := parseCaptions(ctx, text)
	if err != nil {
		return rep, err
	}
	if len(cues) == 0 {
		return rep, refusedf("the file holds no timed captions")
	}
	// Sections follow time; a file whose cues are out of order is put in it.
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].Start < cues[j].Start })
	w := newWriter(ctx, sink, opts)
	md := WriteTranscript(joinCues(cues), TranscriptOptions{
		Title: title(opts.Name, "Transcript"), SectionLength: captionSection, Timestamps: true,
	})
	if err := w.text(md); err != nil {
		rep.finish(w)
		return rep, err
	}
	rep.finish(w)
	return rep, nil
}

// parseCaptions reads the cues of a WebVTT or SubRip file: in each block of
// lines, the timing line and the text lines after it. Blocks without a
// timing line (the header, NOTE, STYLE, REGION, a SubRip index alone) are
// skipped.
func parseCaptions(ctx context.Context, text string) ([]TranscriptCue, error) {
	var cues []TranscriptCue
	for i, block := range strings.Split(text, "\n\n") {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		lines := strings.Split(strings.Trim(block, "\n"), "\n")
		for j, l := range lines {
			m := cueTiming.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			start, ok1 := cueTime(m[1])
			end, ok2 := cueTime(m[2])
			if !ok1 || !ok2 {
				break
			}
			if end < start {
				end = start
			}
			payload := strings.Join(lines[j+1:], " ")
			speaker := ""
			if v := voiceTag.FindStringSubmatch(payload); v != nil {
				speaker = captionEntities.Replace(strings.TrimSpace(v[1]))
			}
			body := strings.TrimSpace(captionEntities.Replace(anyTag.ReplaceAllString(payload, "")))
			if body != "" {
				if len(cues) >= maxCaptionCues {
					return cues, nil
				}
				cues = append(cues, TranscriptCue{Start: start, End: end, Speaker: speaker, Text: body})
			}
			break
		}
	}
	return cues, nil
}

// cueTime parses [hh:]mm:ss.ttt (or ,ttt).
func cueTime(s string) (time.Duration, bool) {
	s = strings.ReplaceAll(s, ",", ".")
	parts := strings.Split(s, ":")
	var h, m int
	var err error
	switch len(parts) {
	case 3:
		if h, err = strconv.Atoi(parts[0]); err != nil || h > 9999 {
			return 0, false
		}
		parts = parts[1:]
	case 2:
	default:
		return 0, false
	}
	if m, err = strconv.Atoi(parts[0]); err != nil || m > 59 {
		return 0, false
	}
	sec, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || sec >= 60 {
		return 0, false
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec*float64(time.Second)), true
}

// joinCues joins consecutive cues of one speaker into paragraphs of at most
// captionParagraph: captions come a line at a time.
func joinCues(cues []TranscriptCue) []TranscriptCue {
	var out []TranscriptCue
	for _, c := range cues {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.Speaker == c.Speaker && c.Start-last.Start < captionParagraph && c.Start >= last.Start {
				last.Text += " " + c.Text
				last.End = max(last.End, c.End)
				continue
			}
		}
		out = append(out, c)
	}
	return out
}
