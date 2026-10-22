package main

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
)

func captureBootLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prev); log.SetFlags(flags) }()
	fn()
	return buf.String()
}

// The boot lines name the rows an operator should look at, capped so a large
// pre-pepper deployment does not print thousands, and the unpeppered rows'
// warning names the command that rewraps them.
func TestLogTokenHashes_ListsUntaggedRowsCappedAndNamesTheRewrapCommand(t *testing.T) {
	var many []string
	for i := 0; i < tokenRewrapLogMaxIDs+3; i++ {
		many = append(many, fmt.Sprintf("def_%03d", i))
	}
	out := captureBootLog(t, func() {
		logTokenHashes(lchttp.TokenHashReport{Untagged: many, Wrapped: 4, Previous: 2, OtherKey: 1, UntaggedDead: 3}, "0123456789abcdef")
	})
	for _, want := range []string{
		fmt.Sprintf("WARNING: %d token row(s) carry an unpeppered hash", len(many)),
		"`loomcycle tokens rewrap`",
		"def_000", "and 3 more",
		"4 wrapped, 2 under a previous LOOMCYCLE_SECRET_KEY",
		"1 token(s) hashed under a LOOMCYCLE_SECRET_KEY that is neither",
		"fingerprint 0123456789abcdef",
		"3 retired or expired token row(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("boot log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, many[len(many)-1]) {
		t.Error("the def_id list was not capped")
	}

	quiet := captureBootLog(t, func() { logTokenHashes(lchttp.TokenHashReport{}, "0123456789abcdef") })
	if strings.Contains(quiet, "WARNING") {
		t.Errorf("an empty report logged more than the counts: %q", quiet)
	}
}
