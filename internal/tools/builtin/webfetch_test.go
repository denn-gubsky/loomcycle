package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebFetchRefusesWhenHTTPMissing(t *testing.T) {
	f := &WebFetch{}
	res, err := f.Execute(context.Background(), json.RawMessage(`{"url":"https://example.com/"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("expected error when HTTP backend missing, got %q", res.Text)
	}
}

// SSRF defence flows through unchanged: rejection happens at the HTTP
// layer, WebFetch just surfaces it.
func TestWebFetchInheritsAllowlist(t *testing.T) {
	f := &WebFetch{HTTP: &HTTP{HostAllowlist: []string{"good.example"}}}
	res, err := f.Execute(context.Background(), json.RawMessage(`{"url":"https://attacker.example/"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "allowlist") {
		t.Errorf("expected allowlist rejection, got %q", res.Text)
	}
}

func TestWebFetchExtractsTextFromHTML(t *testing.T) {
	html := `<!DOCTYPE html>
<html>
<head>
<title>Test</title>
<script>var x = 1;</script>
<style>body { color: red; }</style>
</head>
<body>
<h1>Hello   World</h1>
<p>This is a <a href="x">link</a> in a paragraph.</p>
<p>Another&nbsp;paragraph with &amp; entities.</p>
</body>
</html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, html)
	}))
	defer srv.Close()

	f := &WebFetch{HTTP: &HTTP{HostAllowlist: []string{mustHost(t, srv.URL)}, AllowPrivateIPs: true}}
	body, _ := json.Marshal(map[string]string{"url": srv.URL})
	res, err := f.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	out := res.Text
	if strings.Contains(out, "<h1>") || strings.Contains(out, "</p>") {
		t.Errorf("HTML tags survived stripping: %q", out)
	}
	if strings.Contains(out, "var x = 1") {
		t.Errorf("script body leaked through stripping: %q", out)
	}
	if strings.Contains(out, "color: red") {
		t.Errorf("style body leaked through stripping: %q", out)
	}
	if !strings.Contains(out, "Hello World") {
		t.Errorf("text content missing: %q", out)
	}
	if !strings.Contains(out, "& entities") {
		t.Errorf("&amp; not decoded: %q", out)
	}
	if !strings.Contains(out, "Another paragraph") {
		t.Errorf("&nbsp; not decoded to space: %q", out)
	}
}

// Regression: WebFetch splits HTTP's response on the FIRST "\n\n" to
// drop the headers/body separator. A body containing its own blank
// lines must not be over-trimmed by that split — switching to
// LastIndex would do exactly that. Both paragraphs must survive.
func TestWebFetchPreservesBlankLinesInBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "para1\n\npara2")
	}))
	defer srv.Close()

	f := &WebFetch{HTTP: &HTTP{HostAllowlist: []string{mustHost(t, srv.URL)}, AllowPrivateIPs: true}}
	body, _ := json.Marshal(map[string]string{"url": srv.URL})
	res, err := f.Execute(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	if !strings.Contains(res.Text, "para1") || !strings.Contains(res.Text, "para2") {
		t.Errorf("blank-line body over-trimmed: %q", res.Text)
	}
}

func TestStripHTML(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"plain text", "hello", "hello"},
		{"single tag", "<b>hi</b>", "hi"},
		{"script removed entirely", "before<script>evil()</script>after", "beforeafter"},
		{"style removed entirely", "before<style>x{}</style>after", "beforeafter"},
		{"nested tags", "<p><b><i>x</i></b></p>", "x"},
		{"entities", "&lt;x&gt; &amp; &quot;q&quot;", `<x> & "q"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripHTML(tc.input); got != tc.want {
				t.Errorf("stripHTML(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
