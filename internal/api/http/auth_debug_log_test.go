package http

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// A rejected credential's server-side line follows the log level: absent at
// info, present at debug. The 401 itself is the same either way. Not
// parallel: it swaps the default logger.
func TestAuth_RejectedCredentialIsLoggedOnlyAtDebugLevel(t *testing.T) {
	const line = "auth: rejected credential"
	for _, tc := range []struct {
		name  string
		level slog.Level
		want  bool
	}{
		{"info", slog.LevelInfo, false},
		{"debug", slog.LevelDebug, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf syncBuffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: tc.level})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			_, _, ts, _ := strictAppFixture(t)
			if status, _ := bearerCall(t, ts, "not-a-token", http.MethodGet, "/v1/_me", ""); status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", status)
			}
			if got := strings.Contains(buf.String(), line); got != tc.want {
				t.Errorf("log holds %q = %v, want %v:\n%s", line, got, tc.want, buf.String())
			}
		})
	}
}
