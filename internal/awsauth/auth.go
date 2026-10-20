package awsauth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Modes of authenticating a provider request with AWS credentials.
const (
	// ModeSigV4 signs every request. The default: no key crosses the wire,
	// and it works in accounts whose policy denies Bedrock API keys.
	ModeSigV4 = "aws_sigv4"
	// ModeBedrockToken mints a short-term Bedrock API key from the
	// credentials and sends it as the driver's ordinary key.
	ModeBedrockToken = "aws_bedrock_token"
)

// tokenRefreshFraction is how much of a minted key's life may pass before a
// new one is minted, so a key never lapses while a request is in flight.
const tokenRefreshFraction = 0.8

// Config is one provider's AWS authentication.
type Config struct {
	Mode    string
	Region  string
	Service string // SigV4 service; "" = DefaultService(BaseURL)
	BaseURL string // the provider's base URL, for model listing
	Chain   ChainOptions
}

// Auth authenticates a provider's outgoing requests with AWS credentials. It
// is built once per provider and shared by all its calls.
type Auth struct {
	mode, region, service, baseURL string

	src  CredentialSource
	http *http.Client
	now  func() time.Time
	sink atomic.Pointer[func(string)]
	// controlURL overrides the Bedrock control plane in tests.
	controlURL string

	mu          sync.Mutex // guards the minted key
	token       string
	tokenIssued time.Time
	tokenUntil  time.Time
}

// New builds an Auth on the standard credential chain. Nothing is fetched
// until the first request.
func New(ctx context.Context, cfg Config) (*Auth, error) {
	cfg.Chain.Region = cfg.Region
	src, err := NewChain(ctx, cfg.Chain)
	if err != nil {
		return nil, err
	}
	return NewWithSource(cfg, src), nil
}

// NewWithSource builds an Auth on a given credential source (tests, and
// anything that already holds credentials).
func NewWithSource(cfg Config, src CredentialSource) *Auth {
	service := cfg.Service
	if service == "" {
		service = DefaultService(cfg.BaseURL)
	}
	return &Auth{
		mode: cfg.Mode, region: cfg.Region, service: service, baseURL: cfg.BaseURL,
		src:  src,
		http: &http.Client{Timeout: 10 * time.Second},
		now:  time.Now,
	}
}

// SetSecretSink registers f to receive every secret value this Auth obtains
// (secret keys, session tokens, minted keys), so the server's redactor masks
// them if anything ever echoes one. It may be set after construction: the sink
// is read at each use.
func (a *Auth) SetSecretSink(f func(string)) {
	if f == nil {
		a.sink.Store(nil)
		return
	}
	a.sink.Store(&f)
}

func (a *Auth) secret(v string) {
	if v == "" {
		return
	}
	if f := a.sink.Load(); f != nil {
		(*f)(v)
	}
}

// Mode, Region and Service describe the configuration (none is secret).
func (a *Auth) Mode() string    { return a.mode }
func (a *Auth) Region() string  { return a.region }
func (a *Auth) Service() string { return a.service }

// Apply authenticates req, whose complete body is body. In SigV4 mode it signs
// the request; in token mode it hands a minted key to setKey, the driver's own
// way of sending a key (x-api-key or Authorization: Bearer). Call it for each
// attempt, after the driver has set its headers.
func (a *Auth) Apply(ctx context.Context, req *http.Request, body []byte, setKey func(*http.Request, string)) error {
	switch a.mode {
	case ModeSigV4:
		c, err := a.credentials(ctx)
		if err != nil {
			return err
		}
		SignRequest(req, body, c, a.region, a.service, a.now())
		return nil
	case ModeBedrockToken:
		tok, err := a.bedrockToken(ctx)
		if err != nil {
			return err
		}
		setKey(req, tok)
		return nil
	}
	return fmt.Errorf("aws auth: unknown mode %q", a.mode)
}

// Invalidate drops the cached credentials and minted key, after AWS answered
// that one of them had expired.
func (a *Auth) Invalidate() {
	a.src.Invalidate()
	a.mu.Lock()
	a.token, a.tokenIssued, a.tokenUntil = "", time.Time{}, time.Time{}
	a.mu.Unlock()
}

// Classify reads an error answer: whether it says the credentials or key had
// expired (IsExpiredCredentialAnswer), and a hint for a signature failure.
func (a *Auth) Classify(status int, body []byte) (expired bool, hint string) {
	if IsExpiredCredentialAnswer(status, body) {
		return true, ""
	}
	if IsSignatureFailure(status, body) {
		return false, "SigV4 signature refused: check this server's clock and that auth.region " + a.region + " matches the base_url"
	}
	return false, ""
}

// ListModels lists the model ids the provider's Bedrock host serves.
func (a *Auth) ListModels(ctx context.Context) ([]string, error) {
	return a.listModels(ctx)
}

// Check fetches credentials once, so a caller can tell at boot whether this
// provider can authenticate at all. It reports where they came from, never
// the credentials.
func (a *Auth) Check(ctx context.Context) (source string, err error) {
	c, err := a.credentials(ctx)
	if err != nil {
		return "", err
	}
	return c.Source, nil
}

func (a *Auth) credentials(ctx context.Context) (Credentials, error) {
	c, err := a.src.Retrieve(ctx)
	if err != nil {
		return Credentials{}, err
	}
	a.secret(c.SecretAccessKey)
	a.secret(c.SessionToken)
	return c, nil
}

// bedrockToken returns a minted key with life left in it, minting a new one
// once tokenRefreshFraction of its lifetime has passed. One mint at a time:
// a burst of calls at the refresh point mints once.
func (a *Auth) bedrockToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if a.token != "" {
		life := a.tokenUntil.Sub(a.tokenIssued)
		if now.Before(a.tokenIssued.Add(time.Duration(float64(life) * tokenRefreshFraction))) {
			return a.token, nil
		}
	}
	c, err := a.credentials(ctx)
	if err != nil {
		return "", err
	}
	tok, until, err := MintBedrockToken(c, a.region, maxBedrockTokenLifetime, now)
	if err != nil {
		return "", err
	}
	a.secret(tok)
	a.token, a.tokenIssued, a.tokenUntil = tok, now, until
	return tok, nil
}
