package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/tools"
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

// webFetchServe serves page as text/html and returns a WebFetch pointed at it.
func webFetchServe(t *testing.T, page string) (*WebFetch, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, page)
	}))
	t.Cleanup(srv.Close)
	return &WebFetch{HTTP: &HTTP{HostAllowlist: []string{mustHost(t, srv.URL)}, AllowPrivateIPs: true}}, srv.URL
}

func webFetchRun(t *testing.T, f *WebFetch, ctx context.Context, input map[string]any) string {
	t.Helper()
	body, _ := json.Marshal(input)
	res, err := f.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Text)
	}
	return res.Text
}

// cssRule is a line of the kind a modern page inlines by the hundred KiB.
const cssRule = "@font-face{font-family:Inter;src:url(/f/inter.woff2) format(\"woff2\")}\n"

// Regression: a page whose first 256 KiB is an inline <style> came back as CSS.
// The raw body was cut at 256 KiB BEFORE stripping, inside the style block, so
// its closing tag never arrived and the CSS survived as text while the article
// after it was never read.
func TestWebFetch_ReturnsArticleTextAfterLargeInlineStyle(t *testing.T) {
	css := strings.Repeat(cssRule, (300<<10)/len(cssRule)+1)
	page := "<html><head><style>" + css + "</style></head><body>" +
		"<p>The reviewer measured 42 frames per second.</p></body></html>"
	f, url := webFetchServe(t, page)
	out := webFetchRun(t, f, context.Background(), map[string]any{"url": url})
	if strings.Contains(out, "@font-face") {
		t.Errorf("CSS survived stripping (%d bytes returned), starts %q", len(out), out[:min(len(out), 120)])
	}
	if !strings.Contains(out, "42 frames per second") {
		t.Errorf("article text missing from %d-byte result", len(out))
	}
}

// A page longer than the raw read is cut inside its trailing <style>; the
// opened-but-never-closed block must be dropped, not returned as text, and the
// result must still say it was cut.
func TestWebFetch_DropsUnterminatedTrailingStyleAndMarksTheCut(t *testing.T) {
	css := strings.Repeat(cssRule, (webFetchMaxRawBytes+(64<<10))/len(cssRule))
	page := "<html><body><p>Intro paragraph.</p><style>" + css + "</style></body></html>"
	f, url := webFetchServe(t, page)
	out := webFetchRun(t, f, context.Background(), map[string]any{"url": url})
	if strings.Contains(out, "@font-face") {
		t.Errorf("unterminated style survived as text; starts %q", out[:min(len(out), 120)])
	}
	if !strings.HasPrefix(out, "Intro paragraph.") || !strings.HasSuffix(out, "[truncated]") {
		t.Errorf("want the intro then a [truncated] marker, got %q", out[:min(len(out), 200)])
	}
}

