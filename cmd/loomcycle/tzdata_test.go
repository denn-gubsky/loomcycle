package main

import (
	"go/parser"
	"go/token"
	"strconv"
	"testing"
	"time"
)

// The binary embeds the IANA zone database so a minimal image with no
// /usr/share/zoneinfo answers zone questions (Calc's tz(), cron zones, ACL
// time conditions) exactly as a full host does. time.LoadLocation prefers
// the host's files, so a run on a full host cannot tell whether the embedded
// copy is there; this reads the import from source instead, and then checks
// a zone resolves in this package's own binary.
func TestMain_EmbedsTheZoneDatabase(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, imp := range file.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "time/tzdata" {
			found = imp.Name != nil && imp.Name.Name == "_"
		}
	}
	if !found {
		t.Fatal(`main.go does not blank-import "time/tzdata"; a distroless image would fail every named zone`)
	}
	for _, zone := range []string{"Europe/Kyiv", "America/New_York", "Asia/Kolkata"} {
		if _, err := time.LoadLocation(zone); err != nil {
			t.Errorf("LoadLocation(%q): %v", zone, err)
		}
	}
}
