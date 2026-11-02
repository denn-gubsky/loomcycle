package http

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/cdc"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// feedFixture is a server whose store captures changes (the CDC decorator, as
// LOOMCYCLE_MEMORY_CHANGES_ENABLED wires it) with SQL Memory, plus a writer:
// the Document tool on the same store, granted the user and tenant scopes as
// alice of tenant tnt.
type feedFixture struct {
	srv    *Server
	store  *cdc.Store
	doc    *builtin.Document
	writer context.Context
}

func newFeedFixture(t *testing.T) *feedFixture {
	t.Helper()
	raw, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	st := cdc.Wrap(raw, nil)
	f := &feedFixture{
		srv:   &Server{store: st, sqlMem: mgr, cfgHolder: config.NewHolder(&config.Config{})},
		store: st,
		doc:   &builtin.Document{Store: st, SqlMem: mgr},
	}
	f.writer = f.as("tnt", "alice")
	return f
}

// as is a writer ctx for subject of tenant, granted the user and tenant scopes.
func (f *feedFixture) as(tenant, subject string) context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "w", UserID: subject, TenantID: tenant})
	return tools.WithRunPolicy(ctx, tools.RunPolicy{
		Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"user", "tenant"}},
		SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"user", "tenant"}},
	})
}

func (f *feedFixture) op(t *testing.T, ctx context.Context, body string) map[string]any {
	t.Helper()
	res, err := f.doc.Execute(ctx, json.RawMessage(body))
	if err != nil || res.IsError {
		t.Fatalf("%s: %v %s", body, err, res.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	return out
}

// newDoc files a document with one chunk at path, returning both ids.
func (f *feedFixture) newDoc(t *testing.T, ctx context.Context, scope, path string) (string, string) {
	t.Helper()
	d := f.op(t, ctx, fmt.Sprintf(`{"op":"create_document","scope":%q,"title":%q,"path":%q}`, scope, path, path))
	docID, _ := d["document_id"].(string)
	c := f.op(t, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":%q,"document_id":%q,"title":"part","body":"text"}`, scope, docID))
	chunkID, _ := c["id"].(string)
	return docID, chunkID
}

func (f *feedFixture) touch(t *testing.T, ctx context.Context, scope, docID string) {
	t.Helper()
	f.op(t, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":%q,"document_id":%q,"title":"more","body":"more text"}`, scope, docID))
}

// readerSet is a member's set: read on the tenant scope's documents, with
// reading denied at the given paths.
func readerSet(t *testing.T, subject string, denied ...string) *authz.Effective {
	t.Helper()
	rules := []authz.Rule{{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant", "document:user"}}}
	if len(denied) > 0 {
		var res []string
		for _, p := range denied {
			res = append(res, "document:tenant:"+p)
		}
		rules = append(rules, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: res})
	}
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "tnt", ID: subject},
		Bindings: []authz.Binding{{ACL: "reader", SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{"reader": {ACL: authz.ACL{Name: "reader", Rules: rules}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

func member(tenant, subject string, eff *authz.Effective) auth.Principal {
	return auth.Principal{TenantID: tenant, Subject: subject, Scopes: []string{auth.ScopeRunsCreate}, Effective: eff}
}

type feedFrame struct {
	event string
	data  string
}

func (fr feedFrame) change(t *testing.T) builtin.DocumentFeedFrame {
	t.Helper()
	var c builtin.DocumentFeedFrame
	if err := json.Unmarshal([]byte(fr.data), &c); err != nil {
		t.Fatalf("change frame %q: %v", fr.data, err)
	}
	return c
}

// feedClock is a clock a test moves by hand.
type feedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *feedClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *feedClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testFeedPolling is fast polling on clock, re-checking the request's principal
// through reauth (nil: the principal unchanged) and with no credential expiry.
func testFeedPolling(clock *feedClock, reauth func(*http.Request) (auth.Principal, error)) documentFeedPolling {
	if reauth == nil {
		reauth = func(r *http.Request) (auth.Principal, error) {
			p, _ := auth.PrincipalFromContext(r.Context())
			return p, nil
		}
	}
	return documentFeedPolling{
		changeFeedPolling: changeFeedPolling{base: 5 * time.Millisecond, max: 20 * time.Millisecond, sleep: sleepCtx},
		ttl:               documentFeedAccessTTL,
		now:               clock.Now,
		reauth:            reauth,
		expiry:            func(*http.Request) time.Time { return time.Time{} },
	}
}

// feedStream is one open GET /v1/_document/feed, its frames on a channel.
type feedStream struct {
	status  int
	body    string    // a refusal's body
	opening feedFrame // the `feed` frame
	frames  chan feedFrame
	cancel  context.CancelFunc
}

// openFeed opens the reader feed as p, through a real HTTP server so frames
// stream as they are sent; it returns once the opening frame arrived (the
// stream has read its head) or the request was refused.
func openFeed(t *testing.T, f *feedFixture, p auth.Principal, query string, poll documentFeedPolling) *feedStream {
	t.Helper()
	return openFeedVia(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.srv.serveDocumentFeed(w, r.WithContext(auth.WithPrincipal(r.Context(), p)), poll)
	}), query, "")
}

// openFeedVia is openFeed through any handler, sending bearer when set.
func openFeedVia(t *testing.T, h http.Handler, query, bearer string) *feedStream {
	t.Helper()
	ts := httptest.NewServer(h)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ts.Close() })
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/_document/feed?"+query, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &feedStream{status: resp.StatusCode, frames: make(chan feedFrame, 256), cancel: cancel}
	if resp.StatusCode != http.StatusOK {
		var b strings.Builder
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			b.WriteString(sc.Text())
		}
		_ = resp.Body.Close()
		s.body = b.String()
		return s
	}
	go func() {
		defer resp.Body.Close()
		defer close(s.frames)
		sc := bufio.NewScanner(resp.Body)
		var event string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				s.frames <- feedFrame{event: event, data: strings.TrimPrefix(line, "data: ")}
			}
		}
	}()
	if s.opening = s.next(t); s.opening.event != "feed" {
		t.Fatalf("first frame = %+v, want the opening feed frame", s.opening)
	}
	return s
}

// ended waits for the stream to end and returns the frames sent before it did.
func (s *feedStream) ended(t *testing.T) []feedFrame {
	t.Helper()
	var got []feedFrame
	deadline := time.After(10 * time.Second)
	for {
		select {
		case fr, ok := <-s.frames:
			if !ok {
				return got
			}
			got = append(got, fr)
		case <-deadline:
			t.Fatalf("the stream did not end within 10s; frames: %+v", got)
		}
	}
}

func (s *feedStream) next(t *testing.T) feedFrame {
	t.Helper()
	select {
	case fr, ok := <-s.frames:
		if !ok {
			t.Fatal("the stream ended")
		}
		return fr
	case <-time.After(10 * time.Second):
		t.Fatal("no frame within 10s")
	}
	return feedFrame{}
}

// until collects change frames up to and including the first for sentinel.
func (s *feedStream) until(t *testing.T, sentinel string) []builtin.DocumentFeedFrame {
	t.Helper()
	var got []builtin.DocumentFeedFrame
	for {
		fr := s.next(t)
		if fr.event != "change" {
			t.Fatalf("unexpected %s frame: %s", fr.event, fr.data)
		}
		c := fr.change(t)
		if c.DocumentID == sentinel {
			return got
		}
		got = append(got, c)
	}
}

func docSet(frames []builtin.DocumentFeedFrame) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range frames {
		if !seen[f.DocumentID] {
			seen[f.DocumentID] = true
			out = append(out, f.DocumentID)
		}
	}
	sort.Strings(out)
	return out
}

// A member granted reading two of five tenant documents sees changes to
// exactly those two: writes to all five, and the three it may not read never
// reach the stream — no frame, and nothing in any frame names them.
func TestDocumentFeed_MemberSeesOnlyReadableDocuments(t *testing.T) {
	f := newFeedFixture(t)
	docs := make([]string, 5)
	for i := range docs {
		docs[i], _ = f.newDoc(t, f.writer, "tenant", fmt.Sprintf("/kb/d%d", i+1))
	}
	bob := member("tnt", "bob", readerSet(t, "bob", "/kb/d2", "/kb/d4", "/kb/d5"))
	s := openFeed(t, f, bob, "scope=tenant", testFeedPolling(&feedClock{now: time.Now()}, nil))

	for _, d := range docs {
		f.touch(t, f.writer, "tenant", d)
	}
	sentinel, _ := f.newDoc(t, f.writer, "tenant", "/kb/sentinel")
	got := s.until(t, sentinel)

	want := []string{docs[0], docs[2]}
	sort.Strings(want)
	if g := docSet(got); strings.Join(g, ",") != strings.Join(want, ",") {
		t.Errorf("frames named documents %v, want exactly the readable %v", g, want)
	}
	for _, f := range got {
		if f.Type != store.DocumentChangeUpdated || f.ChunkID == "" || f.At.IsZero() {
			t.Errorf("frame %+v: want a document.chunk.updated naming its chunk and time", f)
		}
	}
}

// Rows of another scope, another subject's user scope, another tenant, and
// plain memory rows never reach a reader of its own user scope.
func TestDocumentFeed_OtherScopeAndSubjectNeverArrive(t *testing.T) {
	f := newFeedFixture(t)
	bobCtx, otherTenantBob := f.as("tnt", "bob"), f.as("other", "bob")
	aliceDoc, _ := f.newDoc(t, f.writer, "user", "/a")
	tenantDoc, _ := f.newDoc(t, f.writer, "tenant", "/t")
	elsewhereDoc, _ := f.newDoc(t, otherTenantBob, "user", "/b")
	bobDoc, _ := f.newDoc(t, bobCtx, "user", "/b")

	bob := member("tnt", "bob", nil)
	s := openFeed(t, f, bob, "scope=user", testFeedPolling(&feedClock{now: time.Now()}, nil))

	f.touch(t, f.writer, "user", aliceDoc)
	f.touch(t, f.writer, "tenant", tenantDoc)
	f.touch(t, otherTenantBob, "user", elsewhereDoc)
	if err := f.store.MemorySet(context.Background(), "tnt", store.MemoryScopeUser, "bob", "note", json.RawMessage(`"x"`), 0); err != nil {
		t.Fatal(err)
	}
	f.touch(t, bobCtx, "user", bobDoc)
	sentinel, _ := f.newDoc(t, bobCtx, "user", "/sentinel")
	got := s.until(t, sentinel)
	if g := docSet(got); len(g) != 1 || g[0] != bobDoc {
		t.Errorf("bob's user-scope feed named %v, want only his own document %s", g, bobDoc)
	}
}

// A deletion is placed by the document recorded when it happened: the chunk
// row is gone by the time the feed reads it, so without capture-time stamping
// the frame could not be placed and would never arrive. A reader whose access
// is unconditional gets the deleted document's frames; one whose rules
// restrict reading does not (RFC FV G5) — it reconciles on its next reload.
func TestDocumentFeed_DeletionCarriesTheDocument(t *testing.T) {
	f := newFeedFixture(t)
	gone, part := f.newDoc(t, f.writer, "tenant", "/kb/gone")
	kept, keptPart := f.newDoc(t, f.writer, "tenant", "/kb/kept")
	clock := &feedClock{now: time.Now()}
	open := openFeed(t, f, member("tnt", "bob", nil), "scope=tenant", testFeedPolling(clock, nil))
	ruled := openFeed(t, f, member("tnt", "carol", readerSet(t, "carol", "/kb/secret")), "scope=tenant", testFeedPolling(clock, nil))

	f.op(t, f.writer, fmt.Sprintf(`{"op":"delete_chunk","scope":"tenant","id":%q}`, keptPart))
	f.op(t, f.writer, fmt.Sprintf(`{"op":"delete_document","scope":"tenant","id":%q}`, gone))
	sentinel, _ := f.newDoc(t, f.writer, "tenant", "/kb/sentinel")

	got := open.until(t, sentinel)
	deleted := map[string]string{}
	for _, fr := range got {
		if fr.Type == store.DocumentChangeDeleted {
			deleted[fr.DocumentID] = fr.ChunkID
		}
	}
	if deleted[kept] != keptPart {
		t.Errorf("delete_chunk: deleted frames %v, want chunk %s of %s", deleted, keptPart, kept)
	}
	if _, ok := deleted[gone]; !ok {
		t.Errorf("delete_document: no deleted frame for document %s (its part %s): %+v", gone, part, got)
	}

	for _, fr := range ruled.until(t, sentinel) {
		if fr.DocumentID == gone {
			t.Errorf("a reader whose rules restrict reading got a frame for the deleted document: %+v", fr)
		}
	}
}

// A grant revoked mid-stream stops frames once the access window passes: the
// stream re-reads the caller's rules and asks again.
func TestDocumentFeed_RevokedGrantStopsFramesWithinTTL(t *testing.T) {
	f := newFeedFixture(t)
	d1, _ := f.newDoc(t, f.writer, "tenant", "/kb/d1")
	d2, _ := f.newDoc(t, f.writer, "tenant", "/kb/d2")
	before, after := readerSet(t, "bob"), readerSet(t, "bob", "/kb/d2")
	var revoked atomic.Bool
	reread := make(chan struct{}, 16)
	reauth := func(r *http.Request) (auth.Principal, error) {
		p, _ := auth.PrincipalFromContext(r.Context())
		if revoked.Load() {
			p.Effective = after
		}
		reread <- struct{}{}
		return p, nil
	}
	clock := &feedClock{now: time.Now()}
	s := openFeed(t, f, member("tnt", "bob", before), "scope=tenant", testFeedPolling(clock, reauth))

	f.touch(t, f.writer, "tenant", d2)
	if got := s.until(t, d2); len(got) != 0 {
		t.Fatalf("frames before d2's: %+v", got)
	}

	revoked.Store(true)
	clock.Add(documentFeedAccessTTL + time.Second)
	select {
	case <-reread:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream never re-read the caller's rules after the access window")
	}
	f.touch(t, f.writer, "tenant", d2)
	f.touch(t, f.writer, "tenant", d1)
	for _, fr := range s.until(t, d1) {
		if fr.DocumentID == d2 {
			t.Errorf("a frame for %s arrived after its grant was revoked and the window passed: %+v", d2, fr)
		}
	}
}

// A principal holds at most LOOMCYCLE_DOCUMENT_FEED_MAX_STREAMS streams; the
// next is refused with 429 while another principal is unaffected, and closing
// one frees its slot.
func TestDocumentFeed_StreamCap(t *testing.T) {
	f := newFeedFixture(t)
	cfg := &config.Config{}
	cfg.Env.DocumentFeedMaxStreams = 4
	f.srv.cfgHolder = config.NewHolder(cfg)
	bob := member("tnt", "bob", nil)
	poll := testFeedPolling(&feedClock{now: time.Now()}, nil)
	var open []*feedStream
	for i := 0; i < 4; i++ {
		s := openFeed(t, f, bob, "scope=user", poll)
		if s.status != http.StatusOK {
			t.Fatalf("stream %d: status %d %s", i+1, s.status, s.body)
		}
		open = append(open, s)
	}
	if s := openFeed(t, f, bob, "scope=user", poll); s.status != http.StatusTooManyRequests {
		t.Errorf("a fifth stream: status %d, want 429", s.status)
	}
	if s := openFeed(t, f, member("tnt", "carol", nil), "scope=user", poll); s.status != http.StatusOK {
		t.Errorf("another principal's stream: status %d %s, want 200", s.status, s.body)
	}
	open[0].cancel()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := openFeed(t, f, bob, "scope=user", poll)
		if s.status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("closing a stream never freed its slot: status %d", s.status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The reader route refuses an agent scope (run-private) and anything that is
// not a scope, before it opens a stream.
func TestDocumentFeed_RefusesAgentScope(t *testing.T) {
	f := newFeedFixture(t)
	for _, q := range []string{"scope=agent", "", "scope=global"} {
		if s := openFeed(t, f, member("tnt", "bob", nil), q, testFeedPolling(&feedClock{now: time.Now()}, nil)); s.status != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", q, s.status)
		}
	}
}

// The reader feed is member-accessible with the same posture as POST
// /v1/_document; the operator feed keeps its carve-out, and its frames are
// unchanged apart from the additive document_id (and the new edges type on the
// document family). Memory frames carry no document_id key at all.
func TestDocumentFeed_OperatorFeedUnchanged(t *testing.T) {
	if got := requiredScopeFor(http.MethodGet, "/v1/_document/feed"); got != auth.ScopeTenant {
		t.Errorf("/v1/_document/feed scope = %q, want %s (as POST /v1/_document)", got, auth.ScopeTenant)
	}
	if !tenantMemberAccessible(http.MethodGet, "/v1/_document/feed") {
		t.Error("/v1/_document/feed must be member-accessible")
	}
	for _, p := range []string{"/v1/_memory/changes", "/v1/_document/changes"} {
		if requiredScopeFor(http.MethodGet, p) != auth.ScopeTenant || tenantMemberAccessible(http.MethodGet, p) {
			t.Errorf("%s: the operator feed's gate moved", p)
		}
	}

	f := newFeedFixture(t)
	a, aPart := f.newDoc(t, f.writer, "tenant", "/kb/a")
	_, bPart := f.newDoc(t, f.writer, "tenant", "/kb/b")
	f.op(t, f.writer, fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites"}`, aPart, bPart))
	if err := f.store.MemorySet(context.Background(), "tnt", store.MemoryScopeUser, "alice", "note", json.RawMessage(`"x"`), 0); err != nil {
		t.Fatal(err)
	}

	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "tnt", Subject: "op", Scopes: []string{auth.ScopeTenant}})
	frames := func(h http.HandlerFunc, path string) []map[string]any {
		ctx, cancel := context.WithTimeout(op, 350*time.Millisecond)
		defer cancel()
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		var out []map[string]any
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok && strings.Contains(data, `"seq"`) {
				var m map[string]any
				_ = json.Unmarshal([]byte(data), &m)
				out = append(out, m)
			}
		}
		return out
	}
	docFrames := frames(f.srv.handleDocumentChanges, "/v1/_document/changes")
	edges := 0
	for _, m := range docFrames {
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if got := strings.Join(keys, ","); got != "at,chunk_id,document_id,scope,scope_id,seq,tenant,type" {
			t.Errorf("operator document frame keys = %s, want the RFC CD shape plus document_id", got)
		}
		if m["type"] == string(store.DocumentEdgesChanged) {
			edges++
		}
	}
	if len(docFrames) == 0 || edges != 2 {
		t.Errorf("operator document feed: %d frames, %d edges rows, want the cross-document link's two", len(docFrames), edges)
	}
	if docFrames[0]["document_id"] != a {
		t.Errorf("first operator frame document_id = %v, want %s", docFrames[0]["document_id"], a)
	}
	for _, m := range frames(f.srv.handleMemoryChanges, "/v1/_memory/changes") {
		if _, has := m["document_id"]; has {
			t.Errorf("a memory frame gained a document_id key: %v", m)
		}
	}
}

