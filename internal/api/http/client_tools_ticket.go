package http

// client_tools_ticket.go — RFC GJ, a one-time ticket for the client-tools
// WebSocket.
//
// A browser cannot set Authorization on a WebSocket, and strict mode does not
// read the bearer from the subprotocol (RFC FA §4), so a browser client that
// is not the Web UI (an extension signed in with an app token) had no way to
// open /v1/client-tools. Rather than put a durable credential back into a
// header proxies and logs do not treat as one, the client mints a ticket over
// an ordinary authenticated call and presents it as `ticket.<t>` on the
// upgrade. What crosses the handshake is worth nothing after one use or
// thirty seconds.
//
// The ticket is HMAC-signed under a key derived from LOOMCYCLE_SECRET_KEY and
// names the credential that minted it; the handshake re-resolves that
// credential through the same paths a bearer or a session takes, so a ticket
// never outlives its credential and never carries more than it. Its jti is
// spent in the one-time cache the external gates use (auth_assertion_jtis),
// so a replay is refused on every replica.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/credential"
)

const (
	// clientToolsTicketPath mints a ticket. Gated as the socket is (see
	// requiredScopeFor, runPlaneRoute, authz.HTTPSurface).
	clientToolsTicketPath = "/v1/client-tools/ticket"
	// clientToolsTicketTTL is fixed (D2): a connect follows the mint at once,
	// and a reconnect mints again.
	clientToolsTicketTTL = 30 * time.Second
	// clientToolsTicketReplayGate namespaces the ticket's jti in the shared
	// one-time cache, apart from every sign-in gate's.
	clientToolsTicketReplayGate = "client-tools-ticket"
	// ticketSubprotocolPrefix marks the ticket among the Sec-WebSocket-Protocol
	// entries. Never echoed: only clientToolSubprotocol is negotiated.
	ticketSubprotocolPrefix = "ticket."
	// ticketFormat versions the ticket's shape; it is also the MAC's domain
	// label, so a ticket of another shape cannot verify as this one.
	ticketFormat = "lct1"
	// ticketMaxLen bounds what the handshake parses before the MAC check.
	ticketMaxLen = 2048
)

// The credential kinds a ticket can name. Each is re-resolved at the
// handshake by the path that kind of credential takes on a request.
const (
	ticketKindToken     = "token"     // a minted token row (operator, user or app token)
	ticketKindPrincipal = "principal" // a config-declared principal
	ticketKindLegacy    = "legacy"    // the LOOMCYCLE_AUTH_TOKEN shared secret
	ticketKindSession   = "session"   // a Web UI session
)

// ticketClaims is what a ticket carries. The client can read it (base64url
// JSON), so it holds no secret: the credential is named by reference and
// bound by credMAC, a peppered hash of the credential's own hash keyed by the
// jti, which says nothing without the pepper and links no two tickets.
type ticketClaims struct {
	JTI string `json:"jti"`
	// Exp is the end of the ticket's life, Unix milliseconds.
	Exp     int64  `json:"exp"`
	Tenant  string `json:"t"`
	Subject string `json:"s"`
	Kind    string `json:"k"`
	// Ref names the credential: a token def id, a config principal's id, or
	// a session's row key. Empty for the legacy secret.
	Ref string `json:"r,omitempty"`
	// Client is the app (RFC FL) an app token belongs to.
	Client  string `json:"c,omitempty"`
	CredMAC string `json:"cm"`
	// Authority is a digest of the minting credential's scopes, restrictions
	// and authority mode; the handshake's principal must match it.
	Authority string `json:"a"`
}

// errTicketUnavailable: this server cannot mint tickets (no secret key).
var errTicketUnavailable = errors.New("client-tools tickets need LOOMCYCLE_SECRET_KEY")

// clientToolsTicketKey is the HMAC key tickets are signed with, derived from
// the current LOOMCYCLE_SECRET_KEY. Tickets live 30 s, so the previous key of
// a rotation is not tried: a ticket minted across a rotation is refused and
// the client mints again.
func (s *Server) clientToolsTicketKey() ([]byte, error) {
	key, err := credential.DeriveClientToolsTicketKey(s.cfg().Env.SecretKey)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, errTicketUnavailable
	}
	return key, nil
}

// ticketMAC signs a ticket's encoded payload.
func ticketMAC(key []byte, payload string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(ticketFormat))
	m.Write([]byte{0})
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// ticketCredMAC binds a ticket to the credential hash it was minted from,
// keyed by the pepper and the jti (deviceCredMAC's pattern).
func ticketCredMAC(h auth.TokenHasher, jti, credHash string) string {
	return h.Hash("loomcycle/client-tools-ticket-cred/v1:" + jti + "\x00" + credHash)
}

