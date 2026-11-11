package tenanttelemetry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ErrInvalid is a policy a write may not store: wrapped with what is wrong,
// in words the tenant can act on.
var ErrInvalid = errors.New("tenant telemetry: invalid policy")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// The signals a policy may name, and the ones this build exports.
const (
	SignalTraces  = "traces"
	SignalMetrics = "metrics"
)

// Content capture modes. Only "off" exists in this build.
const (
	ContentCaptureOff = "off"
	ContentCaptureOn  = "on"
)

// PolicyInput is a write as a transport received it, before the checks.
type PolicyInput struct {
	Endpoint          string
	Protocol          string
	HeadersCredential string
	// Signals empty means the default, traces.
	Signals []string
	// SamplerRatio nil means the default: 1, or the operator's cap below it.
	SamplerRatio *float64
	// ContentCapture is "" or "off"; "on" is refused until it ships.
	ContentCapture string
}

const maxEndpointLen = 2048

// credentialNameRe is the credential store's own name shape.
var credentialNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// forbiddenHeaders are the names a tenant's header block may not set: the
// transport's framing and routing headers, which the exporter owns, and
// gRPC's reserved prefixes.
var forbiddenHeaders = map[string]bool{
	"host": true, "content-length": true, "content-type": true, "content-encoding": true,
	"transfer-encoding": true, "connection": true, "keep-alive": true, "te": true,
	"trailer": true, "upgrade": true, "proxy-authorization": true, "proxy-connection": true,
	"user-agent": true,
}

const (
	maxHeaders     = 32
	maxHeaderBytes = 8192
)

// endpointCheck parses and checks an endpoint against the operator's rules.
func (m *Manager) endpointCheck(raw, protocol string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", invalid("endpoint is required: the OTLP base URL of your collector, for example https://otel.example.com:4318")
	}
	if len(raw) > maxEndpointLen {
		return "", invalid("endpoint is longer than %d characters", maxEndpointLen)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" {
		return "", invalid("endpoint is not an absolute URL with a host")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && m.cfg.AllowHTTP:
	case u.Scheme == "http":
		return "", invalid("endpoint must use https: the headers carry your collector credentials")
	default:
		return "", invalid("endpoint must be an https URL")
	}
	if u.User != nil {
		return "", invalid("endpoint must not carry a user name or password; put credentials in the header credential")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", invalid("endpoint must not carry a query or a fragment")
	}
	if protocol == string(lcotel.ProtocolGRPC) && strings.Trim(u.Path, "/") != "" {
		return "", invalid("a grpc endpoint is a host and port with no path")
	}
	if !hostAllowed(m.cfg.AllowedHosts, u.Hostname()) {
		return "", invalid("endpoint host %q is not one the operator allows telemetry to be sent to", u.Hostname())
	}
	return strings.TrimRight(raw, "/"), nil
}

// normalize checks in against the operator's rules (not yet the credential)
// and returns the row it would store for tenant.
func (m *Manager) normalize(tenant string, in PolicyInput) (store.TenantTelemetryPolicyRow, error) {
	if tenant == "" {
		return store.TenantTelemetryPolicyRow{}, invalid("a telemetry policy belongs to a tenant: name one")
	}
	protocol := strings.TrimSpace(in.Protocol)
	switch protocol {
	case "":
		protocol = string(lcotel.ProtocolHTTPProtobuf)
	case string(lcotel.ProtocolHTTPProtobuf), string(lcotel.ProtocolGRPC):
	default:
		return store.TenantTelemetryPolicyRow{}, invalid("protocol must be http/protobuf or grpc")
	}
	endpoint, err := m.endpointCheck(in.Endpoint, protocol)
	if err != nil {
		return store.TenantTelemetryPolicyRow{}, err
	}
	// No signals named is the default, traces. An empty list is the same: gRPC
	// cannot tell it from an absent one, and stopping an export is a delete.
	signals := in.Signals
	if len(signals) == 0 {
		signals = []string{SignalTraces}
	}
	for _, s := range signals {
		switch s {
		case SignalTraces:
		case SignalMetrics:
			return store.TenantTelemetryPolicyRow{}, invalid("metrics export is not available yet; ask for traces")
		default:
			return store.TenantTelemetryPolicyRow{}, invalid("signals may name traces only (got %q)", s)
		}
	}
	signals, err = store.NormalizeTelemetrySignals(signals)
	if err != nil {
		return store.TenantTelemetryPolicyRow{}, invalid("%v", err)
	}
	ratio := math.Min(1, m.cfg.MaxRatio)
	if in.SamplerRatio != nil {
		ratio = *in.SamplerRatio
		switch {
		case !(ratio >= 0 && ratio <= 1):
			return store.TenantTelemetryPolicyRow{}, invalid("sampler_ratio must be between 0 and 1")
		case ratio > m.cfg.MaxRatio:
			return store.TenantTelemetryPolicyRow{}, invalid("sampler_ratio may be at most %g on this deployment", m.cfg.MaxRatio)
		}
	}
	switch strings.TrimSpace(in.ContentCapture) {
	case "", ContentCaptureOff:
	case ContentCaptureOn:
		return store.TenantTelemetryPolicyRow{}, invalid("content capture is not available yet")
	default:
		return store.TenantTelemetryPolicyRow{}, invalid("content_capture must be off")
	}
	cred := strings.TrimSpace(in.HeadersCredential)
	if cred != "" && !credentialNameRe.MatchString(cred) {
		return store.TenantTelemetryPolicyRow{}, invalid("headers_credential is not a credential name")
	}
	return store.TenantTelemetryPolicyRow{
		TenantID: tenant, Endpoint: endpoint, Protocol: protocol, HeadersCredential: cred,
		Signals: signals, SamplerRatio: ratio,
	}, nil
}

