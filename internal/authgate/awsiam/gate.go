package awsiam

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
)

// Kind is the AWS IAM gate's kind in `auth.gates.<name>.kind`.
const Kind = "awsiam"

// Issuer names STS as the vouching party in the assertion (and so in the
// identity row): the subject is an ARN only STS can attest.
const Issuer = "https://sts.amazonaws.com"

// AMR is the method an AWS IAM login records.
const AMR = "aws-sigv4"

// maxLoginBody bounds the login request body: four base64 fields, the
// headers the largest. 64 KiB is far beyond any signed GetCallerIdentity.
const maxLoginBody = 64 << 10

// AuthGate is the AWS IAM gate behind the authgate.Gate interface. It is a
// machine login (no browser): Verify reads the LoginData from the request
// body, a JSON object in Vault's field names.
type AuthGate struct{ G *Gate }

// Kind implements authgate.Gate.
func (a AuthGate) Kind() string { return Kind }

// Begin implements authgate.Gate: there is no interactive login.
func (a AuthGate) Begin(context.Context, http.ResponseWriter, *http.Request, authgate.LoginState) error {
	return authgate.ErrNotInteractive
}

// Complete implements authgate.Gate: a machine has no browser to complete in.
func (a AuthGate) Complete(context.Context, *http.Request, authgate.LoginState) (authgate.Assertion, error) {
	return authgate.Assertion{}, authgate.ErrNotInteractive
}

// Verify implements authgate.Gate.
func (a AuthGate) Verify(ctx context.Context, r *http.Request) (authgate.Assertion, error) {
	if r.Body == nil {
		return authgate.Assertion{}, authgate.ErrNoCredential
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxLoginBody+1))
	if err != nil {
		return authgate.Assertion{}, err
	}
	if len(raw) > maxLoginBody {
		return authgate.Assertion{}, invalid("login body too large")
	}
	d, err := ParseLoginData(raw)
	if err != nil {
		return authgate.Assertion{}, err
	}
	id, err := a.G.Verify(ctx, d)
	if err != nil {
		return authgate.Assertion{}, err
	}
	return id.Assertion(), nil
}

// ParseLoginData reads a login body. The four Vault fields are required;
// any other key (the route's own `gate`) is ignored here.
func ParseLoginData(raw []byte) (LoginData, error) {
	var d LoginData
	if err := json.Unmarshal(raw, &d); err != nil {
		return LoginData{}, invalid("login body is not a JSON object")
	}
	if d.Method == "" && d.URL == "" && d.Headers == "" && d.Body == "" {
		return LoginData{}, authgate.ErrNoCredential
	}
	if d.Method == "" || d.URL == "" || d.Headers == "" || d.Body == "" {
		return LoginData{}, invalid("login body needs iam_http_request_method, iam_request_url, iam_request_headers and iam_request_body")
	}
	return d, nil
}

// Assertion is the identity as a gate assertion: the canonical ARN is the
// subject at STS; the caller ARN, account and user id ride as claims (the
// identity map may key on `arn`, the default subject claim is `sub`).
func (id Identity) Assertion() authgate.Assertion {
	return authgate.Assertion{
		Subject:  id.ARN,
		AMR:      []string{AMR},
		AuthTime: id.SignedAt,
		Issuer:   Issuer,
		Claims: map[string]any{
			"sub": id.ARN, "arn": id.ARN, "caller_arn": id.CallerARN,
			"account": id.Account, "user_id": id.UserID,
		},
		ExpiresAt: id.SignedAt.Add(DefaultWindow),
	}
}

// IsUnavailable reports whether err is STS being unreachable rather than a
// refused credential (an answer of 503, not 401).
func IsUnavailable(err error) bool { return errors.Is(err, ErrSTSUnavailable) }
