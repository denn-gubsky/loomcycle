package builtin

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// WebFetch fetches a URL via GET and returns the response with HTML stripped
// to plain text. It's a deliberately thin wrapper over HTTP — the only
// reason it's a separate tool is the model surface: most agents are taught
// to reach for WebFetch when they want a page's text and HTTP when they
// want the raw response. Same SSRF defences (allowlist + private-IP
// block + redirect re-validation) flow through unchanged.
type WebFetch struct {
	HTTP            *HTTP
	MaxOutputBytes  int64 // optional override; default 256 KiB
	AllowPrivateIPs bool  // tests only
}

func (f *WebFetch) Name() string        { return "WebFetch" }
func (f *WebFetch) Description() string { return "Fetch a URL via GET and return text-extracted body." }

func (f *WebFetch) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "Absolute URL (http or https). Host must be allowlisted."}
		},
		"required": ["url"]
	}`)
}

func (f *WebFetch) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.Result{Text: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if f.HTTP == nil {
		return tools.Result{Text: "WebFetch is not configured (HTTP backend missing)", IsError: true}, nil
	}
	res, err := f.HTTP.do(ctx, "GET", args.URL, nil, "")
	if err != nil || res.IsError {
		return res, err
	}
	// res.Text starts with status + headers + blank line + body. Split on
	// the first blank line to keep only body text for extraction.
	body := res.Text
	if i := strings.Index(body, "\n\n"); i >= 0 {
		body = body[i+2:]
	}
	out := stripHTML(body)
	max := f.MaxOutputBytes
	if max == 0 {
		max = 256 * 1024
	}
	if int64(len(out)) > max {
		out = out[:max] + "\n[truncated]"
	}
	return tools.Result{Text: out}, nil
}

// htmlTagRe matches a single HTML tag (open, close, or self-closing).
// Naive but sufficient for the "give me the gist of this page" use
// case the model needs. Rendering parity with a browser is explicitly
// not a goal.
var htmlTagRe = regexp.MustCompile(`<[^>]*>`)
var whitespaceRe = regexp.MustCompile(`[ \t]+`)
var manyNewlinesRe = regexp.MustCompile(`\n{3,}`)
var scriptStyleRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)

// stripHTML reduces HTML to plain text. Removes script/style blocks
// in their entirety (their contents would be noise), then strips tags.
// Collapses runs of whitespace and three-or-more newlines.
func stripHTML(s string) string {
	s = scriptStyleRe.ReplaceAllString(s, "")
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
