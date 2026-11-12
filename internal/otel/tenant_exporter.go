package otel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// errTenantRedirect is what a tenant export's HTTP client answers a redirect
// with: the collector is the endpoint the tenant named and nothing it points
// elsewhere to, so a header bound to that endpoint never follows a hop.
var errTenantRedirect = errors.New("not following a redirect from the tenant's collector")

// TracesURL is the URL an http/protobuf export of a destination is posted to:
// the OTLP base endpoint with the traces path. A header credential is checked
// against it, as the URL its value is sent to.
func TracesURL(endpoint string, protocol Protocol) string {
	if protocol == ProtocolGRPC {
		return endpoint
	}
	return strings.TrimRight(endpoint, "/") + "/v1/traces"
}

// newTenantExporter builds the OTLP exporter for one tenant destination. Every
// setting comes from d: the OTEL_EXPORTER_OTLP_* environment configures the
// operator's exporter, and an option left unset here would let the exporter
// read it — the operator's headers and client certificate sent to a tenant's
// collector. release closes what the exporter's Shutdown does not.
func newTenantExporter(ctx context.Context, d TenantDestination, timeout time.Duration) (exp sdktrace.SpanExporter, release func(), err error) {
	u, headers, err := tenantEndpoint(d)
	if err != nil {
		return nil, nil, err
	}
	if d.Protocol == ProtocolGRPC {
		conn, err := tenantGRPCConn(u, d)
		if err != nil {
			return nil, nil, err
		}
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithGRPCConn(conn),
			otlptracegrpc.WithHeaders(headers),
			otlptracegrpc.WithTimeout(timeout),
			otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: false}),
		)
		if err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
		return exp, func() { _ = conn.Close() }, nil
	}
	client, tr := tenantHTTPClient(d, timeout)
	exp, err = otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(TracesURL(d.Endpoint, ProtocolHTTPProtobuf)),
		otlptracehttp.WithHTTPClient(client),
		otlptracehttp.WithHeaders(headers),
		otlptracehttp.WithTimeout(timeout),
		// One attempt per batch, bounded by the export timeout: a retry
		// would hold the tenant's queue on a collector that is down.
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
	)
	if err != nil {
		return nil, nil, err
	}
	return exp, tr.CloseIdleConnections, nil
}

// tenantEndpoint checks d's endpoint and copies its headers. The copy is a
// non-nil, possibly empty map: nil would leave an exporter reading
// OTEL_EXPORTER_OTLP_HEADERS, the operator's collector credentials.
func tenantEndpoint(d TenantDestination) (*url.URL, map[string]string, error) {
	if d.Dial == nil {
		return nil, nil, errors.New("tenant export: no dialer")
	}
	u, err := url.Parse(d.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, nil, fmt.Errorf("tenant export: endpoint is not an http(s) URL")
	}
	headers := make(map[string]string, len(d.Headers))
	for k, v := range d.Headers {
		headers[k] = v
	}
	return u, headers, nil
}

// tenantHTTPClient is the client an http/protobuf export to d is sent with:
// d's dialer (the guard), d's TLS, no proxy and no redirect. The transport is
// returned for its idle connections to be closed with the exporter.
func tenantHTTPClient(d TenantDestination, timeout time.Duration) (*http.Client, *http.Transport) {
	tr := &http.Transport{
		DialContext:           d.Dial,
		TLSClientConfig:       d.TLSConfig,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		// Proxy stays nil: a proxy from the environment would be dialled in
		// place of the collector, outside the destination the guard judges.
	}
	return &http.Client{
		Transport:     statusRecorder{next: tr},
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errTenantRedirect },
	}, tr
}

// tenantGRPCConn is the connection a gRPC export to d is sent over, dialled
// through d's dialer (the guard).
func tenantGRPCConn(u *url.URL, d TenantDestination) (*grpc.ClientConn, error) {
	hostport := u.Host
	if u.Port() == "" {
		port := "443"
		if u.Scheme == "http" {
			port = "80"
		}
		hostport = net.JoinHostPort(u.Hostname(), port)
	}
	creds := credentials.NewTLS(d.TLSConfig)
	if u.Scheme == "http" {
		creds = insecure.NewCredentials()
	}
	dial := d.Dial
	// passthrough: the guard resolves and judges the name the tenant gave;
	// gRPC's own resolver would hand it an address instead.
	return grpc.NewClient("passthrough:///"+hostport,
		grpc.WithTransportCredentials(creds),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) { return dial(ctx, "tcp", addr) }),
	)
}

