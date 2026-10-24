// Package awsiam is the AWS IAM gate (RFC FA §3.2 AWSIAMGate): an AWS
// workload proves which IAM role or user it runs as by presenting a SigV4-
// signed sts:GetCallerIdentity request it did not send. loomcycle sends it to
// STS and trusts the ARN STS answers with — the HashiCorp Vault AWS IAM
// method, and its field names, so Vault's client libraries produce a valid
// login body.
//
// What makes the borrowed signature safe to accept:
//
//   - The request is sent only to an allowlisted STS host, rebuilt by this
//     package. The client's URL is read only to learn which of those hosts it
//     signed for; it is never dialled (SSRF).
//   - Only GetCallerIdentity, with no other parameter, is forwarded: this is
//     not a general STS proxy.
//   - The X-Loomcycle-AWS-Server-ID header must be SIGNED and equal this
//     server's id, so a request signed for another service that accepts STS
//     proofs cannot be replayed here.
//   - X-Amz-Date must be within five minutes, and each signature is admitted
//     once.
//   - The ARN must match an allowed_arns pattern that names an account: STS
//     answers for every AWS account in the world.
//
// STS places no authorization in front of GetCallerIdentity, so MFA cannot be
// required on this path; it is for machines.
package awsiam

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/replay"
	"github.com/denn-gubsky/loomcycle/internal/awsauth"
)

// ServerIDHeader binds a signed request to one loomcycle deployment.
const ServerIDHeader = "X-Loomcycle-Aws-Server-Id"

// DefaultWindow bounds |now - X-Amz-Date|.
const DefaultWindow = 5 * time.Minute

// GlobalHost is the global STS endpoint; it signs as us-east-1.
const GlobalHost = "sts.amazonaws.com"

const (
	maxField     = 16 << 10
	maxSTSAnswer = 64 << 10
)

// LoginData is a client's presented request, in Vault's field names: the
// method as is, the URL, the header object and the body base64-encoded.
type LoginData struct {
	Method  string `json:"iam_http_request_method"`
	URL     string `json:"iam_request_url"`
	Headers string `json:"iam_request_headers"`
	Body    string `json:"iam_request_body"`
}

// Identity is what STS said about the signer.
type Identity struct {
	// ARN is the canonical principal: an assumed role's role ARN
	// (arn:aws:iam::<acct>:role/<name>), else the caller ARN as STS gave it.
	// allowed_arns are matched against it and it is the default subject.
	ARN string
	// CallerARN is STS's own answer (an assumed role's carries the session
	// name, which whoever assumes the role chooses).
	CallerARN string
	Account   string
	UserID    string
	// SignedAt is the request's X-Amz-Date.
	SignedAt time.Time
}

// SharedAdmitter is the cross-replica replay record (store.AuthReplayStore).
type SharedAdmitter interface {
	AuthReplayAdmit(ctx context.Context, scope, key string, expires, now time.Time) (bool, error)
}

// Gate verifies presented STS requests.
type Gate struct {
	// ServerID is the value the signed X-Loomcycle-AWS-Server-ID must carry.
	ServerID string
	// AllowedARNs are the patterns (ValidPattern) a canonical ARN must match.
	AllowedARNs []string
	// Regions whose regional STS endpoints are accepted besides the global
	// one.
	Regions []string
	Window  time.Duration
	// Replay holds signatures in flight and remembers the ones STS accepted,
	// on this replica.
	Replay *replay.Cache
	// Shared, when set, records each accepted signature (as a SHA-256 hash)
	// for every replica, under SharedScope.
	Shared      SharedAdmitter
	SharedScope string
	// HTTP is the client STS is called with; nil uses a 10-second-timeout
	// client that follows no redirects.
	HTTP *http.Client
	Now  func() time.Time

	// endpoint maps an allowlisted host to the URL dialled; tests point it
	// at a local server.
	endpoint func(host string) string
}

var regionRE = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)

// ValidRegion reports whether r looks like an AWS region name.
func ValidRegion(r string) bool { return regionRE.MatchString(r) }

