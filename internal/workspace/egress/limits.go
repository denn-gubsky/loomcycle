package egress

import (
	"io"
	"sync"
	"time"
)

// Defaults of Limits (O8 of the P4 plan).
const (
	DefaultRatePerMinute = 600
	DefaultMaxBytes      = int64(4) << 30
)

// Limits bound one workspace's use of one route, in this process: a replica
// keeps its own count (like the token budgets, advisory across replicas).
type Limits struct {
	// RatePerMinute is the requests a workspace may send through a route
	// in a minute, as a bucket that refills evenly. Default 600.
	RatePerMinute int
	// MaxBytes is the request and answer bytes a workspace may move
	// through a route over its life. Default 4 GiB.
	MaxBytes int64
}

func (l Limits) resolved() Limits {
	if l.RatePerMinute <= 0 {
		l.RatePerMinute = DefaultRatePerMinute
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultMaxBytes
	}
	return l
}

type usageKey struct{ workspace, route string }

// usage is one workspace's use of one route.
type usage struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	spent  int64
	max    int64
}

// reserve takes one request from the workspace's bucket for the route, or
// reports nil when the bucket is empty or the byte budget is spent.
func (p *Proxy) reserve(workspace, route string) *usage {
	p.mu.Lock()
	k := usageKey{workspace, route}
	u := p.usage[k]
	now := p.now()
	if u == nil {
		u = &usage{tokens: float64(p.limits.RatePerMinute), last: now, max: p.limits.MaxBytes}
		p.usage[k] = u
	}
	p.mu.Unlock()

	rate := float64(p.limits.RatePerMinute)
	u.mu.Lock()
	defer u.mu.Unlock()
	if d := now.Sub(u.last); d > 0 {
		u.tokens = min(rate, u.tokens+d.Minutes()*rate)
		u.last = now
	}
	if u.tokens < 1 || u.spent >= u.max {
		return nil
	}
	u.tokens--
	return u
}

// charge counts n bytes against the budget and reports how many of them fit.
func (u *usage) charge(n int) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	left := u.max - u.spent
	if left <= 0 {
		return 0
	}
	if int64(n) > left {
		n = int(left)
	}
	u.spent += int64(n)
	return n
}

// Forget drops what the proxy counts for a workspace; its owner calls it when
// the workspace is released.
func (p *Proxy) Forget(workspace string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.usage {
		if k.workspace == workspace {
			delete(p.usage, k)
		}
	}
}

// countingReader charges a request body to the budget as the transport reads
// it, and ends it at the budget.
type countingReader struct {
	r io.Reader
	u *usage
	a *auditor
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if n > 0 {
		ok := c.u.charge(n)
		c.a.bytes.Add(int64(ok))
		if ok < n {
			return ok, ErrByteBudget
		}
	}
	return n, err
}

// countingBody charges an answer to the budget as the session reads it, and
// cuts it at the budget.
type countingBody struct {
	rc io.ReadCloser
	u  *usage
	a  *auditor
}

func (c *countingBody) Read(b []byte) (int, error) {
	n, err := c.rc.Read(b)
	if n > 0 {
		ok := c.u.charge(n)
		c.a.bytes.Add(int64(ok))
		if ok < n {
			return ok, ErrByteBudget
		}
	}
	return n, err
}

func (c *countingBody) Close() error { return c.rc.Close() }
