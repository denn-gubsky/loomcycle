package awsauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource hands out fixed credentials and counts fetches.
type fakeSource struct {
	c           Credentials
	err         error
	fetches     atomic.Int32
	invalidated atomic.Int32
}

func (f *fakeSource) Retrieve(context.Context) (Credentials, error) {
	f.fetches.Add(1)
	return f.c, f.err
}
func (f *fakeSource) Invalidate() { f.invalidated.Add(1) }

func TestDefaultService_FromTheBedrockHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic": "bedrock",
		"https://bedrock-mantle.us-east-1.api.aws/v1":                  "bedrock-mantle",
		"https://api.anthropic.com":                                    "",
		"https://evil.example/bedrock-runtime.us-east-1.amazonaws.com": "",
	} {
		if got := DefaultService(in); got != want {
			t.Errorf("DefaultService(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostHasRegion_MatchesAWholeLabelOnly(t *testing.T) {
	cases := []struct {
		url, region string
		want        bool
	}{
		{"https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic", "eu-central-1", true},
		{"https://bedrock-mantle.us-east-1.api.aws/v1", "us-east-1", true},
		{"https://bedrock-runtime.eu-central-1.amazonaws.com", "us-east-1", false},
		{"https://bedrock-runtime.us-east-10.amazonaws.com", "us-east-1", false},
		{"https://bedrock-runtime.eu-central-1.amazonaws.com", "", false},
	}
	for _, c := range cases {
		if got := HostHasRegion(c.url, c.region); got != c.want {
			t.Errorf("HostHasRegion(%q, %q) = %v, want %v", c.url, c.region, got, c.want)
		}
	}
}

// The key is the generator libraries' format: prefix, then base64 of the
// presigned URL without its scheme, with &Version=1 inside the encoding.
func TestMintBedrockToken_HasTheGeneratorFormat(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	tok, until, err := MintBedrockToken(testCreds, "eu-central-1", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "bedrock-api-key-") {
		t.Fatalf("token %q lacks the prefix", tok[:20])
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(tok, "bedrock-api-key-"))
	if err != nil {
		t.Fatalf("not standard base64: %v", err)
	}
	s := string(raw)
	if !strings.HasPrefix(s, "bedrock.amazonaws.com/?") || !strings.HasSuffix(s, "&Version=1") {
		t.Fatalf("decoded = %q", s)
	}
	q, _ := url.ParseQuery(strings.TrimPrefix(strings.TrimSuffix(s, "&Version=1"), "bedrock.amazonaws.com/?"))
	if q.Get("Action") != "CallWithBearerToken" || q.Get("X-Amz-Expires") != "43200" ||
		!strings.Contains(q.Get("X-Amz-Credential"), "/eu-central-1/bedrock/aws4_request") || q.Get("X-Amz-Signature") == "" {
		t.Errorf("query = %v", q)
	}
	if want := now.Add(12 * time.Hour); !until.Equal(want) {
		t.Errorf("until = %v, want %v", until, want)
	}
}

// A key cannot outlive the session that signed it.
func TestMintBedrockToken_LifetimeCappedBySession(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	c := withToken(testCreds)
	c.CanExpire, c.Expires = true, now.Add(50*time.Minute)
	tok, until, err := MintBedrockToken(c, "us-east-1", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(tok, "bedrock-api-key-"))
	if !strings.Contains(string(raw), "X-Amz-Expires=3000&") || !strings.Contains(string(raw), "X-Amz-Security-Token=") {
		t.Errorf("decoded = %q", raw)
	}
	if !until.Equal(c.Expires) {
		t.Errorf("until = %v, want %v", until, c.Expires)
	}
	c.Expires = now
	if _, _, err := MintBedrockToken(c, "us-east-1", 0, now); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("expired session: err = %v", err)
	}
}

func TestApply_SigV4SignsWithTheHostsService(t *testing.T) {
	for base, svc := range map[string]string{
		"https://bedrock-runtime.us-east-1.amazonaws.com/anthropic": "/us-east-1/bedrock/aws4_request",
		"https://bedrock-mantle.us-east-1.api.aws/v1":               "/us-east-1/bedrock-mantle/aws4_request",
	} {
		src := &fakeSource{c: withToken(testCreds)}
		a := NewWithSource(Config{Mode: ModeSigV4, Region: "us-east-1", BaseURL: base}, src)
		req := httptest.NewRequest(http.MethodPost, base+"/v1/messages", nil)
		called := false
		if err := a.Apply(context.Background(), req, []byte(`{}`), func(*http.Request, string) { called = true }); err != nil {
			t.Fatal(err)
		}
		if called {
			t.Error("SigV4 mode handed a key to the driver")
		}
		if got := req.Header.Get("Authorization"); !strings.Contains(got, svc) {
			t.Errorf("%s: Authorization = %q, want scope %s", base, got, svc)
		}
		if req.Header.Get("X-Amz-Security-Token") == "" {
			t.Errorf("%s: session token not sent", base)
		}
	}
}

// Token mode mints once, reuses the key until 80% of its life has passed,
// and mints again after Invalidate.
func TestApply_TokenModeReusesTheKeyUntilRefresh(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	src := &fakeSource{c: testCreds}
	a := NewWithSource(Config{Mode: ModeBedrockToken, Region: "us-east-1", BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com/openai/v1"}, src)
	a.now = func() time.Time { return now }
	var keys []string
	setKey := func(_ *http.Request, k string) { keys = append(keys, k) }
	apply := func() {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "https://x/", nil)
		if err := a.Apply(context.Background(), req, nil, setKey); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("Authorization") != "" {
			t.Error("token mode also signed the request")
		}
	}
	apply()
	now = now.Add(9 * time.Hour) // 75% of 12 h
	apply()
	if src.fetches.Load() != 1 || keys[0] != keys[1] {
		t.Fatalf("before the refresh point: fetches=%d, same key=%v", src.fetches.Load(), keys[0] == keys[1])
	}
	now = now.Add(time.Hour) // 83%
	apply()
	if src.fetches.Load() != 2 || keys[2] == keys[1] {
		t.Fatalf("after the refresh point: fetches=%d, new key=%v", src.fetches.Load(), keys[2] != keys[1])
	}
	a.Invalidate()
	apply()
	if src.fetches.Load() != 3 || src.invalidated.Load() != 1 {
		t.Fatalf("after Invalidate: fetches=%d invalidated=%d", src.fetches.Load(), src.invalidated.Load())
	}
}

// A burst of calls at the refresh point mints one key.
func TestApply_TokenModeMintsOnceUnderABurst(t *testing.T) {
	src := &fakeSource{c: testCreds}
	a := NewWithSource(Config{Mode: ModeBedrockToken, Region: "us-east-1"}, src)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.Apply(context.Background(), httptest.NewRequest(http.MethodPost, "https://x/", nil), nil, func(*http.Request, string) {})
		}()
	}
	wg.Wait()
	if n := src.fetches.Load(); n != 1 {
		t.Errorf("fetches = %d, want 1", n)
	}
}

