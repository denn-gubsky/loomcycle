package worker

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// ClientTLS is the TLS a worker dials a network dispatcher with. caFile
// verifies the dispatcher's certificate (the system roots when empty);
// certFile and keyFile, set together, are the worker's own certificate for an
// endpoint that requires one. serverName overrides the name the certificate
// is checked against (default: the host the worker dials).
func ClientTLS(caFile, certFile, keyFile, serverName string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if caFile != "" {
		// The operator's LOOMCYCLE_WORKER_TLS_CA, never a path from a job.
		pem, err := os.ReadFile(filepath.Clean(caFile))
		if err != nil {
			return nil, fmt.Errorf("the dispatcher CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("the dispatcher CA: %s holds no PEM certificate", caFile)
		}
		cfg.RootCAs = pool
	}
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("a worker certificate needs its key, and a key its certificate")
	}
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("the worker certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// plaintextAllowed reports whether target may be dialled without TLS: a Unix
// socket, or a TCP address on this host's loopback. Anything else carries the
// token and every file sent for conversion across a network.
func plaintextAllowed(target string) bool {
	if strings.HasPrefix(target, "unix:") {
		return true
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
