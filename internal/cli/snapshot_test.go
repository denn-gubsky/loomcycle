package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunSnapshot_HappyPath(t *testing.T) {
	var body []byte
	srv := stubServer(t, "POST", "/v1/_snapshots", 201,
		`{"id":"snap_123","created_at":"2026-05-18T00:00:00Z","schema_version":1,"byte_size":1024,"label":"before-backup"}`,
		&body)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunSnapshot([]string{"--target", srv.URL, "--description", "before-backup"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "id=snap_123") {
		t.Errorf("stdout = %q, expected id=snap_123", stdout.String())
	}
	if !strings.Contains(string(body), `"label":"before-backup"`) {
		t.Errorf("request body = %q, expected label", body)
	}
}

func TestRunSnapshot_IncludeHistorySendsSince(t *testing.T) {
	var body []byte
	srv := stubServer(t, "POST", "/v1/_snapshots", 201, `{"id":"x","schema_version":1,"byte_size":1}`, &body)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunSnapshot([]string{
		"--target", srv.URL,
		"--include-history",
		"--since", "2026-05-01T00:00:00Z",
	}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if !strings.Contains(string(body), `"include_history":true`) ||
		!strings.Contains(string(body), `"include_history_since":"2026-05-01T00:00:00Z"`) {
		t.Errorf("body = %q missing include_history fields", body)
	}
}

func TestRunSnapshotsList_FormatsRows(t *testing.T) {
	srv := stubServer(t, "GET", "/v1/_snapshots", 200, `{"entries":[
		{"id":"snap_a","created_at":"2026-05-18T00:00:00Z","schema_version":1,"byte_size":100,"label":"l1"},
		{"id":"snap_b","created_at":"2026-05-18T01:00:00Z","schema_version":1,"byte_size":200,"label":""}
	]}`, nil)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunSnapshotsList([]string{"--target", srv.URL}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "snap_a") || !strings.Contains(out, "snap_b") {
		t.Errorf("stdout = %q missing snapshot ids", out)
	}
	// Tab-separated, one per line.
	if strings.Count(out, "\n") != 2 {
		t.Errorf("expected 2 lines, got %d (out = %q)", strings.Count(out, "\n"), out)
	}
}

func TestRunSnapshotsList_AppendsQueryParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"entries":[]}`)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunSnapshotsList([]string{"--target", srv.URL, "--limit", "5", "--label-contains", "before backup"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	// url.QueryEscape encodes space as '+' (HTML form-encoding) —
	// servers using net/url's Query().Get() decode both '+' and
	// '%20' as space.
	if !strings.Contains(gotQuery, "limit=5") || !strings.Contains(gotQuery, "label_contains=before+backup") {
		t.Errorf("query = %q", gotQuery)
	}
}

func TestRunSnapshotsExport_WritesToFile(t *testing.T) {
	envelope := `{"schema_version":1,"sections":{}}`
	srv := stubServer(t, "GET", "/v1/_snapshots/snap_x/export", 200, envelope, nil)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "snap.json")
	var stdout, stderr bytes.Buffer
	rc := RunSnapshotsExport([]string{"--target", srv.URL, "--out", out, "snap_x"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, stderr.String())
	}
	data, _ := os.ReadFile(out)
	if string(data) != envelope {
		t.Errorf("file = %q, want %q", string(data), envelope)
	}
}

func TestRunSnapshotsExport_WritesToStdout(t *testing.T) {
	envelope := `{"schema_version":1,"sections":{}}`
	srv := stubServer(t, "GET", "/v1/_snapshots/snap_x/export", 200, envelope, nil)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunSnapshotsExport([]string{"--target", srv.URL, "snap_x"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if stdout.String() != envelope {
		t.Errorf("stdout = %q, want envelope", stdout.String())
	}
}

func TestRunSnapshotsExport_MissingIDReturnsUserError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunSnapshotsExport([]string{"--target", "http://nowhere"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc = %d, want 2 (missing positional arg)", rc)
	}
}

func TestRunSnapshotsDelete_HappyPath(t *testing.T) {
	srv := stubServer(t, "DELETE", "/v1/_snapshots/snap_x", 204, "", nil)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunSnapshotsDelete([]string{"--target", srv.URL, "snap_x"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "deleted id=snap_x") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunRestore_ReadsFileAndSendsInline(t *testing.T) {
	envelope := `{"schema_version":1,"sections":{}}`
	path := filepath.Join(t.TempDir(), "snap.json")
	_ = os.WriteFile(path, []byte(envelope), 0o644)

	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"memory_restored":3,"paused_runs_restored":1}`)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunRestore([]string{"--target", srv.URL, path}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, stderr.String())
	}
	if !strings.Contains(string(body), `"json"`) {
		t.Errorf("body = %q, expected {\"json\": ...}", body)
	}
	if !strings.Contains(stdout.String(), "memory=3") || !strings.Contains(stdout.String(), "paused_runs=1") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunRestore_MissingFileReturnsUserError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunRestore([]string{"--target", "http://nowhere"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc = %d, want 2 (missing positional arg)", rc)
	}
}

func TestRunRestore_BadFilePathReturnsUserError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunRestore([]string{"--target", "http://nowhere", "/no/such/file.json"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc = %d, want 2 (file read error)", rc)
	}
}
