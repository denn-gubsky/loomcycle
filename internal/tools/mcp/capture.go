package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// renderResult turns a tool result's content blocks into the text the model
// sees (RFC FF). Text is kept as it came. A file — an image, an audio clip,
// a resource's blob — is written to the run tree's inbox and stands in the
// text as a one-line stub naming it; it used to be dropped without a word,
// so an image-only result reached the model as an empty string. A resource
// that carries text is text, and a resource link is shown as one line the
// agent can act on.
//
// args are the call's arguments: a file the result does not name is named
// from them (callArgName), and only then after the tool.
func renderResult(ctx context.Context, server, tool string, args json.RawMessage, res *CallToolResult, pol ResultPolicy) string {
	if res == nil {
		return ""
	}
	origin := "mcp:" + server + "/" + tool
	fallback := tool
	if n := inbox.SanitizeName(callArgName(args)); n != "" {
		fallback = n
	}
	// Joined by JoinTextContent's rule, so a text-only result reads exactly
	// as it always did.
	var out strings.Builder
	for i, b := range res.Content {
		if i > 0 && out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(renderBlock(ctx, origin, fallback, b, pol))
	}
	return out.String()
}

// renderBlock is one block's text. fallback names a file the block does not.
func renderBlock(ctx context.Context, origin, fallback string, b ContentBlock, pol ResultPolicy) string {
	switch b.Type {
	case "text":
		if pol.CaptureBase64 {
			return captureTextBase64(ctx, origin, fallback, b.Text, pol.minBytes())
		}
		return b.Text
	case "image", "audio":
		var blk struct {
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		}
		if err := json.Unmarshal(b.Raw, &blk); err != nil {
			return fmt.Sprintf("[file not kept: the %s block could not be read: %v · from %s]", b.Type, err, origin)
		}
		it := inbox.Item{MediaType: blk.MimeType, Origin: origin, Fallback: fallback}
		if b.Type == "audio" {
			it.Dir = inbox.DirDocuments
		}
		return captureBase64(ctx, it, blk.Data)
	case "resource":
		var blk struct {
			Resource struct {
				URI      string  `json:"uri"`
				MimeType string  `json:"mimeType"`
				Text     *string `json:"text"`
				Blob     *string `json:"blob"`
			} `json:"resource"`
		}
		if err := json.Unmarshal(b.Raw, &blk); err != nil {
			return fmt.Sprintf("[resource not shown: it could not be read: %v · from %s]", err, origin)
		}
		r := blk.Resource
		switch {
		case r.Blob != nil:
			return captureBase64(ctx, inbox.Item{Name: r.URI, MediaType: r.MimeType, Origin: origin, Fallback: fallback}, *r.Blob)
		case r.Text != nil:
			if r.URI == "" {
				return *r.Text
			}
			return "[resource: " + r.URI + "]\n" + *r.Text
		}
		return "[resource: " + r.URI + " — no content]"
	case "resource_link":
		var blk struct {
			URI         string `json:"uri"`
			Name        string `json:"name"`
			MimeType    string `json:"mimeType"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(b.Raw, &blk); err != nil {
			return fmt.Sprintf("[link not shown: it could not be read: %v · from %s]", err, origin)
		}
		fields := []string{blk.URI}
		if blk.Name != "" {
			fields = append([]string{blk.Name}, fields...)
		}
		if blk.MimeType != "" {
			fields = append(fields, blk.MimeType)
		}
		if blk.Description != "" {
			fields = append(fields, blk.Description)
		}
		return "[link: " + strings.Join(fields, " · ") + "]"
	}
	return fmt.Sprintf("[content of type %q not shown · from %s]", b.Type, origin)
}

// callArgNameKeys are the call arguments that name the file a download tool
// returns, most specific first: a path may name the folder the file is in,
// so it ranks last. Matched without regard to case.
var callArgNameKeys = []string{"filename", "file_name", "name", "path"}

// callArgName is the file name a call's arguments give, "" when none does.
// Only a top-level string is taken; the caller keeps its last segment,
// sanitised, as it does for any name.
func callArgName(args json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	for _, want := range callArgNameKeys {
		for k, v := range m {
			if !strings.EqualFold(k, want) {
				continue
			}
			var s string
			// A name is short; anything longer is not one.
			if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" && len(s) <= 1024 {
				return s
			}
		}
	}
	return ""
}

// captureBase64 decodes a block's base64 payload and keeps it in the inbox.
// MCP says standard base64; the URL alphabet and missing padding are taken
// too, because servers send both.
func captureBase64(ctx context.Context, it inbox.Item, data string) string {
	data = strings.TrimSpace(data)
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err = enc.DecodeString(data); err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Sprintf("[file not kept: its base64 did not decode · from %s]", it.Origin)
	}
	return inbox.Capture(ctx, it, raw).Stub
}
