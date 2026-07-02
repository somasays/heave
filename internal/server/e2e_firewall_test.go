package server

// End-to-end validation of the project's GOAL (Invariant #9): a runtime spend &
// quota firewall that stops agentic overspend with HARD, real-time, PRE-vendor
// enforcement. These tests drive the real HTTP handler and assert the wedge as a
// COUNTERFACTUAL: the same agent traffic with the firewall OFF vs ON, comparing
// how many calls actually reached the vendor and how much was billed.
//
// The fake provider counts every call that reaches it, so "blocked calls cost $0"
// is proven directly: a denied request returns before dispatch, so fp.calls never
// moves. Cost is the ledger's real accounting. This is hermetic (no secrets, no
// spend); a `live`-tagged twin (live_test.go) reruns the core scenario against
// real Anthropic so "pre-vendor" is also proven with real money.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somasays/heave/internal/controls"
	"github.com/somasays/heave/internal/firewall"
	"github.com/somasays/heave/internal/ledger"
	"github.com/somasays/heave/internal/provider"
	"github.com/somasays/heave/internal/router"
)

// e2eEnv is one gateway instance with auth ON (the firewall is only meaningful
// with auth on) and unlimited budget/rate, so the firewall is the ONLY gate.
type e2eEnv struct {
	h   http.Handler
	fp  *fakeProvider
	led *ledger.Ledger
	fw  *firewall.Firewall
	key string
}

// Each served request bills Cost(1000,1000,·, in=1,out=5/mtok) = 0.001 + 0.005 =
// $0.006; the counterfactual reads real spend from the ledger.

func newE2E(t *testing.T, fwEnabled bool, limits firewall.Limits) e2eEnv {
	t.Helper()
	const key = "agent-key"
	clients := []controls.Client{{
		Name: "agent", KeySHA256: sha(key), MonthlyBudgetUSD: 0, RateLimitRPM: 0, // 0 = unlimited
	}}
	led := ledger.New(discardLog())
	fw := firewall.New(fwEnabled, limits, nil)
	rtr := router.New([]router.ModelConfig{{
		Alias: "agent-model", Provider: "fake", Upstream: "up",
		Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}, MaxOutputTokens: 4096, AcceptsSampling: true,
	}}, "agent-model")
	fp := &fakeProvider{resp: &provider.Response{Content: "ok", InputTokens: 1000, OutputTokens: 1000, FinishReason: "stop"}}
	srv := newTestServer(t, Deps{
		Router: rtr, Providers: map[string]provider.Provider{"fake": fp},
		Guard: controls.New(true, clients, nil), Ledger: led, Firewall: fw,
	}, Options{MaxRequestBytes: 1 << 20, RequestTimeout: 2 * time.Second})
	return e2eEnv{h: srv.Handler(), fp: fp, led: led, fw: fw, key: key}
}

func (e e2eEnv) send(runID, prompt string) int {
	body := fmt.Sprintf(`{"model":"agent-model","max_tokens":1000,"messages":[{"role":"user","content":%q}]}`, prompt)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.key)
	if runID != "" {
		req.Header.Set("X-Heave-Run-Id", runID)
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr.Code
}

func (e e2eEnv) kill(runID string) int {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/runs/"+runID+"/kill", nil)
	req.Header.Set("Authorization", "Bearer "+e.key)
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr.Code
}

func (e e2eEnv) spend() float64 { _, _, c := e.led.Totals(); return c }

