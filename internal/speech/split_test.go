package speech

import (
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// TestSpeech_SplitsAtSentenceBoundaries — pieces are filled with whole
// sentences and cut between them.
func TestSpeech_SplitsAtSentenceBoundaries(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want []Piece
	}{
		{"fits in one", "One. Two!", 100, []Piece{{"One. Two!", true}}},
		{"cut between sentences", "First one. Second one? Third one!", 24,
			[]Piece{{"First one. Second one?", false}, {"Third one!", true}}},
		{"ellipsis ends a sentence", "Wait… Then go.", 8, []Piece{{"Wait…", false}, {"Then go.", true}}},
		{"closing quote stays with its sentence", `He said "stop." Then left.`, 16,
			[]Piece{{`He said "stop."`, false}, {"Then left.", true}}},
		{"decimal point is not an end", "Pi is 3.14 today. Ok.", 17, []Piece{{"Pi is 3.14 today.", false}, {"Ok.", true}}},
		{"exact limit fits", "abcd. efgh.", 11, []Piece{{"abcd. efgh.", true}}},
		{"one over the limit cuts", "abcd. efgh.", 10, []Piece{{"abcd.", false}, {"efgh.", true}}},
		{"empty", "", 10, nil},
		{"only whitespace", " \n\n\t ", 10, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Split(c.in, c.max); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Split(%q, %d)\n got %+v\nwant %+v", c.in, c.max, got, c.want)
			}
		})
	}
}

// TestSpeech_SplitMarksParagraphEnds — a piece never spans paragraphs, and
// the last piece of each paragraph is flagged for the longer pause.
func TestSpeech_SplitMarksParagraphEnds(t *testing.T) {
	in := "Title.\n\nFirst sentence here. Second sentence here.\n\n\nLast\nparagraph  folded."
	got := Split(in, 25)
	want := []Piece{
		{"Title.", true},
		{"First sentence here.", false},
		{"Second sentence here.", true},
		{"Last paragraph folded.", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	// Short paragraphs are not merged into one piece even when they would fit.
	if got := Split("A.\n\nB.", 100); len(got) != 2 || !got[0].ParagraphEnd || !got[1].ParagraphEnd {
		t.Errorf("Split two short paragraphs = %+v, want two paragraph-end pieces", got)
	}
}

// TestSpeech_NeverSplitsInsideAWord — a sentence longer than a piece is cut
// at whitespace, and every piece is a run of whole words of the input.
func TestSpeech_NeverSplitsInsideAWord(t *testing.T) {
	in := "Це дуже довге речення без жодної крапки яке має бути розрізане лише між словами"
	words := map[string]bool{}
	for _, w := range strings.Fields(in) {
		words[w] = true
	}
	for _, max := range []int{12, 15, 20, 33} {
		got := Split(in, max)
		var joined []string
		for _, p := range got {
			if n := utf8.RuneCountInString(p.Text); n > max {
				t.Errorf("max %d: piece %q has %d runes", max, p.Text, n)
			}
			for _, w := range strings.Fields(p.Text) {
				if !words[w] {
					t.Errorf("max %d: %q is not a word of the input (cut inside a word)", max, w)
				}
			}
			joined = append(joined, p.Text)
		}
		if strings.Join(joined, " ") != in {
			t.Errorf("max %d: pieces do not rejoin to the input: %q", max, joined)
		}
	}
}

// TestSpeech_SplitCountsRunesNotBytes — Ukrainian letters are two bytes each;
// the limit is characters.
func TestSpeech_SplitCountsRunesNotBytes(t *testing.T) {
	in := "Привіт світе. Як справи?" // 24 runes, 44 bytes
	if got := Split(in, 24); len(got) != 1 {
		t.Errorf("Split at 24 runes = %+v, want one piece", got)
	}
	if got := Split(in, 23); len(got) != 2 || got[0].Text != "Привіт світе." {
		t.Errorf("Split at 23 runes = %+v, want a cut after the first sentence", got)
	}
}

// TestSpeech_SplitHardCutsOnlyAnOverlongWord — a word longer than a piece is
// cut at the limit, and a stress mark is never separated from its letter.
func TestSpeech_SplitHardCutsOnlyAnOverlongWord(t *testing.T) {
	got := Split("ok abcdefghij ok.", 4)
	want := []Piece{{"ok", false}, {"abcd", false}, {"efgh", false}, {"ij", false}, {"ok.", true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	// "ма́ма" is м а U+0301 м а: a cut at 2 would start a part with the mark.
	word := "ма́ма"
	for _, p := range Split(word, 2) {
		if r, _ := utf8.DecodeRuneInString(p.Text); unicode.Is(unicode.Mn, r) {
			t.Errorf("piece %q starts with a combining mark", p.Text)
		}
		if utf8.RuneCountInString(p.Text) > 2 {
			t.Errorf("piece %q over the limit", p.Text)
		}
	}
	var joined strings.Builder
	for _, p := range Split(word, 2) {
		joined.WriteString(p.Text)
	}
	if joined.String() != word {
		t.Errorf("parts rejoin to %q, want %q", joined.String(), word)
	}
}

// TestSpeech_SplitLongSentenceRemainderPacksWithTheNext — after a long
// sentence is cut at whitespace, its tail shares a piece with what follows.
func TestSpeech_SplitLongSentenceRemainderPacksWithTheNext(t *testing.T) {
	got := Split("aaa bbb ccc ddd. Eee.", 8)
	want := []Piece{{"aaa bbb", false}, {"ccc ddd.", false}, {"Eee.", true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	got = Split("aaa bbb cc. Eee.", 8)
	want = []Piece{{"aaa bbb", false}, {"cc. Eee.", true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestSpeech_SplitNonPositiveMaxIsOne — a nonsense limit cannot loop or
// return pieces over one character.
func TestSpeech_SplitNonPositiveMaxIsOne(t *testing.T) {
	for _, max := range []int{0, -5} {
		got := Split("ab c", max)
		want := []Piece{{"a", false}, {"b", false}, {"c", true}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Split(max=%d) = %+v, want %+v", max, got, want)
		}
	}
}

// TestSpeech_SplitAfterNormalize — the normaliser's output splits on its
// paragraph breaks.
func TestSpeech_SplitAfterNormalize(t *testing.T) {
	got := Split(Normalize("# Звіт\n\nПерше речення. Друге речення.\n\n- пункт"), 4000)
	want := []Piece{{"Звіт.", true}, {"Перше речення. Друге речення.", true}, {"пункт.", true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
