package awsiam

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/replay"
	"github.com/denn-gubsky/loomcycle/internal/awsauth"
)

var t0 = time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC)

const (
	serverID = "loomcycle.acme.internal"
	ciRole   = "arn:aws:iam::123456789012:role/ci-deploy"
)

// The fake STS knows two keys, each the credentials of one principal.
var principals = map[string]struct {
	creds awsauth.Credentials
	arn   string
}{
	"AKIACIROLE": {awsauth.Credentials{AccessKeyID: "AKIACIROLE", SecretAccessKey: "ci-secret", SessionToken: "tok"}, "arn:aws:sts::123456789012:assumed-role/ci-deploy/i-0abc"},
	"AKIAOTHER":  {awsauth.Credentials{AccessKeyID: "AKIAOTHER", SecretAccessKey: "other-secret"}, "arn:aws:iam::123456789012:user/alice"},
	"AKIAFOREIGN": {awsauth.Credentials{AccessKeyID: "AKIAFOREIGN", SecretAccessKey: "foreign-secret"},
		"arn:aws:sts::999999999999:assumed-role/ci-deploy/x"},
}

// fakeSTS checks a request's SigV4 signature the way STS does — by signing
// the same request again with the caller's secret — and answers
// GetCallerIdentity.
type fakeSTS struct {
	srv   *httptest.Server
	calls atomic.Int32
	hosts []string
}

