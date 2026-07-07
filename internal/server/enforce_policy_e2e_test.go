package server

// End-to-end validation of the LIVE control-plane wiring (task 6.5): a request
// from a policy-governed key is enforced against its RESOLVED chain (org▸team▸app▸
// run) via firewall.EnterChain — not the flat per-key path — and a node kill or a
// run kill actually denies it BEFORE the vendor. These prove the pieces built in
// isolation (policy resolver + EnterChain + KillRun) hold together on the real
// HTTP path, including the run-key single-sourcing the security review flagged.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/somasays/heave/internal/controls"
	"github.com/somasays/heave/internal/enforcer"
	"github.com/somasays/heave/internal/firewall"
	"github.com/somasays/heave/internal/ledger"
	"github.com/somasays/heave/internal/policy"
	"github.com/somasays/heave/internal/provider"
	"github.com/somasays/heave/internal/router"
)

const govKey = "agent-key"      // provisioned: mapped to app:bot
const ungovKey = "stranger-key" // authenticated but NOT in the policy store

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// newGovEnv builds a gateway with the control plane ON: org acme▸team eng▸app bot,
// with govKey mapped to the app carrying appLimits. A second authenticated key
// (ungovKey) maps to no node, to exercise the ungoverned fall-through.
func newGovEnv(t *testing.T, appLimits policy.Limits) (e2eEnv, *policy.Store) {
	t.Helper()
	clients := []controls.Client{
		{Name: "agent", KeySHA256: sha(govKey)},
		{Name: "stranger", KeySHA256: sha(ungovKey)},
	}
	store := policy.New()
	must(t, store.CreateOrg("acme", "Acme", policy.Limits{}))
	must(t, store.CreateTeam("eng", "Engineering", "acme", policy.Limits{}))
	must(t, store.CreateApp("bot", "Bot", "eng", appLimits))
	must(t, store.IssueKey(sha(govKey), policy.App, "bot"))

	led := ledger.New(discardLog())
	fw := firewall.New(true, firewall.Limits{}, nil) // enabled; ALL caps come from the chain
	rtr := router.New([]router.ModelConfig{{
		Alias: "agent-model", Provider: "fake", Upstream: "up",
		Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}, MaxOutputTokens: 4096, AcceptsSampling: true,
	}}, "agent-model")
	fp := &fakeProvider{resp: &provider.Response{Content: "ok", InputTokens: 1000, OutputTokens: 1000, FinishReason: "stop"}}
	srv := newTestServer(t, Deps{
		Router: rtr, Providers: map[string]provider.Provider{"fake": fp},
		Guard: controls.New(true, clients, nil), Ledger: led, Firewall: fw,
		Policy: store, Resolver: enforcer.NewResolver(store),
	}, Options{MaxRequestBytes: 1 << 20, RequestTimeout: 2 * time.Second})
	return e2eEnv{h: srv.Handler(), fp: fp, led: led, fw: fw, key: govKey}, store
}

