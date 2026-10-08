package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// ollamaPath is Ollama's decision endpoint (Ollama 0.35 and later).
const ollamaPath = "/v1/systemone"

// The bounds /v1/systemone enforces on every model: it refuses a request outside
// them with a 400, so they are checked before a call is spent.
var ollamaLimits = Limits{MaxQuestions: 64, MinOptions: 2, MaxOptions: 26}

func init() {
	Register("ollama", newOllama)
}

type ollamaDriver struct {
	baseURL    string
	providerID string
	apiKey     string
	keyEnvName string
	timeout    time.Duration
	slots      chan struct{}
	client     *http.Client
}

func newOllama(o Options) (Driver, error) {
	if strings.TrimSpace(o.BaseURL) == "" {
		return nil, fmt.Errorf("provider %q has no base URL", o.ProviderID)
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	n := o.MaxConcurrent
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	return &ollamaDriver{
		baseURL: strings.TrimRight(o.BaseURL, "/"), providerID: o.ProviderID,
		apiKey: o.APIKey, keyEnvName: o.KeyEnvName,
		timeout: timeout,
		slots:   make(chan struct{}, n),
		client:  &http.Client{},
	}, nil
}

func (d *ollamaDriver) Limits(string) Limits { return ollamaLimits }

type ollamaRequest struct {
	Model     string              `json:"model"`
	State     map[string]any      `json:"state"`
	Questions map[string]Question `json:"questions"`
}

type ollamaResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (d *ollamaDriver) Decide(ctx context.Context, req Request) (*Response, error) {
	if err := Validate(req, ollamaLimits); err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	case <-callCtx.Done():
		return nil, callError(callCtx, callCtx.Err(), "waiting for a slot")
	}
	// The same key rule as the Ollama chat driver: a keyless endpoint has no
	// operator key to protect and is never restricted; otherwise a tenant's own
	// stored key wins, and a run barred from the operator's key gets no call at all.
	key, source, scopeID := d.apiKey, "operator", ""
	if d.keyEnvName != "" {
		var err error
		if key, source, scopeID, err = providers.ResolveKeyOrOperator(ctx, d.keyEnvName, d.apiKey); err != nil {
			return nil, &Error{Code: CodeCallFailed, Message: "no key for this run", Err: err}
		}
	}
	state := req.State
	if state == nil {
		state = map[string]any{}
	}
	questions := make(map[string]Question, len(req.Questions))
	for name, q := range req.Questions {
		if bytes.Equal(bytes.TrimSpace(q.Criteria), []byte("null")) {
			q.Criteria = nil // an absent noul criteria is left out, not sent as null
		}
		questions[name] = q
	}
	body, err := json.Marshal(ollamaRequest{Model: req.Model, State: state, Questions: questions})
	if err != nil {
		return nil, &Error{Code: CodeBadQuestion, Message: "the state or criteria is not JSON", Err: err}
	}
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, d.baseURL+ollamaPath, bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Code: CodeCallFailed, Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, callError(callCtx, err, ollamaPath)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, callError(callCtx, err, ollamaPath)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ollamaRefusal(resp.StatusCode, raw)
	}
	var out ollamaResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &Error{Code: CodeCallFailed, Message: ollamaPath + ": the reply is not JSON", Err: err}
	}
	answers := make(map[string]Answer, len(out.Answers))
	for name, rawAnswer := range out.Answers {
		var a Answer
		if err := json.Unmarshal(rawAnswer, &a); err != nil {
			return nil, &Error{Code: CodeCallFailed, Question: name, Message: ollamaPath + ": the answer is not readable", Err: err}
		}
		a.Raw = rawAnswer
		answers[name] = a
	}
	return &Response{
		Provider: d.providerID, Model: out.Model, Answers: answers,
		Usage: providers.Usage{
			InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens,
			Model: req.Model, Provider: d.providerID,
			CredentialSource: source, CredentialScopeID: scopeID,
		},
	}, nil
}

// callError reports a call cut off by the driver's own timeout as CodeTimeout,
// wrapping context.DeadlineExceeded whatever text the transport wrapped it in,
// and anything else as CodeCallFailed.
func callError(callCtx context.Context, err error, what string) error {
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		if !errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%v: %w", err, context.DeadlineExceeded)
		}
		return &Error{Code: CodeTimeout, Message: what, Err: err}
	}
	return &Error{Code: CodeCallFailed, Message: what, Err: err}
}

var (
	// "prompt 0 has 12144 tokens; expected 1–8194 (input is never truncated)"
	ollamaTooLarge = regexp.MustCompile(`has (\d+) tokens; expected \d+\D+?(\d+)`)
	// `question "route": …`
	ollamaQuestion = regexp.MustCompile(`question "([^"]*)"`)
	// "must contain 1–64 fields", "must contain 2–26 candidates"
	ollamaRange = regexp.MustCompile(`must contain (\d+)\D+?(\d+)`)
)

// ollamaRefusal maps a non-200 reply to its code. Ollama says what is wrong only
// in the message text, so the text is what is matched; a refusal that matches
// nothing is CodeCallFailed with the message kept.
func ollamaRefusal(status int, raw []byte) error {
	msg := strings.TrimSpace(string(raw))
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error != "" {
		msg = body.Error
	}
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300]) + "…"
	}
	e := &Error{Code: CodeCallFailed, Message: fmt.Sprintf("%s: HTTP %d: %s", ollamaPath, status, msg)}
	// A question's name is the caller's own word and may say anything, "tokens"
	// included, so it is taken out before the rest of the message is matched.
	if m := ollamaQuestion.FindStringSubmatch(msg); m != nil {
		e.Question = m[1]
		msg = strings.Replace(msg, m[0], "", 1)
	}
	switch {
	case status == http.StatusBadRequest && strings.Contains(msg, "tokens"):
		e.Code = CodePromptTooLarge
		if m := ollamaTooLarge.FindStringSubmatch(msg); m != nil {
			e.Tokens, _ = strconv.Atoi(m[1])
			e.Limit, _ = strconv.Atoi(m[2])
		}
	case status == http.StatusBadRequest && strings.Contains(msg, "questions must contain"):
		e.Code = CodeTooManyQuestions
		if m := ollamaRange.FindStringSubmatch(msg); m != nil {
			e.Limit, _ = strconv.Atoi(m[2])
		}
	case status == http.StatusBadRequest && strings.Contains(msg, "criteria must contain"):
		e.Code = CodeBadOptions
		if m := ollamaRange.FindStringSubmatch(msg); m != nil {
			e.Min, _ = strconv.Atoi(m[1])
			e.Max, _ = strconv.Atoi(m[2])
		}
	case status == http.StatusBadRequest && (strings.Contains(msg, "type must be") ||
		strings.Contains(msg, "instructions must be") || strings.Contains(msg, "criteria must")):
		e.Code = CodeBadQuestion
	case status == http.StatusNotFound && strings.Contains(msg, "model") && strings.Contains(msg, "not found"):
		e.Code = CodeModelNotFound
	}
	return e
}
