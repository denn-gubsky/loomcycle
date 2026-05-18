package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// v0.8.17 PR 4: CLI subcommands for the snapshot HTTP surface.
//
//   loomcycle snapshot [--description ...] [--include-history --since RFC3339]
//   loomcycle snapshots list [--limit N] [--label-contains S]
//   loomcycle snapshots export <snapshot_id> [--out file.json]
//   loomcycle snapshots delete <snapshot_id>
//   loomcycle restore <file.json>
//
// All commands share the --target / --token flags + env-var defaults
// from pause.go.

type snapshotCreateBody struct {
	Label               string `json:"label,omitempty"`
	IncludeHistory      bool   `json:"include_history,omitempty"`
	IncludeHistorySince string `json:"include_history_since,omitempty"`
	MaxBytes            int64  `json:"max_bytes,omitempty"`
}

type snapshotCreateResp struct {
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	Label         string `json:"label,omitempty"`
	SchemaVersion int    `json:"schema_version"`
	ByteSize      int64  `json:"byte_size"`
}

type snapshotListEntry struct {
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	Label         string `json:"label,omitempty"`
	SchemaVersion int    `json:"schema_version"`
	ByteSize      int64  `json:"byte_size"`
}

type snapshotListResp struct {
	Entries []snapshotListEntry `json:"entries"`
}

type snapshotRestoreBody struct {
	IncludeHistory bool            `json:"include_history,omitempty"`
	JSON           json.RawMessage `json:"json,omitempty"`
}

type snapshotRestoreResp struct {
	AgentDefsRestored          int      `json:"agent_defs_restored,omitempty"`
	AgentDefActiveRestored     int      `json:"agent_def_active_restored,omitempty"`
	MemoryRestored             int      `json:"memory_restored,omitempty"`
	ChannelMessagesRestored    int      `json:"channel_messages_restored,omitempty"`
	ChannelCursorsRestored     int      `json:"channel_cursors_restored,omitempty"`
	EvaluationsRestored        int      `json:"evaluations_restored,omitempty"`
	PausedRunsRestored         int      `json:"paused_runs_restored,omitempty"`
	SynthesizedSessions        int      `json:"synthesized_sessions,omitempty"`
	TranscriptEventsRestored   int      `json:"transcript_events_restored,omitempty"`
	InteractionHistoryRestored int      `json:"interaction_history_restored,omitempty"`
	Warnings                   []string `json:"warnings,omitempty"`
}

// RunSnapshot — POST /v1/_snapshots. Captures a new snapshot and
// prints the minted id. Operators chain it with `snapshots export`
// to save the JSON; the bare verb just records the row in the store.
func RunSnapshot(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", defaultBaseURL(), "loomcycle base URL")
	token := fs.String("token", defaultAuthToken(), "bearer token")
	description := fs.String("description", "", "free-text label stored on snapshots.label (operator's marker)")
	includeHistory := fs.Bool("include-history", false, "also capture interaction_history (large; opt-in)")
	since := fs.String("since", "", "RFC3339 timestamp; only honoured with --include-history")
	maxBytes := fs.Int64("max-bytes", 0, "override LOOMCYCLE_SNAPSHOT_MAX_BYTES for this call; 0 ⇒ server default")
	httpTimeout := fs.Duration("http-timeout", 5*time.Minute, "client-side HTTP timeout (capture reads every section; large stores need this generous)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	body, _ := json.Marshal(snapshotCreateBody{
		Label:               *description,
		IncludeHistory:      *includeHistory,
		IncludeHistorySince: *since,
		MaxBytes:            *maxBytes,
	})
	url := strings.TrimRight(*target, "/") + "/v1/_snapshots"
	rc, resp, err := doAdminRequest(http.MethodPost, url, *token, body, *httpTimeout)
	if err != nil {
		return failOp(stderr, "POST %s: %v", url, err)
	}
	if rc != 0 {
		return failPrintingBody(stderr, url, resp, rc)
	}
	var out snapshotCreateResp
	if err := json.Unmarshal(resp, &out); err != nil {
		return failOp(stderr, "decode response: %v (body: %s)", err, truncate(resp, 200))
	}
	fmt.Fprintf(stdout, "id=%s byte_size=%d schema_version=%d created_at=%s",
		out.ID, out.ByteSize, out.SchemaVersion, out.CreatedAt)
	if out.Label != "" {
		fmt.Fprintf(stdout, " label=%q", out.Label)
	}
	fmt.Fprintln(stdout)
	return 0
}

// RunSnapshotsList — GET /v1/_snapshots. One row per line, tab-
// separated for easy shell scripting. Operators wanting JSON use
// curl directly; this is the human-friendly path.
func RunSnapshotsList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("snapshots list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", defaultBaseURL(), "loomcycle base URL")
	token := fs.String("token", defaultAuthToken(), "bearer token")
	limit := fs.Int("limit", 200, "max rows returned (server cap also applies)")
	labelContains := fs.String("label-contains", "", "filter by label substring")
	httpTimeout := fs.Duration("http-timeout", 30*time.Second, "client-side HTTP timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	url := strings.TrimRight(*target, "/") + "/v1/_snapshots"
	q := []string{}
	if *limit > 0 {
		q = append(q, fmt.Sprintf("limit=%d", *limit))
	}
	if *labelContains != "" {
		q = append(q, "label_contains="+queryEscape(*labelContains))
	}
	if len(q) > 0 {
		url += "?" + strings.Join(q, "&")
	}
	rc, resp, err := doAdminRequest(http.MethodGet, url, *token, nil, *httpTimeout)
	if err != nil {
		return failOp(stderr, "GET %s: %v", url, err)
	}
	if rc != 0 {
		return failPrintingBody(stderr, url, resp, rc)
	}
	var out snapshotListResp
	if err := json.Unmarshal(resp, &out); err != nil {
		return failOp(stderr, "decode response: %v (body: %s)", err, truncate(resp, 200))
	}
	for _, e := range out.Entries {
		fmt.Fprintf(stdout, "%s\t%s\t%d\t%s\n", e.ID, e.CreatedAt, e.ByteSize, e.Label)
	}
	return 0
}