// ticketAuthority digests what a principal may do as its credential grants
// it: the scopes, the restrictions and the authority mode. The ACL set is
// not in it — it is resolved at the handshake, as on any request.
func ticketAuthority(p auth.Principal) (string, error) {
	scopes := slices.Clone(p.Scopes)
	slices.Sort(scopes)
	r, err := json.Marshal(p.Restrictions)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.Join(scopes, "\n") + "\x00" + string(r) + "\x00" + p.AuthorityMode() +
		"\x00" + p.IssuedByGate + "\x00" + p.OAuthFamily))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// signTicket encodes and signs claims.
func signTicket(key []byte, c ticketClaims) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return ticketFormat + "." + payload + "." + base64.RawURLEncoding.EncodeToString(ticketMAC(key, payload)), nil
}

// verifyTicket checks a ticket's MAC (constant time) and decodes it. It says
// nothing of expiry or use; the reason is for the server log.
func verifyTicket(key []byte, t string) (ticketClaims, string) {
	if len(t) > ticketMaxLen {
		return ticketClaims{}, "malformed"
	}
	parts := strings.Split(t, ".")
	if len(parts) != 3 || parts[0] != ticketFormat {
		return ticketClaims{}, "malformed"
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ticketClaims{}, "malformed"
	}
	if !hmac.Equal(mac, ticketMAC(key, parts[1])) {
		return ticketClaims{}, "bad_signature"
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ticketClaims{}, "malformed"
	}
	var c ticketClaims
	if err := json.Unmarshal(b, &c); err != nil || c.JTI == "" || c.Exp == 0 || c.CredMAC == "" || c.Authority == "" {
		return ticketClaims{}, "malformed"
	}
	return c, ""
}

// clientToolsTicketOf returns the ticket a client-tools upgrade carries as a
// `ticket.<t>` subprotocol entry. Read on that upgrade only: anywhere else
// the entry is ignored, as the `bearer.` entry is.
func clientToolsTicketOf(r *http.Request) (string, bool) {
	if !isClientToolsUpgrade(r) {
		return "", false
	}
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			if t, ok := strings.CutPrefix(strings.TrimSpace(p), ticketSubprotocolPrefix); ok && t != "" {
				return t, true
			}
		}
	}
	return "", false
}

// handleClientToolsTicket serves POST /v1/client-tools/ticket: a one-time
// ticket for the client-tools socket, bound to the caller's credential. The
// auth middleware already admitted the caller exactly as it admits the
// socket, so a credential that could not open the socket was refused there,
// with the socket's code.
func (s *Server) handleClientToolsTicket(w http.ResponseWriter, r *http.Request) {
	if s.clientTools == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "client_tools_unavailable",
			"the client-tool host is not enabled on this server")
		return
	}
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		// Open mode: the socket takes no credential at all.
		writeJSONError(w, http.StatusServiceUnavailable, "ticket_unavailable",
			"sign-in is off on this server; open the client-tools socket without a credential")
		return
	}
	key, err := s.clientToolsTicketKey()
	if err != nil {
		if !errors.Is(err, errTicketUnavailable) {
			slog.Error("auth: client-tools ticket key unavailable", "err", err)
		}
		writeJSONError(w, http.StatusServiceUnavailable, "ticket_unavailable",
			"this server issues no client-tools tickets: LOOMCYCLE_SECRET_KEY is not set")
		return
	}
	c, err := s.ticketBinding(r, p)
	if err != nil {
		slog.Warn("auth: client-tools ticket not issued", "tenant", p.TenantID, "subject", p.Subject, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "ticket_unavailable",
			"a ticket could not be issued for this credential; try again")
		return
	}
	exp := time.Now().Add(clientToolsTicketTTL)
	c.Exp = exp.UnixMilli()
	t, err := signTicket(key, c)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "the ticket could not be issued")
		return
	}
	// D8: a ticket's use is traceable to the credential that minted it, by
	// its jti (logged again at the handshake). Never the ticket itself.
	s.recordAuthEvent(audit.Event{Action: "client_tools_ticket_issued", ActorTenant: p.TenantID, ActorSubject: p.Subject,
		ActorTokenSuffix: p.TokenSuffix, TargetDefID: tokenDefOf(c), AuthClient: p.OAuthClient,
		AuthGate: p.IssuedByGate, AuthReason: c.Kind, AuthRecordID: c.JTI, SourceAddr: s.clientSource(r)})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"ticket": t, "expires_at": exp.UTC().Format(time.RFC3339Nano)})
}

// tokenDefOf is the token def a ticket names, for the audit record.
func tokenDefOf(c ticketClaims) string {
	if c.Kind == ticketKindToken || c.Kind == ticketKindPrincipal {
		return c.Ref
	}
	return ""
}