// RegionalHost is a region's STS host.
func RegionalHost(region string) string {
	if strings.HasPrefix(region, "cn-") {
		return "sts." + region + ".amazonaws.com.cn"
	}
	return "sts." + region + ".amazonaws.com"
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: awsiam: "+format, append([]any{authgate.ErrInvalidCredential}, a...)...)
}

// ErrSTSUnavailable: STS could not be asked. Not the caller's fault; the
// login is refused all the same.
var ErrSTSUnavailable = errors.New("awsiam: STS unavailable")

// Verify checks a presented request and, if it holds, asks STS who signed it.
func (g *Gate) Verify(ctx context.Context, d LoginData) (Identity, error) {
	if g.ServerID == "" || len(g.AllowedARNs) == 0 || g.Replay == nil {
		return Identity{}, errors.New("awsiam: gate is not configured")
	}
	if d.Method != http.MethodPost {
		return Identity{}, invalid("only a POST GetCallerIdentity request is accepted")
	}
	host, region, err := g.stsHost(d.URL)
	if err != nil {
		return Identity{}, err
	}
	body, err := decodeField(d.Body)
	if err != nil {
		return Identity{}, invalid("request body is not base64")
	}
	if err := checkBody(body); err != nil {
		return Identity{}, err
	}
	hdr, err := decodeHeaders(d.Headers)
	if err != nil {
		return Identity{}, err
	}
	auth, err := awsauth.ParseAuthorization(single(hdr, "Authorization"))
	if err != nil {
		return Identity{}, invalid("%v", err)
	}
	if auth.Service != "sts" || auth.Region != region {
		return Identity{}, invalid("request is not signed for STS in %s", region)
	}
	for _, h := range []string{"host", "x-amz-date", strings.ToLower(ServerIDHeader)} {
		if !auth.Signs(h) {
			return Identity{}, invalid("%s is not among the signed headers", h)
		}
	}
	if sid := hdr.Values(ServerIDHeader); len(sid) != 1 || sid[0] != g.ServerID {
		return Identity{}, invalid("server id header does not name this server")
	}
	if h := hdr.Values("Host"); len(h) > 1 || (len(h) == 1 && !strings.EqualFold(h[0], host)) {
		return Identity{}, invalid("host header does not match the request URL")
	}
	signedAt, err := awsauth.ParseAmzDate(single(hdr, "X-Amz-Date"))
	if err != nil || signedAt.UTC().Format("20060102") != auth.Day {
		return Identity{}, invalid("X-Amz-Date is missing or disagrees with the credential scope")
	}
	// One clock read judges the window and stamps the replay entry.
	now := time.Now()
	if g.Now != nil {
		now = g.Now()
	}
	window := g.Window
	if window <= 0 {
		window = DefaultWindow
	}
	if dt := now.Sub(signedAt); dt >= window || dt <= -window {
		return Identity{}, invalid("request was signed outside the %s window", window)
	}
	// The signature is held in flight while STS judges it, so a concurrent
	// identical presentation is refused, and recorded only once STS said it
	// is genuine: an unauthenticated caller can never leave an entry behind
	// (or fill a bounded cache and so lock out real logins).
	key := "sig:" + auth.Signature
	if err := g.Replay.Reserve(key, now); err != nil {
		return Identity{}, invalid("request %v", err)
	}
	id, err := g.ask(ctx, host, hdr, auth, body)
	if err != nil {
		g.Replay.Release(key)
		return Identity{}, err
	}
	// Authorised before anything is recorded: STS vouches for every AWS
	// principal in the world, so recording a refused one would let any AWS
	// account fill the cache and the store. A refused ARN replayed is simply
	// refused again.
	if !MatchAny(g.AllowedARNs, id.ARN) {
		g.Replay.Release(key)
		return Identity{}, invalid("%s is not in allowed_arns", id.ARN)
	}
	expires := replay.EntryExpiry(signedAt, window)
	if g.Shared != nil {
		// Another replica may have been handed the same request.
		sum := sha256.Sum256([]byte(auth.Signature))
		ok, err := g.Shared.AuthReplayAdmit(ctx, g.SharedScope, hex.EncodeToString(sum[:]), expires, now)
		if err != nil {
			g.Replay.Release(key)
			return Identity{}, fmt.Errorf("awsiam: recording the request: %w", err)
		}
		if !ok {
			g.Replay.Commit(key, expires)
			return Identity{}, invalid("request %v", replay.ErrReplayed)
		}
	}
	g.Replay.Commit(key, expires)
	id.SignedAt = signedAt
	return id, nil
}