// TestE2E_RunawayLoopIsBoundedPreVendor: a runaway agent resending the same
// prompt on one run is auto-killed by loop detection after the threshold; every
// call past that is refused BEFORE reaching the vendor, so spend is bounded.
func TestE2E_RunawayLoopIsBoundedPreVendor(t *testing.T) {
	const attempts = 8

	// Baseline: firewall OFF — the runaway bills every call.
	off := newE2E(t, false, firewall.Limits{})
	for i := 0; i < attempts; i++ {
		if code := off.send("agent-1", "loop forever"); code != http.StatusOK {
			t.Fatalf("firewall OFF: call %d want 200, got %d", i, code)
		}
	}

	// Firewall ON with loop detection: killed after 3 identical prefixes.
	on := newE2E(t, true, firewall.Limits{LoopThreshold: 3})
	codes := make([]int, attempts)
	for i := 0; i < attempts; i++ {
		codes[i] = on.send("agent-1", "loop forever")
	}

	blocked := 0
	for _, c := range codes {
		if c == http.StatusForbidden {
			blocked++
		}
	}
	if on.fp.calls >= off.fp.calls {
		t.Fatalf("ON must reach the vendor fewer times: on=%d off=%d", on.fp.calls, off.fp.calls)
	}
	if blocked == 0 {
		t.Fatal("expected the runaway to be killed and subsequent calls refused")
	}
	// The blocked calls are PRE-vendor: fp.calls counts only served requests, so
	// (attempts - fp.calls) requests never reached the provider at all.
	if got := attempts - on.fp.calls; got != blocked {
		t.Fatalf("every blocked call must be pre-vendor: blocked=%d, non-dispatched=%d", blocked, got)
	}
	if on.fw.Stats().LocalKills == 0 {
		t.Fatal("a kill should be recorded and observable in firewall stats")
	}
	if on.spend() >= off.spend() {
		t.Fatalf("ON spend must be lower: on=$%.4f off=$%.4f", on.spend(), off.spend())
	}

	logCounterfactual(t, "identical-retry storm (same prompt x8) — caught by loop detection", off, on, codes)
}

// TestE2E_GrowingContextRunawayNeedsBudgetNotLoopDetection is the HONEST headline
// Fable asked for: a real agent runaway grows its context every turn, so every
// request hashes differently and LOOP DETECTION NEVER FIRES. We show that failure
// openly, then show the per-run $ budget catching exactly this shape.
func TestE2E_GrowingContextRunawayNeedsBudgetNotLoopDetection(t *testing.T) {
	const attempts = 8
	// Distinct, growing prompts — like a tool loop appending each turn's output.
	prompts := make([]string, attempts)
	ctx := "start"
	for i := range prompts {
		prompts[i] = ctx
		ctx += fmt.Sprintf(" | turn-%d output blah blah", i)
	}

	// Control A: loop detection only. It is BLIND to a changing-context runaway.
	loopOnly := newE2E(t, true, firewall.Limits{LoopThreshold: 3})
	for i := 0; i < attempts; i++ {
		if code := loopOnly.send("agent-1", prompts[i]); code != http.StatusOK {
			t.Fatalf("loop-only: call %d unexpectedly blocked (%d) — the honest negative is that it should NOT fire here", i, code)
		}
	}
	if loopOnly.fw.Stats().LocalKills != 0 {
		t.Fatal("loop detection must NOT fire on a growing-context runaway (that is the whole point)")
	}

	// Control B: hard per-run $ budget. It catches exactly what loop detection can't.
	budget := newE2E(t, true, firewall.Limits{LoopThreshold: 3, MaxUSDPerRun: 0.02})
	codes := make([]int, attempts)
	for i := 0; i < attempts; i++ {
		codes[i] = budget.send("agent-1", prompts[i])
	}
	killed := countCode(codes, http.StatusForbidden)
	if killed == 0 {
		t.Fatalf("per-run budget must stop the runaway loop detection missed; codes=%v", codes)
	}
	if budget.fw.Stats().LocalKills == 0 {
		t.Fatal("a per-run-budget kill should be recorded")
	}
	// The core guarantee: the run's billed spend is bounded by the cap (the request
	// that would exceed it is refused pre-vendor, so it never bills).
	if budget.spend() > 0.02+1e-9 {
		t.Fatalf("billed spend must be bounded by the $0.02 per-run cap, got $%.4f", budget.spend())
	}
	t.Logf("\n=== HONEST RUNAWAY: growing context (distinct prompt each turn) ===\n"+
		"loop detection   BLIND — %d/%d served, 0 kills (every turn hashes differently)\n"+
		"per-run $ budget $0.02 cap — %d served, %d refused pre-vendor, billed $%.4f (≤ cap)\n"+
		"budget codes     %v",
		loopOnly.fp.calls, attempts, budget.fp.calls, killed, budget.spend(), codes)
}

