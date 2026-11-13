package dispatch

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// ServerTLS is the TLS a worker endpoint on the network serves: the
// endpoint's certificate and key, and, when clientCA is set, a client
// certificate every worker must present signed by it (mutual TLS). The
// worker token is checked on top either way.
func ServerTLS(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" {
		return nil, errors.New("a certificate and its key are both required")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("the endpoint certificate: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		// gRPC is HTTP/2; a client checks the protocol was agreed.
		NextProtos: []string{"h2"},
	}
	if clientCAFile != "" {
		pool, err := certPool(clientCAFile)
		if err != nil {
			return nil, fmt.Errorf("the worker client CA: %w", err)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// TLSListener serves cfg on l.
func TLSListener(l net.Listener, cfg *tls.Config) net.Listener { return tls.NewListener(l, cfg) }

func certPool(file string) (*x509.CertPool, error) {
	// The operator's conversion.tls.client_ca_file, never a path from a run.
	pem, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no PEM certificate", file)
	}
	return pool, nil
}