// RunSnapshotsExport — GET /v1/_snapshots/{id}/export. Streams the
// raw JSON envelope to stdout by default, or --out file.json. The
// server already returned canonical JSON (Capture's marshalled
// bytes) so we don't re-format.
func RunSnapshotsExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("snapshots export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", defaultBaseURL(), "loomcycle base URL")
	token := fs.String("token", defaultAuthToken(), "bearer token")
	out := fs.String("out", "", "write envelope to this file instead of stdout")
	httpTimeout := fs.Duration("http-timeout", 5*time.Minute, "client-side HTTP timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		return fail(stderr, "snapshot id is required: `loomcycle snapshots export <snap_id>`")
	}
	id := rest[0]
	url := strings.TrimRight(*target, "/") + "/v1/_snapshots/" + id + "/export"
	rc, resp, err := doAdminRequest(http.MethodGet, url, *token, nil, *httpTimeout)
	if err != nil {
		return failOp(stderr, "GET %s: %v", url, err)
	}
	if rc != 0 {
		return failPrintingBody(stderr, url, resp, rc)
	}
	if *out != "" {
		if err := os.WriteFile(*out, resp, 0o644); err != nil {
			return failOp(stderr, "write %s: %v", *out, err)
		}
		fmt.Fprintf(stderr, "wrote %d bytes to %s\n", len(resp), *out)
		return 0
	}
	_, _ = stdout.Write(resp)
	return 0
}

// RunSnapshotsDelete — DELETE /v1/_snapshots/{id}. Idempotent on the
// server side; CLI reports 204 as success.
func RunSnapshotsDelete(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("snapshots delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", defaultBaseURL(), "loomcycle base URL")
	token := fs.String("token", defaultAuthToken(), "bearer token")
	httpTimeout := fs.Duration("http-timeout", 30*time.Second, "client-side HTTP timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		return fail(stderr, "snapshot id is required: `loomcycle snapshots delete <snap_id>`")
	}
	id := rest[0]
	url := strings.TrimRight(*target, "/") + "/v1/_snapshots/" + id
	rc, resp, err := doAdminRequest(http.MethodDelete, url, *token, nil, *httpTimeout)
	if err != nil {
		return failOp(stderr, "DELETE %s: %v", url, err)
	}
	if rc != 0 {
		return failPrintingBody(stderr, url, resp, rc)
	}
	fmt.Fprintf(stdout, "deleted id=%s\n", id)
	return 0
}

// RunRestore — POST /v1/_snapshots/{ignored}/restore with the
// {"json": <envelope>} body. The {id} path segment is ignored when
// an inline envelope is supplied. Operators use this to import an
// envelope file onto a fresh loomcycle instance.
func RunRestore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", defaultBaseURL(), "loomcycle base URL")
	token := fs.String("token", defaultAuthToken(), "bearer token")
	includeHistory := fs.Bool("include-history", false, "also restore interaction_history events")
	httpTimeout := fs.Duration("http-timeout", 10*time.Minute, "client-side HTTP timeout (per-section idempotent inserts, large envelopes take time)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 1 {
		return fail(stderr, "envelope file is required: `loomcycle restore <file.json>`")
	}
	path := rest[0]
	envelope, err := os.ReadFile(path)
	if err != nil {
		return fail(stderr, "read %s: %v", path, err)
	}
	body, _ := json.Marshal(snapshotRestoreBody{
		IncludeHistory: *includeHistory,
		JSON:           json.RawMessage(envelope),
	})
	url := strings.TrimRight(*target, "/") + "/v1/_snapshots/inline/restore"
	rc, resp, err := doAdminRequest(http.MethodPost, url, *token, body, *httpTimeout)
	if err != nil {
		return failOp(stderr, "POST %s: %v", url, err)
	}
	if rc != 0 {
		return failPrintingBody(stderr, url, resp, rc)
	}
	var out snapshotRestoreResp
	if err := json.Unmarshal(resp, &out); err != nil {
		return failOp(stderr, "decode response: %v (body: %s)", err, truncate(resp, 200))
	}
	fmt.Fprintf(stdout, "restored agent_defs=%d agent_def_active=%d memory=%d channel_messages=%d channel_cursors=%d evaluations=%d paused_runs=%d transcript_events=%d synthesized_sessions=%d\n",
		out.AgentDefsRestored, out.AgentDefActiveRestored, out.MemoryRestored,
		out.ChannelMessagesRestored, out.ChannelCursorsRestored, out.EvaluationsRestored,
		out.PausedRunsRestored, out.TranscriptEventsRestored, out.SynthesizedSessions)
	for _, w := range out.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	return 0
}

// queryEscape is a tiny wrapper around url.QueryEscape so callers
// don't import "net/url" just for this. Path doesn't need escaping
// (snapshot ids from mintID are [a-z0-9_-]) but query values from
// --label-contains can contain anything operator types.
func queryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == '~':
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}
