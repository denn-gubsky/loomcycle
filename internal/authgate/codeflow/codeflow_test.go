package codeflow

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestDerivedValues_AreBoundToTheBrowserSecret(t *testing.T) {
	a, err := NewLogin()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewLogin()
	if a.State == b.State || string(a.Secret) == string(b.Secret) {
		t.Fatal("two logins drew the same randomness")
	}
	if Verifier(a.Secret, a.State) != Verifier(a.Secret, a.State) {
		t.Fatal("verifier is not deterministic")
	}
	// Another browser's secret with the same state derives nothing usable.
	if Verifier(b.Secret, a.State) == Verifier(a.Secret, a.State) || Nonce(b.Secret, a.State) == Nonce(a.Secret, a.State) {
		t.Fatal("verifier/nonce do not depend on the secret")
	}
	if Verifier(a.Secret, a.State) == Nonce(a.Secret, a.State) {
		t.Fatal("verifier and nonce are not domain-separated")
	}
	if n := len(Verifier(a.Secret, a.State)); n < 43 || n > 128 {
		t.Fatalf("verifier length %d outside RFC 7636", n)
	}
	got, ok := DecodeSecret(EncodeSecret(a.Secret))
	if !ok || string(got) != string(a.Secret) {
		t.Fatal("secret round trip")
	}
	if _, ok := DecodeSecret(BindingHash(a.Secret)); ok {
		t.Fatal("a stored binding hash decodes as a cookie secret")
	}
}

func TestAuthCodeURL_IsS256WithNonceAndCannotBeOverridden(t *testing.T) {
	l, _ := NewLogin()
	ls := State(l.Secret, l.State, "https://lc.example/v1/_auth/complete/g", false, time.Now())
	c := &Client{ClientID: "cid", AuthURL: "https://idp.example/auth", TokenURL: "https://idp.example/token", Scopes: []string{"openid", "email"}}
	raw, err := c.AuthCodeURL(ls, true, map[string]string{"hd": "acme.com"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	sum := sha256.Sum256([]byte(ls.CodeVerifier))
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatalf("challenge = %q/%q", q.Get("code_challenge_method"), q.Get("code_challenge"))
	}
	if q.Get("state") != l.State || q.Get("nonce") != ls.Nonce || q.Get("redirect_uri") != ls.RedirectURI ||
		q.Get("response_type") != "code" || q.Get("hd") != "acme.com" || q.Get("code_verifier") != "" {
		t.Fatalf("query = %v", q)
	}
	for _, k := range []string{"state", "nonce", "redirect_uri", "code_challenge", "Response_Type"} {
		if _, err := c.AuthCodeURL(ls, true, map[string]string{k: "x"}); err == nil {
			t.Errorf("extra %q overrode a flow parameter", k)
		}
	}
	ls.Reauth = true
	raw, _ = c.AuthCodeURL(ls, true, map[string]string{"prompt": "none"})
	u, _ = url.Parse(raw)
	if u.Query().Get("prompt") != "login" || u.Query().Get("max_age") != "0" {
		t.Fatalf("reauth query = %v", u.Query())
	}
}

func TestExchange_SendsTheVerifier(t *testing.T) {
	l, _ := NewLogin()
	ls := State(l.Secret, l.State, "https://lc.example/cb", false, time.Now())
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": "idt"})
	}))
	defer srv.Close()
	c := &Client{ClientID: "cid", ClientSecret: "sec", AuthURL: srv.URL + "/auth", TokenURL: srv.URL + "/token", HTTP: srv.Client()}
	tok, err := c.Exchange(context.Background(), "the-code", ls)
	if err != nil {
		t.Fatal(err)
	}
	if form.Get("code") != "the-code" || form.Get("code_verifier") != ls.CodeVerifier || form.Get("redirect_uri") != ls.RedirectURI {
		t.Fatalf("form = %v", form)
	}
	if id, _ := tok.Extra("id_token").(string); id != "idt" {
		t.Fatalf("id_token = %v", tok.Extra("id_token"))
	}
	if _, err := c.Exchange(context.Background(), "", ls); err == nil {
		t.Fatal("empty code exchanged")
	}
}

func TestCheckCallback_RefusesAMismatchedStateAndProviderErrors(t *testing.T) {
	ls := State([]byte("0123456789abcdef0123456789abcdef"), "st", "https://x/cb", false, time.Now())
	r := httptest.NewRequest(http.MethodGet, "/cb?code=c&state=other", nil)
	if _, err := CheckCallback(r, ls); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	r = httptest.NewRequest(http.MethodGet, "/cb?code=c", nil)
	if _, err := CheckCallback(r, ls); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("missing state: %v", err)
	}
	r = httptest.NewRequest(http.MethodGet, "/cb?error=access_denied&state=st", nil)
	if _, err := CheckCallback(r, ls); err == nil {
		t.Fatal("provider error passed")
	}
	r = httptest.NewRequest(http.MethodGet, "/cb?code=c&state=st", nil)
	if code, err := CheckCallback(r, ls); err != nil || code != "c" {
		t.Fatalf("good callback: %q %v", code, err)
	}
}
