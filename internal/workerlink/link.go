// Package workerlink is the link between the core's worker endpoint and the
// workers that attach to it, shared by every worker role: on the worker side,
// the dispatcher targets (a comma list of unix://, host:port and dns:///
// names, the last followed as the name's addresses change), the per-target
// attach loop with its reconnect backoff, and the client TLS; on the core
// side, the endpoint's server TLS and gRPC flow-control settings.
//
// It knows nothing of what a worker does once attached: the role passes a
// Session that runs one attachment over a connection.
package workerlink

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/denn-gubsky/loomcycle/internal/safego"
)

const (
	// DefaultResolveEvery is how often a dns:/// target is looked up again.
	DefaultResolveEvery = 30 * time.Second
	// BackoffMin and BackoffMax bound the wait before attaching again to a
	// dispatcher that was lost. A session that lasted over a minute starts
	// the next wait from BackoffMin again.
	BackoffMin = time.Second
	BackoffMax = 30 * time.Second
)

// ErrRefused is a dispatcher refusing this worker for good (another protocol
// version, a bad token): retrying cannot help. A Session returns an error
// wrapping it to stop attaching to that dispatcher.
var ErrRefused = errors.New("worker: the dispatcher refused this worker")

// Target is one --dispatcher entry: a gRPC target dialled as it is, or a DNS
// name whose every address is one dispatcher.
type Target struct {
	addr string // unix://..., host:port
	// dnsHost and dnsPort are set for dns:///host:port.
	dnsHost, dnsPort string
}

// String is the target as it was written.
func (t Target) String() string {
	if t.dnsHost != "" {
		return "dns:///" + net.JoinHostPort(t.dnsHost, t.dnsPort)
	}
	return t.addr
}

// ParseTargets reads a comma-separated dispatcher list.
func ParseTargets(spec string) ([]Target, error) {
	var out []Target
	for _, e := range strings.Split(spec, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(e, "dns:///"); ok {
			host, port, err := net.SplitHostPort(rest)
			if err != nil || host == "" || port == "" {
				return nil, fmt.Errorf("worker: --dispatcher %q: want dns:///name:port", e)
			}
			out = append(out, Target{dnsHost: host, dnsPort: port})
			continue
		}
		out = append(out, Target{addr: e})
	}
	if len(out) == 0 {
		return nil, errors.New("worker: no dispatcher address (--dispatcher)")
	}
	return out, nil
}

// Session runs one attachment to a dispatcher over conn until it ends. An
// error wrapping ErrRefused stops attaching to that dispatcher; any other
// ends this attachment, and the link attaches again after a backoff.
type Session func(ctx context.Context, conn *grpc.ClientConn, log *slog.Logger) error

// Options configure the link.
type Options struct {
	// TLS, when set, is used to dial every dispatcher that is not a Unix
	// socket (ClientTLS).
	TLS *tls.Config
	// Insecure allows dialling a dispatcher off this host without TLS. Off,
	// such a dispatcher is refused: the worker would send its token, and
	// every file it is handed, in the clear.
	Insecure bool
	// Resolve looks a dns:/// name up; nil is the system resolver.
	Resolve func(ctx context.Context, host string) ([]string, error)
	// ResolveEvery is how often a dns:/// name is looked up again; zero is
	// DefaultResolveEvery.
	ResolveEvery time.Duration
	// Name prefixes the names of the link's goroutines (for example
	// conversion.worker), so a panic report says which role it was.
	Name   string
	Logger *slog.Logger
}

