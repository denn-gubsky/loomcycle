package reranker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// The wire shape of the one request a decision rerank sends, as the endpoint
// double in decision_test.go decodes it. The reranker no longer builds the
// request itself (the shared decision driver does), so these describe what must
// arrive, not what any production type holds.
const decisionPath = "/v1/systemone"

type systemOneRequest struct {
	Model     string                       `json:"model"`
	State     map[string]string            `json:"state"`
	Questions map[string]systemOneQuestion `json:"questions"`
}

type systemOneQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

// TestDecision_TheRequestIsByteForByteTheMeasuredOne — the rerank's numbers were
// measured against one exact request. This is that request, to the byte, so a
// change to how it is built (the shared decision driver builds it now) cannot
// alter what the model is asked without failing here.
func TestDecision_TheRequestIsByteForByteTheMeasuredOne(t *testing.T) {
	var got, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got, contentType = string(raw), r.Header.Get("Content-Type")
		_, _ = io.WriteString(w, `{"answers":{"best":{"type":"choice","probabilities":{"A":0.1,"B":0.9}}}}`)
	}))
	defer srv.Close()
	_, rep := newDecision(t, srv.URL, config.RerankerConfig{}).Rank(context.Background(), `which <one> & "why"?`, []string{"alpha one", "bravo two"}, 5)
	if !rep.Applied {
		t.Fatalf("report = %+v, want applied", rep)
	}
	// encoding/json writes <, > and & as escapes; esc spells one without this
	// file containing the escape itself.
	esc := func(hex string) string { return string(rune('\\')) + "u" + hex }
	want := `{"model":"nimble","state":{"question":"which ` + esc("003c") + `one` + esc("003e") + ` ` + esc("0026") + ` \"why\"?"},` +
		`"questions":{"best":{"type":"choice","instructions":"Which passage best answers the question?",` +
		`"criteria":{"A":"alpha","B":"bravo"}}}}`
	if got != want {
		t.Errorf("request body\n got: %s\nwant: %s", got, want)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
}