// Every secret the Auth obtains reaches the sink, including one set after
// construction (the redactor exists only after providers are built).
func TestSecretSink_ReceivesKeysTokensAndMintedKeys(t *testing.T) {
	src := &fakeSource{c: withToken(testCreds)}
	a := NewWithSource(Config{Mode: ModeBedrockToken, Region: "us-east-1"}, src)
	got := map[string]bool{}
	var mu sync.Mutex
	a.SetSecretSink(func(v string) { mu.Lock(); got[v] = true; mu.Unlock() })
	var minted string
	if err := a.Apply(context.Background(), httptest.NewRequest(http.MethodPost, "https://x/", nil), nil, func(_ *http.Request, k string) { minted = k }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{src.c.SecretAccessKey, src.c.SessionToken, minted} {
		if !got[want] {
			t.Errorf("sink missed a secret (len %d)", len(want))
		}
	}
	if got[src.c.AccessKeyID] {
		t.Error("the access key id is not secret and should not be registered")
	}
}

func TestIsExpiredCredentialAnswer(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{403, `{"message":"The security token included in the request is expired"}`, true},
		{400, `{"__type":"ExpiredTokenException","message":"x"}`, true},
		{401, `{"error":{"message":"Bearer token has expired"}}`, true},
		// P0: runtime's OpenAI route answers an access denial with 401.
		{401, `{"error":{"message":"User: arn:aws:iam::1:user/u is not authorized to perform: bedrock:InvokeModel","type":"permission_denied_error","code":"access_denied"}}`, false},
		{403, `{"type":"error","error":{"type":"permission_error","message":"not authorized"}}`, false},
		{500, `ExpiredToken`, false},
	}
	for _, c := range cases {
		if got := IsExpiredCredentialAnswer(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%d %s = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// redirect sends every request to srv whatever its host, so host-based logic
// (runtime vs mantle) sees the real Bedrock hosts.
type redirect struct{ srv *httptest.Server }

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(r.srv.URL)
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestListModels_RuntimeUsesTheControlPlaneWithPaging(t *testing.T) {
	var signedFor []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signedFor = append(signedFor, r.URL.Path+" "+r.Header.Get("Authorization"))
		switch {
		case r.URL.Path == "/inference-profiles" && r.URL.Query().Get("nextToken") == "":
			fmt.Fprint(w, `{"inferenceProfileSummaries":[{"inferenceProfileId":"us.anthropic.claude-haiku-5-5"}],"nextToken":"p2"}`)
		case r.URL.Path == "/inference-profiles":
			fmt.Fprint(w, `{"inferenceProfileSummaries":[{"inferenceProfileId":"eu.anthropic.claude-sonnet-5-5"}]}`)
		case r.URL.Path == "/foundation-models":
			fmt.Fprint(w, `{"modelSummaries":[{"modelId":"openai.gpt-oss-20b-1:0"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := NewWithSource(Config{Mode: ModeBedrockToken, Region: "us-east-1", BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com/anthropic"}, &fakeSource{c: testCreds})
	a.http = &http.Client{Transport: redirect{srv}}
	ids, err := a.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids, ","); got != "eu.anthropic.claude-sonnet-5-5,openai.gpt-oss-20b-1:0,us.anthropic.claude-haiku-5-5" {
		t.Errorf("ids = %s", got)
	}
	for _, s := range signedFor {
		if !strings.Contains(s, "/us-east-1/bedrock/aws4_request") {
			t.Errorf("listing not signed as bedrock even in token mode: %s", s)
		}
	}
}

func TestListModels_MantleUsesItsOwnList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || !strings.Contains(r.Header.Get("Authorization"), "/bedrock-mantle/") {
			http.Error(w, "bad", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "openai.gpt-oss-20b"}, {"id": "anthropic.claude-haiku-4-5"}}})
	}))
	defer srv.Close()
	a := NewWithSource(Config{Mode: ModeSigV4, Region: "us-east-1", BaseURL: "https://bedrock-mantle.us-east-1.api.aws/anthropic"}, &fakeSource{c: testCreds})
	a.http = &http.Client{Transport: redirect{srv}}
	ids, err := a.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids, ","); got != "anthropic.claude-haiku-4-5,openai.gpt-oss-20b" {
		t.Errorf("ids = %s", got)
	}
}

// isolateAWSEnv points the chain at nothing on this machine: no env keys, no
// profile files, no container endpoint, so only what a test sets is found.
func isolateAWSEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_DEFAULT_PROFILE",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "AWS_EC2_METADATA_DISABLED",
		"AWS_EC2_METADATA_V1_DISABLED", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_STS", "AWS_EC2_METADATA_SERVICE_ENDPOINT"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

func TestChain_EnvironmentKeys(t *testing.T) {
	isolateAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "env-secret-value-0123456789")
	src, err := NewChain(context.Background(), ChainOptions{Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := src.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "AKIDENV" || !strings.Contains(c.Source, "Env") {
		t.Errorf("got key %q from %q", c.AccessKeyID, c.Source)
	}
}

// fakeIMDS is an instance metadata service. v2 says whether it issues tokens;
// it records any credential read made without one.
type fakeIMDS struct {
	v2            bool
	tokenlessRead atomic.Int32
}

func (f *fakeIMDS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut && r.URL.Path == "/latest/api/token" {
		if !f.v2 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("X-aws-ec2-metadata-token-ttl-seconds", "21600")
		fmt.Fprint(w, "imds-session-token")
		return
	}
	if r.Header.Get("X-aws-ec2-metadata-token") != "imds-session-token" {
		f.tokenlessRead.Add(1)
	}
	switch r.URL.Path {
	case "/latest/meta-data/iam/security-credentials/", "/latest/meta-data/iam/security-credentials":
		fmt.Fprint(w, "loomcycle-role")
	case "/latest/meta-data/iam/security-credentials/loomcycle-role":
		_ = json.NewEncoder(w).Encode(map[string]string{"Code": "Success", "Type": "AWS-HMAC", "AccessKeyId": "ASIAIMDS",
			"SecretAccessKey": "imds-secret-0123456789", "Token": "imds-session-credential-token",
			"Expiration": time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339)})
	default:
		http.NotFound(w, r)
	}
}

func TestChain_InstanceMetadataV2(t *testing.T) {
	isolateAWSEnv(t)
	imds := &fakeIMDS{v2: true}
	srv := httptest.NewServer(imds)
	defer srv.Close()
	t.Setenv("AWS_EC2_METADATA_DISABLED", "false")
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", srv.URL)
	src, err := NewChain(context.Background(), ChainOptions{Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := src.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "ASIAIMDS" || c.SessionToken == "" || !c.CanExpire {
		t.Errorf("got %q token=%v canExpire=%v", c.AccessKeyID, c.SessionToken != "", c.CanExpire)
	}
	if n := imds.tokenlessRead.Load(); n != 0 {
		t.Errorf("%d metadata reads without an IMDSv2 token", n)
	}
}

// IMDSv1 is never used: when the token handshake fails, the chain fails
// rather than reading credentials without a token.
func TestChain_NeverFallsBackToIMDSv1(t *testing.T) {
	isolateAWSEnv(t)
	imds := &fakeIMDS{v2: false}
	srv := httptest.NewServer(imds)
	defer srv.Close()
	t.Setenv("AWS_EC2_METADATA_DISABLED", "false")
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", srv.URL)
	src, err := NewChain(context.Background(), ChainOptions{Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := src.Retrieve(ctx); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
	if n := imds.tokenlessRead.Load(); n != 0 {
		t.Errorf("fell back to IMDSv1: %d tokenless reads", n)
	}
}

// role_arn assumes the role over the base credentials, with the configured
// session name and external id.
func TestChain_AssumeRole(t *testing.T) {
	isolateAWSEnv(t)
	var form url.Values
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=AKIDBASE/") {
			http.Error(w, "unsigned", 403)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>ASIAROLE</AccessKeyId>`+
			`<SecretAccessKey>role-secret-0123456789</SecretAccessKey><SessionToken>role-session-token</SessionToken>`+
			`<Expiration>%s</Expiration></Credentials><AssumedRoleUser><Arn>arn:aws:sts::1:assumed-role/r/s</Arn>`+
			`<AssumedRoleId>AROA:s</AssumedRoleId></AssumedRoleUser></AssumeRoleResult></AssumeRoleResponse>`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer sts.Close()
	t.Setenv("AWS_ENDPOINT_URL_STS", sts.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDBASE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "base-secret-0123456789")
	src, err := NewChain(context.Background(), ChainOptions{Region: "eu-central-1", RoleARN: "arn:aws:iam::123456789012:role/loomcycle-bedrock", ExternalID: "tenant-7"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := src.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "ASIAROLE" || c.SessionToken != "role-session-token" {
		t.Errorf("got %q", c.AccessKeyID)
	}
	if form.Get("Action") != "AssumeRole" || form.Get("RoleSessionName") != "loomcycle" || form.Get("ExternalId") != "tenant-7" {
		t.Errorf("STS form = %v", form)
	}
}
