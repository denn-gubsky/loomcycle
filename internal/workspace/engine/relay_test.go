package engine

import (
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func needPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 on PATH")
	}
}

// The relay takes a port and nothing else: no argument names a host, so it
// can reach only 127.0.0.1 of the namespace it runs in (P9 plan §3.2,
// D-P9-7). Anything but one plain port is refused before a connection.
func TestRelay_TakesAPortAndNothingElse(t *testing.T) {
	needPython(t)
	for _, args := range [][]string{{"10.0.0.1"}, {"localhost:80"}, {"80 "}, {"²"}, {"0"}, {"70000"}, {"80", "81"}, {}} {
		cmd := exec.Command("python3", append([]string{"-I", "-c", relaySource}, args...)...)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Errorf("relay %q = %v %q, want exit 2", args, err, out)
		}
	}
	// The one address in the source is the loopback's: a change that adds
	// another must change this test too.
	if n := strings.Count(relaySource, "create_connection("); n != 1 || !strings.Contains(relaySource, `create_connection(("127.0.0.1", port)`) {
		t.Errorf("the relay connects somewhere other than 127.0.0.1:<port>")
	}
	for _, other := range []string{"socket.socket(", ".connect(", "getaddrinfo", "bind("} {
		if strings.Contains(relaySource, other) {
			t.Errorf("the relay uses %s", other)
		}
	}
}

// Through the host engine's Start (as the container engine starts it in a
// session), the relay carries bytes both ways at once and ends what it
// sends at CloseWrite; a port nothing answers on is ErrUnreachable.
func TestRelay_CarriesBytesBothWaysAndReportsAnUnansweredPort(t *testing.T) {
	needPython(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	port := l.Addr().(*net.TCPAddr).Port
	h := &Host{}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := h.Start(ctx, StartRequest{Workspace: "ws1", Root: dir, Dir: dir, Argv: RelayArgv(port)})
	if err != nil {
		t.Fatal(err)
	}
	c, err := relayed(ctx, p, port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, word := range []string{"ping", "pong"} {
		if _, err := c.Write([]byte(word)); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(word))
		if _, err := io.ReadFull(c, got); err != nil || string(got) != word {
			t.Fatalf("echo of %q = %q, %v", word, got, err)
		}
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(c); err != nil || len(rest) != 0 {
		t.Errorf("after CloseWrite: %q, %v", rest, err)
	}

	_ = l.Close()
	p2, err := h.Start(ctx, StartRequest{Workspace: "ws1", Root: dir, Dir: dir, Argv: RelayArgv(port)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relayed(ctx, p2, port); !errors.Is(err, ErrUnreachable) {
		t.Errorf("an unanswered port = %v, want ErrUnreachable", err)
	}
}
