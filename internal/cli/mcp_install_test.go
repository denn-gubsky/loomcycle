package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestRunMCPInstall_JSONOnlyDocker pins the wire shape of --json
// --transport=docker output: a single-key object containing
// {command, args, env?} with the docker invocation laid out
// correctly + the API-key passthrough -e flags present.
func TestRunMCPInstall_JSONOnlyDocker(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunMCPInstall(
		[]string{"--transport", "docker", "--json", "--config", "/tmp/lc.yaml"},
		&stdout, &stderr,
	)
	if rc != 0 {
		t.Fatalf("rc=%d, stderr=%q", rc, stderr.String())
	}
	var doc map[string]mcpServerConfig
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	cfg, ok := doc["loomcycle"]
	if !ok {
		t.Fatalf("expected key 'loomcycle' in output, got: %v", doc)
	}
	if cfg.Command != "docker" {
		t.Errorf("command = %q, want %q", cfg.Command, "docker")
	}
	// Mount + config path + image must all appear in args.
	args := strings.Join(cfg.Args, " ")
	if !strings.Contains(args, "/tmp:/etc/loomcycle:ro") {
		t.Errorf("expected mount of /tmp (dirname of --config) in args; got: %v", cfg.Args)
	}
	if !strings.Contains(args, "denngubsky/loomcycle") {
		t.Errorf("expected image in args; got: %v", cfg.Args)
	}
	if !strings.Contains(args, "/etc/loomcycle/loomcycle.yaml") {
		t.Errorf("expected mapped config path in args; got: %v", cfg.Args)
	}
	// At least one -e KEY entry must be present (the contract is that
	// API keys flow through from the parent shell).
	if !strings.Contains(args, "-e ANTHROPIC_API_KEY") {
		t.Errorf("expected `-e ANTHROPIC_API_KEY` in args; got: %v", cfg.Args)
	}
}

// TestRunMCPInstall_JSONOnlyDocker_CustomImage proves --docker-image
// overrides the default `denngubsky/loomcycle:latest` pin — relevant
// for operators pinning a specific version tag in production.
func TestRunMCPInstall_JSONOnlyDocker_CustomImage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunMCPInstall(
		[]string{"--transport", "docker", "--json", "--config", "/tmp/lc.yaml",
			"--docker-image", "denngubsky/loomcycle:0.12.6"},
		&stdout, &stderr,
	)
	if rc != 0 {
		t.Fatalf("rc=%d, stderr=%q", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "denngubsky/loomcycle:0.12.6") {
		t.Errorf("expected pinned image in output; got: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), ":latest") {
		t.Errorf("default :latest should NOT appear when --docker-image overrides")
	}
}

// TestRunMCPInstall_CustomServerName ensures the JSON top-level key
// reflects --server-name so an operator can register multiple
// loomcycle instances side-by-side (e.g., a "loomcycle-prod" alongside
// a "loomcycle-staging" pointing at different yaml configs).
func TestRunMCPInstall_CustomServerName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunMCPInstall(
		[]string{"--transport", "docker", "--json", "--config", "/tmp/lc.yaml",
			"--server-name", "loomcycle-prod"},
		&stdout, &stderr,
	)
	if rc != 0 {
		t.Fatalf("rc=%d, stderr=%q", rc, stderr.String())
	}
	var doc map[string]mcpServerConfig
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["loomcycle-prod"]; !ok {
		t.Errorf("expected key 'loomcycle-prod', got keys: %v", keysOf(doc))
	}
}

// TestRunMCPInstall_UnknownTransport surfaces a clear error for an
// invalid --transport. Exit code 2 means "fix your invocation."
func TestRunMCPInstall_UnknownTransport(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunMCPInstall(
		[]string{"--transport", "wat", "--json", "--config", "/tmp/lc.yaml"},
		&stdout, &stderr,
	)
	if rc != 2 {
		t.Errorf("rc = %d, want 2 (user error)", rc)
	}
	if !strings.Contains(stderr.String(), "unknown transport") {
		t.Errorf("stderr should mention 'unknown transport'; got: %q", stderr.String())
	}
}

// TestRunMCPInstall_HumanReadableSections_Docker pins the structure of
// the non-JSON output (the default UX). Should contain both the Claude
// Code CLI command AND the Claude Desktop JSON paste-block.
func TestRunMCPInstall_HumanReadableSections_Docker(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunMCPInstall(
		[]string{"--transport", "docker", "--config", "/tmp/lc.yaml"},
		&stdout, &stderr,
	)
	if rc != 0 {
		t.Fatalf("rc=%d, stderr=%q", rc, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Transport: docker",
		"claude mcp add-json loomcycle",
		"Claude Desktop",
		"\"mcpServers\":",
		"docs/MCP_SERVER.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q; full output:\n%s", want, out)
		}
	}
}

// TestBuildMCPServerConfig_UnknownTransport is a direct test on the
// builder so we don't depend on flag-set wiring.
func TestBuildMCPServerConfig_UnknownTransport(t *testing.T) {
	if _, _, err := buildMCPServerConfig("bogus", "/x.yaml", "img"); err == nil {
		t.Errorf("expected error for unknown transport, got nil")
	}
}

func keysOf(m map[string]mcpServerConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
