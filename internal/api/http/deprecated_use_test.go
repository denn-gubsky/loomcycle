package http

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

func captureDeprecatedLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// shellDef is a stored definition read back after v1.172.0: it names Bash, an
// MCP tool of a server called sandbox (an ordinary MCP tool now, P15 Q2), and
// the shell_credentials its stored JSON still holds.
func shellDef() config.AgentDef {
	return config.AgentDef{
		Tools:         []string{"Read", "Bash", "mcp__sandbox__sandbox_exec", "mcp__sandbox__sandbox_read"},
		RemovedFields: config.RemovedFieldsInJSON([]byte(`{"shell_credentials":["cred-name-5e1d"]}`)),
	}
}

// A run whose definition names removed items counts each row once, by its
// kind and the table's name: the label set stays bounded by the table, and
// an mcp__sandbox__ tool is not counted (RFC GW P15 Q2).
func TestDeprecatedUse_CountsADroppedItem(t *testing.T) {
	log, _ := captureDeprecatedLog()
	d := newDeprecatedUse(log)
	d.note(context.Background(), "t1", "coder", shellDef(), true)
	d.note(context.Background(), "t1", "coder", config.AgentDef{Tools: []string{"Read", "Workspace"}}, true)
	got := d.snapshot()
	want := map[deprecatedUseKey]int64{
		{config.RemovedKindTool, "Bash"}:               1,
		{config.RemovedKindField, "shell_credentials"}: 1,
	}
	if len(got) != len(want) {
		t.Fatalf("counts = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%v = %d, want %d", k, got[k], v)
		}
	}
}

// One log line per (tenant, agent) in a process, naming the agent and the
// items by name: never a credential the definition lists.
func TestDeprecatedUse_LogsOncePerTenantAndAgentWithNamesOnly(t *testing.T) {
	log, buf := captureDeprecatedLog()
	d := newDeprecatedUse(log)
	for range 3 {
		d.note(context.Background(), "t1", "coder", shellDef(), true)
	}
	d.note(context.Background(), "t2", "coder", shellDef(), true)
	d.note(context.Background(), "t1", "other", shellDef(), true)
	out := buf.String()
	if n := strings.Count(out, "and the run was not given it"); n != 3 {
		t.Fatalf("got %d log lines, want 3 (t1/coder, t2/coder, t1/other):\n%s", n, out)
	}
	for _, want := range []string{"coder", "Bash", "shell_credentials", config.MigrationGuide} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cred-name-5e1d") {
		t.Errorf("the log names a credential:\n%s", out)
	}
}

// A static agent (an embedded bundle's; an operator's would not load) is
// counted and not logged.
func TestDeprecatedUse_AStaticAgentIsCountedNotLogged(t *testing.T) {
	log, buf := captureDeprecatedLog()
	d := newDeprecatedUse(log)
	d.note(context.Background(), "t1", "coder", shellDef(), false)
	if buf.Len() != 0 {
		t.Fatalf("logged: %s", buf.String())
	}
	if d.snapshot()[deprecatedUseKey{config.RemovedKindTool, "Bash"}] != 1 {
		t.Fatal("not counted")
	}
}

// The once-per-agent memory is bounded: past it, uses are still counted and
// no longer logged, so a stream of distinct names cannot grow the process.
func TestDeprecatedUse_TheLoggedSetIsBounded(t *testing.T) {
	log, buf := captureDeprecatedLog()
	d := newDeprecatedUse(log)
	for i := range maxDeprecatedLogKeys + 10 {
		d.note(context.Background(), "t", string(rune('a'+i%26))+strings.Repeat("x", i), shellDef(), true)
	}
	if n := strings.Count(buf.String(), "and the run was not given it"); n != maxDeprecatedLogKeys {
		t.Fatalf("logged %d, want %d", n, maxDeprecatedLogKeys)
	}
	if got := d.snapshot()[deprecatedUseKey{config.RemovedKindTool, "Bash"}]; got != maxDeprecatedLogKeys+10 {
		t.Fatalf("counted %d", got)
	}
}

// Every run start goes through startRunSpan, so that is where a use is
// counted; a stored definition (DefID set) is logged, the operator's yaml
// agent of the same name only counted.
func TestStartRunSpan_CountsADeprecatedUse(t *testing.T) {
	log, buf := captureDeprecatedLog()
	srv := newMetricsPromServer(t, false)
	srv.deprecatedUse = newDeprecatedUse(log)
	cfg := srv.cfg()
	cfg.Agents = map[string]config.AgentDef{"coder": shellDef()}
	stored := shellDef()
	stored.DefID = "def-1"
	_, span := srv.startRunSpan(context.Background(), stored, lcotel.RunStartAttrs{AgentName: "coder", Tenant: "t1"})
	span.End()
	_, span = srv.startRunSpan(context.Background(), shellDef(), lcotel.RunStartAttrs{AgentName: "coder", Tenant: "t2"})
	span.End()
	if got := srv.deprecatedUse.snapshot()[deprecatedUseKey{config.RemovedKindTool, "Bash"}]; got != 2 {
		t.Fatalf("counted %d, want 2", got)
	}
	if n := strings.Count(buf.String(), "and the run was not given it"); n != 1 {
		t.Fatalf("logged %d lines, want 1 (the stored definition):\n%s", n, buf.String())
	}
}

func TestMetricsProm_WritesDeprecatedUse(t *testing.T) {
	log, _ := captureDeprecatedLog()
	srv := newMetricsPromServer(t, false)
	srv.deprecatedUse = newDeprecatedUse(log)
	srv.deprecatedUse.note(context.Background(), "t1", "coder", shellDef(), true)
	full := httptest.NewRecorder()
	srv.renderMetricsProm(full, promView{})
	body := full.Body.String()
	for _, want := range []string{
		"# TYPE loomcycle_deprecated_use_total counter",
		`kind="tool",name="Bash"} 1`,
		`kind="field",name="shell_credentials"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	// A tenant's confined view carries no deployment-wide series.
	confined := httptest.NewRecorder()
	srv.renderMetricsProm(confined, promView{tenant: "t1"})
	if strings.Contains(confined.Body.String(), "loomcycle_deprecated_use_total") {
		t.Error("the tenant view carries the deployment's deprecated use")
	}
}
