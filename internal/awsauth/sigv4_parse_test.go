package awsauth

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseAuthorization_ReadsWhatSignRequestWrites(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://sts.amazonaws.com/", strings.NewReader("x"))
	req.Header.Set("X-Loomcycle-AWS-Server-ID", "loomcycle.example")
	now := time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC)
	SignRequest(req, []byte("x"), testCreds, "us-east-1", "sts", now)
	a, err := ParseAuthorization(req.Header.Get("Authorization"))
	if err != nil {
		t.Fatal(err)
	}
	if a.AccessKeyID != "AKIDEXAMPLE" || a.Day != "20261024" || a.Region != "us-east-1" || a.Service != "sts" || len(a.Signature) != 64 {
		t.Fatalf("parsed %+v", a)
	}
	if !a.Signs("X-Loomcycle-AWS-Server-ID") || !a.Signs("host") || a.Signs("content-type") {
		t.Fatalf("signed headers %v", a.SignedHeaders)
	}
	d, err := ParseAmzDate(req.Header.Get("X-Amz-Date"))
	if err != nil || !d.Equal(now) {
		t.Fatalf("date %v %v", d, err)
	}
}

func TestParseAuthorization_RefusesMalformedHeaders(t *testing.T) {
	sig := strings.Repeat("a", 64)
	for _, h := range []string{
		"",
		"Bearer x",
		"AWS4-HMAC-SHA256 Credential=K/20261024/us-east-1/sts/aws4_request, SignedHeaders=host",
		"AWS4-HMAC-SHA256 Credential=K/20261024/us-east-1/sts/aws4_request, SignedHeaders=host, Signature=" + sig + ", Signature=" + sig,
		"AWS4-HMAC-SHA256 Credential=K/20261024/us-east-1/sts, SignedHeaders=host, Signature=" + sig,
		"AWS4-HMAC-SHA256 Credential=K/20261024/us-east-1/sts/aws4_request, SignedHeaders=Host, Signature=" + sig,
		"AWS4-HMAC-SHA256 Credential=K/20261024/us-east-1/sts/aws4_request, SignedHeaders=host, Signature=zz",
		"AWS4-HMAC-SHA256 Credential=K/20261024/us-east-1/sts/aws4_request, SignedHeaders=host;;date, Signature=" + sig,
	} {
		if _, err := ParseAuthorization(h); err == nil {
			t.Errorf("accepted %q", h)
		}
	}
}
