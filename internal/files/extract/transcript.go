package extract

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// A transcript — a speech-to-text answer, or a meeting's VTT/SRT captions —
// is written in one shape, so an imported recording and an imported caption
// file read alike:
//
//	# call
//	<!-- loom: stt … -->              (when the caller gives provenance)
//	## [00:00:00 – 00:10:00]
//	**Speaker 1** [00:00:04] …
//
// A section per stretch of SectionLength keeps a long recording's import
// in chunks of a sensible size and lets a later pass replace one section.

// TranscriptCue is one timed stretch of speech.
type TranscriptCue struct {
	Start, End time.Duration
	// Speaker is a display name ("Speaker 1", or a caption file's voice
	// name); "" when unknown.
	Speaker string
	Text    string
}

// TranscriptOptions shape WriteTranscript's Markdown.
type TranscriptOptions struct {
	// Title is the document's title, usually the recording's file name.
	Title string
	// Meta is a provenance comment under the title (its key=value body,
	// without the comment markers), already safe to place in a comment; ""
	// for none.
	Meta string
	// SectionLength is the stretch each "## [start – end]" section covers;
	// 0 puts everything in one.
	SectionLength time.Duration
	// Total is the recording's length, the end of the last section; 0 takes
	// the last cue's end.
	Total time.Duration
	// Timestamps puts each cue's start before its text.
	Timestamps bool
}

// maxSpeakerRunes bounds a speaker label.
const maxSpeakerRunes = 64

// WriteTranscript renders cues, which must be in time order. Everything in a
// cue is untrusted: its text is one escaped paragraph that cannot open a
// heading, a list, a fence or HTML, and a speaker is a short label of
// letters, digits, spaces and . ' -.
func WriteTranscript(cues []TranscriptCue, o TranscriptOptions) string {
	var b strings.Builder
	b.WriteString(heading(1, o.Title))
	if o.Meta != "" {
		b.WriteString("\n<!-- loom: " + o.Meta + " -->")
	}
	end := o.Total
	if n := len(cues); end <= 0 && n > 0 {
		end = cues[n-1].End
	}
	section := -1
	for _, c := range cues {
		text := escapeLine(oneLine(c.Text))
		if text == "" {
			continue
		}
		if o.SectionLength > 0 {
			if k := int(c.Start / o.SectionLength); k != section {
				section = k
				from := time.Duration(k) * o.SectionLength
				to := min(from+o.SectionLength, max(end, from))
				fmt.Fprintf(&b, "\n\n## [%s – %s]", Clock(from), Clock(to))
			}
		} else if section < 0 {
			section = 0
			fmt.Fprintf(&b, "\n\n## [%s – %s]", Clock(0), Clock(end))
		}
		b.WriteString("\n\n")
		if sp := speakerLabel(c.Speaker); sp != "" {
			b.WriteString("**" + sp + "** ")
		}
		if o.Timestamps {
			b.WriteString("[" + Clock(c.Start) + "] ")
		}
		b.WriteString(text)
	}
	return b.String()
}

// Clock writes d as hh:mm:ss.
func Clock(d time.Duration) string {
	s := max(int64(d/time.Second), 0)
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60)
}

// speakerLabel keeps letters, digits, spaces and . ' - of a label, folds its
// spaces, and cuts it to maxSpeakerRunes: it sits between ** markers, where
// anything else could end the emphasis or start markup.
func speakerLabel(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '\'' || r == '-':
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		default:
			space = true
		}
		if utf8.RuneCountInString(b.String()) >= maxSpeakerRunes {
			break
		}
	}
	return b.String()
}
