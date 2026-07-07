package console

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Provider identifies how an operator authenticated.
const (
	ProviderLocal  = "local"
	ProviderGoogle = "google"
	ProviderGitHub = "github"
)

// Session is an authenticated console operator, carried in a SIGNED, HttpOnly
// cookie — never a server-side store — so any replica validates it statelessly.
type Session struct {
	Subject  string `json:"sub"`  // stable id: the SSO email, or "local:<user>"
	Name     string `json:"name"` // display name
	Provider string `json:"idp"`  // local | google | github
	Admin    bool   `json:"adm"`
	Expires  int64  `json:"exp"` // unix seconds
}

// Account is a local (username/password) operator, provisioned in config. The
// stored PWHash is a PBKDF2 string (see HashPassword); the plaintext is never kept.
type Account struct {
	Username string
	PWHash   string
	Admin    bool
}

// Options configures the Manager. Secret MUST be >= 32 bytes (from env, Invariant
// #4). Cookies are Secure by DEFAULT (safe-by-default for a control-plane session);
// AllowInsecure opts out for local HTTP dev only.
type Options struct {
	Secret        []byte
	TTL           time.Duration
	Accounts      []Account
	AdminEmails   []string
	AdminDomains  []string
	CookieName    string
	AllowInsecure bool // dev-only: emit session cookies WITHOUT the Secure flag
}

// Manager verifies logins and mints/validates session + state tokens.
type Manager struct {
	secret     []byte
	ttl        time.Duration
	accounts   map[string]Account // username -> account
	adminEmail map[string]bool    // lowercased
	adminDom   map[string]bool    // lowercased, no leading '@'
	cookie     string
	secure     bool
}

// ErrShortSecret means the signing secret is too weak to sign sessions.
var ErrShortSecret = errors.New("console: session secret must be >= 32 bytes")

// ErrBadLogin is returned for any local-login failure (unknown user OR wrong
// password) — one error so the response can't distinguish the two (no user enum).
var ErrBadLogin = errors.New("console: invalid credentials")

// New builds a Manager. It fails closed if the secret is too short.
func New(o Options) (*Manager, error) {
	if len(o.Secret) < 32 {
		return nil, ErrShortSecret
	}
	if o.TTL <= 0 {
		o.TTL = 8 * time.Hour
	}
	if o.CookieName == "" {
		o.CookieName = "heave_console"
	}
	m := &Manager{
		secret:     o.Secret,
		ttl:        o.TTL,
		accounts:   make(map[string]Account, len(o.Accounts)),
		adminEmail: map[string]bool{},
		adminDom:   map[string]bool{},
		cookie:     o.CookieName,
		secure:     !o.AllowInsecure, // Secure by default; opt out only for dev HTTP
	}
	for _, a := range o.Accounts {
		m.accounts[a.Username] = a
	}
	for _, e := range o.AdminEmails {
		m.adminEmail[strings.ToLower(strings.TrimSpace(e))] = true
	}
	for _, d := range o.AdminDomains {
		// Normalize trim → lower → strip '@', so " @Eng.Acme.com " keys as
		// "eng.acme.com" (stripping '@' before trimming would keep a leading '@').
		m.adminDom[strings.TrimPrefix(strings.ToLower(strings.TrimSpace(d)), "@")] = true
	}
	return m, nil
}

// LocalLogin verifies a username/password and returns a session. To blunt user
// enumeration + timing, an unknown user still runs a PBKDF2 verify against a dummy
// hash, and every failure returns the same ErrBadLogin.
func (m *Manager) LocalLogin(username, password string) (Session, error) {
	acct, ok := m.accounts[username]
	if !ok {
		// Constant-ish work + uniform error: verify against a throwaway hash so the
		// unknown-user path costs about the same as the wrong-password path.
		_ = VerifyPassword(dummyHash, password)
		return Session{}, ErrBadLogin
	}
	if !VerifyPassword(acct.PWHash, password) {
		return Session{}, ErrBadLogin
	}
	return m.newSession("local:"+acct.Username, acct.Username, ProviderLocal, acct.Admin), nil
}

// dummyHash is a fixed, well-formed PBKDF2 hash of an unguessable placeholder,
// used only to equalize the unknown-user login path's cost (it parses and runs a
// real PBKDF2 verify). A constant — not a computed global — so it never matches a
// real password and costs nothing at startup.
const dummyHash = "pbkdf2$sha256$210000$bfbL6+18NVK+LlEp6KHcdQ$Z7JwLYiNgG4UW/9UiHLGyApFlwkLYjc02AT6JPXZZoA"