func newFakeSTS(t *testing.T) *fakeSTS {
	t.Helper()
	f := &fakeSTS{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.hosts = append(f.hosts, r.Host)
		body, _ := io.ReadAll(r.Body)
		a, err := awsauth.ParseAuthorization(r.Header.Get("Authorization"))
		p, known := principals[a.AccessKeyID]
		if err != nil || !known {
			http.Error(w, "<Error><Code>InvalidClientTokenId</Code></Error>", http.StatusForbidden)
			return
		}
		date, _ := awsauth.ParseAmzDate(r.Header.Get("X-Amz-Date"))
		re, _ := http.NewRequest(r.Method, "https://"+r.Host+"/", bytes.NewReader(body))
		for _, h := range a.SignedHeaders {
			switch h {
			case "host", "x-amz-date", "x-amz-security-token":
				continue
			}
			re.Header[http.CanonicalHeaderKey(h)] = r.Header.Values(h)
		}
		awsauth.SignRequest(re, body, p.creds, a.Region, "sts", date)
		if re.Header.Get("Authorization") != r.Header.Get("Authorization") {
			http.Error(w, "<Error><Code>SignatureDoesNotMatch</Code></Error>", http.StatusForbidden)
			return
		}
		acct := strings.Split(p.arn, ":")[4]
		fmt.Fprintf(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>`+
			`<Arn>%s</Arn><UserId>AROAEXAMPLE:i-0abc</UserId><Account>%s</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`, p.arn, acct)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSTS) gate() *Gate {
	return &Gate{
		ServerID: serverID, AllowedARNs: []string{"arn:aws:iam::123456789012:role/ci-*"},
		Regions: []string{"eu-west-1"}, Replay: replay.New(0), HTTP: f.srv.Client(),
		Now:      func() time.Time { return t0 },
		endpoint: func(string) string { return f.srv.URL + "/" },
	}
}

const stsBody = "Action=GetCallerIdentity&Version=2011-06-15"

// login builds what a workload sends: a request it signed but did not send.
func login(t *testing.T, key, rawURL, region, body string, at time.Time, mutate func(*http.Request)) LoginData {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set(ServerIDHeader, serverID)
	if mutate != nil {
		mutate(req)
	}
	awsauth.SignRequest(req, []byte(body), principals[key].creds, region, "sts", at)
	hb, _ := json.Marshal(req.Header)
	enc := base64.StdEncoding.EncodeToString
	return LoginData{Method: http.MethodPost, URL: enc([]byte(rawURL)), Headers: enc(hb), Body: enc([]byte(body))}
}

func validLogin(t *testing.T) LoginData {
	return login(t, "AKIACIROLE", "https://sts.amazonaws.com/", "us-east-1", stsBody, t0, nil)
}

func TestGate_SignedRequestYieldsTheRoleARN(t *testing.T) {
	f := newFakeSTS(t)
	id, err := f.gate().Verify(context.Background(), validLogin(t))
	if err != nil {
		t.Fatal(err)
	}
	if id.ARN != ciRole || id.Account != "123456789012" || !strings.Contains(id.CallerARN, "assumed-role/ci-deploy/i-0abc") {
		t.Fatalf("identity %+v", id)
	}
	if len(f.hosts) != 1 || f.hosts[0] != GlobalHost {
		t.Fatalf("STS saw host %v, want %s", f.hosts, GlobalHost)
	}
}

func TestGate_RegionalEndpointIsAcceptedWhenListed(t *testing.T) {
	f := newFakeSTS(t)
	d := login(t, "AKIACIROLE", "https://sts.eu-west-1.amazonaws.com/", "eu-west-1", stsBody, t0, nil)
	if _, err := f.gate().Verify(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

func TestGate_ReplayedRequestIsRefused(t *testing.T) {
	f := newFakeSTS(t)
	g := f.gate()
	d := validLogin(t)
	if _, err := g.Verify(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Verify(context.Background(), d); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("replay = %v, want ErrInvalidCredential", err)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("the replay reached STS (%d calls)", f.calls.Load())
	}
}

func TestGate_UnsignedServerIDHeaderIsRefused(t *testing.T) {
	f := newFakeSTS(t)
	// Signed without the server-id header, which is then added: STS would
	// accept the signature, so only this check stops a request signed for
	// another service.
	req, _ := http.NewRequest(http.MethodPost, "https://sts.amazonaws.com/", strings.NewReader(stsBody))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	awsauth.SignRequest(req, []byte(stsBody), principals["AKIACIROLE"].creds, "us-east-1", "sts", t0)
	req.Header.Set(ServerIDHeader, serverID)
	hb, _ := json.Marshal(req.Header)
	enc := base64.StdEncoding.EncodeToString
	d := LoginData{Method: http.MethodPost, URL: enc([]byte("https://sts.amazonaws.com/")), Headers: enc(hb), Body: enc([]byte(stsBody))}
	if _, err := f.gate().Verify(context.Background(), d); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("unsigned server id = %v, want ErrInvalidCredential", err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("reached STS")
	}
}

func TestGate_ServerIDForAnotherServerIsRefused(t *testing.T) {
	f := newFakeSTS(t)
	d := login(t, "AKIACIROLE", "https://sts.amazonaws.com/", "us-east-1", stsBody, t0, func(r *http.Request) {
		r.Header.Set(ServerIDHeader, "vault.acme.internal")
	})
	if _, err := f.gate().Verify(context.Background(), d); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("Verify = %v, want ErrInvalidCredential", err)
	}
}

func TestGate_NonAllowlistedSTSHostIsRefusedAndNeverDialled(t *testing.T) {
	var dialled atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { dialled.Add(1) }))
	defer evil.Close()
	f := newFakeSTS(t)
	for _, u := range []string{
		evil.URL + "/",
		"https://sts.amazonaws.com.evil.example/",
		"https://sts.us-east-2.amazonaws.com/", // a real region, not listed
		"https://169.254.169.254/",
		"https://sts.amazonaws.com:8443/",
		"https://user@sts.amazonaws.com/",
		"https://sts.amazonaws.com/?Action=AssumeRole",
		"https://sts.amazonaws.com/other",
		"http://sts.amazonaws.com/",
	} {
		t.Run(u, func(t *testing.T) {
			d := login(t, "AKIACIROLE", strings.Replace(u, "?Action=AssumeRole", "", 1), "us-east-1", stsBody, t0, nil)
			d.URL = base64.StdEncoding.EncodeToString([]byte(u))
			if _, err := f.gate().Verify(context.Background(), d); !errors.Is(err, authgate.ErrInvalidCredential) {
				t.Fatalf("Verify = %v, want ErrInvalidCredential", err)
			}
		})
	}
	if dialled.Load() != 0 || f.calls.Load() != 0 {
		t.Fatalf("a refused host was dialled (evil %d, sts %d)", dialled.Load(), f.calls.Load())
	}
}

func TestGate_ARNOutsideAllowedARNsIsRefused(t *testing.T) {
	f := newFakeSTS(t)
	for _, key := range []string{"AKIAOTHER", "AKIAFOREIGN"} {
		d := login(t, key, "https://sts.amazonaws.com/", "us-east-1", stsBody, t0, nil)
		if _, err := f.gate().Verify(context.Background(), d); !errors.Is(err, authgate.ErrInvalidCredential) {
			t.Fatalf("%s: Verify = %v, want ErrInvalidCredential", key, err)
		}
	}
}

func TestGate_RequestSignedOutsideTheWindowIsRefused(t *testing.T) {
	f := newFakeSTS(t)
	for _, d := range []time.Duration{-6 * time.Minute, 6 * time.Minute} {
		l := login(t, "AKIACIROLE", "https://sts.amazonaws.com/", "us-east-1", stsBody, t0.Add(d), nil)
		if _, err := f.gate().Verify(context.Background(), l); !errors.Is(err, authgate.ErrInvalidCredential) {
			t.Fatalf("signed %s off: %v, want ErrInvalidCredential", d, err)
		}
	}
}

func TestGate_OnlyGetCallerIdentityIsForwarded(t *testing.T) {
	f := newFakeSTS(t)
	for _, body := range []string{
		"Action=AssumeRole&Version=2011-06-15&RoleArn=arn:aws:iam::123456789012:role/admin&RoleSessionName=x",
		"Action=GetCallerIdentity&Version=2011-06-15&Extra=1",
		"Action=GetCallerIdentity&Action=AssumeRole&Version=2011-06-15",
		"Version=2011-06-15",
	} {
		d := login(t, "AKIACIROLE", "https://sts.amazonaws.com/", "us-east-1", body, t0, nil)
		if _, err := f.gate().Verify(context.Background(), d); !errors.Is(err, authgate.ErrInvalidCredential) {
			t.Fatalf("body %q: %v, want ErrInvalidCredential", body, err)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("a non-GetCallerIdentity body reached STS")
	}
}

func TestGate_TamperedRequestIsRefusedBySTS(t *testing.T) {
	f := newFakeSTS(t)
	d := validLogin(t)
	// Swap the key id: the signature no longer matches, as STS will say.
	raw, _ := base64.StdEncoding.DecodeString(d.Headers)
	tampered := strings.Replace(string(raw), "AKIACIROLE/", "AKIAOTHER/", 1)
	d.Headers = base64.StdEncoding.EncodeToString([]byte(tampered))
	if _, err := f.gate().Verify(context.Background(), d); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("Verify = %v, want ErrInvalidCredential", err)
	}
}

func TestGate_SignedForTheWrongRegionOrServiceIsRefused(t *testing.T) {
	f := newFakeSTS(t)
	d := login(t, "AKIACIROLE", "https://sts.amazonaws.com/", "eu-west-1", stsBody, t0, nil)
	if _, err := f.gate().Verify(context.Background(), d); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("global host signed as eu-west-1: %v", err)
	}
}

func TestGate_HeaderGivenTwiceInDifferentCaseIsRefused(t *testing.T) {
	f := newFakeSTS(t)
	d := validLogin(t)
	raw, _ := base64.StdEncoding.DecodeString(d.Headers)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["x-loomcycle-aws-server-id"] = []string{"other"}
	b, _ := json.Marshal(m)
	d.Headers = base64.StdEncoding.EncodeToString(b)
	if _, err := f.gate().Verify(context.Background(), d); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("Verify = %v, want ErrInvalidCredential", err)
	}
}

func TestGate_STSDownIsNotACredentialRefusal(t *testing.T) {
	f := newFakeSTS(t)
	g := f.gate()
	g.endpoint = func(string) string { return "http://127.0.0.1:1/" }
	_, err := g.Verify(context.Background(), validLogin(t))
	if !errors.Is(err, ErrSTSUnavailable) || errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("Verify = %v, want ErrSTSUnavailable", err)
	}
}

func TestMatchAny(t *testing.T) {
	cases := []struct {
		pat, arn string
		want     bool
	}{
		{"arn:aws:iam::123456789012:role/ci-*", "arn:aws:iam::123456789012:role/ci-deploy", true},
		{"arn:aws:iam::123456789012:role/ci-*", "arn:aws:iam::123456789012:role/admin", false},
		{"arn:aws:iam::123456789012:role/ci-*", "arn:aws:iam::999999999999:role/ci-deploy", false},
		{"arn:aws:iam::123456789012:role/*-ci", "arn:aws:iam::123456789012:role/a-ci", true},
		{"arn:aws:iam::123456789012:role/a*b*c", "arn:aws:iam::123456789012:role/aXbYc", true},
		{"arn:aws:iam::123456789012:role/a*b*c", "arn:aws:iam::123456789012:role/acb", false},
		{"arn:aws:iam::123456789012:role/ci", "arn:aws:iam::123456789012:role/ci-deploy", false},
	}
	for _, c := range cases {
		if got := MatchAny([]string{c.pat}, c.arn); got != c.want {
			t.Errorf("MatchAny(%q, %q) = %v", c.pat, c.arn, got)
		}
	}
}

func TestCanonicalARN(t *testing.T) {
	got, acct, ok := CanonicalARN("arn:aws:sts::123456789012:assumed-role/ci-deploy/i-0abc")
	if !ok || got != ciRole || acct != "123456789012" {
		t.Fatalf("%q %q %v", got, acct, ok)
	}
	for _, bad := range []string{"", "arn:aws:sts::123456789012:assumed-role/ci-deploy", "arn:aws:iam::12:role/x", "arn:aws:iam::123456789012:role/x y"} {
		if _, _, ok := CanonicalARN(bad); ok {
			t.Errorf("CanonicalARN(%q) accepted", bad)
		}
	}
}

// junkLogin is well-formed but signed with a key STS does not know: only STS
// can tell it is not genuine.
func junkLogin(t *testing.T, i int) LoginData {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "https://sts.amazonaws.com/", strings.NewReader(stsBody))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set(ServerIDHeader, serverID)
	creds := awsauth.Credentials{AccessKeyID: fmt.Sprintf("AKIAJUNK%d", i), SecretAccessKey: fmt.Sprintf("junk-%d", i)}
	awsauth.SignRequest(req, []byte(stsBody), creds, "us-east-1", "sts", t0)
	hb, _ := json.Marshal(req.Header)
	enc := base64.StdEncoding.EncodeToString
	return LoginData{Method: http.MethodPost, URL: enc([]byte("https://sts.amazonaws.com/")), Headers: enc(hb), Body: enc([]byte(stsBody))}
}

func TestGate_UnauthenticatedSignaturesNeverFillTheReplayCache(t *testing.T) {
	f := newFakeSTS(t)
	g := f.gate()
	g.Replay = replay.New(4)
	for i := 0; i < 20; i++ {
		if _, err := g.Verify(context.Background(), junkLogin(t, i)); !errors.Is(err, authgate.ErrInvalidCredential) {
			t.Fatalf("junk %d: %v", i, err)
		}
	}
	if n := g.Replay.Len(); n != 0 {
		t.Fatalf("STS-refused signatures left %d entries in the replay cache", n)
	}
	if _, err := g.Verify(context.Background(), validLogin(t)); err != nil {
		t.Fatalf("a genuine login after a flood of junk: %v", err)
	}
}

func TestGate_ConcurrentIdenticalRequestsAdmitExactlyOne(t *testing.T) {
	f := newFakeSTS(t)
	release := make(chan struct{})
	var arrived atomic.Int32
	inner := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Add(1)
		<-release
		inner.ServeHTTP(w, r)
	})
	g := f.gate()
	d := validLogin(t)
	const n = 6
	var ok atomic.Int32
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			if _, err := g.Verify(context.Background(), d); err == nil {
				ok.Add(1)
			}
			done <- struct{}{}
		}()
	}
	// Let the presentations reach the point where they call STS (or are
	// refused), then answer every one STS received.
	deadline := time.Now().Add(5 * time.Second)
	for arrived.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	for i := 0; i < n; i++ {
		<-done
	}
	if ok.Load() != 1 {
		t.Fatalf("%d of %d identical concurrent requests accepted, want 1", ok.Load(), n)
	}
}

func TestGate_RequestAtTheWindowEdgeCannotBeReplayedLater(t *testing.T) {
	f := newFakeSTS(t)
	g := f.gate()
	signed := t0.Add(-DefaultWindow + time.Second) // one second from the edge
	d := login(t, "AKIACIROLE", "https://sts.amazonaws.com/", "us-east-1", stsBody, signed, nil)
	if _, err := g.Verify(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	// At every later moment the window still accepts it, the entry still
	// refuses it.
	for _, at := range []time.Time{t0.Add(500 * time.Millisecond), signed.Add(DefaultWindow - time.Nanosecond)} {
		at := at
		g.Now = func() time.Time { return at }
		if _, err := g.Verify(context.Background(), d); !errors.Is(err, authgate.ErrInvalidCredential) {
			t.Fatalf("replay at +%s: %v, want ErrInvalidCredential", at.Sub(signed), err)
		}
	}
	// Exactly one window old, the window itself refuses it.
	edge := login(t, "AKIACIROLE", "https://sts.amazonaws.com/", "us-east-1", stsBody, t0.Add(-DefaultWindow), nil)
	if _, err := f.gate().Verify(context.Background(), edge); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("a request signed exactly one window ago: %v, want ErrInvalidCredential", err)
	}
}

// sharedRecord stands in for the store's shared replay record.
type sharedRecord struct {
	mu   sync.Mutex
	seen map[string]time.Time
	err  error
}

func (s *sharedRecord) UseOnce(_ context.Context, scope, key string, expires, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return false, s.err
	}
	if s.seen == nil {
		s.seen = map[string]time.Time{}
	}
	if until, ok := s.seen[scope+"/"+key]; ok && now.Before(until) {
		return false, nil
	}
	s.seen[scope+"/"+key] = expires
	return true, nil
}

func TestGate_RequestReplayedToAnotherReplicaIsRefused(t *testing.T) {
	f := newFakeSTS(t)
	shared := &sharedRecord{}
	a, b := f.gate(), f.gate() // two replicas: separate caches, one store
	a.Shared, a.SharedScope = shared, "awsiam:ci"
	b.Shared, b.SharedScope = shared, "awsiam:ci"
	d := validLogin(t)
	if _, err := a.Verify(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify(context.Background(), d); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("replay to the second replica = %v, want ErrInvalidCredential", err)
	}
	sig := mustAuth(t, d).Signature
	for k := range shared.seen {
		if strings.Contains(k, sig) {
			t.Fatal("the shared record holds the raw signature, not its hash")
		}
	}
}

func TestGate_SharedRecordFailureRefusesTheLogin(t *testing.T) {
	f := newFakeSTS(t)
	g := f.gate()
	g.Shared, g.SharedScope = &sharedRecord{err: errors.New("db down")}, "awsiam:ci"
	if _, err := g.Verify(context.Background(), validLogin(t)); err == nil {
		t.Fatal("accepted a login whose replay record could not be written")
	}
	// The local reservation was released: once the store is back, the same
	// (still fresh) request is judged again rather than refused as a replay.
	g.Shared = &sharedRecord{}
	if _, err := g.Verify(context.Background(), validLogin(t)); err != nil {
		t.Fatalf("after the store recovered: %v", err)
	}
}

func mustAuth(t *testing.T, d LoginData) awsauth.SigV4Authorization {
	t.Helper()
	raw, _ := base64.StdEncoding.DecodeString(d.Headers)
	var h map[string][]string
	_ = json.Unmarshal(raw, &h)
	a, err := awsauth.ParseAuthorization(h["Authorization"][0])
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestGate_GenuineButDisallowedARNRecordsNothing(t *testing.T) {
	// STS answers 200 for every AWS principal in the world. A signature for
	// an ARN this gate refuses must not take room in the cache or the store,
	// or any AWS account could fill them.
	f := newFakeSTS(t)
	shared := &sharedRecord{}
	g := f.gate()
	g.Replay = replay.New(2)
	g.Shared, g.SharedScope = shared, "awsiam:ci"
	for i, key := range []string{"AKIAOTHER", "AKIAFOREIGN", "AKIAOTHER"} {
		d := login(t, key, "https://sts.amazonaws.com/", "us-east-1", stsBody, t0.Add(time.Duration(i)*time.Second), nil)
		if _, err := g.Verify(context.Background(), d); !errors.Is(err, authgate.ErrInvalidCredential) {
			t.Fatalf("%s: %v, want ErrInvalidCredential", key, err)
		}
	}
	if n := g.Replay.Len(); n != 0 {
		t.Fatalf("refused ARNs left %d entries in the cache", n)
	}
	if len(shared.seen) != 0 {
		t.Fatalf("refused ARNs left %d rows in the shared record", len(shared.seen))
	}
	if _, err := g.Verify(context.Background(), validLogin(t)); err != nil {
		t.Fatalf("a genuine allowed login afterwards: %v", err)
	}
}
