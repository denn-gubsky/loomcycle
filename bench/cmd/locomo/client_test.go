package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// clearBearerEnv unsets every name bearer() consults. Without it a test
// asserting "no bearer" passes or fails according to what the developer running
// it happens to export — this suite was written on a machine with a real
// LOCOMO_BENCH_TENANT_TOKEN in the environment, and it caught exactly that.
func clearBearerEnv(t *testing.T) {
	t.Helper()
	for _, env := range []string{"LOOMCYCLE_LOCOMO_TOKEN", "LOCOMO_BENCH_TENANT_TOKEN", "LOOMCYCLE_AUTH_TOKEN"} {
		t.Setenv(env, "")
	}
}

func meServer(t *testing.T, tenant string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/_me" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(Identity{
			TenantID: tenant, Subject: "bench", Scopes: []string{"substrate:tenant"},
		})
	}))
}

// TestConnect_RefusesTheDefaultTenantSoTheCorpusCannotLandInRealMemory is the
// isolation guard. ~6,000 synthetic conversational rows written into the tenant
// an operator actually uses is not a mistake you notice until recall degrades,
// so the harness asks who it is before it writes anything.
func TestConnect_RefusesTheDefaultTenantSoTheCorpusCannotLandInRealMemory(t *testing.T) {
	srv := meServer(t, "")
	defer srv.Close()
	clearBearerEnv(t)
	t.Setenv("LOOMCYCLE_LOCOMO_TOKEN", "tok")

	var out bytes.Buffer
	_, _, err := connect(context.Background(), options{instance: srv.URL, timeout: time.Second}, &out)
	if err == nil {
		t.Fatal("connect accepted a bearer resolving to the default/legacy tenant")
	}
	if !strings.Contains(err.Error(), "dedicated tenant") {
		t.Errorf("error does not tell the operator how to fix it: %v", err)
	}
}

func TestConnect_AcceptsADedicatedTenant(t *testing.T) {
	srv := meServer(t, "locomo-bench")
	defer srv.Close()
	clearBearerEnv(t)
	t.Setenv("LOOMCYCLE_LOCOMO_TOKEN", "tok")

	var out bytes.Buffer
	_, id, err := connect(context.Background(), options{instance: srv.URL, timeout: time.Second}, &out)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if id.TenantID != "locomo-bench" {
		t.Errorf("TenantID = %q, want locomo-bench", id.TenantID)
	}
	// The resolved principal is printed, so a run's output records which tenant
	// received the corpus.
	if !strings.Contains(out.String(), "locomo-bench") {
		t.Errorf("connect did not report the tenant it resolved: %q", out.String())
	}
}

// TestConnect_AllowSharedTenantIsAnExplicitOverride — the guard must be
// escapable, or an operator running single-tenant cannot use the harness at all.
func TestConnect_AllowSharedTenantIsAnExplicitOverride(t *testing.T) {
	srv := meServer(t, "")
	defer srv.Close()
	clearBearerEnv(t)
	t.Setenv("LOOMCYCLE_LOCOMO_TOKEN", "tok")

	var out bytes.Buffer
	if _, _, err := connect(context.Background(),
		options{instance: srv.URL, timeout: time.Second, allowSharedTenant: true}, &out); err != nil {
		t.Fatalf("connect with -allow-shared-tenant: %v", err)
	}
}

func TestConnect_RefusesWithNoBearerRatherThanCallingUnauthenticated(t *testing.T) {
	clearBearerEnv(t)
	var out bytes.Buffer
	if _, _, err := connect(context.Background(), options{instance: "http://127.0.0.1:1", timeout: time.Second}, &out); err == nil ||
		!strings.Contains(err.Error(), "no bearer") {
		t.Fatalf("err = %v, want a missing-bearer refusal", err)
	}
}