// AdminForEmail reports whether an SSO-verified email is authorized as admin
// (exact email allowlist OR its domain). Email is matched case-insensitively.
func (m *Manager) AdminForEmail(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	if m.adminEmail[email] {
		return true
	}
	at := strings.LastIndexByte(email, '@')
	return at >= 0 && m.adminDom[email[at+1:]]
}

// NewSSOSession builds a session for an SSO-verified identity, authorizing admin
// via the allowlist. Non-allowlisted identities get Admin=false (the caller
// decides whether a non-admin may hold a session at all).
func (m *Manager) NewSSOSession(email, name, provider string) Session {
	email = strings.ToLower(strings.TrimSpace(email)) // match AdminForEmail's normalization
	if name == "" {
		name = email
	}
	return m.newSession(email, name, provider, m.AdminForEmail(email))
}

func (m *Manager) newSession(sub, name, provider string, admin bool) Session {
	return Session{Subject: sub, Name: name, Provider: provider, Admin: admin,
		Expires: time.Now().Add(m.ttl).Unix()}
}

// --- signed-token codec (sessions + OAuth state share it) ---

func (m *Manager) sign(payload []byte) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify checks the HMAC (constant time) and returns the raw payload.
func (m *Manager) verify(token string) ([]byte, bool) {
	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(token[:dot])
	if err != nil {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(token[dot+1:])
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, m.secret)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return nil, false
	}
	return payload, true
}

// Encode signs a session into an opaque cookie value. json.Marshal cannot fail for
// the fixed-shape Session (only strings/bool/int64), so its error is discarded.
func (m *Manager) Encode(s Session) string {
	b, _ := json.Marshal(s)
	return m.sign(b)
}

// Decode verifies a cookie value and returns the session if the signature is valid
// AND it has not expired. NOTE: sessions are STATELESS — there is no server-side
// revocation, so a leaked token is valid until its expiry (Clear only deletes the
// browser cookie). Keep the TTL modest; a jti/denylist revocation hook is a
// deferred enhancement (see docs/reviews/phase8-console-auth.md).
func (m *Manager) Decode(token string) (Session, bool) {
	payload, ok := m.verify(token)
	if !ok {
		return Session{}, false
	}
	var s Session
	if err := json.Unmarshal(payload, &s); err != nil {
		return Session{}, false
	}
	if time.Now().Unix() >= s.Expires {
		return Session{}, false
	}
	return s, true
}

// --- cookies ---

// Issue writes the session cookie.
func (m *Manager) Issue(w http.ResponseWriter, s Session) {
	http.SetCookie(w, &http.Cookie{
		Name: m.cookie, Value: m.Encode(s), Path: "/",
		HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode,
		Expires: time.Unix(s.Expires, 0),
	})
}

// Clear expires the session cookie (logout).
func (m *Manager) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: m.cookie, Value: "", Path: "/",
		HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode,
		MaxAge: -1,
	})
}

// SessionFrom reads and validates the session cookie on a request.
func (m *Manager) SessionFrom(r *http.Request) (Session, bool) {
	c, err := r.Cookie(m.cookie)
	if err != nil {
		return Session{}, false
	}
	return m.Decode(c.Value)
}

// --- OAuth state (CSRF) — a short-lived signed token binding the callback ---

type stateClaim struct {
	Nonce    string `json:"n"`
	Redirect string `json:"r"`
	Expires  int64  `json:"exp"`
}

// SignState returns a signed, short-lived state token carrying a nonce (echoed via
// a cookie for double-submit) and an optional post-login redirect path. The
// redirect is integrity-protected (HMAC) but NOT validated here — the caller MUST
// pass only a sanitized, same-origin/relative path, or an attacker who controls it
// pre-sign gets an open redirect post-login.
func (m *Manager) SignState(nonce, redirect string, ttl time.Duration) string {
	b, _ := json.Marshal(stateClaim{Nonce: nonce, Redirect: redirect, Expires: time.Now().Add(ttl).Unix()})
	return m.sign(b)
}

// VerifyState validates a state token against the expected nonce (constant-time)
// and returns the redirect path. Fails on a bad signature, expiry, or nonce
// mismatch — the OAuth CSRF guard.
func (m *Manager) VerifyState(token, expectNonce string) (redirect string, ok bool) {
	payload, ok := m.verify(token)
	if !ok {
		return "", false
	}
	var c stateClaim
	if err := json.Unmarshal(payload, &c); err != nil {
		return "", false
	}
	if time.Now().Unix() >= c.Expires {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(expectNonce)) != 1 {
		return "", false
	}
	return c.Redirect, true
}
