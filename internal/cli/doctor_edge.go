package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate/awsiam"
	"github.com/denn-gubsky/loomcycle/internal/authgate/headergate"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// doctor_edge.go — RFC FA P3: the edge gates verify against AWS and
// Cloudflare endpoints the server reaches at sign-in time. A firewall that
// blocks one only shows up as every sign-in failing, so doctor asks each
// endpoint once, up front.

// DoctorEdgeProbe is one endpoint an edge gate's verifier reaches.
type DoctorEdgeProbe struct {
	Gate string
	What string // what the gate fetches there, for the report line
	URL  string
	// NeedOK: the endpoint must answer 200 (a JWKS the verifier loads).
	// Otherwise any HTTP answer shows it is reachable: STS refuses an
	// unsigned request and the ALB key host has nothing at its root.
	NeedOK bool
}

// edgeGateProbes lists the endpoints of the configured header and awsiam
// gates, built with the verifiers' own URL helpers so the probe asks where
// the gate will. A forward-auth-hmac gate reaches nothing.
func edgeGateProbes(gates []config.AuthGate) []DoctorEdgeProbe {
	var out []DoctorEdgeProbe
	for _, g := range gates {
		switch {
		case g.Header != nil && g.Header.Preset == config.HeaderPresetCloudflareAccess:
			out = append(out, DoctorEdgeProbe{Gate: g.Name, What: "Cloudflare Access signing keys (JWKS)",
				URL: headergate.CertsURL(g.Header.TeamDomain), NeedOK: true})
		case g.Header != nil && g.Header.Preset == config.HeaderPresetAWSALB:
			out = append(out, DoctorEdgeProbe{Gate: g.Name, What: "AWS ALB public-key endpoint",
				URL: headergate.ALBKeyURL(g.Header.ALBRegion, "")})
		case g.AWSIAM != nil:
			out = append(out, DoctorEdgeProbe{Gate: g.Name, What: "AWS STS", URL: "https://" + awsiam.GlobalHost + "/"})
			for _, region := range g.AWSIAM.STSRegions {
				out = append(out, DoctorEdgeProbe{Gate: g.Name, What: "AWS STS (" + region + ")",
					URL: "https://" + awsiam.RegionalHost(region) + "/"})
			}
		}
	}
	return out
}

// edgeProbeForDoctor GETs one endpoint and returns its status. A seam so
// tests need no network. Redirects are not followed: the verifiers do not
// follow them either.
var edgeProbeForDoctor = func(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// checkEdgeGates reports whether each edge gate's endpoints answer. An
// unreachable one is a WARN, as for the AWS providers: doctor may run where
// the server does not (a laptop outside the VPC).
func checkEdgeGates(r *doctorRun, cfg configForDoctor) {
	for _, p := range cfg.EdgeGateProbes() {
		label := fmt.Sprintf("Auth gate %s", p.Gate)
		ctx, cancel := timeoutContext(5 * time.Second)
		status, err := edgeProbeForDoctor(ctx, p.URL)
		cancel()
		switch {
		case err != nil:
			r.warn(label, fmt.Sprintf("%s at %s unreachable: %v — every sign-in through this gate will fail", p.What, p.URL, err))
		case p.NeedOK && status != http.StatusOK:
			r.warn(label, fmt.Sprintf("%s at %s answered %d, want 200 — check the team domain", p.What, p.URL, status))
		default:
			r.pass(label, fmt.Sprintf("%s reachable at %s", p.What, p.URL))
		}
	}
}
