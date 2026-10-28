package http

// A software WebAuthn authenticator for the passkey tests: it answers the
// server's offers the way a browser and a platform authenticator would — a
// "none" attestation over a fresh P-256 key, and ES256 assertions — so the
// server's real go-webauthn verification runs end to end. Fixtures only: the
// CBOR and COSE encodings come from go-webauthn's own packages.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

const (
	waFlagUP = 0x01
	waFlagUV = 0x04
	waFlagAT = 0x40
)

var b64u = base64.RawURLEncoding

// softAuthenticator holds one credential.
type softAuthenticator struct {
	t      *testing.T
	key    *ecdsa.PrivateKey
	credID []byte
	handle []byte
	rpID   string
	origin string
	count  uint32
	// uv is whether the authenticator verifies its user (a PIN, biometrics).
	uv bool
}

func newSoftAuthenticator(t *testing.T, rpID, origin string) *softAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return &softAuthenticator{t: t, key: key, credID: id, rpID: rpID, origin: origin, uv: true}
}

// offerChallenge pulls the challenge (and, for a creation, the user handle)
// out of an offer's publicKey options.
func offerOptions(t *testing.T, offer map[string]any) (challenge string, userID []byte) {
	t.Helper()
	pk, ok := offer["publicKey"].(map[string]any)
	if !ok {
		t.Fatalf("offer has no publicKey: %v", offer)
	}
	challenge, _ = pk["challenge"].(string)
	if u, ok := pk["user"].(map[string]any); ok {
		id, _ := u["id"].(string)
		var err error
		if userID, err = b64u.DecodeString(id); err != nil {
			t.Fatalf("user.id: %v", err)
		}
	}
	return challenge, userID
}

func (a *softAuthenticator) clientData(typ, challenge, origin string) []byte {
	b, err := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false})
	if err != nil {
		a.t.Fatal(err)
	}
	return b
}

func (a *softAuthenticator) authData(rpID string, flags byte, attested []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	out := append([]byte{}, h[:]...)
	out = append(out, flags)
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], a.count)
	out = append(out, c[:]...)
	return append(out, attested...)
}

// create answers a registration offer, returning the request body fields.
func (a *softAuthenticator) create(offer map[string]any) map[string]any {
	a.t.Helper()
	challenge, userID := offerOptions(a.t, offer)
	a.handle = userID
	cose, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: int64(webauthncose.EllipticKey), Algorithm: int64(webauthncose.AlgES256)},
		Curve:         1, XCoord: pad32(a.key.X.Bytes()), YCoord: pad32(a.key.Y.Bytes()),
	})
	if err != nil {
		a.t.Fatal(err)
	}
	attested := make([]byte, 16) // AAGUID: zero
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(a.credID)))
	attested = append(attested, l[:]...)
	attested = append(attested, a.credID...)
	attested = append(attested, cose...)
	flags := byte(waFlagUP | waFlagAT)
	if a.uv {
		flags |= waFlagUV
	}
	att, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": a.authData(a.rpID, flags, attested)})
	if err != nil {
		a.t.Fatal(err)
	}
	id := b64u.EncodeToString(a.credID)
	return map[string]any{"ceremony": offer["ceremony"], "credential": map[string]any{
		"id": id, "rawId": id, "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64u.EncodeToString(a.clientData("webauthn.create", challenge, a.origin)),
			"attestationObject": b64u.EncodeToString(att),
			"transports":        []string{"internal"},
		},
		"clientExtensionResults": map[string]any{"credProps": map[string]any{"rk": true}},
	}}
}

// get answers an assertion offer with the next counter value.
func (a *softAuthenticator) get(offer map[string]any) map[string]any {
	a.t.Helper()
	a.count++
	return a.getWith(offer, a.count, a.origin, a.rpID)
}

// getWith answers with a chosen counter, origin and RP ID (for the tests
// that must be refused).
func (a *softAuthenticator) getWith(offer map[string]any, count uint32, origin, rpID string) map[string]any {
	a.t.Helper()
	challenge, _ := offerOptions(a.t, offer)
	saved := a.count
	a.count = count
	flags := byte(waFlagUP)
	if a.uv {
		flags |= waFlagUV
	}
	ad := a.authData(rpID, flags, nil)
	a.count = saved
	cd := a.clientData("webauthn.get", challenge, origin)
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		a.t.Fatal(err)
	}
	id := b64u.EncodeToString(a.credID)
	resp := map[string]any{
		"clientDataJSON":    b64u.EncodeToString(cd),
		"authenticatorData": b64u.EncodeToString(ad),
		"signature":         b64u.EncodeToString(sig),
	}
	if a.handle != nil {
		resp["userHandle"] = b64u.EncodeToString(a.handle)
	}
	return map[string]any{"ceremony": offer["ceremony"], "credential": map[string]any{
		"id": id, "rawId": id, "type": "public-key", "response": resp, "clientExtensionResults": map[string]any{},
	}}
}

func pad32(b []byte) []byte {
	if len(b) >= 32 {
		return b
	}
	return append(make([]byte, 32-len(b)), b...)
}
