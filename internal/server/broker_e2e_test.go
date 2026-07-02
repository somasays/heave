package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/somasays/heave/internal/broker"
	"github.com/somasays/heave/internal/provider"
	"github.com/somasays/heave/internal/redisstore"
	"github.com/somasays/heave/internal/router"
)

// brokerEnv builds a gateway with provider-quota brokering over a shared store.
// Model "m" routes to provider "pa" with a fallback model "mb" on provider "pb".
type brokerEnv struct {
	h      http.Handler
	pa, pb *fakeProvider
}

func newBrokerEnv(t *testing.T, limits map[string]broker.Limit, withFallback bool, store *redisstore.Store) brokerEnv {
	t.Helper()
	pa := &fakeProvider{resp: &provider.Response{Content: "a", InputTokens: 10, OutputTokens: 10, FinishReason: "stop"}}
	pb := &fakeProvider{resp: &provider.Response{Content: "b", InputTokens: 10, OutputTokens: 10, FinishReason: "stop"}}
	models := []router.ModelConfig{{Alias: "m", Provider: "pa", Upstream: "u", Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}}}
	if withFallback {
		models[0].Fallbacks = []string{"mb"}
		models = append(models, router.ModelConfig{Alias: "mb", Provider: "pb", Upstream: "u", Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}})
	}
	rtr := router.New(models, "m")
	srv := newTestServer(t, Deps{
		Router:    rtr,
		Providers: map[string]provider.Provider{"pa": pa, "pb": pb},
		Broker:    broker.New(store, limits),
	}, Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	return brokerEnv{h: srv.Handler(), pa: pa, pb: pb}
}

func sharedStore(t *testing.T) *redisstore.Store {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	s := redisstore.NewClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), time.Hour)
	s.SetClock(func() int64 { return 1000 })
	return s
}

func chat(h http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestProviderQuotaAwareFailover: when the primary provider is at its brokered
// RPM, the request fails over to another provider PRE-vendor (no 429 provoked).
func TestProviderQuotaAwareFailover(t *testing.T) {
	env := newBrokerEnv(t, map[string]broker.Limit{"pa": {RPM: 1}}, true, sharedStore(t))
	if rr := chat(env.h); rr.Code != 200 { // consumes pa's 1 RPM
		t.Fatalf("first request want 200, got %d", rr.Code)
	}
	if env.pa.calls != 1 || env.pb.calls != 0 {
		t.Fatalf("first request must hit pa: pa=%d pb=%d", env.pa.calls, env.pb.calls)
	}
	if rr := chat(env.h); rr.Code != 200 { // pa exhausted → fail over to pb
		t.Fatalf("second request want 200 (failover), got %d", rr.Code)
	}
	if env.pa.calls != 1 || env.pb.calls != 1 {
		t.Fatalf("second request must skip pa and hit pb: pa=%d pb=%d", env.pa.calls, env.pb.calls)
	}
}

// TestProviderQuotaExhaustedReturns429: with no fallback and the sole provider at
// quota, the client gets a truthful 429 + Retry-After (not a vendor error, and no
// vendor call).
func TestProviderQuotaExhaustedReturns429(t *testing.T) {
	env := newBrokerEnv(t, map[string]broker.Limit{"pa": {RPM: 1}}, false, sharedStore(t))
	if rr := chat(env.h); rr.Code != 200 {
		t.Fatalf("first request want 200, got %d", rr.Code)
	}
	rr := chat(env.h)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("quota-exhausted (no fallback) want 429, got %d", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry a Retry-After hint")
	}
	if env.pa.calls != 1 {
		t.Fatalf("the quota-blocked request must NOT reach the vendor: pa=%d", env.pa.calls)
	}
}

// brokerHandler builds a gateway with an explicit model list + providers, for
// TPM / streaming / error-path tests.
func brokerHandler(t *testing.T, store *redisstore.Store, limits map[string]broker.Limit, models []router.ModelConfig, provs map[string]provider.Provider) http.Handler {
	t.Helper()
	srv := newTestServer(t, Deps{
		Router: router.New(models, models[0].Alias), Providers: provs, Broker: broker.New(store, limits),
	}, Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	return srv.Handler()
}

func chatBody(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestProviderTPMFailClosedOnZeroUsage is the must-fix regression: a usage-omitting
// backend reports 0 tokens though it consumed the vendor's TPM, so the broker must
// FAIL CLOSED to the estimate (keep the reservation), not zero it.
func TestProviderTPMFailClosedOnZeroUsage(t *testing.T) {
	pa := &fakeProvider{resp: &provider.Response{Content: "x", InputTokens: 0, OutputTokens: 0, FinishReason: "stop"}}
	h := brokerHandler(t, sharedStore(t), map[string]broker.Limit{"pa": {TPM: 1500}},
		[]router.ModelConfig{{Alias: "m", Provider: "pa", Upstream: "u", Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}}},
		map[string]provider.Provider{"pa": pa})
	body := `{"model":"m","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`
	if rr := chatBody(h, body); rr.Code != 200 { // reserves ~1000 tokens
		t.Fatalf("first request want 200, got %d", rr.Code)
	}
	// If the zero-usage settle had drained the window to 0, this would admit. With
	// fail-closed it stays at ~1000, so a second ~1000 request exceeds TPM 1500.
	if rr := chatBody(h, body); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("a zero-usage success must keep its TPM reservation (fail closed); 2nd want 429, got %d", rr.Code)
	}
}

// TestProviderTPMExhaustionFailsOver: a request estimated over the primary's TPM
// fails over to a provider with headroom.
func TestProviderTPMExhaustionFailsOver(t *testing.T) {
	pa := &fakeProvider{resp: &provider.Response{Content: "a", InputTokens: 10, OutputTokens: 10, FinishReason: "stop"}}
	pb := &fakeProvider{resp: &provider.Response{Content: "b", InputTokens: 10, OutputTokens: 10, FinishReason: "stop"}}
	h := brokerHandler(t, sharedStore(t), map[string]broker.Limit{"pa": {TPM: 500}},
		[]router.ModelConfig{
			{Alias: "m", Provider: "pa", Upstream: "u", Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}, Fallbacks: []string{"mb"}},
			{Alias: "mb", Provider: "pb", Upstream: "u", Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}},
		},
		map[string]provider.Provider{"pa": pa, "pb": pb})
	// est ~1000 tokens (max_tokens) > pa's TPM 500 → pa skipped, pb serves.
	if rr := chatBody(h, `{"model":"m","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`); rr.Code != 200 {
		t.Fatalf("want 200 via failover, got %d", rr.Code)
	}
	if pa.calls != 0 || pb.calls != 1 {
		t.Fatalf("TPM-exhausted pa must be skipped, pb serve: pa=%d pb=%d", pa.calls, pb.calls)
	}
}

