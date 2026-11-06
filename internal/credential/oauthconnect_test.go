package credential

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential/oauthtest"
)

func TestOAuthConnectStart_ReturnsUserCodeAndStoresDeviceCodeSealed(t *testing.T) {
	h := newOAuthHarness(t)
	h.f.Set(func(f *oauthtest.Server) { f.ApproveAfter = 1000 })
	h.gate = make(chan struct{}) // hold the poller at its first wait
	id := oauthID("docs")
	h.create(t, id)
	start, err := h.m.ConnectStart(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if start.UserCode != "ABCD-EFGH" || start.VerificationURI == "" || start.ExpiresIn != 299 || start.Interval != 5 {
		t.Errorf("start = %+v", start)
	}
	raw, _ := json.Marshal(start)
	row, _ := h.st.CredentialDefGet(context.Background(), "t", "tenant", "", "docs")
	for _, dc := range h.f.Secrets() {
		if strings.Contains(string(raw), dc) {
			t.Error("the start answer carries the device code")
		}
		if strings.Contains(string(row.Definition), dc) {
			t.Error("the device code is stored in the clear")
		}
	}
	_, sec := h.secretOf(t, id)
	if sec.DeviceCode == "" || sec.Interval != 5 || !sec.DeviceExpiresAt.Equal(h.clock.now().Add(299*time.Second)) {
		t.Errorf("sealed device state = code? %t interval %d expires %v", sec.DeviceCode != "", sec.Interval, sec.DeviceExpiresAt)
	}
	if m, _ := h.e.Get(context.Background(), id); m.OAuth.Status.State != OAuthPendingConnect {
		t.Errorf("state %q, want pending_connect", m.OAuth.Status.State)
	}
	close(h.gate)
}

func TestOAuthConnectStart_ReusesCachedRegistration(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	h.connect(t, id) // a second connect of the same credential
	if n := h.f.Count("register"); n != 1 {
		t.Errorf("registered %d times, want once", n)
	}
}

func TestOAuthConnectStart_ReRegistersOnlyOnInvalidClient(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	h.f.Set(func(f *oauthtest.Server) { f.InvalidClient = 1 })
	h.connect(t, id)
	if n := h.f.Count("register"); n != 2 {
		t.Errorf("registered %d times, want twice (once more after invalid_client)", n)
	}
	if !slices.Contains(h.sink.actions(), "oauth_reregistered") {
		t.Errorf("actions %v, want oauth_reregistered", h.sink.actions())
	}
	// Twice in a row is not retried a second time.
	h.f.Set(func(f *oauthtest.Server) { f.InvalidClient = 2 })
	if _, err := h.m.ConnectStart(context.Background(), id); err == nil {
		t.Error("a second invalid_client was retried into success")
	}
	if n := h.f.Count("register"); n != 3 {
		t.Errorf("registered %d times, want 3", n)
	}
}

func TestOAuthPoller_HonoursSlowDownByAddingFiveSeconds(t *testing.T) {
	h := newOAuthHarness(t)
	h.f.Set(func(f *oauthtest.Server) { f.ApproveAfter = 1; f.SlowDowns = 2 })
	id := oauthID("docs")
	h.connect(t, id)
	got := h.waitLog()
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second}
	if !slices.Equal(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
}

func TestOAuthPoller_StopsAtDeviceExpiryWithNeedsReauth(t *testing.T) {
	h := newOAuthHarness(t)
	h.f.Set(func(f *oauthtest.Server) { f.ApproveAfter = 1 << 30 })
	id := oauthID("docs")
	h.create(t, id)
	if _, err := h.m.ConnectStart(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h.awaitPoller(t, id)
	m, _ := h.e.Get(context.Background(), id)
	if m.OAuth.Status.State != OAuthNeedsReauth || !strings.Contains(m.OAuth.Status.LastError, "expired") {
		t.Errorf("status %+v, want needs_reauth for an expired device code", m.OAuth.Status)
	}
	// One poll per interval until the deadline (299 s at 5 s): no more.
	if polls := h.f.Count("token"); polls < 55 || polls > 60 {
		t.Errorf("%d polls, want about 59", polls)
	}
	_, sec := h.secretOf(t, id)
	if sec.DeviceCode != "" {
		t.Error("the expired device code was kept")
	}
	if sec.ClientID == "" {
		t.Error("the registration was dropped")
	}
}

func TestOAuthPoller_AccessDeniedSetsNeedsReauth(t *testing.T) {
	h := newOAuthHarness(t)
	h.f.Set(func(f *oauthtest.Server) { f.DenyDevice = true })
	id := oauthID("docs")
	h.create(t, id)
	if _, err := h.m.ConnectStart(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h.awaitPoller(t, id)
	m, _ := h.e.Get(context.Background(), id)
	if m.OAuth.Status.State != OAuthNeedsReauth || m.OAuth.Status.LastError != "access_denied" {
		t.Errorf("status %+v", m.OAuth.Status)
	}
}

func TestOAuthPoller_SecondStartSupersedesFirst(t *testing.T) {
	h := newOAuthHarness(t)
	h.f.Set(func(f *oauthtest.Server) { f.ApproveAfter = 1 << 30 })
	h.gate = make(chan struct{})
	id := oauthID("docs")
	h.create(t, id)
	if _, err := h.m.ConnectStart(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	<-h.waited
	h.m.mu.Lock()
	first := h.m.pollers[oauthRow{row: rowKey(id)}.key()]
	h.m.mu.Unlock()
	if _, err := h.m.ConnectStart(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the first poller kept running")
	}
	_, sec := h.secretOf(t, id)
	var latest string
	h.f.Set(func(f *oauthtest.Server) { latest = f.IssuedDevice[len(f.IssuedDevice)-1] })
	if sec.DeviceCode != latest {
		t.Error("the stored device code is not the second start's")
	}
	// Let the second one finish: approve now.
	h.f.Set(func(f *oauthtest.Server) { f.ApproveAfter = 0 })
	close(h.gate)
	h.awaitPoller(t, id)
	if m, _ := h.e.Get(context.Background(), id); m.OAuth.Status.State != OAuthConnected {
		t.Errorf("state %q", m.OAuth.Status.State)
	}
}

// A connect started on a replica that is gone is finished by a status
// check: no poller holds its lease any more.
func TestOAuthConnectStatus_CompletesPendingConnectAfterRestart(t *testing.T) {
	h := newOAuthHarness(t)
	h.gate = make(chan struct{})
	id := oauthID("docs")
	h.create(t, id)
	if _, err := h.m.ConnectStart(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	<-h.waited
	// The replica restarts under the same name: its poller is gone.
	h.m.stopPoller(oauthRow{row: rowKey(id)}.key())
	h2 := newOAuthHarnessOn(t, h.st, h.f, "replica-a")
	h2.clock.t = h.clock.now()
	m, err := h2.m.ConnectStatus(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if m.OAuth.Status.State != OAuthConnected {
		t.Errorf("state %q (%s), want connected", m.OAuth.Status.State, m.OAuth.Status.LastError)
	}
	// While another replica's poller holds a live lease, status only reads.
	h3 := newOAuthHarness(t)
	h3.f.Set(func(f *oauthtest.Server) { f.ApproveAfter = 1 << 30 })
	h3.gate = make(chan struct{})
	h3.create(t, id)
	if _, err := h3.m.ConnectStart(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	<-h3.waited
	other := newOAuthHarnessOn(t, h3.st, h3.f, "replica-b")
	other.clock.t = h3.clock.now()
	before := h3.f.Count("token")
	if _, err := other.m.ConnectStatus(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if h3.f.Count("token") != before {
		t.Error("a status check polled beside a live poller")
	}
	close(h3.gate)
}

func TestOAuthConnected_ConnectedAsFromUserinfoOrEmpty(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("plain")
	h.connect(t, id)
	if m, _ := h.e.Get(context.Background(), id); m.OAuth.Status.ConnectedAs != "" {
		t.Errorf("connected_as %q without openid", m.OAuth.Status.ConnectedAs)
	}
	if h.f.Count("userinfo") != 0 {
		t.Error("userinfo was asked without openid granted")
	}
	h.f.Set(func(f *oauthtest.Server) { f.GrantOpenID = true })
	id2 := oauthID("oidc")
	h.connect(t, id2)
	if m, _ := h.e.Get(context.Background(), id2); m.OAuth.Status.ConnectedAs != "someone@example.test" {
		t.Errorf("connected_as %q", m.OAuth.Status.ConnectedAs)
	}
}

func TestOAuthDisconnect_RevokesClearsSecretAndSetsRevoked(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	_, before := h.secretOf(t, id)
	m, err := h.m.Disconnect(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if m.OAuth.Status.State != OAuthRevoked {
		t.Errorf("state %q", m.OAuth.Status.State)
	}
	var revoked bool
	h.f.Set(func(f *oauthtest.Server) { revoked = slices.Contains(f.Revoked, before.RefreshToken) })
	if !revoked {
		t.Error("the refresh token was not revoked")
	}
	_, after := h.secretOf(t, id)
	if after.RefreshToken != "" || after.AccessToken != "" {
		t.Error("the tokens were kept")
	}
	if after.ClientID == "" {
		t.Error("the registration was dropped")
	}
}

func TestOAuthDisconnect_RevocationFailureStillClearsState(t *testing.T) {
	h := newOAuthHarness(t)
	id := oauthID("docs")
	h.connect(t, id)
	h.f.Set(func(f *oauthtest.Server) { f.Revoke500 = true })
	m, err := h.m.Disconnect(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	_, after := h.secretOf(t, id)
	if m.OAuth.Status.State != OAuthRevoked || after.RefreshToken != "" {
		t.Errorf("state %q, refresh kept %t", m.OAuth.Status.State, after.RefreshToken != "")
	}
}

func TestOAuthConnectStart_NotAnOAuthCredentialIsRefused(t *testing.T) {
	h := newOAuthHarness(t)
	if _, err := h.e.PutInline(context.Background(), oauthID("plain"), "v", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.ConnectStart(context.Background(), oauthID("plain")); !errors.Is(err, ErrNotOAuth) {
		t.Errorf("err = %v, want ErrNotOAuth", err)
	}
}
