package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// WebFetch `assets`: the images a page shows, and the PDFs, Office files and
// images it links to, are side-downloaded into the run's inbox under
// web/<host>/, so an agent that read a page can work with its figures and
// attachments without a second round of fetches. Each one goes out on the
// same host policy as the page: the HTTP tool's host check and guarded
// dialer, with a redirect re-checked on every hop.

const (
	webFetchAssetsDefault = 20
	// webFetchAssetsCeiling bounds assets_max: each asset is a request, made
	// one after another inside one tool call.
	webFetchAssetsCeiling = 100
)

// webAsset is one link the page referenced, fetched or not.
type webAsset struct {
	URL       string `json:"url"`
	Ref       string `json:"ref,omitempty"`
	Size      int64  `json:"size,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Skipped   string `json:"skipped,omitempty"`
}

var (
	// assetTagRe matches an anchor with its content, or an image tag.
	assetTagRe  = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a\s*>|<(img|source)\b([^>]*)>`)
	assetImgRe  = regexp.MustCompile(`(?is)<(img|source)\b([^>]*)>`)
	assetAttrRe = regexp.MustCompile(`(?is)(?:^|\s)(src|srcset|href|alt)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
	// assetTextRe is what an alt or link text may keep: the rewritten link
	// is Markdown, and these would break it.
	assetTextRe = regexp.MustCompile(`[\[\]()<>\r\n]+`)
)

// assetLinkExts are the extensions an <a href> must end in to be fetched:
// a link to a page is not an asset.
var assetLinkExts = map[string]bool{
	".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": true, ".bmp": true, ".tif": true, ".tiff": true,
}

func assetAttrs(s string) map[string]string {
	out := map[string]string{}
	for _, m := range assetAttrRe.FindAllStringSubmatch(s, -1) {
		k := strings.ToLower(m[1])
		if _, seen := out[k]; !seen {
			out[k] = html.UnescapeString(m[2] + m[3] + m[4])
		}
	}
	return out
}

// assetFetcher fetches a page's assets in document order, each URL once.
type assetFetcher struct {
	f       *WebFetch
	ctx     context.Context
	client  *http.Client
	base    *url.URL
	max     int
	tried   int
	byURL   map[string]*webAsset
	list    []*webAsset
	noInbox bool
}

// fetchAssets rewrites each asset link in page to its inbox reference (or to
// its absolute URL with the reason it was not fetched) and returns the page
// and the assets in the order the page referenced them.
func (f *WebFetch) fetchAssets(ctx context.Context, page, pageURL string, max int) (string, []webAsset) {
	base, err := url.Parse(pageURL)
	if err != nil {
		return page, nil
	}
	client := f.HTTP.transferClient(ctx, redirectAllowlisted, nil)
	defer client.CloseIdleConnections()
	a := &assetFetcher{f: f, ctx: ctx, client: client, base: base, max: max, byURL: map[string]*webAsset{}, noInbox: !inbox.Available(ctx)}
	// Script and style blocks are dropped by the strip; a URL inside one is
	// not a link on the page.
	page = scriptStyleRe.ReplaceAllString(page, "")
	if loc := openScriptStyleRe.FindStringIndex(page); loc != nil {
		page = page[:loc[0]]
	}
	page = assetTagRe.ReplaceAllStringFunc(page, func(tag string) string {
		m := assetTagRe.FindStringSubmatch(tag)
		if m[3] != "" {
			return a.image(m[3], m[4])
		}
		idx := assetTagRe.FindStringSubmatchIndex(tag)
		inner := assetImgRe.ReplaceAllStringFunc(m[2], func(img string) string {
			im := assetImgRe.FindStringSubmatch(img)
			return a.image(im[1], im[2])
		})
		href := assetAttrs(m[1])["href"]
		u := a.resolve(href)
		if u == nil || !assetLinkExts[strings.ToLower(path.Ext(u.Path))] {
			return tag[:idx[4]] + inner + tag[idx[5]:]
		}
		return a.link("["+inner+"]", u)
	})
	out := make([]webAsset, len(a.list))
	for i, w := range a.list {
		out[i] = *w
	}
	return page, out
}

// image rewrites one <img> or <source>: its src, else its srcset's first
// candidate. A <source> is taken only for its srcset: a <source src> is a
// video's or a sound's.
func (a *assetFetcher) image(tag, attrs string) string {
	at := assetAttrs(attrs)
	src := ""
	if strings.EqualFold(tag, "img") {
		src = at["src"]
	}
	if strings.TrimSpace(src) == "" || strings.HasPrefix(strings.TrimSpace(src), "data:") {
		src = ""
		if first, _, _ := strings.Cut(at["srcset"], ","); strings.TrimSpace(first) != "" {
			src = strings.Fields(first)[0]
		}
	}
	u := a.resolve(src)
	if u == nil {
		return "<" + tag + attrs + ">"
	}
	alt := strings.TrimSpace(assetTextRe.ReplaceAllString(at["alt"], " "))
	return " " + a.link("!["+alt+"]", u) + " "
}

// link fetches u (once per URL) and renders it as a Markdown link with label.
func (a *assetFetcher) link(label string, u *url.URL) string {
	w := a.fetch(u)
	if w.Ref != "" {
		return label + "(" + w.Ref + ")"
	}
	return label + "(" + w.URL + ") [not fetched: " + w.Skipped + "]"
}

// resolve makes a link absolute against the page; nil for anything that is
// not an http(s) URL.
func (a *assetFetcher) resolve(link string) *url.URL {
	link = strings.TrimSpace(link)
	if link == "" || strings.HasPrefix(link, "data:") || strings.HasPrefix(link, "#") {
		return nil
	}
	u, err := a.base.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil
	}
	u.Fragment, u.RawFragment = "", ""
	return u
}

func (a *assetFetcher) fetch(u *url.URL) *webAsset {
	key := u.String()
	if w, ok := a.byURL[key]; ok {
		return w
	}
	w := &webAsset{URL: displayURL(u)}
	a.byURL[key] = w
	switch {
	case a.tried >= a.max:
		w.Skipped = fmt.Sprintf("assets_max (%d) reached", a.max)
		return w
	case a.noInbox:
		w.Skipped = "this run has no inbox to keep files in"
	case u.User != nil:
		w.Skipped = "URL carries a user or password"
	case !a.f.HTTP.hostPermitted(a.ctx, u.Hostname()):
		w.Skipped = "host not on the allowlist"
	default:
		a.tried++
		a.get(u, w)
	}
	a.list = append(a.list, w)
	return w
}

func (a *assetFetcher) get(u *url.URL, w *webAsset) {
	ctx, cancel := context.WithTimeout(a.ctx, a.f.HTTP.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		w.Skipped = "invalid URL"
		return
	}
	resp, err := a.client.Do(req)
	if err != nil {
		w.Skipped = transferError(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		w.Skipped = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return
	}
	br := bufio.NewReaderSize(resp.Body, 512)
	head, _ := br.Peek(512)
	name := responseFileName(resp)
	mt, ok := binaryResponse(http.MethodGet, resp, head, name)
	if !ok {
		if mt == "" {
			mt = "empty"
		}
		w.Skipped = "not a file (" + mt + ")"
		return
	}
	res := inbox.CaptureReader(a.ctx, inbox.Item{Name: name, MediaType: mt, Origin: "webfetch", Dir: inbox.DirWeb + "/" + u.Hostname()}, br, resp.ContentLength)
	if !res.Kept {
		w.Skipped = strings.TrimPrefix(strings.Trim(res.Stub, "[]"), "file not kept: ")
		return
	}
	w.Ref, w.Size, w.MediaType = res.Record.Ref, res.Record.Size, res.Record.MediaType
}

// assetsSection is the line that lists the assets after the page's text. The
// text stays text — callers read it as a page — and the list is one JSON
// array on a labelled last line, present only when assets were asked for.
func assetsSection(list []webAsset) string {
	if list == nil {
		list = []webAsset{}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(list)
	return "\n\n[assets] " + strings.TrimSuffix(b.String(), "\n")
}