// Serve attaches to every target and keeps each attached until ctx ends,
// running session over each attachment. It returns ErrRefused once every
// dispatcher it was given refused this worker: with nothing left to serve,
// the process should stop and say why.
func Serve(ctx context.Context, targets []Target, opt Options, session Session) error {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Name == "" {
		opt.Name = "workerlink"
	}
	l := &link{opt: opt, session: session}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		refused int
		lastErr error
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := func(err error) {
		if !errors.Is(err, ErrRefused) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		refused++
		lastErr = err
		if refused == len(targets) {
			cancel()
		}
	}
	for _, t := range targets {
		wg.Add(1)
		safego.Go(ctx, opt.Name+".target", func() {
			defer wg.Done()
			if t.dnsHost == "" {
				done(l.attach(ctx, t.addr, ""))
				return
			}
			done(l.followDNS(ctx, t))
		})
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if refused == len(targets) {
		return lastErr
	}
	return nil
}

type link struct {
	opt     Options
	session Session
}

// followDNS attaches to every address t's name resolves to, attaching to a
// new address as it appears and leaving one that is gone. A lookup that fails
// keeps the attachments it has.
func (l *link) followDNS(ctx context.Context, t Target) error {
	resolve := l.opt.Resolve
	if resolve == nil {
		resolve = net.DefaultResolver.LookupHost
	}
	every := l.opt.ResolveEvery
	if every <= 0 {
		every = DefaultResolveEvery
	}
	log := l.opt.Logger.With("dispatcher", t.String())
	type attached struct {
		cancel context.CancelFunc
		done   chan error
	}
	live := map[string]*attached{}
	defer func() {
		for _, a := range live {
			a.cancel()
			<-a.done
		}
	}()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		addrs, err := resolve(ctx, t.dnsHost)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("could not look the dispatchers up; keeping the ones attached", "err", err)
		case err == nil:
			want := map[string]bool{}
			for _, a := range addrs {
				want[net.JoinHostPort(a, t.dnsPort)] = true
			}
			for addr, a := range live {
				if !want[addr] {
					log.Info("a dispatcher left the name; detaching", "addr", addr)
					a.cancel()
					<-a.done
					delete(live, addr)
				}
			}
			for _, addr := range sortedKeys(want) {
				if live[addr] != nil {
					continue
				}
				actx, acancel := context.WithCancel(ctx)
				a := &attached{cancel: acancel, done: make(chan error, 1)}
				live[addr] = a
				safego.Go(actx, l.opt.Name+".dns-target", func() { a.done <- l.attach(actx, addr, t.dnsHost) })
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		// An attachment that ended on its own (refused) is dropped, so the
		// next lookup can try it again only if the name still lists it. When
		// every address the name listed refused this worker, the name is
		// refused: the worker stops and says why rather than idling forever.
		total, refused := len(live), 0
		var lastErr error
		for addr, a := range live {
			select {
			case err := <-a.done:
				a.cancel()
				delete(live, addr)
				if errors.Is(err, ErrRefused) {
					refused++
					lastErr = err
				}
			default:
			}
		}
		if total > 0 && refused == total {
			return lastErr
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// attach serves one dispatcher until ctx ends, reconnecting with backoff. It
// returns ErrRefused when the dispatcher refuses this worker for good.
// serverName is the name a TLS certificate is checked against when the
// target is an address found by name.
func (l *link) attach(ctx context.Context, target, serverName string) error {
	log := l.opt.Logger.With("dispatcher", target)
	creds := insecure.NewCredentials()
	switch {
	case strings.HasPrefix(target, "unix:"):
	case l.opt.TLS != nil:
		c := l.opt.TLS.Clone()
		if c.ServerName == "" {
			c.ServerName = serverName
		}
		creds = credentials.NewTLS(c)
	case !l.opt.Insecure && !plaintextAllowed(target):
		err := fmt.Errorf("%w: %s is off this host and no TLS is configured (set LOOMCYCLE_WORKER_TLS_CA, or LOOMCYCLE_WORKER_INSECURE=1 on a network only the runtime and its workers share)", ErrRefused, target)
		log.Error("refusing to dial a dispatcher in the clear", "err", err)
		return err
	}
	conn, err := grpc.NewClient(target, append(DialOptions(), grpc.WithTransportCredentials(creds))...)
	if err != nil {
		// A target that cannot even be set up will not come right by
		// retrying: count it as a refusal, so a worker left with nothing to
		// serve stops and says why.
		err = fmt.Errorf("%w: %s: %v", ErrRefused, target, err)
		log.Error("cannot dial the dispatcher", "err", err)
		return err
	}
	defer conn.Close()
	backoff := BackoffMin
	for {
		start := time.Now()
		err := l.session(ctx, conn, log)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrRefused) {
			log.Error("the dispatcher refused this worker", "err", err)
			return err
		}
		if time.Since(start) > time.Minute {
			backoff = BackoffMin
		}
		log.Warn("lost the dispatcher; reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, BackoffMax)
	}
}
