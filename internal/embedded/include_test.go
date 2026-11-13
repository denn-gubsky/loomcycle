package embedded

import (
	"strings"
	"testing"
)

// registry builds a unit registry from name -> yaml body.
func registry(bodies map[string]string) map[string]Unit {
	reg := map[string]Unit{}
	for name, body := range bodies {
		reg[name] = Unit{Name: name, Kind: "preset", Data: []byte(body)}
	}
	return reg
}

func names(us []Unit) string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.Name
	}
	return strings.Join(out, ",")
}

// An include is expanded depth-first, in the order written, and the including
// unit comes after everything it includes: the merge is last-wins, so that is
// what lets its own keys override them.
func TestExpandUnits_IncludesComeFirstInWrittenOrderThenTheUnitItself(t *testing.T) {
	reg := registry(map[string]string{
		"a":   "x: 1\n",
		"b":   "include: [c]\nx: 2\n",
		"c":   "x: 3\n",
		"top": "include: [b, a]\nx: 4\n",
	})
	got, err := expandUnits(reg, []string{"top"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "c,b,a,top"; names(got) != want {
		t.Errorf("stack = %s, want %s", names(got), want)
	}
}

// A unit reached twice (selected and included, or included by two units) is
// layered once, at its first position.
func TestExpandUnits_AUnitReachedTwiceIsLayeredOnceAtItsFirstPosition(t *testing.T) {
	reg := registry(map[string]string{
		"base":   "x: 1\n",
		"chat":   "y: 1\n",
		"server": "include: [base, chat]\n",
		"other":  "include: [chat, base]\n",
	})
	for selection, want := range map[string]string{
		"server,chat":       "base,chat,server",
		"chat,server":       "chat,base,server",
		"server,other":      "base,chat,server,other",
		"base,server,base":  "base,chat,server",
		"server,chat,other": "base,chat,server,other",
	} {
		got, err := expandUnits(reg, strings.Split(selection, ","))
		if err != nil {
			t.Fatalf("%s: %v", selection, err)
		}
		if names(got) != want {
			t.Errorf("%s: stack = %s, want %s", selection, names(got), want)
		}
	}
}

// A cycle has no order to apply; the error names the chain so the author can
// see which include closed it.
func TestExpandUnits_ACycleIsRefusedNamingTheChain(t *testing.T) {
	for name, tc := range map[string]struct {
		reg  map[string]string
		want string
	}{
		"self":     {map[string]string{"a": "include: [a]\n"}, "a -> a"},
		"two":      {map[string]string{"a": "include: [b]\n", "b": "include: [a]\n"}, "a -> b -> a"},
		"indirect": {map[string]string{"a": "include: [b]\n", "b": "include: [c]\n", "c": "include: [b]\n"}, "a -> b -> c -> b"},
	} {
		_, err := expandUnits(registry(tc.reg), []string{"a"})
		if err == nil || !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want an include cycle naming %q", name, err, tc.want)
		}
	}
}

// A diamond (two units including the same third) is not a cycle.
func TestExpandUnits_ADiamondIsNotACycle(t *testing.T) {
	reg := registry(map[string]string{
		"d":   "x: 1\n",
		"l":   "include: [d]\n",
		"r":   "include: [d]\n",
		"top": "include: [l, r]\n",
	})
	got, err := expandUnits(reg, []string{"top"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "d,l,r,top"; names(got) != want {
		t.Errorf("stack = %s, want %s", names(got), want)
	}
}

// An unknown name is refused: a selected one as before, an included one with
// the chain that reached it.
func TestExpandUnits_AnUnknownNameIsRefusedNamingTheChain(t *testing.T) {
	reg := registry(map[string]string{"a": "include: [b]\n", "b": "include: [nope]\n"})

	_, err := expandUnits(reg, []string{"nope"})
	if err == nil || !strings.Contains(err.Error(), `unknown preset/bundle "nope"`) || !strings.Contains(err.Error(), "available: a, b") {
		t.Errorf("selected unknown: err = %v", err)
	}
	_, err = expandUnits(reg, []string{"a"})
	if err == nil || !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), "a -> b -> nope") {
		t.Errorf("included unknown: err = %v, want the chain a -> b -> nope", err)
	}
}

// An include: that is not a list of names is an error of that unit, not an
// empty list.
func TestExpandUnits_AMalformedIncludeIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"scalar":  "include: base\n",
		"mapping": "include: {base: true}\n",
		"empty":   "include: [\"\"]\n",
	} {
		_, err := expandUnits(registry(map[string]string{"a": body, "base": "x: 1\n"}), []string{"a"})
		if err == nil || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), "include:") {
			t.Errorf("%s: err = %v, want an include: error naming the unit", name, err)
		}
	}
}

// Every shipped unit's include: parses and resolves: a typo in a shipped
// preset would otherwise fail only when an operator selects it.
func TestUnits_EveryShippedIncludeResolves(t *testing.T) {
	for _, u := range Units() {
		if _, err := includesOf(u.Data); err != nil {
			t.Errorf("%s: %v", u.Name, err)
		}
		if _, err := ResolveUnits([]string{u.Name}); err != nil {
			t.Errorf("ResolveUnits(%s): %v", u.Name, err)
		}
	}
}
