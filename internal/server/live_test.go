//go:build live

// Live smoke tests. These make REAL, billed calls to provider APIs and are
// therefore NOT part of the hermetic `make check` gate (they need secrets, cost
// money, and are non-deterministic). They are gated behind the `live` build tag
// and skip when the relevant API key is absent. Run with: make smoke
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/somasays/heave/internal/controls"
	"github.com/somasays/heave/internal/firewall"
	"github.com/somasays/heave/internal/ledger"
	"github.com/somasays/heave/internal/openai"
	"github.com/somasays/heave/internal/provider"
	"github.com/somasays/heave/internal/router"
)

// TestLiveAnthropicChatCompletion drives a real request end-to-end through the
// gateway handler to Anthropic, using the cheapest model and a tiny max_tokens
// to keep the cost negligible. Skips when ANTHROPIC_API_KEY is unset.
func TestLiveAnthropicChatCompletion(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live smoke test")
	}

	prov := provider.NewAnthropic("anthropic", key, "")
	rtr := router.New([]router.ModelConfig{{
		Alias: "smoke", Provider: "anthropic", Upstream: "claude-haiku-4-5",
		Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}, MaxOutputTokens: 16, AcceptsSampling: true,
	}}, "smoke")
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"anthropic": prov}},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: 60 * time.Second})

	body := `{"model":"smoke","max_tokens":16,"messages":[{"role":"user","content":"Reply with the single word: pong"}]}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("live request failed: status %d body %s", rr.Code, rr.Body.String())
	}
	var resp openai.ChatCompletionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content.Text == "" {
		t.Fatalf("expected non-empty completion, got %+v", resp)
	}
	if resp.Usage.CompletionTokens <= 0 || resp.Usage.PromptTokens <= 0 {
		t.Fatalf("expected real token usage, got %+v", resp.Usage)
	}
	t.Logf("live ok: model=%s tokens=%d/%d content=%q",
		resp.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Choices[0].Message.Content.Text)
}

// TestLiveAnthropicStreaming drives a real streaming request end-to-end and
// checks the SSE framing + a real usage trailer. Skips without a key.
func TestLiveAnthropicStreaming(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live streaming smoke test")
	}
	prov := provider.NewAnthropic("anthropic", key, "")
	rtr := router.New([]router.ModelConfig{{
		Alias: "smoke", Provider: "anthropic", Upstream: "claude-haiku-4-5",
		Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}, MaxOutputTokens: 16, AcceptsSampling: true,
	}}, "smoke")
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"anthropic": prov}},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: 60 * time.Second})

	body := `{"model":"smoke","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"Reply with the single word: pong"}]}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("live stream failed: status %d body %s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	if !strings.Contains(out, "data: [DONE]") || !strings.Contains(out, `"role":"assistant"`) || !strings.Contains(out, `"usage"`) {
		t.Fatalf("stream missing framing/usage:\n%s", out)
	}
	t.Logf("live stream ok (%d bytes of SSE)", len(out))
}

// TestLiveFirewallBoundsRunawaySpend is the GOAL test against real money: a
// runaway agent resends the same prompt on one run; loop detection must kill it
// after the threshold so only a bounded number of calls reach Anthropic and the
// rest are refused PRE-vendor (never billed). Cheapest model, tiny max_tokens.
func TestLiveFirewallBoundsRunawaySpend(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live firewall test")
	}

	const clientKey = "live-agent-key"
	led := ledger.New(discardLog())
	fw := firewall.New(true, firewall.Limits{LoopThreshold: 3}, nil)
	prov := provider.NewAnthropic("anthropic", key, "")
	rtr := router.New([]router.ModelConfig{{
		Alias: "smoke", Provider: "anthropic", Upstream: "claude-haiku-4-5",
		Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}, MaxOutputTokens: 16, AcceptsSampling: true,
	}}, "smoke")
	srv := newTestServer(t, Deps{
		Router: rtr, Providers: map[string]provider.Provider{"anthropic": prov},
		Guard:  controls.New(true, []controls.Client{{Name: "agent", KeySHA256: sha(clientKey)}}, nil),
		Ledger: led, Firewall: fw,
	}, Options{MaxRequestBytes: 1 << 20, RequestTimeout: 60 * time.Second})
	h := srv.Handler()

	const attempts = 6
	body := `{"model":"smoke","max_tokens":16,"messages":[{"role":"user","content":"Reply with the single word: pong"}]}`
	codes := make([]int, attempts)
	for i := 0; i < attempts; i++ {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+clientKey)
		req.Header.Set("X-Heave-Run-Id", "runaway-live")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		codes[i] = rr.Code
	}

	killed := 0
	for _, c := range codes {
		if c == http.StatusForbidden {
			killed++
		}
	}
	reqs, toks, cost := led.Totals()
	if killed == 0 {
		t.Fatalf("runaway was never killed; codes=%v", codes)
	}
	// Only the pre-kill calls should have billed real usage.
	if reqs > int64(attempts-killed) {
		t.Fatalf("more requests billed (%d) than reached the vendor pre-kill (%d); codes=%v", reqs, attempts-killed, codes)
	}
	if fw.Stats().LocalKills == 0 {
		t.Fatal("expected a kill to be recorded")
	}
	t.Logf("live firewall ok: codes=%v — %d real Anthropic calls (%d tokens, $%.5f), %d refused pre-vendor by the kill switch",
		codes, reqs, toks, cost, killed)
}
