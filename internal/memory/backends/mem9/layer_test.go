package mem9_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/mem9"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// IMPORTANT — same honesty as mem9_test.go: the layerStub below IS the
// contract under test. It implements the ASSUMED Mem9 v1alpha2 smart-mode
// write (202) + the q= recall that layer.go isolates behind its wire-shape
// banner. These tests prove the loomcycle-side mapping (Add mode selection,
// AddPending honesty, credential header, tenant scoping, recall→facts with
// TopK trim + Threshold filter) is internally consistent against THAT stub.
// They do NOT prove it matches the real github.com/mem9-ai/mem9 API — that
// is verified operator-side. If the real API differs, update the wire block
// in layer.go AND this stub together.

// layerStub implements the assumed memory-layer endpoints. It records the
// last write request and the last recall query so tests can assert mode,
// auth, and tenant scoping without a real Mem9 server.
type layerStub struct {
	srv          *httptest.Server
	lastAPIKey   string
	lastAddMode  string
	lastAddSess  string
	lastRecallQ  string
	lastRecallSx string
	recallItems  []layerStubItem
}

type layerStubItem struct {
	id      string
	content string
	score   float64
}

func newLayerStub(t *testing.T) *layerStub {
	t.Helper()
	s := &layerStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1alpha2/mem9s/memories", s.handle)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *layerStub) handle(w http.ResponseWriter, r *http.Request) {
	s.lastAPIKey = r.Header.Get("X-API-Key")
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Mode      string `json:"mode"`
			SessionID string `json:"session_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.lastAddMode = req.Mode
		s.lastAddSess = req.SessionID
		// Mem9's documented behavior: 202 Accepted, no object echoed.
		w.WriteHeader(http.StatusAccepted)
	case http.MethodGet:
		s.lastRecallQ = r.URL.Query().Get("q")
		s.lastRecallSx = r.URL.Query().Get("session_id")
		type item struct {
			ID        string  `json:"id"`
			Content   string  `json:"content"`
			Relevance float64 `json:"relevance"`
		}
		out := make([]item, 0, len(s.recallItems))
		for _, it := range s.recallItems {
			out = append(out, item{ID: it.id, Content: it.content, Relevance: it.score})
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func newLayerBackend(s *layerStub, tenancy mem9.Tenancy) *mem9.Backend {
	return mem9.New(mem9.Config{
		BaseURL:    s.srv.URL,
		APIVersion: "v1alpha2",
		Tenancy:    tenancy,
		CredentialResolver: func(context.Context) (string, error) {
			return testAPIKey, nil
		},
		HTTPClient:  s.srv.Client(),
		BackendName: "test-mem9-layer",
	})
}

// TestMem9Layer_CapabilitiesAdvertisesMemoryLayer pins that the Mem9
// backend advertises the MemoryLayer capability so the tool routes
// add/recall here instead of refusing capability_unsupported.
func TestMem9Layer_CapabilitiesAdvertisesMemoryLayer(t *testing.T) {
	s := newLayerStub(t)
	b := newLayerBackend(s, mem9.Tenancy{})
	caps := b.Capabilities()
	if !caps.MemoryLayer {
		t.Error("Capabilities().MemoryLayer = false, want true")
	}
	if !caps.KV || !caps.VectorSearch || !caps.Stats {
		t.Errorf("Capabilities() = %+v, want all of KV/VectorSearch/Stats true (Mem9 serves both shapes)", caps)
	}
}

// TestMem9Layer_AddSendsSmartModeWhenInfer pins mode="smart" for Infer=true,
// that the write reports AddPending (Mem9's write is 202-async), and that
// the resolved X-API-Key reaches the server.
func TestMem9Layer_AddSendsSmartModeWhenInfer(t *testing.T) {
	s := newLayerStub(t)
	b := newLayerBackend(s, mem9.Tenancy{})

	res, err := b.Add(context.Background(), store.MemoryScopeAgent, "qa",
		[]memory.LayerMessage{{Role: "user", Content: "I prefer dark mode"}},
		memory.AddOptions{Infer: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if s.lastAddMode != "smart" {
		t.Errorf("write mode = %q, want smart (Infer=true)", s.lastAddMode)
	}
	if res.Status != memory.AddPending {
		t.Errorf("AddResult.Status = %v, want AddPending (Mem9 write is async)", res.Status)
	}
	if s.lastAPIKey != testAPIKey {
		t.Errorf("server saw X-API-Key %q, want %q", s.lastAPIKey, testAPIKey)
	}
}

// TestMem9Layer_AddSendsRawModeWhenNotInfer pins mode="raw" for Infer=false
// (verbatim storage, no LLM extraction).
func TestMem9Layer_AddSendsRawModeWhenNotInfer(t *testing.T) {
	s := newLayerStub(t)
	b := newLayerBackend(s, mem9.Tenancy{})

	if _, err := b.Add(context.Background(), store.MemoryScopeAgent, "qa",
		[]memory.LayerMessage{{Role: "user", Content: "x"}},
		memory.AddOptions{Infer: false}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if s.lastAddMode != "raw" {
		t.Errorf("write mode = %q, want raw (Infer=false)", s.lastAddMode)
	}
}

// TestMem9Layer_AddScopesSessionToTenant pins that the tenant prefix +
// loomcycle scope land in session_id, so a shared_key_with_prefix tenant
// cannot write into another tenant's space.
func TestMem9Layer_AddScopesSessionToTenant(t *testing.T) {
	s := newLayerStub(t)
	b := newLayerBackend(s, mem9.Tenancy{KeyPrefix: "tenant-b::"})

	if _, err := b.Add(context.Background(), store.MemoryScopeUser, "bob",
		[]memory.LayerMessage{{Role: "user", Content: "x"}},
		memory.AddOptions{Infer: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !strings.HasPrefix(s.lastAddSess, "tenant-b::") {
		t.Errorf("session_id = %q, want tenant-b:: prefix (tenant isolation)", s.lastAddSess)
	}
	if !strings.Contains(s.lastAddSess, "user/bob/") {
		t.Errorf("session_id = %q, want it to namespace scope user/bob/", s.lastAddSess)
	}
}

// TestMem9Layer_RecallMapsFactsWithTopKAndThreshold pins the recall mapping:
// memory objects → RecallFacts, the Threshold relevance floor filters
// client-side, and the result is trimmed to TopK.
func TestMem9Layer_RecallMapsFactsWithTopKAndThreshold(t *testing.T) {
	s := newLayerStub(t)
	b := newLayerBackend(s, mem9.Tenancy{})

	// Four candidates; one below threshold should be dropped, then trim to 2.
	s.recallItems = []layerStubItem{
		{id: "u1", content: "fact A", score: 0.95},
		{id: "u2", content: "fact B", score: 0.80},
		{id: "u3", content: "fact C", score: 0.60},
		{id: "u4", content: "fact D", score: 0.20}, // below threshold 0.5
	}

	res, err := b.Recall(context.Background(), store.MemoryScopeUser, "bob",
		memory.RecallQuery{Query: "preferences", TopK: 2, Threshold: 0.5})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if s.lastRecallQ != "preferences" {
		t.Errorf("recall q = %q, want %q", s.lastRecallQ, "preferences")
	}
	if len(res.Facts) != 2 {
		t.Fatalf("Recall returned %d facts, want 2 (threshold drops u4, TopK trims to 2)", len(res.Facts))
	}
	if res.Facts[0].ID != "u1" || res.Facts[0].Memory != "fact A" || res.Facts[0].Score != 0.95 {
		t.Errorf("Facts[0] = %+v, want {u1, fact A, 0.95}", res.Facts[0])
	}
	for _, f := range res.Facts {
		if f.Score < 0.5 {
			t.Errorf("fact %q has score %v below threshold 0.5 — filter failed", f.ID, f.Score)
		}
	}
}

// TestMem9Layer_RecallScopesQueryToTenant pins that the recall query carries
// the tenant prefix (shared_key_with_prefix), mirroring KV Search isolation.
func TestMem9Layer_RecallScopesQueryToTenant(t *testing.T) {
	s := newLayerStub(t)
	b := newLayerBackend(s, mem9.Tenancy{KeyPrefix: "tenant-b::"})

	if _, err := b.Recall(context.Background(), store.MemoryScopeUser, "bob",
		memory.RecallQuery{Query: "q", TopK: 5}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !strings.HasPrefix(s.lastRecallSx, "tenant-b::") {
		t.Errorf("recall session_id = %q, want tenant-b:: prefix", s.lastRecallSx)
	}
}

// TestMem9Layer_CredentialNeverInError pins that a resolver failure fails
// the op without reaching the server with a credential and without leaking
// the (would-be) key into the error — same posture as the KV path.
func TestMem9Layer_CredentialNeverInError(t *testing.T) {
	s := newLayerStub(t)
	b := mem9.New(mem9.Config{
		BaseURL:    s.srv.URL,
		APIVersion: "v1alpha2",
		HTTPClient: s.srv.Client(),
		CredentialResolver: func(context.Context) (string, error) {
			return "", errResolve
		},
	})
	_, err := b.Recall(context.Background(), store.MemoryScopeAgent, "qa",
		memory.RecallQuery{Query: "q", TopK: 3})
	if err == nil {
		t.Fatal("Recall with failing resolver: want error, got nil")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Errorf("error leaked a credential: %v", err)
	}
	if s.lastAPIKey != "" {
		t.Errorf("server saw an API key %q despite resolver failure — unauthenticated call leaked", s.lastAPIKey)
	}
}
