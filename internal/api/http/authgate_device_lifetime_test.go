package http

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate/device"
)

// RFC FA P5 — a device login hands a CLI a bearer on the strength of one Web
// UI session. That bearer must not outlive the session that approved it, nor
// the credential the session stands on, and must not be issued at all when
// either ended between the approval and the CLI's poll: otherwise a session
// about to expire (or a pasted token about to be retired) could launder itself
// into a fresh, longer-lived token.

// approveAndRedeem runs one device login approved from cookie c, with fn run
// between the approval and the poll.
func approveAndRedeem(t *testing.T, s *Server, c *http.Cookie, between func()) pollAnswer {
	t.Helper()
	ds := startDevice(t, s)
	if rec := decide(s, c, "/v1/_auth/device/approve", ds.UserCode, yes); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if between != nil {
		between()
	}
	if err := forcePollable(s.store, ds.DeviceCode); err != nil {
		t.Fatal(err)
	}
	return pollDevice(t, s, ds.DeviceCode, "loomcycle-cli", "")
}

// lifetimeSlack absorbs the clock moving between the handler computing the
// remaining lifetime and the token row being stamped.
const lifetimeSlack = 2

func TestDeviceLogin_TokenNeverOutlivesTheApprovingSession(t *testing.T) {
	s, st, _ := deviceServer(t)
	// Sessions live two hours; the device gate would issue for twelve.
	s.cfg().ResolvedAuth.Absolute = 2 * time.Hour
	alice, _ := userSession(t, s, st, "acme", "alice", "isolated", "")

	a := approveAndRedeem(t, s, alice, nil)
	if a.Status != http.StatusOK || a.AccessToken == "" {
		t.Fatalf("redeem = %d %q", a.Status, a.Error)
	}
	if max := int64((2 * time.Hour).Seconds()) + lifetimeSlack; a.ExpiresIn > max {
		t.Fatalf("expires_in = %ds, want at most the session's remaining %ds", a.ExpiresIn, max)
	}
}

func TestDeviceLogin_TokenNeverOutlivesTheSessionsOwnToken(t *testing.T) {
	s, st, _ := deviceServer(t)
	// The session was made by pasting a token that expires in an hour.
	alice, _ := userSession(t, s, st, "acme", "alice", "isolated", `{"ttl_seconds":3600}`)

	a := approveAndRedeem(t, s, alice, nil)
	if a.Status != http.StatusOK || a.AccessToken == "" {
		t.Fatalf("redeem = %d %q", a.Status, a.Error)
	}
	if max := int64(3600) + lifetimeSlack; a.ExpiresIn > max {
		t.Fatalf("expires_in = %ds, want at most the pasted token's remaining %ds", a.ExpiresIn, max)
	}
}

func TestDeviceLogin_NoTokenWhenTheApprovingSessionEndedBeforeThePoll(t *testing.T) {
	s, st, _ := deviceServer(t)
	alice, _ := userSession(t, s, st, "acme", "alice", "isolated", "")

	a := approveAndRedeem(t, s, alice, func() {
		// The person signs out (or an operator ends their sessions).
		if _, err := st.UISessionRevokeSubject(context.Background(), "acme", "alice", time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	if a.AccessToken != "" || a.Error != device.ErrAccessDenied {
		t.Fatalf("redeem after the session ended = %d %q token=%v, want access_denied and no token", a.Status, a.Error, a.AccessToken != "")
	}
}

func TestDeviceLogin_NoTokenWhenTheSessionsCredentialWasRetiredBeforeThePoll(t *testing.T) {
	s, st, _ := deviceServer(t)
	alice, tok := userSession(t, s, st, "acme", "alice", "isolated", "")
	p, ok := s.resolvePrincipal(context.Background(), tok)
	if !ok {
		t.Fatal("pasted token does not resolve")
	}

	a := approveAndRedeem(t, s, alice, func() {
		// The token the session was made from is retired; the session row
		// itself is only ended on its next request.
		if err := st.OperatorTokenDefSetRetiredAt(context.Background(), p.TokenDefID, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	if a.AccessToken != "" || a.Error != device.ErrAccessDenied {
		t.Fatalf("redeem after the session's token was retired = %d %q token=%v, want access_denied and no token", a.Status, a.Error, a.AccessToken != "")
	}
}