// tokenFeedFixture is newFeedFixture with bearer auth configured and a member
// token for bob of tnt; expires is the token's fixed end (zero: none).
func tokenFeedFixture(t *testing.T, expires time.Time) *feedFixture {
	t.Helper()
	f := newFeedFixture(t)
	cfg := &config.Config{}
	cfg.Env.AuthToken = "legacy-secret"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	f.srv.cfgHolder = config.NewHolder(cfg)
	if _, err := f.store.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_bob", Name: "bob", TenantID: "tnt", Subject: "bob",
		TokenHash:     auth.HashToken(testTokenPepper, "bob-token"),
		AllowedScopes: []string{auth.ScopeRunsCreate},
		ExpiresAt:     expires,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// openAuthedFeed opens the reader feed through the real auth middleware with
// bob's bearer and the stream's default credential re-check.
func openAuthedFeed(t *testing.T, f *feedFixture, clock *feedClock) *feedStream {
	t.Helper()
	poll := f.srv.defaultDocumentFeedPolling()
	poll.changeFeedPolling = changeFeedPolling{base: 5 * time.Millisecond, max: 20 * time.Millisecond, sleep: sleepCtx}
	poll.now = clock.Now
	s := openFeedVia(t, f.srv.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.srv.serveDocumentFeed(w, r, poll)
	})), "scope=user", "bob-token")
	if s.status != http.StatusOK {
		t.Fatalf("open: status %d %s", s.status, s.body)
	}
	return s
}