// ticketBinding names the credential that authenticated the mint and binds
// its current hash. A Web UI session is bound as a session (its credential
// is re-checked through the session's own binding, as on every request); a
// bearer as the token, config principal or legacy secret it resolved to.
func (s *Server) ticketBinding(r *http.Request, p auth.Principal) (ticketClaims, error) {
	ctx := r.Context()
	h, err := s.hasher()
	if err != nil {
		return ticketClaims{}, err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return ticketClaims{}, err
	}
	authority, err := ticketAuthority(p)
	if err != nil {
		return ticketClaims{}, err
	}
	c := ticketClaims{JTI: base64.RawURLEncoding.EncodeToString(raw), Tenant: p.TenantID, Subject: p.Subject,
		Client: p.OAuthClient, Authority: authority}
	var credHash string
	if ua, ok := uiAuthFrom(ctx); ok && ua.session != nil {
		sess := *ua.session
		id, ok := s.sessionIDFromCookie(r)
		if !ok || !macEqual(sessionHash(id), sess.SessionHash) {
			return ticketClaims{}, errors.New("the session cookie does not match the session")
		}
		var pending bool
		_, ok, _ = s.boundCredentialPrincipal(ctx, sess, func(ch string) bool {
			if macEqual(credentialMAC(id, h, ch), sess.CredMAC) {
				credHash = ch
				return true
			}
			return false
		}, &pending)
		if !ok || pending || credHash == "" {
			return ticketClaims{}, errors.New("the session's credential did not resolve")
		}
		c.Kind, c.Ref = ticketKindSession, sess.SessionHash
	} else {
		switch {
		case p.Legacy:
			if s.cfg().Env.AuthToken == "" {
				return ticketClaims{}, errors.New("no legacy secret")
			}
			c.Kind, credHash = ticketKindLegacy, h.Hash(s.cfg().Env.AuthToken)
		case strings.HasPrefix(p.TokenDefID, "cfg:"):
			for _, d := range s.cfg().ResolvedPrincipals {
				if d.Principal.TokenDefID == p.TokenDefID && d.Secret != "" {
					credHash = h.Hash(d.Secret)
				}
			}
			c.Kind, c.Ref = ticketKindPrincipal, p.TokenDefID
		case p.TokenDefID != "":
			row, err := s.store.OperatorTokenDefGet(ctx, p.TokenDefID)
			if err != nil {
				return ticketClaims{}, err
			}
			c.Kind, c.Ref, credHash = ticketKindToken, row.DefID, row.TokenHash
		}
	}
	if c.Kind == "" || credHash == "" {
		return ticketClaims{}, errors.New("the credential names no token, principal, session or legacy secret")
	}
	c.CredMAC = ticketCredMAC(h, c.JTI, credHash)
	return c, nil
}

// redeemClientToolsTicket authenticates a client-tools upgrade by its ticket:
// signature, expiry, a single spend across replicas, then the minting
// credential re-resolved as it is now. A refusal is opaque to the client;
// the server log names the reason, never the ticket.
func (s *Server) redeemClientToolsTicket(r *http.Request, t string) (auth.Principal, bool) {
	p, reason := s.ticketPrincipal(r, t)
	if reason != "" {
		slog.Info("auth: client-tools ticket refused", "reason", reason)
		return auth.Principal{}, false
	}
	return p, true
}

func (s *Server) ticketPrincipal(r *http.Request, t string) (auth.Principal, string) {
	ctx := r.Context()
	key, err := s.clientToolsTicketKey()
	if err != nil {
		return auth.Principal{}, "no_ticket_key"
	}
	c, reason := verifyTicket(key, t)
	if reason != "" {
		return auth.Principal{}, reason
	}
	now := time.Now()
	exp := time.UnixMilli(c.Exp)
	if !now.Before(exp) {
		return auth.Principal{}, "expired"
	}
	// A ticket is minted with a 30 s life; one claiming much more was not
	// minted by this build (a clock far apart between replicas aside).
	if exp.Sub(now) > clientToolsTicketTTL+time.Minute {
		return auth.Principal{}, "malformed"
	}
	if s.store == nil {
		return auth.Principal{}, "no_store"
	}
	fresh, err := s.store.AuthAssertionUseOnce(ctx, clientToolsTicketReplayGate, c.JTI, exp, now)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("auth: spending a client-tools ticket failed (refusing)", "err", err)
		}
		return auth.Principal{}, "store_error"
	}
	if !fresh {
		return auth.Principal{}, "spent"
	}
	h, err := s.hasher()
	if err != nil {
		return auth.Principal{}, "no_hasher"
	}
	bound := func(credHash string) bool { return macEqual(ticketCredMAC(h, c.JTI, credHash), c.CredMAC) }
	p, reason := s.ticketCredentialPrincipal(r, c, bound)
	if reason != "" {
		return auth.Principal{}, reason
	}
	authority, err := ticketAuthority(p)
	if err != nil {
		return auth.Principal{}, "authority_unreadable"
	}
	if p.TenantID != c.Tenant || p.Subject != c.Subject || p.OAuthClient != c.Client || !macEqual(authority, c.Authority) {
		return auth.Principal{}, "authority_changed"
	}
	slog.Info("auth: client-tools ticket accepted", "jti", c.JTI, "tenant", p.TenantID, "subject", p.Subject, "kind", c.Kind)
	return p, ""
}

