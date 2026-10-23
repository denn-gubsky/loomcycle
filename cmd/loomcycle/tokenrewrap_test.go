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
	// Each line is found by its message; its values are attributes of that
	// same line.
	for msg, wants := range map[string][]string{
		"token rows carry an unpeppered hash": {
			"WARN ", fmt.Sprintf("rows=%d", len(many)),
			"`loomcycle tokens rewrap`",
			"def_000", "more_def_ids=3",
		},
		"4 wrapped, 2 under a previous LOOMCYCLE_SECRET_KEY":  {"fingerprint 0123456789abcdef"},
		"hashed under a LOOMCYCLE_SECRET_KEY that is neither": {"WARN ", "tokens=1"},
		"retired or expired token rows":                       {"rows=3"},
	} {
		line := logLineWith(out, msg)
		if line == "" {
			t.Errorf("boot log has no line with %q:\n%s", msg, out)
			continue
		}
		for _, want := range wants {
			if !strings.Contains(line, want) {
				t.Errorf("the %q line lacks %q:\n%s", msg, want, line)
			}
		}
	}
	if strings.Contains(out, many[len(many)-1]) {
		t.Error("the def_id list was not capped")
	}

	quiet := captureBootLog(t, func() { logTokenHashes(lchttp.TokenHashReport{}, "0123456789abcdef") })
	if strings.Contains(quiet, "WARN") || strings.Count(strings.TrimSpace(quiet), "\n") != 0 {
		t.Errorf("an empty report logged more than the counts: %q", quiet)
	}
}

// logLineWith returns the first line of log that contains substr, or "".
func logLineWith(log, substr string) string {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}
