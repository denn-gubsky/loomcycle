//go:build unix

package builtin

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A batch of tool calls runs concurrently, and go-git rewrites the index in
// place with no lock: two adds at once lost one of them. Calls on one
// repository are serialised, so every add is staged.
func TestGit_ConcurrentAddsOnOneRepositoryAllStage(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		root := realDir(t)
		g := &Git{}
		ctx := gitCtx(root, false, tools.GitRemotePolicyValue{})
		mustGitOK(t, g, ctx, map[string]any{"op": "init"})
		const n = 6
		for i := 0; i < n; i++ {
			if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d.txt", i)), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				body := fmt.Sprintf(`{"op":"add","paths":["f%d.txt"]}`, i)
				if res, err := g.Execute(ctx, []byte(body)); err != nil || res.IsError {
					t.Errorf("add f%d: %v %s", i, err, res.Text)
				}
			}(i)
		}
		wg.Wait()
		st := mustGitOK(t, g, ctx, map[string]any{"op": "status"})
		for _, f := range st["files"].([]any) {
			fm := f.(map[string]any)
			if fm["staging"] != "A" {
				t.Fatalf("trial %d: %s is %q after a concurrent add, want staged (A)", trial, fm["path"], fm["staging"])
			}
		}
	}
}

// A refused network attempt is audited with its class: the operator sees an
// agent trying a host it may not reach, not only the attempts that ran.
func TestGit_RefusedNetworkAttemptsAreAudited(t *testing.T) {
	root := realDir(t)
	sink := &recordingSink{}
	g := &Git{Source: &GitSourceConfig{Hosts: []string{"git.example.invalid"}, MaxBytes: 1 << 20, MaxObjectBytes: 1 << 20, Transport: noRoundTrip{}}, Audit: sink}
	wantGitClass(t, g, gitCtx(root, false, tools.GitRemotePolicyValue{}), map[string]any{"op": "ls_remote", "url": "https://github.com/acme/app.git"}, "git_remote_not_allowed")
	wantGitClass(t, g, gitCtx(root, false, tools.GitRemotePolicyValue{Fetch: true}), map[string]any{"op": "ls_remote", "url": "https://github.com/acme/app.git", "credential": "$cred:forge"}, "host_not_allowed")
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.evs) != 2 {
		t.Fatalf("audit records: %d, want one per refused attempt", len(sink.evs))
	}
	if ev := sink.evs[0]; ev.Action != "git_ls_remote" || ev.GitOutcome != "git_remote_not_allowed" {
		t.Errorf("first record: %+v", ev)
	}
	if ev := sink.evs[1]; ev.GitOutcome != "host_not_allowed" || ev.GitHost != "github.com" || ev.GitCredential != "$cred:forge" {
		t.Errorf("second record: %+v", ev)
	}
}

// A remote URL someone else wrote with a token in its user part is listed
// without it.
func TestGit_RemoteListHidesUserinfo(t *testing.T) {
	root := realDir(t)
	g := &Git{}
	ctx := gitCtx(root, false, tools.GitRemotePolicyValue{})
	mustGitOK(t, g, ctx, map[string]any{"op": "init"})
	cfg := filepath.Join(root, ".git", "config")
	b, err := os.ReadFile(cfg)
	if err != nil && !os.IsNotExist(err) { // go-git writes no config at init
		t.Fatal(err)
	}
	b = append(b, []byte("[remote \"origin\"]\n\turl = https://ci:"+gitToolTestToken+"@github.com/acme/app.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n")...)
	if err := os.WriteFile(cfg, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, raw := runGit(t, g, ctx, map[string]any{"op": "remote_list"})
	if strings.Contains(raw, gitToolTestToken) || !strings.Contains(raw, "github.com/acme/app.git") {
		t.Errorf("remote_list: %s", raw)
	}
}

// Lists are capped and say so.
func TestGit_StatusIsCappedAndMarkedTruncated(t *testing.T) {
	root := realDir(t)
	g := &Git{}
	ctx := gitCtx(root, false, tools.GitRemotePolicyValue{})
	mustGitOK(t, g, ctx, map[string]any{"op": "init"})
	for i := 0; i < gitMaxListed+5; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%04d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st := mustGitOK(t, g, ctx, map[string]any{"op": "status"})
	if n := len(st["files"].([]any)); n != gitMaxListed || st["truncated"] != true || st["clean"] != false {
		t.Errorf("status: %d files, truncated=%v clean=%v", n, st["truncated"], st["clean"])
	}
}

// noRoundTrip is a transport that must never be used: a refused attempt
// sends nothing.
type noRoundTrip struct{}

func (noRoundTrip) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("a refused git operation made a request")
}
