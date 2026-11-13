package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// defaultResolveEvery is how often a dns:/// target is looked up again.
const defaultResolveEvery = 30 * time.Second

// target is one --dispatcher entry: a gRPC target dialled as it is, or a DNS
// name whose every address is one dispatcher.
type target struct {
	addr string // unix://..., host:port
	// dnsHost and dnsPort are set for dns:///host:port.
	dnsHost, dnsPort string
}

func parseTargets(spec string) ([]target, error) {
	var out []target
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
			out = append(out, target{dnsHost: host, dnsPort: port})
			continue
		}
		out = append(out, target{addr: e})
	}
	if len(out) == 0 {
		return nil, errors.New("worker: no dispatcher address (--dispatcher)")
	}
	return out, nil
}

// serveTargets attaches to every dispatcher and keeps each attached until ctx
// ends. It returns ErrRefused once every dispatcher it was given refused this
// worker: with nothing left to serve, the process should stop and say why.
func (w *worker) serveTargets(ctx context.Context, targets []target) error {
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
		safego.Go(ctx, "conversion.worker.target", func() {
			defer wg.Done()
			if t.dnsHost == "" {
				done(w.attach(ctx, t.addr))
				return
			}
			done(w.followDNS(ctx, t))
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

// followDNS attaches to every address t's name resolves to, attaching to a
// new address as it appears and leaving one that is gone. A lookup that fails
// keeps the attachments it has.
func (w *worker) followDNS(ctx context.Context, t target) error {
	resolve := w.opt.Resolve
	if resolve == nil {
		resolve = net.DefaultResolver.LookupHost
	}
	every := w.opt.ResolveEvery
	if every <= 0 {
		every = defaultResolveEvery
	}
	log := w.log.With("dispatcher", "dns:///"+net.JoinHostPort(t.dnsHost, t.dnsPort))
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
				safego.Go(actx, "conversion.worker.dns-target", func() { a.done <- w.attach(actx, addr) })
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		// An attachment that ended on its own (refused) is dropped, so the
		// next lookup can try it again only if the name still lists it.
		for addr, a := range live {
			select {
			case <-a.done:
				a.cancel()
				delete(live, addr)
			default:
			}
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
