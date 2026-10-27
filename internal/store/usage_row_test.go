package store

import (
	"math"
	"testing"
)

// TestNormalizeUsageRow_AcceptsEachPurposeWithItsOwnUnits — every purpose is
// accepted with the units it carries (tokens on chat and ocr, seconds on stt,
// characters and seconds on tts), an empty purpose is written as chat, and a
// row's seconds come back as given.
func TestNormalizeUsageRow_AcceptsEachPurposeWithItsOwnUnits(t *testing.T) {
	for _, tc := range []struct {
		row         TokenUsageRow
		wantPurpose string
	}{
		{TokenUsageRow{InputTokens: 10}, UsagePurposeChat},
		{TokenUsageRow{Purpose: UsagePurposeChat, InputTokens: 10}, UsagePurposeChat},
		{TokenUsageRow{Purpose: UsagePurposeOCR, InputTokens: 10}, UsagePurposeOCR},
		{TokenUsageRow{Purpose: UsagePurposeSTT, AudioSeconds: 60}, UsagePurposeSTT},
		{TokenUsageRow{Purpose: UsagePurposeTTS, InputChars: 4380, AudioSeconds: 312.4}, UsagePurposeTTS},
		{TokenUsageRow{Purpose: UsagePurposeTTS}, UsagePurposeTTS},
		{TokenUsageRow{Purpose: UsagePurposeEmbedding, InputChars: 812, InputTokens: 190}, UsagePurposeEmbedding},
		{TokenUsageRow{Purpose: UsagePurposeEmbedding, InputChars: 812}, UsagePurposeEmbedding},
	} {
		p, s, err := NormalizeUsageRow(tc.row)
		if err != nil || p != tc.wantPurpose || s != tc.row.AudioSeconds {
			t.Errorf("NormalizeUsageRow(%+v) = (%q, %v, %v), want (%q, %v, nil)",
				tc.row, p, s, err, tc.wantPurpose, tc.row.AudioSeconds)
		}
	}
}

// TestNormalizeUsageRow_RefusesUnitsTheirPurposeCannotCarry — a negative
// character count, characters on any purpose but tts, seconds on a purpose
// with no audio, a bad second count and an unknown purpose are refused.
func TestNormalizeUsageRow_RefusesUnitsTheirPurposeCannotCarry(t *testing.T) {
	for name, row := range map[string]TokenUsageRow{
		"negative chars on tts": {Purpose: UsagePurposeTTS, InputChars: -1},
		"chars on empty (chat)": {InputChars: 1},
		"chars on chat":         {Purpose: UsagePurposeChat, InputChars: 1},
		"chars on ocr":          {Purpose: UsagePurposeOCR, InputChars: 1},
		"seconds on embedding":  {Purpose: UsagePurposeEmbedding, AudioSeconds: 1},
		"chars on stt":          {Purpose: UsagePurposeSTT, InputChars: 1, AudioSeconds: 1},
		"seconds on chat":       {AudioSeconds: 1},
		"seconds on ocr":        {Purpose: UsagePurposeOCR, AudioSeconds: 1},
		"negative seconds tts":  {Purpose: UsagePurposeTTS, AudioSeconds: -1},
		"infinite seconds tts":  {Purpose: UsagePurposeTTS, AudioSeconds: math.Inf(1)},
		"unknown purpose":       {Purpose: "speech"},
	} {
		if p, _, err := NormalizeUsageRow(row); err == nil {
			t.Errorf("%s: NormalizeUsageRow accepted it as %q, want refused", name, p)
		}
	}
}