func TestBearer_PrefersTheBenchmarkSpecificToken(t *testing.T) {
	clearBearerEnv(t)
	t.Setenv("LOOMCYCLE_AUTH_TOKEN", "operator")
	t.Setenv("LOOMCYCLE_LOCOMO_TOKEN", "bench")
	if got := bearer(); got != "bench" {
		t.Errorf("bearer() = %q, want the benchmark token so the operator bearer is not reused", got)
	}
}

// TestBearer_AcceptsTheOperatorsNaturalNameForTheBenchToken — the name an
// operator reaches for when minting a dedicated bench bearer.
func TestBearer_AcceptsTheOperatorsNaturalNameForTheBenchToken(t *testing.T) {
	clearBearerEnv(t)
	t.Setenv("LOOMCYCLE_AUTH_TOKEN", "operator")
	t.Setenv("LOCOMO_BENCH_TENANT_TOKEN", "bench")
	if got := bearer(); got != "bench" {
		t.Errorf("bearer() = %q, want the bench token to win over the operator bearer", got)
	}
}

// TestPutEntry_EscapesTheColonInADiaIDKeyAndSendsAJSONValue — dia_ids contain a
// colon ("D1:3") and go into the URL path, and the endpoint requires the value
// to be valid JSON.
func TestPutEntry_EscapesTheColonInADiaIDKeyAndSendsAJSONValue(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody putEntryBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_ = json.NewEncoder(w).Encode(putEntryResponse{Embedded: true})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", time.Second)
	resp, err := c.PutEntry(context.Background(), "agent", "locomo-conv-1", "D1:3", "Ada: hello", true)
	if err != nil {
		t.Fatalf("PutEntry: %v", err)
	}
	if !resp.Embedded {
		t.Error("Embedded = false, want the server's ack echoed")
	}
	if !strings.Contains(gotPath, "locomo-conv-1") || !strings.Contains(gotPath, "D1") {
		t.Errorf("path = %q, want the scope_id and dia_id in it", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	var value string
	if err := json.Unmarshal(gotBody.Value, &value); err != nil {
		t.Fatalf("value is not a JSON string: %v", err)
	}
	if value != "Ada: hello" {
		t.Errorf("value = %q, want the turn body", value)
	}
	if !gotBody.Embed {
		t.Error("embed flag not sent; the row would be stored without a vector")
	}
}

func TestSearch_ReturnsKeysInRankOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req searchRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ScopeID != "locomo-conv-1" || req.TopK != 3 {
			t.Errorf("request = %+v, want scope_id/top_k threaded through", req)
		}
		_, _ = w.Write([]byte(`{"entries":[{"key":"D1:2","score":0.9},{"key":"D1:1","score":0.7}],` +
			`"query_embedding_dim":1024}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", time.Second)
	keys, dim, err := c.Search(context.Background(), "agent", "locomo-conv-1", "what pet?", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if strings.Join(keys, ",") != "D1:2,D1:1" {
		t.Errorf("keys = %v, want rank order preserved", keys)
	}
	if dim != 1024 {
		t.Errorf("dim = %d, want 1024", dim)
	}
}

func TestClient_SurfacesAnErrorStatusWithItsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"vector_unsupported","message":"no vector support"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", time.Second)
	_, _, err := c.Search(context.Background(), "agent", "s", "q", 5)
	if err == nil || !strings.Contains(err.Error(), "vector_unsupported") {
		t.Fatalf("err = %v, want the server's diagnosis surfaced", err)
	}
}

func TestParseCategories_RejectsGarbageAndRequiresOne(t *testing.T) {
	if _, err := parseCategories("1,x"); err == nil {
		t.Error("parseCategories accepted a non-numeric category")
	}
	if _, err := parseCategories(" , "); err == nil {
		t.Error("parseCategories accepted an empty set")
	}
	got, err := parseCategories(" 4, 1 ,2")
	if err != nil {
		t.Fatalf("parseCategories: %v", err)
	}
	if len(got) != 3 || got[0] != 4 {
		t.Errorf("got %v, want the order preserved as given", got)
	}
}