// headersFor resolves row's header credential for the URL the export is sent
// to, and parses it. The credential must be the tenant's own (tenant scope),
// restricted by its allowed_urls to that URL: a value that may go anywhere is
// refused, so a credential stored for something else cannot be pointed at a
// collector. No credential named is no headers.
func (m *Manager) headersFor(ctx context.Context, row store.TenantTelemetryPolicyRow) (map[string]string, error) {
	if row.HeadersCredential == "" {
		return map[string]string{}, nil
	}
	if m.creds == nil {
		return nil, invalid("credentials are not configured on this deployment (LOOMCYCLE_SECRET_KEY), so no header credential can be used")
	}
	target := lcotel.TracesURL(row.Endpoint, lcotel.Protocol(row.Protocol))
	// Pinnable: a restricted credential refuses to resolve for a request that
	// could be redirected. The export never follows a redirect.
	res, found, err := m.creds.ResolveFor(netguard.WithPinnable(ctx), row.TenantID, "", "", row.HeadersCredential, target)
	switch {
	case errors.Is(err, credential.ErrURLNotAllowed):
		return nil, invalid("headers_credential %q is not allowed to be sent to %s: add the endpoint to its allowed_urls", row.HeadersCredential, row.Endpoint)
	case errors.Is(err, credential.ErrExpired):
		return nil, invalid("headers_credential %q has expired", row.HeadersCredential)
	case err != nil:
		return nil, fmt.Errorf("tenant telemetry: resolve headers_credential %q: %w", row.HeadersCredential, err)
	case !found:
		return nil, invalid("headers_credential %q is not a credential of this tenant", row.HeadersCredential)
	case res.Scope != "tenant":
		return nil, invalid("headers_credential %q must be a tenant credential", row.HeadersCredential)
	case res.AllowedURLs == nil:
		return nil, invalid("headers_credential %q may be sent anywhere: restrict its allowed_urls to the endpoint %s", row.HeadersCredential, row.Endpoint)
	}
	headers, err := lcotel.ParseTenantHeaders(res.Value)
	if err != nil {
		return nil, invalid("headers_credential %q is not a header block (name=value,name=value): %v", row.HeadersCredential, err)
	}
	if err := checkHeaders(headers); err != nil {
		return nil, err
	}
	return headers, nil
}

func checkHeaders(h map[string]string) error {
	if len(h) > maxHeaders {
		return invalid("the header block holds more than %d headers", maxHeaders)
	}
	size := 0
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v := h[k]
		lk := strings.ToLower(k)
		if forbiddenHeaders[lk] || strings.HasPrefix(lk, "grpc-") || strings.HasPrefix(lk, ":") {
			return invalid("the header block may not set %s", lk)
		}
		for _, c := range v {
			if (c < 0x20 && c != '\t') || c == 0x7f {
				return invalid("the value of header %s holds a control character", lk)
			}
		}
		size += len(k) + len(v)
	}
	if size > maxHeaderBytes {
		return invalid("the header block is larger than %d bytes", maxHeaderBytes)
	}
	return nil
}
