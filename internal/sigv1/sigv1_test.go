package sigv1

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var updateVector = flag.Bool("update", false, "rewrite testdata/vector.json from the Go signer")

const secret = "sigv1-test-secret"

func TestVerify_AcceptsWhatSignProduces(t *testing.T) {
	body := []byte(`{"phase":"pre","tool_call":{"name":"Bash"}}`)
	at := time.Unix(1_760_000_000, 0)
	h := Sign(secret, body, at)
	if !strings.HasPrefix(h, "t=1760000000,v1=") {
		t.Fatalf("envelope %q is not t=<unix>,v1=<hex>", h)
	}
	if !Verify(secret, body, h, at, DefaultTolerance) {
		t.Fatal("a fresh envelope did not verify")
	}
	// Stripe-style spacing after the comma is accepted, as the inbound
	// receiver always has.
	if !Verify(secret, body, strings.Replace(h, ",", ", ", 1), at, DefaultTolerance) {
		t.Fatal("an envelope with a space after the comma did not verify")
	}
}

func TestVerify_RefusesEveryAlteredInput(t *testing.T) {
	body := []byte(`{"a":1}`)
	at := time.Unix(1_760_000_000, 0)
	h := Sign(secret, body, at)
	ts, mac := Fields(h)
	cases := map[string]struct {
		secret string
		body   []byte
		header string
		now    time.Time
	}{
		"tampered body":         {secret, []byte(`{"a":2}`), h, at},
		"wrong secret":          {"another", body, h, at},
		"stale t":               {secret, body, h, at.Add(DefaultTolerance + time.Second)},
		"future t":              {secret, body, h, at.Add(-DefaultTolerance - time.Second)},
		"t re-signed elsewhere": {secret, body, "t=" + strconv.FormatInt(at.Unix()+1, 10) + ",v1=" + mac, at},
		"missing v1":            {secret, body, "t=" + ts, at},
		"missing t":             {secret, body, "v1=" + mac, at},
		"unparsable t":          {secret, body, "t=abc,v1=" + mac, at},
		"bad hex":               {secret, body, "t=" + ts + ",v1=zz" + mac[2:], at},
		"truncated mac":         {secret, body, "t=" + ts + ",v1=" + mac[:62], at},
		"empty":                 {secret, body, "", at},
		"bare form":             {secret, body, SignBare(secret, body), at},
	}
	for name, c := range cases {
		if Verify(c.secret, c.body, c.header, c.now, DefaultTolerance) {
			t.Errorf("%s: verified", name)
		}
	}
}

func TestVerify_ToleranceIsInclusive(t *testing.T) {
	body := []byte("x")
	at := time.Unix(1_760_000_000, 0)
	h := Sign(secret, body, at)
	for _, now := range []time.Time{at.Add(DefaultTolerance), at.Add(-DefaultTolerance)} {
		if !Verify(secret, body, h, now, DefaultTolerance) {
			t.Errorf("an envelope exactly %v from now did not verify", now.Sub(at))
		}
	}
}

func TestVerifyBare_AcceptsWhatSignBareProducesAndNothingElse(t *testing.T) {
	body := []byte(`{"changes":[]}`)
	sig := SignBare(secret, body)
	if !VerifyBare(secret, body, sig) || !VerifyBare(secret, body, " "+sig+" ") {
		t.Fatal("a bare signature did not verify")
	}
	if VerifyBare(secret, []byte(`{"changes":[1]}`), sig) || VerifyBare("other", body, sig) || VerifyBare(secret, body, "not-hex") {
		t.Fatal("an altered bare signature verified")
	}
}

// vector is a signature the Go signer produced, which the TypeScript and Python
// verifiers are tested against: they read this file, so none of them restates
// the algorithm to check another.
type vector struct {
	Secret string `json:"secret"`
	Body   string `json:"body"`
	T      int64  `json:"t"`
	Header string `json:"header"`
}

// TestSign_MatchesTheCrossLanguageVector pins testdata/vector.json to the Go
// signer. Regenerate with `go test ./internal/sigv1 -run Vector -update`.
func TestSign_MatchesTheCrossLanguageVector(t *testing.T) {
	path := filepath.Join("testdata", "vector.json")
	// A body with a newline, non-ASCII text and JSON escapes: a verifier that
	// hashes anything other than the exact UTF-8 bytes fails on it.
	want := vector{
		Secret: "sigv1-vector-secret",
		Body:   "{\"phase\":\"pre\",\"hook_name\":\"gate\",\"note\":\"café \\u2014 ok\"}\n",
		T:      1_760_000_000,
	}
	want.Header = Sign(want.Secret, []byte(want.Body), time.Unix(want.T, 0))
	if *updateVector {
		buf, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(buf, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got vector
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("testdata/vector.json is not what the Go signer produces:\n got %+v\nwant %+v", got, want)
	}
}
