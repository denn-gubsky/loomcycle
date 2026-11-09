package otel

import (
	"context"
	"log/slog"
	"net/url"
	"regexp"

	"github.com/go-logr/logr"
)

// sdkLogSink routes the OpenTelemetry SDK's internal logger to slog with the
// message only. The SDK passes key/values and errors that can carry
// configuration verbatim — a malformed collector header is logged with its
// value — and its default logger writes them through the stdlib log package,
// past slog's redaction. Every argument but the static message is dropped, so
// no SDK path can print a value.
type sdkLogSink struct {
	log *slog.Logger
}

// newSDKLogger is the logr.Logger to install with otel.SetLogger.
func newSDKLogger(log *slog.Logger) logr.Logger { return logr.New(sdkLogSink{log: log}) }

// sdkLevel maps logr verbosity as the SDK uses it (0-1 for warnings, 4 info,
// 8 debug) onto slog.
func sdkLevel(v int) slog.Level {
	if v <= 1 {
		return slog.LevelWarn
	}
	return slog.LevelDebug
}

func (sdkLogSink) Init(logr.RuntimeInfo) {}

func (s sdkLogSink) Enabled(v int) bool {
	return s.log.Enabled(context.Background(), sdkLevel(v))
}

func (s sdkLogSink) Info(v int, msg string, _ ...any) {
	s.log.Log(context.Background(), sdkLevel(v), "otel sdk: "+msg)
}

// Error drops err too: its text can quote the input it failed on.
func (s sdkLogSink) Error(_ error, msg string, _ ...any) {
	s.log.Warn("otel sdk: " + msg)
}

func (s sdkLogSink) WithValues(...any) logr.LogSink { return s }
func (s sdkLogSink) WithName(string) logr.LogSink   { return s }

// urlInText finds URLs in an exporter's error text ("Post \"http://…\": …").
var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

// sanitizeError is err's text with every URL's userinfo removed and its query
// values masked: the endpoint an operator configured may carry credentials,
// and the exporter quotes it in a failed request's error.
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return urlInText.ReplaceAllStringFunc(err.Error(), func(raw string) string {
		u, perr := url.Parse(raw)
		if perr != nil {
			return "[url]"
		}
		u.User = nil
		if u.RawQuery != "" {
			q := u.Query()
			for k := range q {
				q[k] = []string{"redacted"}
			}
			u.RawQuery = q.Encode()
		}
		u.Fragment, u.RawFragment = "", ""
		return u.String()
	})
}
