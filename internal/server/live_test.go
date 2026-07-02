//go:build live

// Live smoke tests. These make REAL, billed calls to provider APIs and are
// therefore NOT part of the hermetic `make check` gate (they need secrets, cost
// money, and are non-deterministic). They are gated behind the `live` build tag
// and skip when the relevant API key is absent. Run with: make smoke
package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/somasays/heave/internal/controls"
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
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(rtr, map[string]provider.Provider{"anthropic": prov}, ledger.New(discard),
		controls.New(false, nil, nil), discard, Options{MaxRequestBytes: 1 << 20, RequestTimeout: 60 * time.Second})

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