// ticketCredentialPrincipal re-resolves the credential a ticket names, as a
// request carrying it now would: the token row (live user, issuing gate and
// app, tenant gate policy), the config principal or legacy secret through the
// bearer lookup, or the session through its own credential binding.
func (s *Server) ticketCredentialPrincipal(r *http.Request, c ticketClaims, bound func(string) bool) (auth.Principal, string) {
	ctx := r.Context()
	cfg := s.cfg()
	h, err := s.hasher()
	if err != nil {
		return auth.Principal{}, "no_hasher"
	}
	switch c.Kind {
	case ticketKindToken:
		row, err := s.store.OperatorTokenDefGet(ctx, c.Ref)
		if err != nil {
			if isNotFound(err) {
				return auth.Principal{}, "credential_gone"
			}
			return auth.Principal{}, "store_error"
		}
		if !bound(row.TokenHash) {
			return auth.Principal{}, "credential_changed"
		}
		p, ok, _ := tokenRowPrincipal(row, time.Now())
		if !ok {
			return auth.Principal{}, "credential_ended"
		}
		if p, ok = s.liveTokenPrincipal(ctx, p); !ok || !s.issuedTokenAllowed(ctx, p) {
			return auth.Principal{}, "credential_ended"
		}
		return s.enrichPrincipal(ctx, p), ""
	case ticketKindPrincipal:
		for _, d := range cfg.ResolvedPrincipals {
			if d.Principal.TokenDefID != c.Ref || d.Secret == "" || !bound(h.Hash(d.Secret)) {
				continue
			}
			p, ok := s.resolvePrincipal(ctx, d.Secret)
			if !ok || p.TokenDefID != c.Ref {
				return auth.Principal{}, "credential_ended"
			}
			return p, ""
		}
		return auth.Principal{}, "credential_gone"
	case ticketKindLegacy:
		secret := cfg.Env.AuthToken
		if secret == "" || !bound(h.Hash(secret)) {
			return auth.Principal{}, "credential_gone"
		}
		p, ok := s.resolvePrincipal(ctx, secret)
		if !ok || !p.Legacy {
			return auth.Principal{}, "credential_ended"
		}
		return p, ""
	case ticketKindSession:
		return s.ticketSessionPrincipal(r, c, bound)
	}
	return auth.Principal{}, "malformed"
}

// ticketSessionPrincipal re-checks a Web UI session a ticket was minted
// under: not ended, not expired, its credential still the one bound, the
// tenant's gate policy, and a header gate's proxy still vouching on this
// request. The parts of sessionPrincipal that need the session id (the
// browser check) or write (touching, ending the session) are not repeated:
// the ticket is not the session, and a refused ticket ends nothing.
func (s *Server) ticketSessionPrincipal(r *http.Request, c ticketClaims, bound func(string) bool) (auth.Principal, string) {
	ctx := r.Context()
	row, err := s.store.UISessionGet(ctx, c.Ref)
	if err != nil {
		if isNotFound(err) {
			return auth.Principal{}, "credential_gone"
		}
		return auth.Principal{}, "store_error"
	}
	if !row.RevokedAt.IsZero() || !time.Now().Before(sessionExpiresAt(row, s.cfg().ResolvedAuth)) {
		return auth.Principal{}, "credential_ended"
	}
	var pending bool
	p, ok, _ := s.boundCredentialPrincipal(ctx, row, bound, &pending)
	if !ok || pending {
		return auth.Principal{}, "credential_ended"
	}
	for _, gate := range []string{row.Gate, p.IssuedByGate} {
		if gate == "" {
			continue
		}
		if err := s.tenantGateAllowed(ctx, row.TenantID, gate); err != nil {
			return auth.Principal{}, "tenant_policy"
		}
	}
	if holds, _ := s.edgeSessionHolds(r, row); !holds {
		return auth.Principal{}, "proxy_assertion_lost"
	}
	p.Login = sessionLogin(row)
	return p, ""
}