// TestE2E_PerRunBudgetOvershootIsBoundedNotAbsolute exhibits the HONEST limit the
// reviewers flagged: the cap is on the ESTIMATE, so when a call bills more than
// estimated (here max_tokens=10 but the provider returns 1000 output tokens —
// standing in for a chars/4 input undercount) the run bills slightly OVER the cap.
// We assert the overshoot is real but BOUNDED to ~one call (settle keeps the
// accumulator accurate) — not that spend stays under the cap.
func TestE2E_PerRunBudgetOvershootIsBoundedNotAbsolute(t *testing.T) {
	on := newE2E(t, true, firewall.Limits{MaxUSDPerRun: 0.02})
	codes := []int{}
	for i := 0; i < 8; i++ {
		body := fmt.Sprintf(`{"model":"agent-model","max_tokens":10,"messages":[{"role":"user","content":"under-%d"}]}`, i)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+on.key)
		req.Header.Set("X-Heave-Run-Id", "under")
		rr := httptest.NewRecorder()
		on.h.ServeHTTP(rr, req)
		codes = append(codes, rr.Code)
	}
	if countCode(codes, http.StatusForbidden) == 0 {
		t.Fatalf("run should eventually trip; codes=%v", codes)
	}
	// The cap is NOT absolute: because actual ≫ estimate, billed spend exceeds it.
	if on.spend() <= 0.02 {
		t.Fatalf("this case should exhibit the est<actual overshoot (billed over the cap), got $%.4f", on.spend())
	}
	// But the overshoot is BOUNDED to about one call's actual cost ($0.006): settle
	// keeps the accumulator accurate, so only the in-flight call's error slips past.
	if on.spend() > 0.02+0.006+1e-9 {
		t.Fatalf("overshoot must be bounded to ~one call, got $%.4f (cap $0.02)", on.spend())
	}
	t.Logf("HONEST LIMIT: est≪actual → billed $%.4f, just over the $0.02 cap (bounded to ~1 call); codes=%v", on.spend(), codes)
}

