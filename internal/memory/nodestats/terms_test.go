package nodestats

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestTokenize_KeepsNonLatinWordsWhole(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"ukrainian with apostrophes", "Податкова декларація подана; п'ять звітів, м’ясо, сімʼя.",
			[]string{"податкова", "декларація", "подана", "п'ять", "звітів", "м'ясо", "сім'я"}},
		{"russian", "Налоговая ДЕКЛАРАЦИЯ — подана", []string{"налоговая", "декларация", "подана"}},
		{"english stop words and short words", "The AI is a model of the Ledger, don't 'quoted' it's",
			[]string{"model", "ledger", "quoted"}},
		{"digits kept", "Article 2297 of 2024", []string{"article", "2297", "2024"}},
		{"mixed scripts split at punctuation", "loomcycle/ядро", []string{"loomcycle", "ядро"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Tokenize(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Tokenize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDistinctiveTerms_TermCommonToAllSiblingsDoesNotRank(t *testing.T) {
	// "common" is one word in four in every class: it distinguishes nothing,
	// though plain c-TF-IDF would score it above zero.
	node := []string{"invoice invoice", "payment common"}
	siblings := [][]string{
		{"shipment cargo freight common"},
		{"employee salary payroll common"},
	}
	got, err := DistinctiveTerms(node, siblings, TopTerms)
	if err != nil {
		t.Fatal(err)
	}
	// A = 12 words / 3 classes = 4.
	want := []Term{
		{"invoice", 2.0 / 4 * math.Log(1+4.0/2), 2},
		{"payment", 1.0 / 4 * math.Log(1+4.0/1), 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Term != want[i].Term || got[i].Count != want[i].Count || !near(got[i].Score, want[i].Score) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
}

func TestDistinctiveTerms_CyrillicTermsRankAndTiesBreakByTerm(t *testing.T) {
	node := []string{"податок декларація", "податок звіт"}
	siblings := [][]string{{"зарплата відпустка"}}
	got, err := DistinctiveTerms(node, siblings, 2)
	if err != nil {
		t.Fatal(err)
	}
	// податок first (count 2); декларація and звіт tie on score and break
	// by byte order, so декларація (д < з) takes the second slot.
	if len(got) != 2 || got[0].Term != "податок" || got[1].Term != "декларація" {
		t.Fatalf("got %+v", got)
	}
}

func TestDistinctiveTerms_NoSiblingsIsPlainCTFIDF(t *testing.T) {
	got, err := DistinctiveTerms([]string{"alpha alpha beta"}, nil, TopTerms)
	if err != nil {
		t.Fatal(err)
	}
	// A = 3; alpha 2/3*ln(1+3/2), beta 1/3*ln(1+3).
	if len(got) != 2 || got[0].Term != "alpha" || !near(got[0].Score, 2.0/3*math.Log(2.5)) ||
		got[1].Term != "beta" || !near(got[1].Score, 1.0/3*math.Log(4)) {
		t.Fatalf("got %+v", got)
	}
}

func TestDistinctiveTerms_EmptyNodeAndBadK(t *testing.T) {
	got, err := DistinctiveTerms([]string{"a an"}, [][]string{{"cargo"}}, 3)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v; want empty", got, err)
	}
	if _, err := DistinctiveTerms([]string{"cargo"}, nil, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("k=0: err = %v", err)
	}
}