func TestStripHTML_DropsUnterminatedScriptOrStyle(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"unterminated style", "before<style>x{color:red}", "before"},
		{"unterminated script", "before<script type=\"x\">evil(", "before"},
		{"opening tag itself cut", "before<style media=\"scr", "before"},
		{"closed block then unterminated", "a<style>x{}</style>b<script>y(", "ab"},
		{"custom element is not a style", "a<style-guide>keep</style-guide>b", "akeepb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripHTML(tc.input); got != tc.want {
				t.Errorf("stripHTML(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// textPage is an HTML page carrying n bytes of plain ASCII text.
func textPage(n int) string {
	return "<html><body><p>" + strings.Repeat("word ", n/5) + "</p></body></html>"
}

// cutBody returns the text before the [truncated] marker, failing if absent.
func cutBody(t *testing.T, out string) string {
	t.Helper()
	body, ok := strings.CutSuffix(out, "\n[truncated]")
	if !ok {
		t.Fatalf("result not marked [truncated]; %d bytes, tail %q", len(out), out[max(0, len(out)-40):])
	}
	return body
}

// Regression: the output cut was out[:max], which can split a multi-byte
// character and hand the model invalid UTF-8.
func TestWebFetch_CutsOnARuneBoundary(t *testing.T) {
	f, url := webFetchServe(t, "<p>"+strings.Repeat("é", 1000)+"</p>")
	f.MaxOutputBytes = 101 // é is 2 bytes: 101 falls inside one
	body := cutBody(t, webFetchRun(t, f, context.Background(), map[string]any{"url": url}))
	if !utf8.ValidString(body) || len(body) != 100 {
		t.Errorf("want 100 bytes of valid UTF-8, got %d bytes (valid=%v)", len(body), utf8.ValidString(body))
	}

	f.MaxOutputBytes = 0
	body = cutBody(t, webFetchRun(t, f, context.Background(), map[string]any{"url": url, "max_chars": 51}))
	if !utf8.ValidString(body) || len(body) != 50 {
		t.Errorf("max_chars=51: want 50 bytes of valid UTF-8, got %d bytes (valid=%v)", len(body), utf8.ValidString(body))
	}
}

// A fixed 256 KiB (~64k tokens) overflowed a 32k-token local window on its own.
// With a window known, the default is a quarter of it in tokens at ~4
// characters a token — i.e. the window in characters; without one it stays at
// 256 KiB.
func TestWebFetch_DefaultCapFollowsTheRunsContextWindow(t *testing.T) {
	f, url := webFetchServe(t, textPage(300<<10))

	ctx := tools.WithMaxContextTokens(context.Background(), 32768)
	if got := len(cutBody(t, webFetchRun(t, f, ctx, map[string]any{"url": url}))); got != 32768 {
		t.Errorf("32k window: returned %d bytes, want 32768", got)
	}
	if got := len(cutBody(t, webFetchRun(t, f, context.Background(), map[string]any{"url": url}))); got != 256<<10 {
		t.Errorf("no window: returned %d bytes, want %d", got, 256<<10)
	}
	// A window whose quarter is past the ceiling is held to the ceiling.
	ctx = tools.WithMaxContextTokens(context.Background(), 1_000_000)
	if got := len(cutBody(t, webFetchRun(t, f, ctx, map[string]any{"url": url}))); got != 256<<10 {
		t.Errorf("1M window: returned %d bytes, want the %d ceiling", got, 256<<10)
	}
}

// An explicit max_chars overrides the window default in either direction but
// is clamped to [1, ceiling]; the operator's MaxOutputBytes is the ceiling.
func TestWebFetch_ExplicitMaxCharsIsClampedToTheCeiling(t *testing.T) {
	f, url := webFetchServe(t, textPage(300<<10))
	ctx := tools.WithMaxContextTokens(context.Background(), 32768)
	cases := []struct {
		name     string
		maxChars int
		ceiling  int64
		want     int
	}{
		{"asks for more than the window", 100_000, 0, 100_000},
		{"asks for less than the window", 1000, 0, 1000},
		{"asks past the ceiling", 10_000_000, 0, 256 << 10},
		{"zero is raised to one", 0, 0, 1},
		{"operator ceiling wins", 5000, 2000, 2000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.MaxOutputBytes = tc.ceiling
			out := webFetchRun(t, f, ctx, map[string]any{"url": url, "max_chars": tc.maxChars})
			if got := len(cutBody(t, out)); got != tc.want {
				t.Errorf("max_chars=%d: returned %d bytes, want %d", tc.maxChars, got, tc.want)
			}
		})
	}
}

// Text that fits is returned whole, with no marker.
func TestWebFetch_ShortPageIsNotMarkedTruncated(t *testing.T) {
	f, url := webFetchServe(t, "<p>short page</p>")
	out := webFetchRun(t, f, tools.WithMaxContextTokens(context.Background(), 32768), map[string]any{"url": url})
	if out != "short page" {
		t.Errorf("got %q, want %q", out, "short page")
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
