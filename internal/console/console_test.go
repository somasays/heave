package console

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func secret() []byte { return bytes.Repeat([]byte("k"), 32) }

func newMgr(t *testing.T, o Options) *Manager {
	t.Helper()
	if o.Secret == nil {
		o.Secret = secret()
	}
	m, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestPasswordHashVerifyRoundTrip(t *testing.T) {
	h, err := HashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(h, "hunter2") {
		t.Fatal("correct password must verify")
	}
	if VerifyPassword(h, "wrong") {
		t.Fatal("wrong password must not verify")
	}
	// Two hashes of the same password differ (random salt).
	h2, _ := HashPassword("hunter2")
	if h == h2 {
		t.Fatal("salt must make each hash unique")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, bad := range []string{"", "plain", "pbkdf2$sha256$x$y", "bcrypt$1$2$3$4",
		"pbkdf2$sha256$notint$c2FsdA$aGFzaA", "pbkdf2$sha256$1000$!!!$aGFzaA"} {
		if VerifyPassword(bad, "any") {
			t.Fatalf("malformed hash %q must not verify (and must not panic)", bad)
		}
	}
}

func TestShortSecretRejected(t *testing.T) {
	if _, err := New(Options{Secret: []byte("tooshort")}); err != ErrShortSecret {
		t.Fatalf("a <32B secret must be rejected, got %v", err)
	}
}

func TestLocalLoginUniformErrorAndSession(t *testing.T) {
	pw, _ := HashPassword("s3cret")
	m := newMgr(t, Options{Accounts: []Account{{Username: "ops", PWHash: pw, Admin: true}}})
	// Success.
	s, err := m.LocalLogin("ops", "s3cret")
	if err != nil || !s.Admin || s.Provider != ProviderLocal || s.Subject != "local:ops" {
		t.Fatalf("valid login must return an admin session, got %+v err=%v", s, err)
	}
	// Wrong password AND unknown user both return the SAME error (no user enum).
	if _, err := m.LocalLogin("ops", "nope"); err != ErrBadLogin {
		t.Fatalf("wrong password must be ErrBadLogin, got %v", err)
	}
	if _, err := m.LocalLogin("ghost", "whatever"); err != ErrBadLogin {
		t.Fatalf("unknown user must be ErrBadLogin (same as wrong pw), got %v", err)
	}
}

func TestAdminAllowlistEmailAndDomain(t *testing.T) {
	m := newMgr(t, Options{AdminEmails: []string{"CEO@Acme.com"}, AdminDomains: []string{"@eng.acme.com"}})
	cases := map[string]bool{
		"ceo@acme.com":      true,  // exact (case-insensitive)
		"dev@eng.acme.com":  true,  // domain
		"dev@acme.com":      false, // not the exact email, not the eng domain
		"attacker@evil.com": false,
		"":                  false,
		"noat":              false,
	}
	for email, want := range cases {
		if got := m.AdminForEmail(email); got != want {
			t.Fatalf("AdminForEmail(%q) = %v, want %v", email, got, want)
		}
	}
	// A non-allowlisted SSO identity gets a NON-admin session...
	if s := m.NewSSOSession("attacker@evil.com", "Bad Actor", ProviderGoogle); s.Admin {
		t.Fatal("a non-allowlisted SSO email must not be admin")
	}
	// ...and an allowlisted one (exact + domain) gets an ADMIN session.
	if s := m.NewSSOSession("ceo@acme.com", "CEO", ProviderGoogle); !s.Admin {
		t.Fatal("an allowlisted SSO email must be admin")
	}
	if s := m.NewSSOSession("dev@eng.acme.com", "Dev", ProviderGitHub); !s.Admin {
		t.Fatal("an allowlisted SSO domain must be admin")
	}
}

func TestDomainAllowlistNormalizesWhitespace(t *testing.T) {
	// A padded config domain (" @Eng.Acme.com ") must still match (trim→lower→strip).
	m := newMgr(t, Options{AdminDomains: []string{" @Eng.Acme.com "}})
	if !m.AdminForEmail("dev@eng.acme.com") {
		t.Fatal("a whitespace/case-padded admin domain must still authorize")
	}
}

func TestVerifyPasswordRejectsWeakWorkFactor(t *testing.T) {
	// A downgraded stored hash (low iterations / tiny digest) must be rejected even
	// with the "right" password — no silent cheap-verify.
	for _, weak := range []string{
		"pbkdf2$sha256$1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaA", // iter=1
		"pbkdf2$sha256$210000$c2FsdA$aGFzaA",                            // 4-byte salt/dk
	} {
		if VerifyPassword(weak, "any") {
			t.Fatalf("a weak-parameter hash must be rejected: %q", weak)
		}
	}
	// A properly-generated hash still verifies (floor is not an equality).
	h, _ := HashPassword("ok")
	if !VerifyPassword(h, "ok") {
		t.Fatal("a 210k-iter hash must still verify")
	}
}

func TestSessionCookieRoundTripTamperAndExpiry(t *testing.T) {
	m := newMgr(t, Options{TTL: time.Hour})
	s := m.NewSSOSession("ceo@acme.com", "CEO", ProviderGoogle)
	tok := m.Encode(s)
	got, ok := m.Decode(tok)
	if !ok || got.Subject != "ceo@acme.com" {
		t.Fatalf("round-trip failed: ok=%v %+v", ok, got)
	}
	// Tamper: flip a byte in the payload → HMAC fails.
	if _, ok := m.Decode("X" + tok[1:]); ok {
		t.Fatal("a tampered token must not decode")
	}
	// A different secret must not accept the token (no cross-signing).
	other := newMgr(t, Options{Secret: bytes.Repeat([]byte("x"), 32)})
	if _, ok := other.Decode(tok); ok {
		t.Fatal("a token signed by another secret must not decode")
	}
	// Expired session → rejected even with a valid signature.
	expired := Session{Subject: "e", Provider: ProviderLocal, Expires: time.Now().Add(-time.Second).Unix()}
	if _, ok := m.Decode(m.Encode(expired)); ok {
		t.Fatal("an expired session must not decode")
	}
}

func TestIssueClearCookie(t *testing.T) {
	m := newMgr(t, Options{}) // Secure is the DEFAULT (no AllowInsecure)
	s := m.NewSSOSession("ceo@acme.com", "CEO", ProviderGoogle)
	rec := httptest.NewRecorder()
	m.Issue(rec, s)
	c := rec.Result().Cookies()
	if len(c) != 1 || c[0].Value == "" || !c[0].HttpOnly || !c[0].Secure {
		t.Fatalf("session cookie must be set HttpOnly+Secure, got %+v", c)
	}
	// SessionFrom reads it back.
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
	req.AddCookie(c[0])
	if got, ok := m.SessionFrom(req); !ok || got.Subject != "ceo@acme.com" {
		t.Fatalf("SessionFrom must recover the session, got ok=%v %+v", ok, got)
	}
	// Clear expires it (MaxAge<0).
	rec2 := httptest.NewRecorder()
	m.Clear(rec2)
	if cc := rec2.Result().Cookies(); len(cc) != 1 || cc[0].MaxAge >= 0 {
		t.Fatalf("Clear must expire the cookie, got %+v", cc)
	}
}

func TestOAuthStateCSRF(t *testing.T) {
	m := newMgr(t, Options{})
	tok := m.SignState("nonce-abc", "/console", 5*time.Minute)
	// Correct nonce → valid, returns the redirect.
	if redir, ok := m.VerifyState(tok, "nonce-abc"); !ok || redir != "/console" {
		t.Fatalf("valid state must verify with its redirect, got ok=%v redir=%q", ok, redir)
	}
	// Wrong nonce (a forged/mismatched callback) → rejected (the CSRF guard).
	if _, ok := m.VerifyState(tok, "nonce-xyz"); ok {
		t.Fatal("a state token with a mismatched nonce must be rejected")
	}
	// Tampered token → rejected.
	if _, ok := m.VerifyState(tok+"x", "nonce-abc"); ok {
		t.Fatal("a tampered state token must be rejected")
	}
	// Expired state → rejected.
	exp := m.SignState("n", "/", -time.Second)
	if _, ok := m.VerifyState(exp, "n"); ok {
		t.Fatal("an expired state token must be rejected")
	}
}

func TestDummyHashParsesForTimingEqualization(t *testing.T) {
	// The unknown-user path verifies against dummyHash; it must be a well-formed hash
	// that actually runs PBKDF2 (else the timing-equalization is defeated) and never
	// matches a real password.
	if !strings.HasPrefix(dummyHash, "pbkdf2$sha256$") {
		t.Fatal("dummyHash must be a pbkdf2 string")
	}
	if VerifyPassword(dummyHash, "") || VerifyPassword(dummyHash, "password") {
		t.Fatal("dummyHash must not match a real password")
	}
}
