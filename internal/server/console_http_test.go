package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/somasays/heave/internal/console"
	"github.com/somasays/heave/internal/controls"
	"github.com/somasays/heave/internal/policy"
	"github.com/somasays/heave/internal/provider"
	"github.com/somasays/heave/internal/router"
)

// fakeIdP stands in for Google/GitHub so the callback flow is testable without a
// real provider round-trip.
type fakeIdP struct {
	email, name string
	err         error
}

func (f fakeIdP) AuthCodeURL(state, redirectURI string) string {
	return "https://idp.test/authorize?state=" + state + "&redirect_uri=" + redirectURI
}
func (f fakeIdP) FetchIdentity(_ context.Context, _, _ string) (string, string, error) {
	return f.email, f.name, f.err
}

func newConsoleEnv(t *testing.T, idp OAuthProvider) http.Handler {
	t.Helper()
	pw, _ := console.HashPassword("s3cret")
	mgr, err := console.New(console.Options{
		Secret:        secretBytes(),
		Accounts:      []console.Account{{Username: "ops", PWHash: pw, Admin: true}, {Username: "viewer", PWHash: pw, Admin: false}},
		AdminDomains:  []string{"acme.com"},
		AllowInsecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := policy.New()
	must(t, store.CreateOrg("acme", "Acme", policy.Limits{}))
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "up"}}, "m")
	fp := &fakeProvider{resp: &provider.Response{Content: "ok"}}
	oauth := map[string]OAuthProvider{}
	if idp != nil {
		oauth["google"] = idp
	}
	srv := newTestServer(t, Deps{
		Router: rtr, Providers: map[string]provider.Provider{"fake": fp},
		Guard: controls.New(true, nil, nil), Policy: store,
		Console: mgr, OAuth: oauth, ConsoleBaseURL: "https://gw.test", ConsoleSecure: false,
	}, Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	return srv.Handler()
}

func secretBytes() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = 'k'
	}
	return b
}

func postJSON(t *testing.T, h http.Handler, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func sessionCookie(rr *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == "heave_console" && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestConsoleLocalLoginGrantsAdminSession(t *testing.T) {
	h := newConsoleEnv(t, nil)
	rr := postJSON(t, h, "/console/login", `{"username":"ops","password":"s3cret"}`)
	if rr.Code != 200 {
		t.Fatalf("valid login want 200, got %d (%s)", rr.Code, rr.Body)
	}
	sc := sessionCookie(rr)
	if sc == nil {
		t.Fatal("login must set a session cookie")
	}
	// The session cookie grants access to the admin-gated management API.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/policy", nil)
	req.AddCookie(sc)
	pr := httptest.NewRecorder()
	h.ServeHTTP(pr, req)
	if pr.Code != 200 {
		t.Fatalf("an admin session must access /v1/policy, got %d", pr.Code)
	}
}

func TestConsoleLocalLoginRejections(t *testing.T) {
	h := newConsoleEnv(t, nil)
	if rr := postJSON(t, h, "/console/login", `{"username":"ops","password":"wrong"}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password want 401, got %d", rr.Code)
	}
	if rr := postJSON(t, h, "/console/login", `{"username":"viewer","password":"s3cret"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("a non-admin account want 403, got %d", rr.Code)
	}
	// No session and no bearer → management API is unauthorized (401 with auth on).
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/policy", nil)
	pr := httptest.NewRecorder()
	h.ServeHTTP(pr, req)
	if pr.Code != http.StatusUnauthorized {
		t.Fatalf("no session/bearer must be 401, got %d", pr.Code)
	}
}

func TestOAuthStartRedirectsWithStateAndNonce(t *testing.T) {
	h := newConsoleEnv(t, fakeIdP{email: "ceo@acme.com", name: "CEO"})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/console/auth/google/start?return=/console/spend", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("start must 302, got %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "idp.test/authorize") || !strings.Contains(loc, "state=") {
		t.Fatalf("must redirect to the IdP with a state, got %q", loc)
	}
	var nonce *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == nonceCookie {
			nonce = c
		}
	}
	if nonce == nil || nonce.Value == "" {
		t.Fatal("start must set a double-submit nonce cookie")
	}
	// Unknown provider → 404.
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/console/auth/twitter/start", nil)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("unknown provider must 404, got %d", rr2.Code)
	}
}

// oauthRoundTrip runs start → grabs the real signed state + nonce → drives callback.
func oauthRoundTrip(t *testing.T, h http.Handler, provider, code string) *httptest.ResponseRecorder {
	t.Helper()
	sr := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/console/auth/"+provider+"/start", nil)
	srr := httptest.NewRecorder()
	h.ServeHTTP(srr, sr)
	loc := srr.Header().Get("Location")
	state := loc[strings.Index(loc, "state=")+len("state="):]
	if i := strings.IndexByte(state, '&'); i >= 0 {
		state = state[:i]
	}
	var nonce *http.Cookie
	for _, c := range srr.Result().Cookies() {
		if c.Name == nonceCookie {
			nonce = c
		}
	}
	cb := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/console/auth/"+provider+"/callback?code="+code+"&state="+state, nil)
	cb.AddCookie(nonce)
	crr := httptest.NewRecorder()
	h.ServeHTTP(crr, cb)
	return crr
}

