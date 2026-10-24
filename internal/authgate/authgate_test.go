package authgate

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

func testResolver(ctx context.Context, bearer string) (auth.Principal, bool) {
	if bearer == "good" {
		return auth.Principal{TenantID: "acme", Subject: "alice", TokenDefID: "def-1"}, true
	}
	return auth.Principal{}, false
}

func TestBaseAuthGate_VerifyReturnsTheBearersPrincipal(t *testing.T) {
	g := NewBaseAuthGate(testResolver)
	r := httptest.NewRequest("POST", "/ui/session", nil)
	r.Header.Set("Authorization", "bearer good")
	a, err := g.Verify(context.Background(), r)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if a.Subject != "alice" || a.TenantHint != "acme" || len(a.AMR) != 1 || a.AMR[0] != AMRBearer || a.AuthTime.IsZero() {
		t.Errorf("assertion = %+v", a)
	}
	if a.Credential == nil || a.Credential.Bearer != "good" || a.Credential.Principal.TokenDefID != "def-1" {
		t.Errorf("credential = %+v, want the verified bearer and its principal", a.Credential)
	}
}

func TestBaseAuthGate_RefusesMissingAndInvalidBearers(t *testing.T) {
	g := NewBaseAuthGate(testResolver)
	for header, want := range map[string]error{
		"":             ErrNoCredential,
		"Basic Zm9vOg": ErrNoCredential,
		"Bearer ":      ErrNoCredential,
		"Bearer wrong": ErrInvalidCredential,
	} {
		r := httptest.NewRequest("POST", "/ui/session", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if _, err := g.Complete(context.Background(), r, LoginState{}); !errors.Is(err, want) {
			t.Errorf("Authorization %q: err = %v, want %v", header, err, want)
		}
	}
}

func TestBaseAuthGate_HasNoInteractiveBegin(t *testing.T) {
	g := NewBaseAuthGate(testResolver)
	if g.Kind() != KindToken {
		t.Errorf("kind = %q", g.Kind())
	}
	err := g.Begin(context.Background(), httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), LoginState{})
	if !errors.Is(err, ErrNotInteractive) {
		t.Errorf("begin err = %v, want ErrNotInteractive", err)
	}
}
