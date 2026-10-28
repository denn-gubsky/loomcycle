package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestEdgeGateProbes_AskWhereEachVerifierFetches — the probe list names the
// URL each verifier reaches: the Cloudflare team's JWKS, the ALB region's key
// host, STS globally and in each allowed region; a forward-auth gate and the
// non-edge gates reach nothing.
func TestEdgeGateProbes_AskWhereEachVerifierFetches(t *testing.T) {
	gates := []config.AuthGate{
		{Name: "paste", Kind: config.AuthGateKindToken},
		{Name: "cf", Header: &config.AuthHeaderGate{Preset: config.HeaderPresetCloudflareAccess, TeamDomain: "acme.cloudflareaccess.com"}},
		{Name: "alb", Header: &config.AuthHeaderGate{Preset: config.HeaderPresetAWSALB, ALBRegion: "eu-west-1"}},
		{Name: "fwd", Header: &config.AuthHeaderGate{Preset: config.HeaderPresetForwardAuthHMAC}},
		{Name: "ci", AWSIAM: &config.AuthAWSIAMGate{STSRegions: []string{"eu-west-1", "cn-north-1"}}},
	}
	got := edgeGateProbes(gates)
	want := []DoctorEdgeProbe{
		{Gate: "cf", What: "Cloudflare Access signing keys (JWKS)", URL: "https://acme.cloudflareaccess.com/cdn-cgi/access/certs", NeedOK: true},
		{Gate: "alb", What: "AWS ALB public-key endpoint", URL: "https://public-keys.auth.elb.eu-west-1.amazonaws.com/"},
		{Gate: "ci", What: "AWS STS", URL: "https://sts.amazonaws.com/"},
		{Gate: "ci", What: "AWS STS (eu-west-1)", URL: "https://sts.eu-west-1.amazonaws.com/"},
		{Gate: "ci", What: "AWS STS (cn-north-1)", URL: "https://sts.cn-north-1.amazonaws.com.cn/"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("probes:\n got %+v\nwant %+v", got, want)
	}
}

// TestDoctor_EdgeGateEndpointsReportReachability — an unreachable endpoint
// and a JWKS that does not answer 200 WARN (doctor still exits 0); an STS
// host that refuses the unsigned GET is reachable, so it passes.
func TestDoctor_EdgeGateEndpointsReportReachability(t *testing.T) {
	orig := edgeProbeForDoctor
	t.Cleanup(func() { edgeProbeForDoctor = orig })
	var asked []string
	edgeProbeForDoctor = func(_ context.Context, url string) (int, error) {
		asked = append(asked, url)
		switch url {
		case "https://down.example/":
			return 0, errors.New("dial tcp: i/o timeout")
		case "https://jwks.example/certs":
			return 404, nil
		}
		return 403, nil
	}
	out, code := runDoctorWith(t, &stubConfig{edgeProbes: []DoctorEdgeProbe{
		{Gate: "alb", What: "AWS ALB public-key endpoint", URL: "https://down.example/"},
		{Gate: "cf", What: "Cloudflare Access signing keys (JWKS)", URL: "https://jwks.example/certs", NeedOK: true},
		{Gate: "ci", What: "AWS STS", URL: "https://sts.example/"},
	}})
	if code != 0 {
		t.Errorf("exit = %d, want 0 (warnings only):\n%s", code, out)
	}
	for _, want := range []string{
		"[WARN]  Auth gate alb", "https://down.example/ unreachable",
		"[WARN]  Auth gate cf", "answered 404, want 200",
		"[PASS]  Auth gate ci", "AWS STS reachable at https://sts.example/",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if len(asked) != 3 {
		t.Errorf("probed %v, want each endpoint once", asked)
	}
}