// TestTenantExport sends one synthetic span (loomcycle.telemetry.test) to d,
// built exactly as a tenant pipeline builds its exporter, and returns nil or
// a *TenantExportError naming the failure's category (exportCategory), never
// its text. It answers within timeout. TenantRouter.Test is the same inside
// the router's cap on exports in flight.
func TestTenantExport(ctx context.Context, tenant string, d TenantDestination, serviceVersion string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultTenantExportTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx, code := withStatusHolder(ctx)
	exp, release, err := newTenantExporter(ctx, d, timeout)
	if err != nil {
		return &TenantExportError{msg: CategoryFailed}
	}
	defer func() {
		_ = exp.Shutdown(context.Background())
		if release != nil {
			release()
		}
	}()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	_, span := tp.Tracer(TracerName).Start(ctx, SpanTelemetryTest, trace.WithAttributes(attribute.String(AttrTenant, tenant)))
	span.End()
	_ = tp.Shutdown(context.Background())
	ended := rec.Ended()
	copies := make([]sdktrace.ReadOnlySpan, len(ended))
	res := tenantResource(tenant, serviceVersion)
	for i, s := range ended {
		copies[i] = newTenantSpan(s, res)
	}
	if err := exp.ExportSpans(ctx, copies); err != nil {
		if _, partial := partialRejected(err); partial {
			return &TenantExportError{msg: CategoryRejected}
		}
		return &TenantExportError{msg: exportCategory(err, code.code.Load())}
	}
	return nil
}

// ParseTenantHeaders parses a tenant's header block (key=value pairs, comma
// separated, values percent-decoded) exactly as the exporter reads
// OTEL_EXPORTER_OTLP_HEADERS. An error says which entry is wrong, never what
// it holds.
func ParseTenantHeaders(v string) (map[string]string, error) {
	return parseHeaderList("header block", v)
}

// TenantExportError is a test export's failure as a category
// (exportCategory): text a tenant may be shown. Anything else a caller holds
// is not known to be.
type TenantExportError struct{ msg string }

func (e *TenantExportError) Error() string { return e.msg }

// SpanTelemetryTest is the span a tenant's telemetry test sends.
const SpanTelemetryTest = "loomcycle.telemetry.test"

// Bounds of a tenant export error as the status reports it.
const (
	tenantErrorMaxRunes     = 400
	tenantErrorMaxBodyRunes = 200
	// scrubMinLen: shorter header values are not searched for in error text,
	// where they would match ordinary words; a credential is longer.
	scrubMinLen = 6
)

// scrubError is err's text as a tenant (and the operator) may read it: URLs
// without credentials or query values, no header value of the destination (a
// collector may echo a header back in its answer), a collector's response
// body cut to 200 characters and the whole cut to 400.
func scrubError(err error, headers map[string]string) string {
	if err == nil {
		return ""
	}
	// Header values first, on the raw text: the URL pass below re-encodes
	// what it rewrites, which could hide a value from a plain search.
	msg := err.Error()
	for _, v := range headers {
		for _, s := range append([]string{v}, strings.Fields(v)...) {
			if len(s) >= scrubMinLen {
				msg = strings.ReplaceAll(msg, s, "[redacted]")
			}
		}
	}
	msg = sanitizeError(errors.New(msg))
	if i := strings.Index(msg, "body: "); i >= 0 {
		head, body := msg[:i+len("body: ")], msg[i+len("body: "):]
		if utf8.RuneCountInString(body) > tenantErrorMaxBodyRunes {
			body = string([]rune(body)[:tenantErrorMaxBodyRunes]) + "…"
		}
		msg = head + body
	}
	if utf8.RuneCountInString(msg) > tenantErrorMaxRunes {
		msg = string([]rune(msg)[:tenantErrorMaxRunes]) + "…"
	}
	return msg
}