// stsHost validates the client's URL and returns the allowlisted host it
// names and the region that host signs as.
func (g *Gate) stsHost(raw string) (host, region string, err error) {
	b, err := decodeField(raw)
	if err != nil {
		return "", "", invalid("request URL is not base64")
	}
	u, err := url.Parse(string(b))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", "", invalid("request URL must be https://<sts host>/ with no query")
	}
	if u.Path != "" && u.Path != "/" {
		return "", "", invalid("request URL must be https://<sts host>/ with no query")
	}
	if p := u.Port(); p != "" && p != "443" {
		return "", "", invalid("request URL names a port")
	}
	h := strings.ToLower(u.Hostname())
	if h == GlobalHost {
		return GlobalHost, "us-east-1", nil
	}
	for _, r := range g.Regions {
		if h == RegionalHost(r) {
			return h, r, nil
		}
	}
	return "", "", invalid("STS host %q is not allowed", h)
}

// checkBody accepts exactly Action=GetCallerIdentity and a Version.
func checkBody(b []byte) error {
	if len(b) > 1024 {
		return invalid("request body is too long")
	}
	q, err := url.ParseQuery(string(b))
	if err != nil || len(q) != 2 || len(q["Action"]) != 1 || len(q["Version"]) != 1 ||
		q.Get("Action") != "GetCallerIdentity" || q.Get("Version") == "" {
		return invalid("request is not exactly GetCallerIdentity")
	}
	return nil
}

func decodeField(s string) ([]byte, error) {
	if len(s) > maxField {
		return nil, errors.New("field too long")
	}
	return base64.StdEncoding.DecodeString(s)
}

// decodeHeaders reads the base64 JSON header object: each name maps to a list
// of values (Go's http.Header) or one value. Two names that are the same
// header in different case are refused — which one STS would see is not this
// package's guess to make.
func decodeHeaders(s string) (http.Header, error) {
	b, err := decodeField(s)
	if err != nil {
		return nil, invalid("request headers are not base64")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, invalid("request headers are not a JSON object")
	}
	h := http.Header{}
	for k, v := range raw {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if _, dup := h[ck]; dup {
			return nil, invalid("request header %s given twice", ck)
		}
		var vals []string
		if err := json.Unmarshal(v, &vals); err != nil {
			var one string
			if err := json.Unmarshal(v, &one); err != nil {
				return nil, invalid("request header %s is not a string or a list", ck)
			}
			vals = []string{one}
		}
		for _, x := range vals {
			if strings.ContainsAny(x, "\r\n\x00") {
				return nil, invalid("request header %s has a control character", ck)
			}
		}
		h[ck] = vals
	}
	return h, nil
}

func single(h http.Header, k string) string {
	if v := h.Values(k); len(v) == 1 {
		return v[0]
	}
	return ""
}

// ask sends the presented request to STS — the signed headers only, plus the
// content type — and reads the identity from the answer.
func (g *Gate) ask(ctx context.Context, host string, hdr http.Header, auth awsauth.SigV4Authorization, body []byte) (Identity, error) {
	target := "https://" + host + "/"
	if g.endpoint != nil {
		target = g.endpoint(host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return Identity{}, err
	}
	req.Host = host
	for k, vs := range hdr {
		lk := strings.ToLower(k)
		if lk == "host" || lk == "content-length" || (!auth.Signs(lk) && lk != "authorization" && lk != "content-type") {
			continue
		}
		req.Header[k] = vs
	}
	client := g.HTTP
	if client == nil {
		client = &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrSTSUnavailable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSTSAnswer))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrSTSUnavailable, err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 500:
		return Identity{}, fmt.Errorf("%w: status %d", ErrSTSUnavailable, resp.StatusCode)
	default:
		// STS refused the signature (expired keys, a tampered request, a
		// wrong region). Its message is not repeated to the caller.
		return Identity{}, invalid("STS refused the request (status %d)", resp.StatusCode)
	}
	arn, account, userID, ok := parseCallerIdentity(raw)
	if !ok {
		return Identity{}, fmt.Errorf("%w: unreadable GetCallerIdentity answer", ErrSTSUnavailable)
	}
	canon, acct, ok := CanonicalARN(arn)
	if !ok || acct != account {
		return Identity{}, invalid("STS answered with an ARN this gate cannot read")
	}
	return Identity{ARN: canon, CallerARN: arn, Account: account, UserID: userID}, nil
}

