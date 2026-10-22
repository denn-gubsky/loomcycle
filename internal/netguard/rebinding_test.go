package netguard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// sequenceResolver answers each lookup of a host with the next answer in its
// list, the way a DNS server under an attacker's control rebinds a name
// between one lookup and the next. It counts the lookups.
type sequenceResolver struct {
	mu      sync.Mutex
	answers map[string][][]string
	lookups int
}

func (r *sequenceResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seq := r.answers[host]
	if r.lookups >= len(seq) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	answer := seq[r.lookups]
	r.lookups++
	out := make([]net.IPAddr, 0, len(answer))
	for _, a := range answer {
		out = append(out, net.IPAddr{IP: net.ParseIP(a)})
	}
	return out, nil
}

// TestGuardedDial_RebindingIsCheckedPerConnection: DNS rebinding is a name
// that answers one address when it is checked and another when it is used.
// The guard resolves once per dial, filters that answer and connects to the
// address it checked, never to the name, so each connection is judged on the
// address it actually reaches. Here a vouched name first answers loopback
// (allowed for a vouched name; a test server listens there) and then the
// cloud metadata address (never allowed for a vouched name): the first dial
// connects, the second is refused, and each dial looked the name up exactly
// once.
func TestGuardedDial_RebindingIsCheckedPerConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	res := &sequenceResolver{answers: map[string][][]string{
		"peer.vouched.test": {
			{"127.0.0.1"},
			{"169.254.169.254"},
			// One answer mixing both: only the allowed address is dialed.
			{"169.254.169.254", "127.0.0.1"},
		},
	}}
	dial := guardedDialContext(res, false, []string{"vouched.test"})
	dialOnce := func() (net.Conn, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return dial(ctx, "tcp", net.JoinHostPort("peer.vouched.test", port))
	}

	conn, err := dialOnce()
	if err != nil {
		t.Fatalf("first answer (loopback, vouched): %v, want a connection", err)
	}
	if got := conn.RemoteAddr().String(); got != srv.Listener.Addr().String() {
		t.Errorf("first dial reached %s, want the address it checked, %s", got, srv.Listener.Addr())
	}
	_ = conn.Close()

	if conn, err := dialOnce(); !isGuardRefusal(err) {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("rebound answer (metadata address): err = %v, want a guard refusal", err)
	}

	conn, err = dialOnce()
	if err != nil {
		t.Fatalf("mixed answer: %v, want a connection to the allowed address", err)
	}
	if got := conn.RemoteAddr().String(); got != srv.Listener.Addr().String() {
		t.Errorf("mixed answer reached %s, want only the allowed address %s", got, srv.Listener.Addr())
	}
	_ = conn.Close()

	if res.lookups != 3 {
		t.Errorf("the name was looked up %d times for 3 dials; want exactly one lookup per dial", res.lookups)
	}
}
