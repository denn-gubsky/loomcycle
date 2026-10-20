package providers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrAuthUnavailable wraps a failure to authenticate a call at all (no AWS
// credentials could be obtained). It is operator configuration, like a 401,
// not an outage another attempt would ride out.
var ErrAuthUnavailable = errors.New("provider authentication unavailable")

// maxAuthErrorPeek bounds how much of an auth error body is read to decide
// whether it says the credentials had expired.
const maxAuthErrorPeek = 8 << 10

// DoWithAuth sends the request build makes, authenticated by auth. build must
// return a fresh request each time (a body reader is consumed by the send).
// When the answer says the credentials had expired, between being fetched and
// being used, it drops them and sends once more with fresh ones. Any other
// answer, a denial included, is returned as it came.
func DoWithAuth(client *http.Client, auth RequestAuth, body []byte, build func() (*http.Request, error), setKey func(*http.Request, string)) (*http.Response, error) {
	for try := 0; ; try++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		if err := auth.Apply(req.Context(), req, body, setKey); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
		}
		resp, err := client.Do(req)
		if err != nil || try > 0 || !isAuthFailureStatus(resp.StatusCode) {
			return resp, err
		}
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, maxAuthErrorPeek))
		_ = resp.Body.Close()
		expired, hint := auth.Classify(resp.StatusCode, peek)
		if !expired {
			if hint != "" {
				peek = append(peek, []byte(" ("+hint+")")...)
			}
			resp.Body = io.NopCloser(bytes.NewReader(peek))
			return resp, nil
		}
		auth.Invalidate()
	}
}

func isAuthFailureStatus(code int) bool {
	return code == http.StatusBadRequest || code == http.StatusUnauthorized || code == http.StatusForbidden
}