// parseCallerIdentity reads STS's XML answer, or the JSON one a client gets
// by signing an Accept: application/json header.
func parseCallerIdentity(raw []byte) (arn, account, userID string, ok bool) {
	var x struct {
		Result struct {
			Arn     string `xml:"Arn"`
			Account string `xml:"Account"`
			UserID  string `xml:"UserId"`
		} `xml:"GetCallerIdentityResult"`
	}
	if xml.Unmarshal(raw, &x) == nil && x.Result.Arn != "" {
		return x.Result.Arn, x.Result.Account, x.Result.UserID, true
	}
	var j struct {
		Response struct {
			Result struct {
				Arn     string `json:"Arn"`
				Account string `json:"Account"`
				UserID  string `json:"UserId"`
			} `json:"GetCallerIdentityResult"`
		} `json:"GetCallerIdentityResponse"`
	}
	if json.Unmarshal(raw, &j) == nil && j.Response.Result.Arn != "" {
		return j.Response.Result.Arn, j.Response.Result.Account, j.Response.Result.UserID, true
	}
	return "", "", "", false
}

var (
	callerARNRE = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov):(iam|sts)::([0-9]{12}):([A-Za-z0-9+=,.@_/-]{1,512})$`)
	patternRE   = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov):(iam|sts)::[0-9]{12}:[A-Za-z0-9+=,.@_/*-]{1,512}$`)
)

// CanonicalARN maps an assumed-role session ARN to its role ARN (the path a
// role was created under is not in the session ARN and is lost, as in Vault)
// and returns other ARNs unchanged, with the account.
func CanonicalARN(arn string) (canon, account string, ok bool) {
	m := callerARNRE.FindStringSubmatch(arn)
	if m == nil {
		return "", "", false
	}
	partition, service, account, resource := m[1], m[2], m[3], m[4]
	if service == "sts" {
		if rest, isRole := strings.CutPrefix(resource, "assumed-role/"); isRole {
			role, session, found := strings.Cut(rest, "/")
			if !found || role == "" || session == "" {
				return "", "", false
			}
			return "arn:" + partition + ":iam::" + account + ":role/" + role, account, true
		}
	}
	return arn, account, true
}

// ValidPattern checks an allowed_arns entry: an ARN whose account is a
// literal twelve digits, with `*` (any run of characters) only in the
// resource. A pattern that leaves the account open would admit anyone with an
// AWS account who names a role to match.
func ValidPattern(p string) error {
	if !patternRE.MatchString(p) {
		return fmt.Errorf("%q must be an IAM ARN with a literal 12-digit account (e.g. arn:aws:iam::123456789012:role/ci-*); "+
			"* is allowed only after the account", p)
	}
	if strings.Contains(p, ":sts::") && strings.Contains(p, ":assumed-role/") {
		// Matching is against the role ARN, so this pattern would never match.
		return fmt.Errorf("%q: name the role, arn:<partition>:iam::<account>:role/<name>; an assumed-role session ARN is "+
			"matched as its role", p)
	}
	return nil
}

// MatchAny reports whether arn matches one of patterns.
func MatchAny(patterns []string, arn string) bool {
	for _, p := range patterns {
		if globMatch(p, arn) {
			return true
		}
	}
	return false
}

// globMatch: `*` matches any run of characters ('/' included); everything
// else is literal and case-sensitive, as ARNs are.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}