// TestE2E_ConcurrentBurstCannotOvershoot proves the reserve-AT-ADMIT differentiator
// through the full HTTP path: many requests firing at once against a $/min cap
// cannot all slip through (a check-only design would let them TOCTOU past the cap).
func TestE2E_ConcurrentBurstCannotOvershoot(t *testing.T) {
	const burst = 20
	on := newE2E(t, true, firewall.Limits{MaxUSDPerMin: 0.02}) // est ~$0.005/call

	var wg sync.WaitGroup
	var mu sync.Mutex
	served, throttled := 0, 0
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code := on.send("", fmt.Sprintf("concurrent-%d", i))
			mu.Lock()
			switch code {
			case http.StatusOK:
				served++
			case http.StatusTooManyRequests:
				throttled++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if served != on.fp.calls {
		t.Fatalf("served (%d) must equal vendor calls (%d)", served, on.fp.calls)
	}
	if served+throttled != burst {
		t.Fatalf("every request must be served or throttled: %d+%d != %d", served, throttled, burst)
	}
	// Reserve-at-admit holds the estimate under the lock, so concurrent admits see
	// each other and only a handful pass — nowhere near all 20 (which check-only
	// enforcement would allow to overshoot the cap wholesale).
	if served > 6 {
		t.Fatalf("reserve-at-admit must bound concurrent admits well below the burst, served=%d", served)
	}
	t.Logf("concurrent burst: %d fired at once, %d served, %d throttled pre-vendor; billed $%.4f vs $%.4f if all had passed",
		burst, served, throttled, on.spend(), float64(burst)*0.006)
}

// TestE2E_ManualKillStopsSpendImmediately: an operator hits the kill endpoint
// mid-run; every subsequent request on that run is refused pre-vendor.
func TestE2E_ManualKillStopsSpendImmediately(t *testing.T) {
	on := newE2E(t, true, firewall.Limits{})
	if code := on.send("runaway-42", "do work"); code != http.StatusOK {
		t.Fatalf("first call want 200, got %d", code)
	}
	callsBeforeKill := on.fp.calls
	if code := on.kill("runaway-42"); code != http.StatusOK {
		t.Fatalf("kill endpoint want 200, got %d", code)
	}
	for i := 0; i < 5; i++ {
		if code := on.send("runaway-42", "do work"); code != http.StatusForbidden {
			t.Fatalf("post-kill call %d must be 403, got %d", i, code)
		}
	}
	if on.fp.calls != callsBeforeKill {
		t.Fatalf("no vendor call may occur after a kill: before=%d after=%d", callsBeforeKill, on.fp.calls)
	}
	// A DIFFERENT run is unaffected (kill is run-scoped, not a global stop).
	if code := on.send("other-run", "do work"); code != http.StatusOK {
		t.Fatalf("an unrelated run must still be served, got %d", code)
	}
	t.Logf("manual kill: 1 call, then kill -> 5 refused pre-vendor (vendor calls frozen at %d); unrelated run still served", callsBeforeKill)
}

// TestE2E_VelocityCapBlocksBeforeVendor: a burst that exceeds the $/min cap is
// throttled at admit — the throttled requests never reach the vendor.
func TestE2E_VelocityCapBlocksBeforeVendor(t *testing.T) {
	const attempts = 6
	// est per call ≈ max_tokens(1000) * $5/Mtok = $0.005. Cap at $0.013 admits 2,
	// then the rolling window blocks the rest.
	on := newE2E(t, true, firewall.Limits{MaxUSDPerMin: 0.013})
	throttled := 0
	for i := 0; i < attempts; i++ {
		switch code := on.send("", fmt.Sprintf("burst-%d", i)); code {
		case http.StatusOK:
		case http.StatusTooManyRequests:
			throttled++
		default:
			t.Fatalf("call %d unexpected status %d", i, code)
		}
	}
	if throttled == 0 {
		t.Fatal("expected the velocity cap to throttle part of the burst")
	}
	// Throttled calls are pre-vendor: served == fp.calls, throttled == attempts-served.
	if got := attempts - on.fp.calls; got != throttled {
		t.Fatalf("throttled calls must be pre-vendor: throttled=%d non-dispatched=%d", throttled, got)
	}
	t.Logf("velocity cap $0.013/min: %d/%d served, %d throttled at $0 (pre-vendor), billed $%.4f",
		on.fp.calls, attempts, throttled, on.spend())
}

// logCounterfactual prints the money artifact for review. It reports what the
// firewall actually guarantees — a BOUNDED loss, not a spend-reduction percentage
// (that % is just a function of how long the runaway is allowed to run) — and a
// cumulative-$ curve so the flat line after the trip is visible.
func logCounterfactual(t *testing.T, scenario string, off, on e2eEnv, onCodes []int) {
	t.Helper()
	perCall := 0.0
	if off.fp.calls > 0 {
		perCall = off.spend() / float64(off.fp.calls)
	}
	// Cumulative billed $ per request: OFF bills every call; ON bills only 200s.
	var offCurve, onCurve strings.Builder
	offCum, onCum := 0.0, 0.0
	for i, code := range onCodes {
		offCum += perCall // OFF would have served every request
		if code == http.StatusOK {
			onCum += perCall
		}
		fmt.Fprintf(&offCurve, " %5.3f", offCum)
		fmt.Fprintf(&onCurve, " %5.3f", onCum)
		_ = i
	}
	t.Logf("\n=== WEDGE COUNTERFACTUAL: %s ===\n"+
		"                 firewall OFF     firewall ON\n"+
		"vendor calls     %-12d     %d\n"+
		"billed spend     $%-11.4f     $%.4f\n"+
		"outcome          unbounded        killed, %d refused pre-vendor\n"+
		"GUARANTEE        loss BOUNDED: spend stops within 1 request of the trip (ON spend ≈ $%.4f, capped by the control, not the runaway's length)\n"+
		"cum $ OFF       %s\n"+
		"cum $ ON        %s\n"+
		"ON status codes  %v",
		scenario, off.fp.calls, on.fp.calls, off.spend(), on.spend(),
		countCode(onCodes, http.StatusForbidden), on.spend(), offCurve.String(), onCurve.String(), onCodes)
}

func countCode(codes []int, want int) int {
	n := 0
	for _, c := range codes {
		if c == want {
			n++
		}
	}
	return n
}
