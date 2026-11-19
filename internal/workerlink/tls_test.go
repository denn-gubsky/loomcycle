package workerlink

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientTLS_RefusesACertificateWithoutItsKey(t *testing.T) {
	if _, err := ClientTLS("", "/w.crt", "", ""); err == nil || !strings.Contains(err.Error(), "needs its key") {
		t.Fatalf("err = %v, want one saying a certificate needs its key", err)
	}
}

func TestClientTLS_RefusesACAFileWithNoCertificate(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ClientTLS(ca, "", "", ""); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Fatalf("err = %v, want one saying the CA holds no PEM certificate", err)
	}
}

func TestClientTLS_SystemRootsAndTheServerNameWhenNoCAIsGiven(t *testing.T) {
	cfg, err := ClientTLS("", "", "", "conversion.internal")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootCAs != nil || cfg.ServerName != "conversion.internal" || len(cfg.Certificates) != 0 {
		t.Fatalf("cfg = roots %v, name %q, %d certs", cfg.RootCAs, cfg.ServerName, len(cfg.Certificates))
	}
}

func TestServerTLS_RefusesAHalfWrittenCertificate(t *testing.T) {
	for _, c := range [][2]string{{"", ""}, {"/e.crt", ""}, {"", "/e.key"}} {
		if _, err := ServerTLS(c[0], c[1], ""); err == nil {
			t.Errorf("ServerTLS(%q, %q): no error", c[0], c[1])
		}
	}
}
