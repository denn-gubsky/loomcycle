package dispatch_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/worker"
)

// testPKI is a CA, a server certificate for 127.0.0.1 and a client
// certificate, written as PEM files.
type testPKI struct {
	ca, serverCert, serverKey, clientCert, clientKey string
}

func newPKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	issue := func(serial int64, usage x509.ExtKeyUsage, name string) (string, string) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{usage}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		kder, _ := x509.MarshalECPrivateKey(key)
		c, k := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
		writePEM(t, c, "CERTIFICATE", der)
		writePEM(t, k, "EC PRIVATE KEY", kder)
		return c, k
	}
	p := testPKI{ca: filepath.Join(dir, "ca.crt")}
	writePEM(t, p.ca, "CERTIFICATE", caDER)
	p.serverCert, p.serverKey = issue(2, x509.ExtKeyUsageServerAuth, "server")
	p.clientCert, p.clientKey = issue(3, x509.ExtKeyUsageClientAuth, "client")
	return p
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// tlsPool is a dispatcher serving TLS on a loopback listener.
func tlsPool(t *testing.T, p testPKI, clientCA string) (*dispatch.Dispatcher, string) {
	t.Helper()
	cfg, err := dispatch.ServerTLS(p.serverCert, p.serverKey, clientCA)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(dispatch.Options{Token: token})
	go func() { _ = d.Serve(dispatch.TLSListener(l, cfg)) }()
	t.Cleanup(d.Stop)
	return d, l.Addr().String()
}

// A worker that trusts the endpoint's CA attaches over TLS and converts.
func TestWorker_AttachesOverTLS(t *testing.T) {
	p := newPKI(t)
	d, addr := tlsPool(t, p, "")
	tc, err := worker.ClientTLS(p.ca, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	helperWorker(t, addr, "w1", worker.Options{TLS: tc})
	waitIdle(t, d, 1)
	data := samplePDF(t)
	if _, err := d.Client().SelectPages(context.Background(), bytes.NewReader(data), int64(len(data)), []int{1}); err != nil {
		t.Fatalf("a job over TLS: %v", err)
	}
}

// With a client CA, the endpoint admits only a worker with a certificate it
// signed, whatever token it holds.
func TestDispatcher_MutualTLSRefusesAWorkerWithoutACertificate(t *testing.T) {
	p := newPKI(t)
	d, addr := tlsPool(t, p, p.ca)
	noCert, _ := worker.ClientTLS(p.ca, "", "", "")
	helperWorker(t, addr, "no-cert", worker.Options{TLS: noCert})
	time.Sleep(500 * time.Millisecond)
	if n := d.Stats().Workers; n != 0 {
		t.Fatalf("a worker with no client certificate attached (%d workers)", n)
	}
	withCert, err := worker.ClientTLS(p.ca, p.clientCert, p.clientKey, "")
	if err != nil {
		t.Fatal(err)
	}
	helperWorker(t, addr, "with-cert", worker.Options{TLS: withCert})
	waitIdle(t, d, 1)
}

// A worker will not send its token, or receive files, in the clear to a
// dispatcher off this host unless told the network is private.
func TestWorker_RefusesAPlaintextDispatcherOffThisHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := worker.Run(ctx, worker.Options{Target: "192.0.2.10:7788", Token: token, Scratch: t.TempDir()})
	if !errors.Is(err, worker.ErrRefused) {
		t.Fatalf("plaintext to 192.0.2.10: %v, want ErrRefused", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the refusal waited for the context; it must be immediate")
	}
}
