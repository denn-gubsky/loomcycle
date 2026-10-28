package awsiam

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
)

var _ authgate.Gate = AuthGate{}

func loginRequest(t *testing.T, body any) *http.Request {
	t.Helper()
	b, _ := json.Marshal(body)
	return httptest.NewRequest(http.MethodPost, "/v1/_auth/assert", bytes.NewReader(b))
}

func TestAuthGate_LoginBodyBecomesAnAssertionKeyedByTheRoleARN(t *testing.T) {
	f := newFakeSTS(t)
	d := validLogin(t)
	body := map[string]string{
		"gate": "ci", "iam_http_request_method": d.Method, "iam_request_url": d.URL,
		"iam_request_headers": d.Headers, "iam_request_body": d.Body,
	}
	a, err := AuthGate{G: f.gate()}.Verify(context.Background(), loginRequest(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if a.Subject != ciRole || a.Issuer != Issuer || a.Claims["arn"] != ciRole || a.Claims["account"] != "123456789012" ||
		!strings.Contains(a.Claims["caller_arn"].(string), "assumed-role") || !a.AuthTime.Equal(t0) {
		t.Fatalf("assertion %+v", a)
	}
}

func TestAuthGate_EmptyOrPartialBodiesAreRefused(t *testing.T) {
	f := newFakeSTS(t)
	g := AuthGate{G: f.gate()}
	if _, err := g.Verify(context.Background(), loginRequest(t, map[string]string{"gate": "ci"})); !errors.Is(err, authgate.ErrNoCredential) {
		t.Fatalf("empty login = %v, want ErrNoCredential", err)
	}
	d := validLogin(t)
	partial := map[string]string{"iam_http_request_method": d.Method, "iam_request_url": d.URL}
	if _, err := g.Verify(context.Background(), loginRequest(t, partial)); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("partial login = %v, want ErrInvalidCredential", err)
	}
	big := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"x":"`+strings.Repeat("a", maxLoginBody)+`"}`))
	if _, err := g.Verify(context.Background(), big); !errors.Is(err, authgate.ErrInvalidCredential) {
		t.Fatalf("oversized body = %v, want ErrInvalidCredential", err)
	}
	if _, err := g.Complete(context.Background(), loginRequest(t, nil), authgate.LoginState{}); !errors.Is(err, authgate.ErrNotInteractive) {
		t.Fatalf("Complete = %v", err)
	}
}