// TestProviderUnaryFailureReleasesQuota: a provider that ERRORS after a successful
// reserve must RELEASE its quota (the vendor never billed), so a retry can use it
// again — proving no reservation leak on the failure path.
func TestProviderUnaryFailureReleasesQuota(t *testing.T) {
	pa := &fakeProvider{err: &provider.Error{StatusCode: 500, Message: "boom"}} // retryable
	pb := &fakeProvider{resp: &provider.Response{Content: "b", InputTokens: 1, OutputTokens: 1, FinishReason: "stop"}}
	h := brokerHandler(t, sharedStore(t), map[string]broker.Limit{"pa": {RPM: 1}},
		[]router.ModelConfig{
			{Alias: "m", Provider: "pa", Upstream: "u", Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}, Fallbacks: []string{"mb"}},
			{Alias: "mb", Provider: "pb", Upstream: "u", Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}},
		},
		map[string]provider.Provider{"pa": pa, "pb": pb})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		if rr := chatBody(h, body); rr.Code != 200 {
			t.Fatalf("request %d want 200 (failover to pb), got %d", i, rr.Code)
		}
	}
	// pa has RPM 1 but was tried BOTH times — only possible if each failed attempt
	// released its reservation (a leak would skip pa on the 2nd request).
	if pa.calls != 2 {
		t.Fatalf("failed pa attempts must release quota so pa is retried; pa.calls=%d (want 2)", pa.calls)
	}
}

// TestProviderQuotaStreamingMidFailKeepsCount: a stream that fails AFTER writing
// bytes engaged the vendor, so its provider-quota reservation must be KEPT (fail
// closed), not released.
func TestProviderQuotaStreamingMidFailKeepsCount(t *testing.T) {
	pa := &fakeProvider{deltas: []string{"partial"}, midStreamErr: &provider.Error{StatusCode: 500, Message: "boom"}}
	h := brokerHandler(t, sharedStore(t), map[string]broker.Limit{"pa": {RPM: 1}},
		[]router.ModelConfig{{Alias: "m", Provider: "pa", Upstream: "u", Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}}},
		map[string]provider.Provider{"pa": pa})
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	chatBody(h, body) // mid-stream fail: bytes written, vendor engaged → quota kept
	// pa's RPM=1 is now consumed by the kept reservation, so a second request is
	// quota-blocked (429). If the mid-fail had released, this would reach pa again.
	if rr := chatBody(h, body); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("a mid-stream failure must KEEP its provider-quota count; 2nd want 429, got %d", rr.Code)
	}
}

// TestProviderQuotaHoldsAcrossReplicas: two gateway replicas sharing one Redis
// honor a single provider RPM (the multi-team quota-fight closed pre-vendor).
func TestProviderQuotaHoldsAcrossReplicas(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	mkStore := func() *redisstore.Store {
		s := redisstore.NewClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), time.Hour)
		s.SetClock(func() int64 { return 1000 })
		return s
	}
	limits := map[string]broker.Limit{"pa": {RPM: 2}}
	a := newBrokerEnv(t, limits, false, mkStore())
	b := newBrokerEnv(t, limits, false, mkStore())

	served := 0
	for i := 0; i < 4; i++ {
		env := a
		if i%2 == 1 {
			env = b
		}
		if chat(env.h).Code == 200 {
			served++
		}
	}
	if served != 2 {
		t.Fatalf("shared provider RPM=2 must cap TOTAL served across replicas at 2, got %d", served)
	}
}
