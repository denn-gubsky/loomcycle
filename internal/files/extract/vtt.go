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
	// captionParagraph bounds how long one joined paragraph runs, and
	// captionParagraphBytes and captionParagraphCues how much it holds: cues
	// can share a start, so time alone does not bound a paragraph.
	captionParagraph      = 30 * time.Second
	captionParagraphBytes = 4096
	captionParagraphCues  = 256
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
	joined, err := joinCues(ctx, cues)
	if err != nil {
		return rep, err
	}
	w := newWriter(ctx, sink, opts)
	// Written a paragraph at a time, so the sink's output limit cuts a huge
	// transcript where it is reached rather than refusing it whole.
	err = WriteTranscript(ctx, joined, TranscriptOptions{
		Title: title(opts.Name, "Transcript"), SectionLength: captionSection, Timestamps: true,
	}, w.text)
	rep.finish(w)
	return rep, err
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

// joinCues joins consecutive cues of one speaker into paragraphs: captions
// come a line at a time. A paragraph ends at captionParagraph from its start,
// at captionParagraphBytes or at captionParagraphCues, whichever comes first.
// Each paragraph's text is gathered once, so the join is linear.
func joinCues(ctx context.Context, cues []TranscriptCue) ([]TranscriptCue, error) {
	var out []TranscriptCue
	var parts []string
	size := 0
	flush := func() {
		if len(parts) > 0 {
			out[len(out)-1].Text = strings.Join(parts, " ")
		}
		parts, size = parts[:0], 0
	}
	for i, c := range cues {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if n := len(out); n > 0 && len(parts) > 0 {
			last := &out[n-1]
			if last.Speaker == c.Speaker && c.Start >= last.Start && c.Start-last.Start < captionParagraph &&
				size+1+len(c.Text) <= captionParagraphBytes && len(parts) < captionParagraphCues {
				parts = append(parts, c.Text)
				size += 1 + len(c.Text)
				last.End = max(last.End, c.End)
				continue
			}
		}
		flush()
		out = append(out, c)
		parts, size = append(parts, c.Text), len(c.Text)
	}
	flush()
	return out, nil
}