// sendAs is send() with a chosen bearer key.
func (e e2eEnv) sendAs(key, runID, prompt string) int {
	body := `{"model":"agent-model","max_tokens":1000,"messages":[{"role":"user","content":"` + prompt + `"}]}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	if runID != "" {
		req.Header.Set("X-Heave-Run-Id", runID)
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr.Code
}

func TestE2E_GovernedNodeKillDeniesPreVendor(t *testing.T) {
	env, store := newGovEnv(t, policy.Limits{})
	if code := env.send("run-1", "hi"); code != http.StatusOK {
		t.Fatalf("baseline governed request must pass, got %d", code)
	}
	callsBefore := env.fp.calls
	// Kill the TEAM: a node circuit-breaker the firewall itself can't see — the
	// server must deny on chain.KilledBy BEFORE EnterChain / the vendor.
	must(t, store.Kill(policy.Team, "eng"))
	if code := env.send("run-2", "hi"); code != http.StatusForbidden {
		t.Fatalf("a killed ancestor team must deny with 403, got %d", code)
	}
	if env.fp.calls != callsBefore {
		t.Fatalf("a node-killed request must not reach the vendor (calls %d→%d)", callsBefore, env.fp.calls)
	}
}

func TestE2E_GovernedRunKillIsAddressable(t *testing.T) {
	// The regression the security review caught: EnterChain reserves under the
	// POLICY run scope key, so the kill endpoint must target that same key (KillRun),
	// not the flat owner-scoped key. If they diverged, the run would be unkillable.
	env, _ := newGovEnv(t, policy.Limits{})
	if code := env.send("run-x", "hi"); code != http.StatusOK {
		t.Fatalf("first call must pass, got %d", code)
	}
	if code := env.kill("run-x"); code != http.StatusOK {
		t.Fatalf("kill endpoint must succeed, got %d", code)
	}
	if code := env.send("run-x", "hi"); code != http.StatusForbidden {
		t.Fatalf("a killed governed run must be denied on re-entry, got %d (kill/admit keys diverged?)", code)
	}
	// A different run under the same key is unaffected.
	if code := env.send("run-y", "hi"); code != http.StatusOK {
		t.Fatalf("a different run must still pass, got %d", code)
	}
}

func TestE2E_GovernedPerRunCapFromChain(t *testing.T) {
	// The app carries a tiny per-run $ cap; one call's estimate exceeds it, so the
	// run auto-kills on the FIRST request — proving the chain's per-scope cap is what
	// EnterChain enforces (not the flat global Limits, which are empty here).
	env, _ := newGovEnv(t, policy.Limits{MaxUSDPerRun: 0.0001})
	if code := env.send("run-z", "hi"); code != http.StatusForbidden {
		t.Fatalf("a governed per-run cap must deny pre-vendor, got %d", code)
	}
	if env.fp.calls != 0 {
		t.Fatalf("a capped run must not reach the vendor, got %d calls", env.fp.calls)
	}
}

func TestE2E_GovernedWithUppercaseConfigHashStillEnforced(t *testing.T) {
	// Regression (security M1): a client hash configured in UPPERCASE hex must still
	// resolve to its policy node. If auth normalized case but resolution didn't, the
	// key would silently fall through to flat enforcement — no caps, no node kill.
	clients := []controls.Client{{Name: "agent", KeySHA256: strings.ToUpper(sha(govKey))}}
	store := policy.New()
	must(t, store.CreateOrg("acme", "Acme", policy.Limits{}))
	must(t, store.CreateTeam("eng", "E", "acme", policy.Limits{}))
	must(t, store.CreateApp("bot", "Bot", "eng", policy.Limits{}))
	must(t, store.IssueKey(sha(govKey), policy.App, "bot")) // stored lowercase (canonical)
	led := ledger.New(discardLog())
	fw := firewall.New(true, firewall.Limits{}, nil)
	rtr := router.New([]router.ModelConfig{{Alias: "agent-model", Provider: "fake", Upstream: "up",
		Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}, MaxOutputTokens: 4096, AcceptsSampling: true}}, "agent-model")
	fp := &fakeProvider{resp: &provider.Response{Content: "ok", InputTokens: 10, OutputTokens: 10, FinishReason: "stop"}}
	srv := newTestServer(t, Deps{
		Router: rtr, Providers: map[string]provider.Provider{"fake": fp},
		Guard: controls.New(true, clients, nil), Ledger: led, Firewall: fw,
		Policy: store, Resolver: enforcer.NewResolver(store),
	}, Options{MaxRequestBytes: 1 << 20, RequestTimeout: 2 * time.Second})
	env := e2eEnv{h: srv.Handler(), fp: fp, led: led, fw: fw, key: govKey}

	must(t, store.Kill(policy.Team, "eng")) // node kill only bites if the key is GOVERNED
	if code := env.send("run-1", "hi"); code != http.StatusForbidden {
		t.Fatalf("uppercase-config governed key must still be enforced (node kill → 403), got %d — silent fail-open to flat?", code)
	}
}

func TestE2E_UngovernedKeyFallsBackToFlat(t *testing.T) {
	// A key that maps to no policy node is NOT governed: it uses flat enforcement
	// (here: no global caps → admitted), never denied for lack of a chain.
	env, _ := newGovEnv(t, policy.Limits{MaxUSDPerRun: 0.0001})
	if code := env.sendAs(ungovKey, "run-free", "hi"); code != http.StatusOK {
		t.Fatalf("an ungoverned key must fall back to flat enforcement (200), got %d", code)
	}
}