func TestOAuthCallbackHappyPathIssuesSession(t *testing.T) {
	h := newConsoleEnv(t, fakeIdP{email: "ceo@acme.com", name: "CEO"}) // allowlisted domain
	rr := oauthRoundTrip(t, h, "google", "authcode")
	if rr.Code != http.StatusFound {
		t.Fatalf("callback must 302 on success, got %d (%s)", rr.Code, rr.Body)
	}
	if sessionCookie(rr) == nil {
		t.Fatal("a successful SSO callback must issue a session cookie")
	}
}

func TestOAuthCallbackRejectsBadStateAndNonAdmin(t *testing.T) {
	// CSRF: a callback whose state doesn't match the nonce cookie is rejected.
	h := newConsoleEnv(t, fakeIdP{email: "ceo@acme.com"})
	sr := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/console/auth/google/start", nil)
	srr := httptest.NewRecorder()
	h.ServeHTTP(srr, sr)
	var nonce *http.Cookie
	for _, c := range srr.Result().Cookies() {
		if c.Name == nonceCookie {
			nonce = c
		}
	}
	bad := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/console/auth/google/callback?code=x&state=forged", nil)
	bad.AddCookie(nonce)
	brr := httptest.NewRecorder()
	h.ServeHTTP(brr, bad)
	if brr.Code != http.StatusBadRequest {
		t.Fatalf("a forged state must be 400, got %d", brr.Code)
	}
	// A verified but NON-allowlisted identity is refused a session (403).
	h2 := newConsoleEnv(t, fakeIdP{email: "outsider@evil.com", name: "X"})
	if rr := oauthRoundTrip(t, h2, "google", "code"); rr.Code != http.StatusForbidden {
		t.Fatalf("a non-allowlisted identity must be 403, got %d", rr.Code)
	}
}

func TestConsolePageAndInfo(t *testing.T) {
	h := newConsoleEnv(t, fakeIdP{email: "ceo@acme.com"})
	// The SPA shell is served (open, like a login page).
	pg := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/console", nil)
	pr := httptest.NewRecorder()
	h.ServeHTTP(pr, pg)
	if pr.Code != 200 || !strings.Contains(pr.Body.String(), "control plane") {
		t.Fatalf("/console must serve the SPA, got %d", pr.Code)
	}
	if ct := pr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("/console must be html, got %q", ct)
	}
	// The script must be an EXTERNAL same-origin file (the page CSP is default-src
	// 'self', which forbids inline <script> — regression: an inline script rendered
	// a blank page). And that file must actually serve.
	if !strings.Contains(pr.Body.String(), `src="/console/app.js"`) {
		t.Fatal("console must load its script from /console/app.js (not inline; CSP blocks inline)")
	}
	jr := httptest.NewRecorder()
	h.ServeHTTP(jr, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/console/app.js", nil))
	if jr.Code != 200 || !strings.HasPrefix(jr.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("/console/app.js must serve JS, got %d %q", jr.Code, jr.Header().Get("Content-Type"))
	}
	// /console/info reflects no session + the configured providers.
	ir := httptest.NewRecorder()
	h.ServeHTTP(ir, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/console/info", nil))
	var info struct {
		Providers     []string `json:"providers"`
		Authenticated bool     `json:"authenticated"`
	}
	_ = json.Unmarshal(ir.Body.Bytes(), &info)
	if info.Authenticated {
		t.Fatal("no cookie ⇒ not authenticated")
	}
	if len(info.Providers) != 1 || info.Providers[0] != "google" {
		t.Fatalf("info must list configured providers, got %+v", info.Providers)
	}
	// After login, info reports authenticated.
	lr := postJSON(t, h, "/console/login", `{"username":"ops","password":"s3cret"}`)
	sc := sessionCookie(lr)
	ir2 := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/console/info", nil)
	req.AddCookie(sc)
	h.ServeHTTP(ir2, req)
	if !strings.Contains(ir2.Body.String(), `"authenticated":true`) {
		t.Fatalf("info with an admin session must be authenticated, got %s", ir2.Body)
	}
}

func TestSafeReturnPath(t *testing.T) {
	for in, want := range map[string]string{
		"/console/spend":      "/console/spend",
		"//evil.com":          "/console", // protocol-relative → rejected
		"/\\evil.com":         "/console", // backslash protocol-relative → rejected
		"/a\\b":               "/console", // any backslash → rejected
		"/a\r\nSet-Cookie: x": "/console", // CRLF → rejected
		"https://evil":        "/console",
		"":                    "/console",
		"javascript:x":        "/console",
	} {
		if got := safeReturnPath(in); got != want {
			t.Fatalf("safeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}
