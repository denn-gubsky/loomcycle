// Package awsauthtest checks, through a real server and store, that a run on a
// provider authenticated with AWS credentials (RFC EM) leaves none of those
// credentials anywhere loomcycle writes: the database, the run's stream, or
// the log. A package of its own because it needs the HTTP server, the store
// and the provider build together.
package awsauthtest

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/anthropic" // registers the driver
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	secretKey    = "never-persist-secret-key-0123456789abcdef"
	sessionToken = "never-persist-session-token-IQoJb3JpZ2luX2Vj"
)

type oneProvider struct{ p providers.Provider }

func (r oneProvider) Get(string) (providers.Provider, error) { return r.p, nil }

// fakeBedrock answers the Messages route, recording the key or signature each
// request carried. fail makes it answer with an access denial instead.
type fakeBedrock struct {
	mu        sync.Mutex
	keys      []string
	fail      bool
	signature bool
}

func (f *fakeBedrock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	if k := r.Header.Get("x-api-key"); k != "" {
		f.keys = append(f.keys, k)
	}
	if strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") && r.Header.Get("X-Amz-Security-Token") == sessionToken {
		f.signature = true
	}
	fail := f.fail
	f.mu.Unlock()
	if fail {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"type":"error","error":{"type":"permission_error","message":"not authorized to perform: bedrock:InvokeModel"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"eu.anthropic.claude-sonnet-5-5\"}}\n\n"+
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n"+
		"event: content_block_stop\ndata: {\"index\":0}\n\n"+
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}\n\n"+
		"event: message_stop\ndata: {}\n\n")
}

func TestRunOnAnAWSProvider_PersistsNoCredential(t *testing.T) {
	for _, mode := range []string{config.ProviderAuthSigV4, config.ProviderAuthBedrockToken} {
		t.Run(mode, func(t *testing.T) { runAndScan(t, mode) })
	}
}

func runAndScan(t *testing.T, mode string) {
	dir := t.TempDir()
	for _, k := range []string{"AWS_PROFILE", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "aws-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "aws-credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ACCESS_KEY_ID", "ASIANEVERPERSIST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", secretKey)
	t.Setenv("AWS_SESSION_TOKEN", sessionToken)

	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	bedrock := &fakeBedrock{}
	upstream := httptest.NewServer(bedrock)
	defer upstream.Close()

	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "bedrock", Model: "eu.anthropic.claude-sonnet-5-5"},
		Agents:      map[string]config.AgentDef{"assistant": {Model: "eu.anthropic.claude-sonnet-5-5", SystemPrompt: "help"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 2, MaxQueueDepth: 2},
		Providers: map[string]config.ProviderConfig{"bedrock": {Driver: "anthropic", BaseURL: upstream.URL,
			Auth: &config.ProviderAuth{Type: mode, Region: "eu-central-1", Service: "bedrock"}}},
	}
	prov, err := providerbuild.Provider(cfg, "bedrock")
	if err != nil {
		t.Fatal(err)
	}
	st, err := storesqlite.Open(filepath.Join(dir, "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := lchttp.New(cfg, oneProvider{prov}, []tools.Tool{}, concurrency.New(2, 2, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	var streams []string
	for _, fail := range []bool{false, true} { // a completed run, then a refused one
		bedrock.mu.Lock()
		bedrock.fail = fail
		bedrock.mu.Unlock()
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"assistant","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		streams = append(streams, string(body))
	}
	if !strings.Contains(streams[0], "done") || !strings.Contains(streams[1], "bedrock:InvokeModel") {
		t.Fatalf("runs did not reach the fake Bedrock:\n%s\n---\n%s", streams[0], streams[1])
	}
	_ = st.Close()

	secrets := map[string]string{"secret access key": secretKey, "session token": sessionToken}
	bedrock.mu.Lock()
	switch mode {
	case config.ProviderAuthSigV4:
		if !bedrock.signature {
			t.Fatal("the fake Bedrock never saw a signed request carrying the session token")
		}
	case config.ProviderAuthBedrockToken:
		if len(bedrock.keys) == 0 {
			t.Fatal("the fake Bedrock never saw a minted key")
		}
		secrets["minted key"] = bedrock.keys[0]
	}
	bedrock.mu.Unlock()

	// Everything loomcycle wrote: the database files (with the WAL), what it
	// streamed back, and what it logged.
	written := map[string]string{"log": logs.String(), "streams": strings.Join(streams, "\n")}
	files, _ := filepath.Glob(filepath.Join(dir, "runs.db*"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		written[filepath.Base(f)] = string(b)
	}
	for where, content := range written {
		for what, v := range secrets {
			if strings.Contains(content, v) {
				t.Errorf("%s found in %s", what, where)
			}
		}
	}
}
