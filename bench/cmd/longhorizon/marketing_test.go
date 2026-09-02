package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestGenerateMktTask_OracleAndDeterminism(t *testing.T) {
	a := GenerateMktTask(9, 60, 6, 0.2)
	b := GenerateMktTask(9, 60, 6, 0.2)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed produced different marketing tasks")
	}
	if len(a.Stream) != 60 || len(a.Competitors) != 6 {
		t.Fatalf("stream=%d comps=%d, want 60/6", len(a.Stream), len(a.Competitors))
	}
	// The first nComp events introduce the competitors; the oracle price for each
	// equals its LATEST repin (or its intro price if never repriced).
	for _, c := range a.Competitors {
		if a.Final[c.Name].Segment != c.Segment {
			t.Errorf("%s segment drifted: %q vs %q", c.Name, a.Final[c.Name].Segment, c.Segment)
		}
	}
	// A price query's answer matches Final.
	for _, q := range a.Queries {
		if q.Kind == "price" && q.Answer != "" {
			want := a.Final[q.Arg].Price
			if q.Answer != itoa(want) {
				t.Errorf("price query %s answer %q, want %d", q.Arg, q.Answer, want)
			}
		}
	}
}

func TestMktQuery_Grade(t *testing.T) {
	if !(MktQuery{Kind: "price", Answer: "99"}).Grade("The price is $99/mo.") {
		t.Error("price grade with prose failed")
	}
	if !(MktQuery{Kind: "who", Answer: "Acme"}).Grade("That would be Acme.") {
		t.Error("who grade failed")
	}
	if (MktQuery{Kind: "who", Answer: "Acme"}).Grade("Bolt") {
		t.Error("who grade false-positive")
	}
	list := MktQuery{Kind: "list", Answer: "Acme,Bolt,Cirrus"}
	if !list.Grade("Acme, Bolt, and Cirrus") {
		t.Error("list grade (all present) failed")
	}
	if list.Grade("Acme and Bolt") {
		t.Error("list grade should fail when one name is missing")
	}
}

func itoa(n int) string {
	// tiny local helper to avoid importing strconv in the test
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return strings.TrimSpace(string(b))
}
