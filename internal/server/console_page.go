package server

import (
	_ "embed"
	"net/http"
	"sort"
)

// consoleHTML is the self-contained admin console SPA (no external assets). It
// authenticates via the session cookie and drives the management API (/v1/policy).
//
//go:embed console.html
var consoleHTML []byte

//go:embed console.js
var consoleJS []byte

// handleConsolePage serves the console shell. It is intentionally open (like a
// login page); all data it fetches is behind the session-gated management API. The
// script is a SEPARATE same-origin file (served below) so the CSP can stay strict
// (no 'unsafe-inline' scripts) while default-src 'self' permits it.
func (s *Server) handleConsolePage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
	_, _ = w.Write(consoleHTML)
}

// handleConsoleAppJS serves the console SPA script (same-origin; allowed by the
// default-src 'self' CSP on the page).
func (s *Server) handleConsoleAppJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(consoleJS)
}

// handleConsoleInfo reports the current session state + which SSO providers are
// configured, so the page renders the right login options / logged-in view. It
// reveals only provider names and whether THIS request's session is a valid admin.
func (s *Server) handleConsoleInfo(w http.ResponseWriter, r *http.Request) {
	providers := make([]string, 0, len(s.oauth))
	for name := range s.oauth {
		providers = append(providers, name)
	}
	sort.Strings(providers)
	sess, ok := s.console.SessionFrom(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"providers":     providers,
		"authenticated": ok && sess.Admin,
		"name":          sess.Name,
	})
}