// lastError is the code of the last `error` frame among frames, "" when none.
func lastError(frames []feedFrame) string {
	code := ""
	for _, fr := range frames {
		if fr.event == "error" {
			var e struct{ Code string }
			_ = json.Unmarshal([]byte(fr.data), &e)
			code = e.Code
		}
	}
	return code
}

// A token revoked mid-stream ends the stream at the next re-check: the stream
// re-validates the credential the way every request is authenticated, not
// only the access-control set it carries.
func TestDocumentFeed_RevokedTokenEndsTheStream(t *testing.T) {
	f := tokenFeedFixture(t, time.Time{})
	clock := &feedClock{now: time.Now()}
	s := openAuthedFeed(t, f, clock)
	doc, _ := f.newDoc(t, f.as("tnt", "bob"), "user", "/x")
	s.until(t, doc)

	if err := f.store.OperatorTokenDefSetRetiredAt(context.Background(), "def_bob", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	clock.Add(documentFeedAccessTTL + time.Second)
	frames := s.ended(t)
	if code := lastError(frames); code != "credential_revoked" {
		t.Errorf("the stream of a revoked token ended with error %q (frames %+v), want credential_revoked", code, frames)
	}
}

// A stream never outlives its token: at the token's expiry it ends.
func TestDocumentFeed_StreamEndsAtTheTokenExpiry(t *testing.T) {
	f := tokenFeedFixture(t, time.Now().Add(time.Hour))
	clock := &feedClock{now: time.Now()}
	s := openAuthedFeed(t, f, clock)
	clock.Add(2 * time.Hour)
	frames := s.ended(t)
	if code := lastError(frames); code != "credential_expired" {
		t.Errorf("the stream outlived its token: ended with error %q (frames %+v), want credential_expired", code, frames)
	}
}

// The reader's opening frame says only whether changes are captured: the
// embedder's health (provider, model, failure counts) is operator
// infrastructure state and stays on the operator feed.
func TestDocumentFeed_OpeningFrameCarriesNoEmbedderHealth(t *testing.T) {
	f := newFeedFixture(t)
	s := openFeed(t, f, member("tnt", "bob", nil), "scope=user", testFeedPolling(&feedClock{now: time.Now()}, nil))
	var opening map[string]any
	if err := json.Unmarshal([]byte(s.opening.data), &opening); err != nil {
		t.Fatal(err)
	}
	if _, has := opening["embedder"]; has || len(opening) != 1 || opening["enabled"] != true {
		t.Errorf("reader opening frame = %s, want exactly {\"enabled\":true}", s.opening.data)
	}
}
