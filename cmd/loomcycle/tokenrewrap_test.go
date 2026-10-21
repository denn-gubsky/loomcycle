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
// pre-pepper deployment does not print thousands.
func TestLogTokenRewrap_ListsRewrappedAndRefusedRowsCapped(t *testing.T) {
	var many []string
	for i := 0; i < tokenRewrapLogMaxIDs+3; i++ {
		many = append(many, fmt.Sprintf("def_%03d", i))
	}
	out := captureBootLog(t, func() {
		logTokenRewrap(lchttp.TokenRewrapReport{Rewrapped: many, Wrapped: len(many), Previous: 2, OtherKey: 1, Refused: []string{"def_forged"}})
	})
	for _, want := range []string{
		fmt.Sprintf("rewrapped %d unpeppered token hash(es)", len(many)),
		"def_000", "and 3 more",
		fmt.Sprintf("%d wrapped, 2 under a previous LOOMCYCLE_SECRET_KEY", len(many)),
		"1 token(s) hashed under a LOOMCYCLE_SECRET_KEY that is neither",
		"1 token row(s) carry an unpeppered hash", "def_forged",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("boot log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, many[len(many)-1]) {
		t.Error("the def_id list was not capped")
	}

	quiet := captureBootLog(t, func() { logTokenRewrap(lchttp.TokenRewrapReport{}) })
	if strings.Contains(quiet, "WARNING") || strings.Contains(quiet, "rewrapped") {
		t.Errorf("an empty report logged more than the counts: %q", quiet)
	}
}
