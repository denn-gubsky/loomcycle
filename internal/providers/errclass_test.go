package providers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestClassifyError_Table(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorClass
	}{
		// Status-code paths — the headline matrix.
		{"anthropic 429", fmt.Errorf("anthropic 429: rate limit exceeded"), ErrorClassRetryable},
		{"openai 429", fmt.Errorf("openai 429: throttled"), ErrorClassRetryable},
		{"deepseek 500", fmt.Errorf("deepseek 500: internal server error"), ErrorClassRetryable},
		{"gemini 503", fmt.Errorf("gemini 503: backend unavailable"), ErrorClassRetryable},
		{"ollama 502", fmt.Errorf("ollama 502: bad gateway"), ErrorClassRetryable},
		{"openai 504", fmt.Errorf("openai 504: gateway timeout"), ErrorClassRetryable},
		// Permanent — would fail on next provider too.
		{"anthropic 400", fmt.Errorf("anthropic 400: bad request"), ErrorClassPermanent},
		{"openai 401", fmt.Errorf("openai 401: invalid api key"), ErrorClassPermanent},
		{"deepseek 403", fmt.Errorf("deepseek 403: forbidden"), ErrorClassPermanent},
		{"gemini 422", fmt.Errorf("gemini 422: unprocessable entity"), ErrorClassPermanent},
		{"openai 404", fmt.Errorf("openai 404: model not found"), ErrorClassPermanent},
		// No AWS credentials for the call (RFC EM): the driver wraps it as a
		// transport error ("http: ..."), which must not make it retryable.
		{"aws auth unavailable", fmt.Errorf("http: %w", fmt.Errorf("%w: aws credentials unavailable: no EC2 IMDS role found", ErrAuthUnavailable)), ErrorClassPermanent},
		// Deprecated — distinct from generic 404. Body contains a
		// retirement marker. The real exemplar from 2026-05-15 when
		// gemini-2.0-flash was retired:
		{"gemini 404 deprecated", fmt.Errorf(`gemini 404: {"error":{"code":404,"message":"This model models/gemini-2.0-flash is no longer available to new users. Please update your code to use a newer model","status":"NOT_FOUND"}}`), ErrorClassDeprecated},
		{"anthropic 404 deprecated", fmt.Errorf("anthropic 404: this model has been deprecated"), ErrorClassDeprecated},
		{"openai 404 retired", fmt.Errorf("openai 404: model retired, please use the latest version"), ErrorClassDeprecated},
		// 404 without a retirement marker stays Permanent (typo / wrong
		// model id). False-negative on deprecation is acceptable UX.
		{"openai 404 plain", fmt.Errorf("openai 404: not found"), ErrorClassPermanent},
		// 404 mentioning an unrelated deprecated PARAMETER (not the
		// model itself) must stay Permanent. The pattern set
		// deliberately anchors on "model" or "no longer available"
		// rather than the bare phrase "is deprecated" to avoid this
		// false-positive (review finding on PR #111).
		{"openai 404 parameter deprecation note", fmt.Errorf("openai 404: artifact not found; note: parameter X is deprecated, use Y"), ErrorClassPermanent},
		// Ctx-side outcomes.
		{"ctx canceled", context.Canceled, ErrorClassCancelled},
		{"ctx canceled wrapped", fmt.Errorf("call: %w", context.Canceled), ErrorClassCancelled},
		{"ctx deadline exceeded", context.DeadlineExceeded, ErrorClassDeadlineExceeded},
		// v0.8.1 stream-idle: outer ctx is fine, body-wrap canceled
		// MID-STREAM. errors.Is reports DeadlineExceeded but we treat
		// as Retryable — different provider may be healthy.
		{"stream-idle", fmt.Errorf("provider error: stream read: context deadline exceeded"), ErrorClassRetryable},
		// Transport.
		{"http wrapped", fmt.Errorf("http: connection refused"), ErrorClassRetryable},
		{"net.OpError wrapped", fmt.Errorf("dial: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")}), ErrorClassRetryable},
		// Garbage in → unknown out (loop treats as non-retryable —
		// safer to surface than silently cascade).
		{"unknown plain", errors.New("something weird happened"), ErrorClassUnknown},
		{"nil", nil, ErrorClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(tc.err)
			if got != tc.want {
				t.Errorf("ClassifyError(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyError_StreamIdleHasPriorityOverDeadlineExceeded pins
// the order-of-checks invariant: the v0.8.1 stream-idle marker MUST
// be detected before the generic DeadlineExceeded branch fires, even
// though the wrapped error chain satisfies errors.Is(...,
// context.DeadlineExceeded). Without this priority, every stream-idle
// would be misclassified as caller-deadline and lose the retry.
func TestClassifyError_StreamIdleHasPriorityOverDeadlineExceeded(t *testing.T) {
	wrapped := fmt.Errorf("stream read: context deadline exceeded: %w", context.DeadlineExceeded)
	if got := ClassifyError(wrapped); got != ErrorClassRetryable {
		t.Errorf("got %s; want retryable (stream-idle must beat ctx.DeadlineExceeded)", got)
	}
}

// TestIsRateLimit pins the 429-specific helper used by the loop to
// skip resolver-matrix stall feedback on rate-limit errors (without
// affecting the 5xx case). Loop test in internal/loop/loop_test.go
// covers the runtime side; this test pins the wire detection.
func TestIsRateLimit(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"anthropic 429", errors.New("anthropic 429: rate limit exceeded"), true},
		{"openai 429", errors.New("openai 429: too many requests"), true},
		{"deepseek 429", errors.New("deepseek 429: rate limited"), true},
		{"anthropic 500 (NOT rate-limit)", errors.New("anthropic 500: internal"), false},
		{"anthropic 503 (NOT rate-limit)", errors.New("anthropic 503: backend unavailable"), false},
		{"anthropic 400 (NOT rate-limit)", errors.New("anthropic 400: bad request"), false},
		{"plain wrapped error", errors.New("connection refused"), false},
		{"context canceled", context.Canceled, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"429 in body but not prefix", errors.New("got body: error 429"), false},
		// Wrapped-driver cases: DeepSeek delegates to the OpenAI
		// inner driver, so its 429 surfaces with an "openai 429:"
		// prefix even though the matrix entry is "deepseek". Same
		// for anthropic-oauth-dev wrapping anthropic. IsRateLimit
		// looks at the status code only, so detection works either
		// way; the resolver-side matrix entry is keyed by what the
		// loop passes via opts.Provider.ID().
		{"openai 429 (also deepseek wrap)", errors.New("openai 429: rate limited"), true},
		{"anthropic 429 (also oauth-dev wrap)", errors.New("anthropic 429: limit exceeded"), true},
		// Ollama uses hyphenated provider IDs ("ollama" or
		// "ollama-local"). The regex accepts hyphens — verify both.
		{"ollama 429", errors.New("ollama 429: too many concurrent requests"), true},
		{"ollama-local 429", errors.New("ollama-local 429: too many requests"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRateLimit(tc.err); got != tc.want {
				t.Errorf("IsRateLimit(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestErrorClass_StringIsHumanReadable(t *testing.T) {
	cases := map[ErrorClass]string{
		ErrorClassUnknown:          "unknown",
		ErrorClassRetryable:        "retryable",
		ErrorClassPermanent:        "permanent",
		ErrorClassCancelled:        "cancelled",
		ErrorClassDeadlineExceeded: "deadline_exceeded",
		ErrorClassDeprecated:       "deprecated",
	}
	for cls, want := range cases {
		if got := cls.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", cls, got, want)
		}
	}
}

// StallsModel decides what the shared resolver matrix hears about a failed
// call. A request error must leave the model alone; a model or provider fault,
// and a model the provider does not have, must stall it.
func TestStallsModel_OnlyModelFaultsStall(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("anthropic 400: invalid_request_error: `temperature` is deprecated for this model."), false},
		{errors.New("anthropic 401: invalid x-api-key"), false},
		{errors.New("anthropic 403: forbidden"), false},
		{errors.New("openai 422: unprocessable"), false},
		{errors.New("anthropic 413: request too large"), false},
		{fmt.Errorf("call: %w", ErrOperatorKeyForbidden), false},
		{errors.New("anthropic 404: model: claude-nope not found"), true},
		{errors.New("gemini 404: model gemini-2.0-flash is no longer available"), true},
		{errors.New("anthropic 500: upstream timeout"), true},
		{errors.New("anthropic 529: overloaded_error"), true},
		{errors.New("http: dial tcp: connection refused"), true},
		{errors.New("something unrecognised"), true},
	} {
		if got := StallsModel(tc.err); got != tc.want {
			t.Errorf("StallsModel(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// Only a refusal of the OPERATOR's own credentials says the provider is
// unusable for every run; a tenant's or user's key refused says nothing about
// anyone else, and a non-auth status says nothing about the key.
func TestOperatorKeyRefused_OnlyOperatorAuthRefusals(t *testing.T) {
	op401 := WithKeySource(errors.New("anthropic 401: invalid x-api-key"), "operator")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"operator 401", op401, true},
		{"operator 403 naming the key", WithKeySource(errors.New(`anthropic 403: {"message":"UnrecognizedClientException: The security token included in the request is invalid."}`), "operator"), true},
		{"operator 403 bad signature, message only", WithKeySource(errors.New(`anthropic 403: {"message":"The request signature we calculated does not match the signature you provided."}`), "operator"), true},
		{"operator 400 Gemini bad key", WithKeySource(errors.New(`gemini 400: {"error":{"status":"INVALID_ARGUMENT","details":[{"reason":"API_KEY_INVALID"}]}}`), "operator"), true},
		{"operator 401 OpenAI", WithKeySource(errors.New(`openai 401: {"error":{"code":"invalid_api_key"}}`), "operator"), true},
		{"operator 403 plain forbidden", WithKeySource(errors.New("openai 403: forbidden"), "operator"), false},
		{"operator 403 model access denied", WithKeySource(errors.New(`anthropic 403: {"message":"AccessDeniedException: You don't have access to the model with the specified model ID."}`), "operator"), false},
		{"operator 403 Marketplace", WithKeySource(errors.New(`anthropic 403: {"message":"Model access is denied due to IAM user or service role is not authorized to perform the required AWS Marketplace actions (aws-marketplace:ViewSubscriptions, aws-marketplace:Subscribe)"}`), "operator"), false},
		{"operator 429 quota", WithKeySource(errors.New(`openai 429: {"error":{"code":"insufficient_quota"}}`), "operator"), false},
		{"wrapped operator 401", fmt.Errorf("call: %w", op401), true},
		{"no AWS credentials", fmt.Errorf("http: %w", fmt.Errorf("%w: no creds", ErrAuthUnavailable)), true},
		{"tenant 401", WithKeySource(errors.New("anthropic 401: invalid x-api-key"), "tenant"), false},
		{"user 403", WithKeySource(errors.New("gemini 403: forbidden"), "user"), false},
		{"operator 400", WithKeySource(errors.New("anthropic 400: invalid_request_error"), "operator"), false},
		{"operator 500", WithKeySource(errors.New("anthropic 500: boom"), "operator"), false},
		{"untagged 401", errors.New("anthropic 401: invalid x-api-key"), false},
	} {
		if got := OperatorKeyRefused(tc.err); got != tc.want {
			t.Errorf("%s: OperatorKeyRefused = %v, want %v", tc.name, got, tc.want)
		}
	}
	// The tag must not change what the error says or how it classifies.
	if op401.Error() != "anthropic 401: invalid x-api-key" || ClassifyError(op401) != ErrorClassPermanent {
		t.Errorf("tagged error = %q / %v, want the driver's message and class unchanged", op401.Error(), ClassifyError(op401))
	}
}

// The confirming probe's error comes from the driver's model listing, whose
// messages carry the status in a path-qualified form. Only a refusal that is
// clearly about the key counts.
func TestIsKeyRefusal_ProbeErrorShapes(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{`anthropic /v1/models: status 401 ({"type":"error","error":{"type":"authentication_error"}})`, true},
		{`openai /v1/models: status 401 (Unauthorized)`, true},
		{`ollama /api/tags: status 401 (unauthorized)`, true},
		{`gemini /models 400: {"error":{"details":[{"reason":"API_KEY_INVALID"}]}}`, true},
		{`bedrock model listing: /inference-profiles: status 403 ({"message":"The security token included in the request is invalid."})`, true},
		{`bedrock model listing: /inference-profiles: status 403 ({"message":"The security token included in the request is expired"})`, true},
		{`bedrock model listing: /inference-profiles: status 403 ({"message":"User is not authorized to perform: bedrock:ListInferenceProfiles"})`, false},
		{`openai /v1/models: status 500 (boom)`, false},
		{`openai /v1/models: Get "http://h:4010/v1/models": EOF`, false},
		{`ollama /api/tags: dial tcp 10.0.0.1:11434: connection refused`, false},
	} {
		if got := IsKeyRefusal(errors.New(tc.msg)); got != tc.want {
			t.Errorf("IsKeyRefusal(%s) = %v, want %v", tc.msg, got, tc.want)
		}
	}
	if !IsKeyRefusal(fmt.Errorf("x: %w", ErrAuthUnavailable)) || IsKeyRefusal(nil) {
		t.Error("ErrAuthUnavailable must count as a key refusal; nil must not")
	}
}
