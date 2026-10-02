package builtin

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// webFetchMaxRawBytes bounds how much of a page WebFetch reads before it strips
// it — well above what it returns, since markup and inline CSS/JS are most of
// a modern page's bytes.
const webFetchMaxRawBytes = 2 << 20

// webFetchMaxChars is the most text one call returns, whatever the run's window
// or the caller's max_chars, unless the operator set MaxOutputBytes.
const webFetchMaxChars = 256 << 10

// WebFetch fetches a URL via GET and returns the response with HTML stripped
// to plain text. It's a deliberately thin wrapper over HTTP — the only
// reason it's a separate tool is the model surface: most agents are taught
// to reach for WebFetch when they want a page's text and HTTP when they
// want the raw response. Same SSRF defences (allowlist + private-IP
// block + redirect re-validation) flow through unchanged.
type WebFetch struct {
	HTTP           *HTTP
	MaxOutputBytes int64 // optional operator ceiling on returned text; default 256 KiB
}

func (f *WebFetch) Name() string { return "WebFetch" }
func (f *WebFetch) Description() string {
	return "GET one http(s) URL and return its body with HTML stripped to readable text. " +
		"Takes `url` (absolute; the host must be on the operator's allowlist). " +
		"Returns extracted text, cut by default to about a quarter of this run's context window and never more than 256 KiB, ending in [truncated] when cut; " +
		"pass `max_chars` to ask for less, or for more up to that 256 KiB ceiling. " +
		"Use this when you already have a URL and want the page's content — documentation, an article, a JSON endpoint you only need to read. " +
		"Do NOT use it to FIND pages: use WebSearch to discover URLs first. " +
		"Do NOT use it when you need a method other than GET, custom headers, or a request body — that is the HTTP tool. " +
		"A host that is not allowlisted is refused, not fetched; extraction is best-effort, so a heavily scripted page may yield little text."
}

func (f *WebFetch) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "Absolute URL (http or https). Host must be allowlisted."},
			"max_chars": {"type": "integer", "minimum": 1, "description": "Most text to return, counted in bytes of UTF-8 (a non-ASCII character counts more than once). Default: about a quarter of this run's context window. Never more than 262144 (256 KiB)."}
		},
		"required": ["url"]
	}`)
}

func (f *WebFetch) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args struct {
		URL      string `json:"url"`
		MaxChars *int   `json:"max_chars"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.Result{Text: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if f.HTTP == nil {
		return tools.Result{Text: "WebFetch is not configured (HTTP backend missing)", IsError: true}, nil
	}
	// The raw read is larger than the text returned: a modern page's first
	// few hundred KiB are often inline CSS, so cutting BEFORE stripping returns
	// the CSS and loses the article. An explicit HTTP.MaxResponseBytes is the
	// operator's own bound on what is read off the wire and still wins.
	rawCap := int64(webFetchMaxRawBytes)
	if f.HTTP.MaxResponseBytes > 0 {
		rawCap = f.HTTP.MaxResponseBytes
	}
	res, err := f.HTTP.do(ctx, "GET", args.URL, nil, "", rawCap)
	if err != nil || res.IsError {
		return res, err
	}
	// res.Text starts with status + headers + blank line + body. Split on
	// the first blank line to keep only body text for extraction.
	body := res.Text
	if i := strings.Index(body, "\n\n"); i >= 0 {
		body = body[i+2:]
	}
	// Take do's own cut marker off before stripping: if the cut landed inside
	// a <script>/<style> block, stripping drops everything after its opening
	// tag, the marker with it, and the page would read as complete.
	rawCut := false
	if m := responseTruncatedMarker(rawCap); strings.HasSuffix(body, m) {
		body = strings.TrimSuffix(body, m)
		rawCut = true
	}
	out, cut := cutOnRune(stripHTML(body), f.outputLimit(ctx, args.MaxChars))
	if cut || rawCut {
		out += "\n[truncated]"
	}
	return tools.Result{Text: out}, nil
}

// outputLimit is how many bytes of stripped text this call returns. A fixed
// 256 KiB is ~64k tokens — twice a 32k local window on its own — so the
// default follows the run's window: a quarter of max_context_tokens at ~4
// characters a token, i.e. max_context_tokens characters. The window read is
// the run's CONFIGURED cap (per-run > per-agent), the one value the loop stamps
// for tools; with none configured the default stays at the ceiling.
func (f *WebFetch) outputLimit(ctx context.Context, maxChars *int) int {
	ceiling := webFetchMaxChars
	if f.MaxOutputBytes > 0 {
		ceiling = int(f.MaxOutputBytes)
	}
	if maxChars != nil {
		return min(max(*maxChars, 1), ceiling)
	}
	if n := tools.MaxContextTokens(ctx); n > 0 {
		return min(n, ceiling)
	}
	return ceiling
}

// cutOnRune returns at most n bytes of s, backing off to a rune boundary so a
// multi-byte character is never split into invalid UTF-8.
func cutOnRune(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// htmlTagRe matches a single HTML tag (open, close, or self-closing).
// Naive but sufficient for the "give me the gist of this page" use
// case the model needs. Rendering parity with a browser is explicitly
// not a goal.
var htmlTagRe = regexp.MustCompile(`<[^>]*>`)
var whitespaceRe = regexp.MustCompile(`[ \t]+`)
var manyNewlinesRe = regexp.MustCompile(`\n{3,}`)
var scriptStyleRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)

// openScriptStyleRe finds a <script>/<style> opening tag (possibly itself cut
// off at end of input). The name must end at whitespace, '>' or end of input,
// so a custom element such as <style-guide> is not mistaken for one.
var openScriptStyleRe = regexp.MustCompile(`(?i)<(?:script|style)(?:[\s>]|$)`)

// stripHTML reduces HTML to plain text. Removes script/style blocks
// in their entirety (their contents would be noise), then strips tags.
// Collapses runs of whitespace and three-or-more newlines.
func stripHTML(s string) string {
	s = scriptStyleRe.ReplaceAllString(s, "")
	// A block whose closing tag never arrived — a body cut mid-<style> — is
	// not matched above, and its CSS/JS would survive as text. Any opening tag
	// still present has no closing tag after it (scriptStyleRe matches an
	// opening tag up to the first closing tag anywhere after it), so it runs
	// to end of input: drop from there.
	if loc := openScriptStyleRe.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	s = htmlTagRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = whitespaceRe.ReplaceAllString(s, " ")
	s = manyNewlinesRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
