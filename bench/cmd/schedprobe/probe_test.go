package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOllama answers /api/chat with a one-chunk reply and a done frame carrying
// the usage and phase durations, and /api/embed with one vector. It records the
// peak number of chat calls in flight and every chat request's message count.
type fakeOllama struct {
	inFlight, peak atomic.Int32
	mu             sync.Mutex
	chatMsgCounts  []int
	sawReply       bool
}

func (f *fakeOllama) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			n := f.inFlight.Add(1)
			defer f.inFlight.Add(-1)
			for {
				p := f.peak.Load()
				if n <= p || f.peak.CompareAndSwap(p, n) {
					break
				}
			}
			var req struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("chat body: %v", err)
			}
			f.mu.Lock()
			f.chatMsgCounts = append(f.chatMsgCounts, len(req.Messages))
			for _, m := range req.Messages {
				if m.Role == "assistant" && m.Content == "fake reply" {
					f.sawReply = true
				}
			}
			f.mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			w.Header().Set("Content-Type", "application/x-ndjson")
			io.WriteString(w, `{"model":"m","message":{"role":"assistant","content":"fake reply"},"done":false}`+"\n")
			io.WriteString(w, `{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop",`+
				`"prompt_eval_count":321,"eval_count":12,"total_duration":40000000,"load_duration":2000000,`+
				`"prompt_eval_duration":30000000,"eval_duration":8000000}`+"\n")
		case "/api/embed":
			io.WriteString(w, `{"embeddings":[[0.1,0.2,0.3]]}`)
		default:
			http.NotFound(w, r)
		}
	})
}

func TestProbe_AKindWithNoLaneIsRefusedBeforeAnyCall(t *testing.T) {
	fake := &fakeOllama{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	dir := t.TempDir()
	wl, err := os.Create(filepath.Join(dir, "w.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-gen", "-agents", "2", "-turns", "2", "-model", "m", "-embed-agents", "1", "-embed-model", "e"}, wl); err != nil {
		t.Fatal(err)
	}
	wl.Close()
	out := filepath.Join(dir, "calls.jsonl")
	err = run([]string{"-base-url", srv.URL, "-workload", wl.Name(), "-policy", "call", "-slots", "chat=1",
		"-tool-ms", "0", "-label", "t", "-out", out}, io.Discard)
	if err == nil {
		t.Fatal("an embed agent with no embed lane was accepted")
	}
	if fake.peak.Load() != 0 {
		t.Fatal("calls were made before the workload was refused")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("output was written for a refused workload")
	}
}

func TestProbe_RecordsServerTimingsAndHoldsTheChatCap(t *testing.T) {
	fake := &fakeOllama{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	dir := t.TempDir()

	wl, err := os.Create(filepath.Join(dir, "w.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-gen", "-agents", "3", "-turns", "3", "-system-chars", "300", "-turn-chars", "80",
		"-model", "m", "-num-ctx", "8192", "-embed-agents", "1", "-embed-model", "e"}, wl); err != nil {
		t.Fatal(err)
	}
	wl.Close()

	out := filepath.Join(dir, "calls.jsonl")
	if err := run([]string{"-base-url", srv.URL, "-workload", wl.Name(), "-policy", "call",
		"-slots", "chat=1,embed=1", "-tool-ms", "0", "-label", "t", "-out", out}, io.Discard); err != nil {
		t.Fatal(err)
	}

	if p := fake.peak.Load(); p != 1 {
		t.Fatalf("peak chat calls at the server = %d, want 1 (chat=1)", p)
	}
	if !fake.sawReply {
		t.Fatal("no chat request carried the model's earlier reply: the conversation does not grow like the loop's")
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var headers, chat, embed int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var r callRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if r.Type == "run" {
			headers++
			continue
		}
		if r.Error != "" {
			t.Fatalf("call failed: %+v", r)
		}
		switch r.Kind {
		case "chat":
			chat++
			if r.PromptEvalCount != 321 || r.EvalCount != 12 || r.PrefillMs != 30 || r.LoadMs != 2 || r.DecodeMs != 8 {
				t.Fatalf("chat record lost the server's counts or timings: %+v", r)
			}
			if r.NumCtx != 8192 || r.PromptChars <= 300 || r.TTFTMs <= 0 || r.Slot != 0 {
				t.Fatalf("chat record incomplete: %+v", r)
			}
		case "embed":
			embed++
		}
	}
	if headers != 2 || chat != 9 || embed != 3 {
		t.Fatalf("records: %d headers, %d chat, %d embed; want 2, 9, 3", headers, chat, embed)
	}
}
