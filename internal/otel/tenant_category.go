package otel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// What a tenant (and the status every tenant reads) learns of a failed export
// is one coarse category. The error itself can name the addresses a host name
// resolved to, the dial guard's verdict on them, a TLS chain or a collector's
// answer — enough to probe the network the endpoint points into. The full,
// scrubbed error goes to the operator's log only.
const (
	CategoryBlocked  = "blocked"  // the address guard refused every address
	CategoryDNS      = "dns"      // the host name did not resolve
	CategoryConnect  = "connect"  // no connection (refused, reset, unreachable)
	CategoryTimeout  = "timeout"  // no answer within the export timeout
	CategoryTLS      = "tls"      // the TLS handshake or certificate failed
	CategoryRedirect = "redirect" // the collector answered with a redirect, not followed
	CategoryRejected = "rejected" // the collector took the request and rejected the span
	CategoryBusy     = "busy"     // every shared export slot was held
	CategoryFailed   = "export_failed"
)

// statusKey carries a statusHolder on an export's ctx, for statusRecorder.
type statusKey struct{}

// statusHolder is the HTTP status of an export's response, 0 when none came.
type statusHolder struct{ code atomic.Int32 }

func withStatusHolder(ctx context.Context) (context.Context, *statusHolder) {
	h := &statusHolder{}
	return context.WithValue(ctx, statusKey{}, h), h
}

// statusRecorder notes each response's status on the request's holder. The
// OTLP exporter reports a 429 or a 503 without its status code, so the
// category is read here rather than from the error text.
type statusRecorder struct{ next http.RoundTripper }

func (s statusRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := s.next.RoundTrip(req)
	if err == nil {
		if h, ok := req.Context().Value(statusKey{}).(*statusHolder); ok {
			h.code.Store(int32(resp.StatusCode))
		}
	}
	return resp, err
}

// exportCategory is err's category; code is the HTTP status the response had
// (0 for none).
func exportCategory(err error, code int32) string {
	msg := err.Error()
	var (
		dnsErr  *net.DNSError
		netErr  net.Error
		opErr   *net.OpError
		unknown x509.UnknownAuthorityError
		host    x509.HostnameError
		invalid x509.CertificateInvalidError
		verify  *tls.CertificateVerificationError
		record  tls.RecordHeaderError
	)
	switch {
	case errors.Is(err, errTenantRedirect):
		return CategoryRedirect
	case code >= 300:
		return fmt.Sprintf("http_%d", code)
	case strings.Contains(msg, "blocked:"):
		return CategoryBlocked
	case errors.As(err, &dnsErr) || strings.Contains(msg, "no such host"):
		return CategoryDNS
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return CategoryTimeout
	case errors.As(err, &unknown), errors.As(err, &host), errors.As(err, &invalid), errors.As(err, &verify),
		errors.As(err, &record), strings.Contains(msg, "tls:"), strings.Contains(msg, "x509:"):
		return CategoryTLS
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
		switch st.Code() {
		case codes.DeadlineExceeded:
			return CategoryTimeout
		case codes.Unavailable:
			return CategoryConnect
		}
		return "grpc_" + strings.ToLower(st.Code().String())
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.As(err, &opErr) {
		return CategoryConnect
	}
	return CategoryFailed
}
