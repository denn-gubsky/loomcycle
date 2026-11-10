// Package lang guesses the natural language of a text: the script its letters
// are written in, then, for the Latin and Cyrillic alphabets, the language
// whose common short words it uses most.
//
// It exists for text the runtime writes ABOUT stored text (an annotation is
// written in its document's language), where a wrong guess costs a paragraph in
// the wrong language rather than a refusal, and where pulling in a statistical
// model would cost more than the guess is worth. It is deliberately small: the
// scripts that name one language (Greek, Hebrew, Hangul, Kana, Thai, …) decide
// by the script alone; Cyrillic separates Ukrainian from Russian by the letters
// only one of them has, then by stopwords; Latin is decided by stopwords among
// the languages listed in latinStopwords. A text with too few words to tell is
// reported as unknown, and the caller picks its default.
package lang

import (
	"strings"
	"unicode"
)

// Unknown is returned when the text does not say which language it is in.
const Unknown = ""

// minLetters is the fewest letters a text needs before its script is trusted;
// minStopwords the fewest stopword hits before a Latin or Cyrillic language is
// named over another.
const (
	minLetters   = 12
	minStopwords = 2
)

// Detect returns the ISO 639-1 code of the language text is most likely in, or
// Unknown.
func Detect(text string) string {
	counts := map[string]int{}
	letters := 0
	ukOnly, ruOnly := 0, 0
	kana := 0
	for _, r := range text {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		switch {
		case unicode.Is(unicode.Latin, r):
			counts["latin"]++
		case unicode.Is(unicode.Cyrillic, r):
			counts["cyrillic"]++
			switch unicode.ToLower(r) {
			case 'і', 'ї', 'є', 'ґ':
				ukOnly++
			case 'ы', 'э', 'ъ', 'ё':
				ruOnly++
			}
		case unicode.Is(unicode.Greek, r):
			counts["el"]++
		case unicode.Is(unicode.Hebrew, r):
			counts["he"]++
		case unicode.Is(unicode.Arabic, r):
			counts["ar"]++
		case unicode.Is(unicode.Hangul, r):
			counts["ko"]++
		case unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r):
			kana++
			counts["cjk"]++
		case unicode.Is(unicode.Han, r):
			counts["cjk"]++
		case unicode.Is(unicode.Thai, r):
			counts["th"]++
		case unicode.Is(unicode.Devanagari, r):
			counts["hi"]++
		case unicode.Is(unicode.Georgian, r):
			counts["ka"]++
		case unicode.Is(unicode.Armenian, r):
			counts["hy"]++
		}
	}
	if letters < minLetters {
		return Unknown
	}
	script, best := "", 0
	for s, n := range counts {
		if n > best || (n == best && s < script) {
			script, best = s, n
		}
	}
	switch script {
	case "latin":
		return byStopwords(text, latinStopwords)
	case "cyrillic":
		switch {
		case ukOnly > ruOnly:
			return "uk"
		case ruOnly > ukOnly:
			return "ru"
		}
		if l := byStopwords(text, cyrillicStopwords); l != Unknown {
			return l
		}
		return Unknown
	case "cjk":
		// Japanese text mixes kana into its kanji; Chinese has none.
		if kana*10 >= best {
			return "ja"
		}
		return "zh"
	case "":
		return Unknown
	}
	return script
}

// DetectOr is Detect with a fallback for Unknown.
func DetectOr(text, fallback string) string {
	if l := Detect(text); l != Unknown {
		return l
	}
	return fallback
}

// byStopwords names the language of table whose stopwords text uses most, or
// Unknown when none reaches minStopwords or two tie.
func byStopwords(text string, table map[string][]string) string {
	idx := stopwordIndex(table)
	hits := map[string]int{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\''
	}) {
		for _, l := range idx[w] {
			hits[l]++
		}
	}
	lang, best, second := Unknown, 0, 0
	for l, n := range hits {
		switch {
		case n > best:
			lang, best, second = l, n, best
		case n == best:
			second = n
		case n > second:
			second = n
		}
	}
	if best < minStopwords || best == second {
		return Unknown
	}
	return lang
}

// latinStopwords are short words frequent in running text of each language and
// rare in the others. A word listed for two languages counts for both.
var latinStopwords = map[string][]string{
	"en": {"the", "and", "of", "to", "is", "in", "that", "for", "with", "this", "are", "be", "on", "it", "as", "by", "or", "from", "which", "an", "not", "we", "you", "was", "have"},
	"de": {"der", "die", "und", "das", "ist", "nicht", "mit", "den", "ein", "eine", "auf", "für", "sich", "dem", "zu", "von", "auch", "wird", "sind", "oder", "werden", "bei"},
	"fr": {"le", "la", "les", "et", "est", "des", "une", "du", "dans", "pour", "que", "qui", "pas", "sur", "au", "avec", "sont", "ce", "cette", "nous", "vous", "aux"},
	"es": {"el", "los", "las", "y", "es", "del", "una", "que", "en", "por", "con", "para", "se", "su", "lo", "como", "más", "pero", "sus", "este", "esta", "son"},
	"it": {"il", "di", "che", "è", "per", "una", "sono", "della", "nel", "con", "gli", "del", "alla", "questo", "anche", "come", "non", "più", "dei", "delle"},
	"pt": {"o", "os", "as", "e", "é", "do", "da", "dos", "das", "uma", "que", "não", "com", "para", "em", "no", "na", "por", "mais", "como", "são", "ao"},
	"nl": {"de", "het", "een", "en", "van", "is", "dat", "niet", "met", "voor", "op", "zijn", "ook", "aan", "er", "maar", "wordt", "deze", "naar", "bij"},
	"pl": {"i", "w", "nie", "się", "na", "jest", "że", "do", "to", "z", "jak", "co", "ale", "dla", "przez", "od", "są", "tak", "oraz", "być"},
	"cs": {"a", "je", "se", "na", "v", "že", "to", "s", "z", "do", "jako", "pro", "jsou", "ale", "také", "není", "by", "podle", "které", "jeho"},
	"sv": {"och", "att", "det", "som", "är", "en", "på", "för", "med", "av", "till", "den", "inte", "har", "om", "ett", "var", "kan", "de", "jag"},
	"tr": {"ve", "bir", "bu", "da", "de", "için", "ile", "olarak", "daha", "çok", "gibi", "olan", "ne", "kadar", "ama", "değil", "her", "sonra"},
}

var cyrillicStopwords = map[string][]string{
	"uk": {"і", "й", "та", "що", "це", "не", "як", "на", "до", "від", "для", "або", "але", "його", "її", "які", "також", "щоб", "був", "є"},
	"ru": {"и", "что", "это", "не", "как", "на", "от", "для", "или", "но", "его", "ее", "её", "которые", "также", "чтобы", "был", "есть", "он", "она", "мы"},
	"bg": {"и", "че", "това", "не", "като", "на", "от", "за", "или", "но", "са", "се", "да", "ще", "със"},
}

// stopwordIndex inverts a table: word → languages. Built per call: the tables
// are small and Detect runs about once per document.
func stopwordIndex(table map[string][]string) map[string][]string {
	idx := map[string][]string{}
	for l, words := range table {
		for _, w := range words {
			idx[w] = append(idx[w], l)
		}
	}
	return idx
}
