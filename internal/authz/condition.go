package authz

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// Transports a condition may name. The surface sets Context.Transport from
// where the request arrived, never from its body.
var Transports = []string{"http", "grpc", "mcp", "webui", "a2a", "webhook", "schedule", "bridge"}

// Condition is the closed `when:` set of RFC EY §3.5 (owner decision D5: no
// expression language in v1). Every field narrows; an absent field is
// always true. A key this build does not know is refused by the strict JSON
// decoder in ParseACL, at write and at read.
type Condition struct {
	// Transport: the request arrived on one of these.
	Transport []string `json:"transport,omitempty"`
	// Time: a wall-clock window.
	Time *TimeWindow `json:"time,omitempty"`
	// Interactive: the run is (or is not) interactive.
	Interactive *bool `json:"interactive,omitempty"`
	// Tier / UserTier: the resolved routing tier is one of these.
	Tier     []string `json:"tier,omitempty"`
	UserTier []string `json:"user_tier,omitempty"`
	// SourceCIDR: the client address is inside one of these.
	SourceCIDR []string `json:"source_cidr,omitempty"`
	// OwnerIsSelf: the resource's owner is (or is not) the principal.
	OwnerIsSelf *bool `json:"owner_is_self,omitempty"`
	// Agent: inside a run, the run's agent matches this glob.
	Agent string `json:"agent,omitempty"`
	// Depth: the run's spawn depth is at most Max (the delegation-hop cap).
	Depth *DepthBound `json:"depth,omitempty"`
}

// TimeWindow is `{days, between, tz}`: the local time in tz is on one of the
// days and inside [between[0], between[1]).
type TimeWindow struct {
	Days    []string `json:"days,omitempty"`
	Between []string `json:"between,omitempty"`
	TZ      string   `json:"tz,omitempty"`
}

// DepthBound is `{max}`.
type DepthBound struct {
	Max int `json:"max"`
}

var weekdays = map[string]time.Weekday{
	"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday,
	"fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday,
}

// Validate checks a condition as a caller supplied it. A condition with no
// field is refused: `when: {}` reads as "always" to a human and as a typo to
// a reviewer, so it is not stored.
func (c *Condition) Validate() error {
	if c == nil {
		return nil
	}
	fields := 0
	if len(c.Transport) > 0 {
		fields++
		for _, t := range c.Transport {
			if !contains(Transports, t) && !strings.HasPrefix(t, "bridge:") {
				return fmt.Errorf("transport %q is not one of %s (or bridge:<name>)", t, strings.Join(Transports, ", "))
			}
		}
	}
	if c.Time != nil {
		fields++
		if err := c.Time.validate(); err != nil {
			return err
		}
	}
	if c.Interactive != nil {
		fields++
	}
	if len(c.Tier) > 0 {
		fields++
	}
	if len(c.UserTier) > 0 {
		fields++
	}
	if len(c.SourceCIDR) > 0 {
		fields++
		for _, s := range c.SourceCIDR {
			if _, err := netip.ParsePrefix(s); err != nil {
				return fmt.Errorf("source_cidr %q: %w", s, err)
			}
		}
	}
	if c.OwnerIsSelf != nil {
		fields++
	}
	if c.Agent != "" {
		fields++
		if _, _, err := parseGlob(c.Agent, '/'); err != nil {
			return fmt.Errorf("agent: %w", err)
		}
	}
	if c.Depth != nil {
		fields++
		if c.Depth.Max < 0 {
			return fmt.Errorf("depth.max must be >= 0")
		}
	}
	if fields == 0 {
		return fmt.Errorf("a condition needs at least one field; omit `when` for always")
	}
	return nil
}

func (w *TimeWindow) validate() error {
	if len(w.Days) == 0 && len(w.Between) == 0 {
		return fmt.Errorf("time: needs days or between")
	}
	for _, d := range w.Days {
		if _, ok := weekdays[d]; !ok {
			return fmt.Errorf("time.days: %q is not one of mon..sun", d)
		}
	}
	if len(w.Between) != 0 {
		if len(w.Between) != 2 {
			return fmt.Errorf("time.between: want [\"HH:MM\", \"HH:MM\"]")
		}
		for _, s := range w.Between {
			if _, err := parseHM(s); err != nil {
				return fmt.Errorf("time.between: %w", err)
			}
		}
	}
	if w.TZ != "" {
		if _, err := time.LoadLocation(w.TZ); err != nil {
			return fmt.Errorf("time.tz: %w", err)
		}
	}
	return nil
}

