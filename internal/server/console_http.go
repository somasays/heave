package server

// The admin console's authentication HTTP surface (mounted only when the console
// is configured): local username/password login, Google/GitHub SSO via the OAuth
// authorization-code flow, and logout. On success it issues the signed session
// cookie from internal/console; requireAdmin then accepts that session (browser)
// OR an admin bearer key (API/PEP). heave is a spend firewall, so the console is
// admin-only: a non-admin identity is refused a session.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	errUnverifiedEmail = errors.New("sso: no verified primary email")
	errNoToken         = errors.New("sso: token exchange returned no access_token")
	errTokenStatus     = errors.New("sso: token endpoint returned non-200")
	errAPIStatus       = errors.New("sso: identity endpoint returned non-200")
)

func encodeQuery(m map[string]string) string {
	v := url.Values{}
	for k, val := range m {
		v.Set(k, val)
	}
	return "?" + v.Encode()
}

// oauthToken does the authorization-code → access-token exchange (form-encoded POST,
// JSON response) shared by both providers.
func oauthToken(ctx context.Context, client *http.Client, tokenURL string, form map[string]string) (string, error) {
	body := url.Values{}
	for k, val := range form {
		body.Set(k, val)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", errTokenStatus
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", err
	}
	if t.AccessToken == "" {
		return "", errNoToken
	}
	return t.AccessToken, nil
}

// oauthGetJSON does an authenticated GET and decodes the JSON body into dst.
func oauthGetJSON(ctx context.Context, client *http.Client, apiURL, token string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errAPIStatus
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

// OAuthProvider is one SSO provider. Concrete Google/GitHub impls do the token exchange;
// tests inject a fake so the callback flow is exercised without a real IdP.
type OAuthProvider interface {
	// AuthCodeURL is where the browser is sent to authenticate.
	AuthCodeURL(state, redirectURI string) string
	// FetchIdentity exchanges the callback code for the user's verified email + name.
	FetchIdentity(ctx context.Context, code, redirectURI string) (email, name string, err error)
}

// NewGoogleIdP / NewGitHubIdP build the concrete providers (composition root only).
// A nil client uses http.DefaultClient with a sane timeout.
func NewGoogleIdP(clientID, clientSecret string) OAuthProvider {
	return &googleIdP{clientID: clientID, clientSecret: clientSecret, http: oauthHTTPClient()}
}

// NewGitHubIdP builds the GitHub OAuth provider (composition root only).
func NewGitHubIdP(clientID, clientSecret string) OAuthProvider {
	return &githubIdP{clientID: clientID, clientSecret: clientSecret, http: oauthHTTPClient()}
}

func oauthHTTPClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

const nonceCookie = "heave_oauth_nonce"

// adminSession returns a valid ADMIN console session on the request, if any.
func (s *Server) adminSession(r *http.Request) bool {
	if s.console == nil {
		return false
	}
	sess, ok := s.console.SessionFrom(r)
	return ok && sess.Admin
}

// handleConsoleLoginLocal authenticates a local operator account.
func (s *Server) handleConsoleLoginLocal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !s.decodeAdmin(w, r, &body) {
		return
	}
	sess, err := s.console.LocalLogin(body.Username, body.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid credentials")
		return
	}
	if !sess.Admin {
		writeError(w, http.StatusForbidden, "permission_error", "account is not authorized for the console")
		return
	}
	s.console.Issue(w, sess)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": sess.Name})
}

// handleConsoleLogout clears the session cookie (client-side; sessions are stateless).
func (s *Server) handleConsoleLogout(w http.ResponseWriter, _ *http.Request) {
	s.console.Clear(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleOAuthStart redirects to the provider with a signed state + a double-submit
// nonce cookie (CSRF), carrying a sanitized post-login return path.
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	idp, ok := s.oauth[r.PathValue("provider")]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown sso provider")
		return
	}
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "state error")
		return
	}
	nonce := hex.EncodeToString(nb[:])
	// The post-login redirect is sanitized to a same-origin relative path so it can't
	// become an open redirect (the state carries it integrity-protected).
	ret := safeReturnPath(r.URL.Query().Get("return"))
	state := s.console.SignState(nonce, ret, 10*time.Minute)
	http.SetCookie(w, &http.Cookie{
		Name: nonceCookie, Value: nonce, Path: "/console/", HttpOnly: true,
		Secure: s.consoleSecure, SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	http.Redirect(w, r, idp.AuthCodeURL(state, s.oauthRedirectURI(r)), http.StatusFound)
}

// handleOAuthCallback verifies the state (CSRF), exchanges the code for a verified
// identity, authorizes it against the admin allowlist, and issues the session.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	idp, ok := s.oauth[provider]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown sso provider")
		return
	}
	c, err := r.Cookie(nonceCookie)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing state")
		return
	}
	ret, ok := s.console.VerifyState(r.URL.Query().Get("state"), c.Value)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid state")
		return
	}
	// Consume the nonce cookie (single-use).
	http.SetCookie(w, &http.Cookie{Name: nonceCookie, Value: "", Path: "/console/", MaxAge: -1,
		HttpOnly: true, Secure: s.consoleSecure, SameSite: http.SameSiteLaxMode})

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	email, name, err := idp.FetchIdentity(ctx, r.URL.Query().Get("code"), s.oauthRedirectURI(r))
	if err != nil || email == "" {
		s.log.Warn("sso identity fetch failed", "provider", provider, "err", err)
		writeError(w, http.StatusBadGateway, "api_error", "sso identity fetch failed")
		return
	}
	// NOTE: the admin allowlist is provider-agnostic — a domain trusted here is
	// satisfied by ANY configured provider that returns a verified email at it. Only
	// configure providers you trust to verify those domains.
	sess := s.console.NewSSOSession(email, name, provider)
	if !sess.Admin {
		writeError(w, http.StatusForbidden, "permission_error", "this identity is not authorized for the console")
		return
	}
	s.console.Issue(w, sess)
	http.Redirect(w, r, ret, http.StatusFound)
}

