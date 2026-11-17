package nodestats

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// minTokenRunes is the shortest token kept, in characters (not bytes, so a
// Cyrillic word is measured like a Latin one).
const minTokenRunes = 3

// stopWords are English function words of three letters or more. Shorter ones
// fall to minTokenRunes. The corpora are also Ukrainian and Russian; those
// languages get no list, and their function words are left to the class
// weighting, which discounts a word every class uses. Built once, read only.
var stopWords = func() map[string]struct{} {
	m := map[string]struct{}{}
	for _, w := range strings.Fields(`
		about above after again against all also and any are aren't because been
		before being below between both but can can't cannot could couldn't did
		didn't does doesn't doing don't down during each few for from further had
		hadn't has hasn't have haven't having her here hers herself him himself
		his how i'm i've into isn't it's its itself let's more most mustn't myself
		nor not off once only other ought our ours ourselves out over own same
		shall she should shouldn't such than that that's the their theirs them
		themselves then there there's these they they're this those through too
		under until upon very was wasn't were weren't what what's when where which
		while who whom why will with won't would wouldn't you you're your yours
		yourself yourselves via per etc
	`) {
		m[w] = struct{}{}
	}
	return m
}()

// isApostrophe reports the apostrophes that may sit inside a word: ASCII,
// the right single quote, and the modifier letter Ukrainian spelling uses.
func isApostrophe(r rune) bool { return r == '\'' || r == '’' || r == 'ʼ' }

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) }

// Tokenize splits text into lower-cased terms.
//
// A term is a run of letters, digits and combining marks in any script. An
// apostrophe between two letters stays in the word (written as ASCII '), so
// Ukrainian п'ять and English don't each stay one term; anywhere else it
// separates. Terms shorter than three characters and English stop words are
// dropped. No stemming: "model" and "models" are two terms.
func Tokenize(text string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		w := strings.ToLower(b.String())
		b.Reset()
		if utf8.RuneCountInString(w) < minTokenRunes {
			return
		}
		if _, stop := stopWords[w]; stop {
			return
		}
		out = append(out, w)
	}
	rs := []rune(text)
	for i, r := range rs {
		// The apostrophe test comes first: U+02BC is itself a letter to
		// unicode, and must still be written as ' so both spellings of a
		// word are one term.
		switch {
		case isApostrophe(r):
			if b.Len() > 0 && i+1 < len(rs) && unicode.IsLetter(rs[i+1]) && !isApostrophe(rs[i+1]) {
				b.WriteRune('\'')
			} else {
				flush()
			}
		case isWordRune(r):
			b.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// Term is a distinctive term with its c-TF-IDF score and its count in the node.
type Term struct {
	Term  string
	Score float64
	Count int
}

// DistinctiveTerms ranks the terms of a node's texts against its siblings'
// texts with class-based TF-IDF (c-TF-IDF, as in BERTopic). Each class is the
// concatenation of one node's texts (its members' briefs and abstracts):
//
//	score(t) = tf(t, node) / words(node) * log(1 + A / f(t))
//
// where A is the average number of words per class over the node and its
// siblings and f(t) is t's total count over the node and its siblings.
//
// c-TF-IDF alone still ranks a term every class uses if the node uses it a
// lot, so a term is kept only when it is over-represented in the node: its
// share of the node's words is greater than its share of the siblings' words
// together. A term used at the same rate everywhere distinguishes nothing.
// With no siblings (or siblings with no words) that filter cannot apply and
// the ranking is plain c-TF-IDF.
//
// Returns at most k terms, highest score first, ties broken by term ascending
// (byte order). Empty texts give an empty result.
func DistinctiveTerms(node []string, siblings [][]string, k int) ([]Term, error) {
	if k < 1 {
		return nil, fmt.Errorf("%w: k = %d", ErrInvalidArgument, k)
	}
	count := func(texts []string) (map[string]int, int) {
		m := map[string]int{}
		total := 0
		for _, t := range texts {
			for _, w := range Tokenize(t) {
				m[w]++
				total++
			}
		}
		return m, total
	}
	nodeTF, nodeWords := count(node)
	if nodeWords == 0 {
		return nil, nil
	}
	sibTF := map[string]int{}
	sibWords := 0
	for _, s := range siblings {
		m, w := count(s)
		for t, c := range m {
			sibTF[t] += c
		}
		sibWords += w
	}
	avg := float64(nodeWords+sibWords) / float64(1+len(siblings))

	out := make([]Term, 0, len(nodeTF))
	for t, c := range nodeTF {
		sc := sibTF[t]
		// Over-representation, compared without division:
		// c/nodeWords > sc/sibWords  <=>  c*sibWords > sc*nodeWords.
		if sibWords > 0 && c*sibWords <= sc*nodeWords {
			continue
		}
		f := float64(c + sc)
		score := float64(c) / float64(nodeWords) * math.Log(1+avg/f)
		out = append(out, Term{Term: t, Score: score, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Term < out[j].Term
	})
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}