func parseHM(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// compiledCondition is a Condition with its parse work done once: the
// time zone loaded, the window minutes computed, the agent glob split and
// the CIDRs parsed. Decide runs holds on every matching rule, so none of
// that may happen per call.
type compiledCondition struct {
	c       *Condition
	loc     *time.Location
	from    int
	to      int
	hasWin  bool
	agent   []string
	prefixe []netip.Prefix
}

// compileCondition prepares c; it assumes c.Validate() passed.
func compileCondition(c *Condition) *compiledCondition {
	if c == nil {
		return nil
	}
	cc := &compiledCondition{c: c, loc: time.UTC}
	if c.Time != nil {
		if c.Time.TZ != "" {
			if l, err := time.LoadLocation(c.Time.TZ); err == nil {
				cc.loc = l
			}
		}
		if len(c.Time.Between) == 2 {
			cc.from, _ = parseHM(c.Time.Between[0])
			cc.to, _ = parseHM(c.Time.Between[1])
			cc.hasWin = true
		}
	}
	if c.Agent != "" {
		cc.agent, _, _ = parseGlob(c.Agent, '/')
	}
	for _, s := range c.SourceCIDR {
		if p, err := netip.ParsePrefix(s); err == nil {
			cc.prefixe = append(cc.prefixe, p)
		}
	}
	return cc
}

// holds reports whether the condition is satisfied by the request. A field
// the request cannot answer (no clock, no source address, no agent) does
// not hold: a condition that could not be checked must never read as true.
func (cc *compiledCondition) holds(req *Request) bool {
	if cc == nil {
		return true
	}
	c := cc.c
	if len(c.Transport) > 0 && !contains(c.Transport, req.Context.Transport) {
		return false
	}
	if c.Time != nil && !cc.timeHolds(req.Context.Now) {
		return false
	}
	if c.Interactive != nil && *c.Interactive != req.Context.Interactive {
		return false
	}
	if len(c.Tier) > 0 && !contains(c.Tier, req.Context.Tier) {
		return false
	}
	if len(c.UserTier) > 0 && !contains(c.UserTier, req.Context.UserTier) {
		return false
	}
	if len(c.SourceCIDR) > 0 && !inAnyPrefix(cc.prefixe, req.Context.SourceIP) {
		return false
	}
	if c.OwnerIsSelf != nil {
		self := req.Resource.Owner != "" && req.Subject.ID != "" && req.Resource.Owner == req.Subject.ID
		if *c.OwnerIsSelf != self {
			return false
		}
	}
	if c.Agent != "" {
		if req.Context.Agent == "" || !doublestarMatch(cc.agent, strings.Split(req.Context.Agent, "/")) {
			return false
		}
	}
	if c.Depth != nil && req.Context.Depth > c.Depth.Max {
		return false
	}
	return true
}

// holdsForScope is holds for a question about EVERY instance of a scope
// (Effective.DecideScope). A field the scope request cannot decide for every
// instance — the owner when the request names none, the agent, tier, user
// tier, source address or clock when the surface supplied none, and the
// interactive bit and spawn depth, which a zero Context cannot tell from
// unknown — is read the safe way round: for a deny it holds (the deny may
// apply to some instance), for an allow it does not (the allow may not cover
// every instance). A field the request does answer is decided as holds does.
func (cc *compiledCondition) holdsForScope(req *Request, deny bool) bool {
	if cc == nil {
		return true
	}
	c := cc.c
	ctx := req.Context
	undecided := (c.OwnerIsSelf != nil && req.Resource.Owner == "") ||
		(len(c.Transport) > 0 && ctx.Transport == "") ||
		(c.Time != nil && ctx.Now.IsZero()) ||
		(len(c.Tier) > 0 && ctx.Tier == "") ||
		(len(c.UserTier) > 0 && ctx.UserTier == "") ||
		(len(c.SourceCIDR) > 0 && ctx.SourceIP == nil) ||
		(c.Agent != "" && ctx.Agent == "") ||
		c.Interactive != nil || c.Depth != nil
	if undecided {
		return deny
	}
	return cc.holds(req)
}

func (cc *compiledCondition) timeHolds(now time.Time) bool {
	if now.IsZero() {
		return false
	}
	t := now.In(cc.loc)
	w := cc.c.Time
	if len(w.Days) > 0 {
		ok := false
		for _, d := range w.Days {
			if weekdays[d] == t.Weekday() {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if cc.hasWin {
		m := t.Hour()*60 + t.Minute()
		if cc.from <= cc.to {
			if m < cc.from || m >= cc.to {
				return false
			}
		} else if m < cc.from && m >= cc.to { // a window over midnight
			return false
		}
	}
	return true
}

func inAnyPrefix(prefixes []netip.Prefix, ip net.IP) bool {
	if ip == nil {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, p := range prefixes {
		if p.Addr().Is4() == addr.Is4() && p.Contains(addr) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
