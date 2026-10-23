package authz

import (
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func bptr(b bool) *bool { return &b }

func TestCondition_Validate(t *testing.T) {
	good := []Condition{
		{Transport: []string{"http", "bridge:telegram"}},
		{Time: &TimeWindow{Days: []string{"mon"}}},
		{Time: &TimeWindow{Between: []string{"22:00", "06:00"}, TZ: "UTC"}},
		{Interactive: bptr(true)},
		{SourceCIDR: []string{"10.0.0.0/8", "::1/128"}},
		{OwnerIsSelf: bptr(true)},
		{Agent: "ci/*"},
		{Depth: &DepthBound{Max: 2}},
	}
	for _, c := range good {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	bad := map[string]Condition{
		"empty":        {},
		"transport":    {Transport: []string{"carrier-pigeon"}},
		"day":          {Time: &TimeWindow{Days: []string{"monday"}}},
		"between":      {Time: &TimeWindow{Between: []string{"7am", "8am"}}},
		"between-one":  {Time: &TimeWindow{Between: []string{"07:00"}}},
		"tz":           {Time: &TimeWindow{Days: []string{"mon"}, TZ: "Mars/Olympus"}},
		"cidr":         {SourceCIDR: []string{"10.0.0.1"}},
		"agent":        {Agent: "../x"},
		"depth":        {Depth: &DepthBound{Max: -1}},
		"time-nothing": {Time: &TimeWindow{TZ: "UTC"}},
	}
	for name, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: %+v validated", name, c)
		}
	}
}

func TestCondition_Holds(t *testing.T) {
	// Tuesday 2026-10-20 10:30 Kyiv (UTC+3) = 07:30 UTC.
	kyiv, _ := time.LoadLocation("Europe/Kyiv")
	now := time.Date(2026, 10, 20, 10, 30, 0, 0, kyiv)
	base := Request{Subject: Subject{TenantID: "t", ID: "alice"},
		Resource: Resource{Type: "run", Name: "own", Owner: "alice"},
		Context:  Context{Transport: "http", Now: now, Interactive: true, Tier: "middle", UserTier: "pro", SourceIP: net.ParseIP("10.1.2.3"), Agent: "ci/build", Depth: 1}}
	cases := []struct {
		name string
		c    Condition
		want bool
	}{
		{"transport yes", Condition{Transport: []string{"grpc", "http"}}, true},
		{"transport no", Condition{Transport: []string{"grpc"}}, false},
		{"weekday window yes", Condition{Time: &TimeWindow{Days: []string{"tue"}, Between: []string{"07:00", "20:00"}, TZ: "Europe/Kyiv"}}, true},
		{"weekday window wrong tz", Condition{Time: &TimeWindow{Between: []string{"08:00", "20:00"}}}, false}, // 07:30 UTC
		{"weekday no", Condition{Time: &TimeWindow{Days: []string{"sat", "sun"}}}, false},
		{"over midnight yes", Condition{Time: &TimeWindow{Between: []string{"22:00", "11:00"}, TZ: "Europe/Kyiv"}}, true},
		{"over midnight no", Condition{Time: &TimeWindow{Between: []string{"22:00", "06:00"}, TZ: "Europe/Kyiv"}}, false},
		{"interactive", Condition{Interactive: bptr(true)}, true},
		{"not interactive", Condition{Interactive: bptr(false)}, false},
		{"tier", Condition{Tier: []string{"middle"}}, true},
		{"user tier no", Condition{UserTier: []string{"free"}}, false},
		{"cidr yes", Condition{SourceCIDR: []string{"10.0.0.0/8"}}, true},
		{"cidr no", Condition{SourceCIDR: []string{"192.168.0.0/16"}}, false},
		{"owner self", Condition{OwnerIsSelf: bptr(true)}, true},
		{"owner not self", Condition{OwnerIsSelf: bptr(false)}, false},
		{"agent glob", Condition{Agent: "ci/*"}, true},
		{"agent glob no", Condition{Agent: "chat/*"}, false},
		{"depth ok", Condition{Depth: &DepthBound{Max: 1}}, true},
		{"depth exceeded", Condition{Depth: &DepthBound{Max: 0}}, false},
	}
	for _, c := range cases {
		req := base
		if got := compileCondition(&c.c).holds(&req); got != c.want {
			t.Errorf("%s: holds = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCondition_UnanswerableFieldsDoNotHold(t *testing.T) {
	// A condition the request cannot answer must never read as true.
	req := Request{Subject: Subject{ID: "alice"}, Resource: Resource{Type: "run", Name: "own"}}
	for name, c := range map[string]Condition{
		"no clock":      {Time: &TimeWindow{Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}}},
		"no address":    {SourceCIDR: []string{"0.0.0.0/0"}},
		"no agent":      {Agent: "*"},
		"no owner":      {OwnerIsSelf: bptr(true)},
		"no transport":  {Transport: Transports},
		"no subject id": {OwnerIsSelf: bptr(true)},
	} {
		if compileCondition(&c).holds(&req) {
			t.Errorf("%s: condition held on a request that cannot answer it", name)
		}
	}
	if compileCondition(nil).holds(&req) == false {
		t.Error("an empty condition (never stored, but reachable) must hold")
	}
}

func TestInAnyPrefix_V4MappedAndV6(t *testing.T) {
	cidr := func(s ...string) []netip.Prefix {
		var out []netip.Prefix
		for _, x := range s {
			out = append(out, netip.MustParsePrefix(x))
		}
		return out
	}
	if !inAnyPrefix(cidr("10.0.0.0/8"), net.ParseIP("10.9.9.9").To16()) {
		t.Error("a v4-mapped address should match its v4 prefix")
	}
	if !inAnyPrefix(cidr("fd00::/8"), net.ParseIP("fd12::1")) {
		t.Error("v6 prefix")
	}
	if inAnyPrefix(cidr("10.0.0.0/8"), nil) {
		t.Error("nil ip")
	}
	// An unparseable prefix never reaches the matcher: Validate refuses it.
	if err := (&Condition{SourceCIDR: []string{"not-a-cidr"}}).Validate(); err == nil {
		t.Error("an unparseable prefix validated")
	}
}

func TestParseHM(t *testing.T) {
	if m, err := parseHM("07:30"); err != nil || m != 450 {
		t.Errorf("parseHM = %d, %v", m, err)
	}
	if _, err := parseHM("25:00"); err == nil || !strings.Contains(err.Error(), "HH:MM") {
		t.Errorf("want an error, got %v", err)
	}
}