func (s *Server) oauthRedirectURI(r *http.Request) string {
	base := strings.TrimRight(s.consoleBaseURL, "/")
	return base + "/console/auth/" + r.PathValue("provider") + "/callback"
}

// safeReturnPath allows only a same-origin ABSOLUTE path so the post-login redirect
// can't be turned into an open redirect. It must start with a single "/" and must
// NOT be "//host" or "/\host" (protocol-relative, which browsers treat as
// cross-origin) nor contain a backslash or a CR/LF/NUL (header-injection / path
// confusion). Anything else falls back to /console.
func safeReturnPath(p string) string {
	if p == "" || p[0] != '/' {
		return "/console"
	}
	if strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return "/console"
	}
	if strings.ContainsAny(p, "\\\r\n\x00") {
		return "/console"
	}
	return p
}

// --- concrete providers ----------------------------------------------------

// googleIdP implements the OpenID Connect authorization-code flow.
type googleIdP struct {
	clientID, clientSecret string
	http                   *http.Client
}

func (g *googleIdP) AuthCodeURL(state, redirectURI string) string {
	return "https://accounts.google.com/o/oauth2/v2/auth" + encodeQuery(map[string]string{
		"client_id": g.clientID, "redirect_uri": redirectURI, "response_type": "code",
		"scope": "openid email profile", "state": state, "access_type": "online",
	})
}

func (g *googleIdP) FetchIdentity(ctx context.Context, code, redirectURI string) (string, string, error) {
	tok, err := oauthToken(ctx, g.http, "https://oauth2.googleapis.com/token", map[string]string{
		"code": code, "client_id": g.clientID, "client_secret": g.clientSecret,
		"redirect_uri": redirectURI, "grant_type": "authorization_code",
	})
	if err != nil {
		return "", "", err
	}
	var ui struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := oauthGetJSON(ctx, g.http, "https://openidconnect.googleapis.com/v1/userinfo", tok, &ui); err != nil {
		return "", "", err
	}
	if !ui.EmailVerified {
		return "", "", errUnverifiedEmail
	}
	return ui.Email, ui.Name, nil
}

// githubIdP implements GitHub's OAuth flow (+ the verified-primary-email lookup).
type githubIdP struct {
	clientID, clientSecret string
	http                   *http.Client
}

func (h *githubIdP) AuthCodeURL(state, redirectURI string) string {
	return "https://github.com/login/oauth/authorize" + encodeQuery(map[string]string{
		"client_id": h.clientID, "redirect_uri": redirectURI,
		"scope": "read:user user:email", "state": state,
	})
}

func (h *githubIdP) FetchIdentity(ctx context.Context, code, redirectURI string) (string, string, error) {
	tok, err := oauthToken(ctx, h.http, "https://github.com/login/oauth/access_token", map[string]string{
		"code": code, "client_id": h.clientID, "client_secret": h.clientSecret,
		"redirect_uri": redirectURI,
	})
	if err != nil {
		return "", "", err
	}
	var user struct {
		Name  string `json:"name"`
		Login string `json:"login"`
	}
	if err := oauthGetJSON(ctx, h.http, "https://api.github.com/user", tok, &user); err != nil {
		return "", "", err
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := oauthGetJSON(ctx, h.http, "https://api.github.com/user/emails", tok, &emails); err != nil {
		return "", "", err
	}
	name := user.Name
	if name == "" {
		name = user.Login
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email, name, nil
		}
	}
	return "", "", errUnverifiedEmail
}
