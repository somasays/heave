package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/somasays/heave/internal/controls"
	"github.com/somasays/heave/internal/firewall"
	"github.com/somasays/heave/internal/health"
	"github.com/somasays/heave/internal/ledger"
	"github.com/somasays/heave/internal/openai"
	"github.com/somasays/heave/internal/provider"
	"github.com/somasays/heave/internal/redact"
	"github.com/somasays/heave/internal/router"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestServer(t *testing.T, d Deps, opts Options) *Server {
	t.Helper()
	if d.Ledger == nil {
		d.Ledger = ledger.New(discardLog())
	}
	if d.Guard == nil {
		d.Guard = controls.New(false, nil, nil)
	}
	if d.Health == nil {
		d.Health = health.New(3, time.Minute, nil)
	}
	if d.Redactor == nil {
		d.Redactor = redact.New(false, nil)
	}
	if d.Log == nil {
		d.Log = discardLog()
	}
	return New(d, opts)
}

type fakeProvider struct {
	resp         *provider.Response
	err          error
	block        bool
	gotReq       *provider.Request
	calls        int
	deltas       []string // if set, streamed instead of resp.Content
	midStreamErr error    // if set, returned AFTER streaming deltas (mid-stream failure)
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) ChatCompletion(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	f.calls++
	f.gotReq = req
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func (f *fakeProvider) ChatCompletionStream(ctx context.Context, req *provider.Request, onDelta provider.StreamFunc) (*provider.Response, error) {
	f.calls++
	f.gotReq = req
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err // error before any delta → failover-safe
	}
	chunks := f.deltas
	if len(chunks) == 0 && f.resp != nil && f.resp.Content != "" {
		chunks = []string{f.resp.Content}
	}
	for _, c := range chunks {
		if err := onDelta(c); err != nil {
			return nil, err
		}
	}
	if f.midStreamErr != nil {
		return nil, f.midStreamErr // failed after bytes were streamed
	}
	return f.resp, nil
}

func testServer(t *testing.T, fp *fakeProvider, sampling bool, timeout time.Duration) http.Handler {
	t.Helper()
	rtr := router.New([]router.ModelConfig{{
		Alias: "m", Provider: "fake", Upstream: "up-1",
		Price: router.Price{InputPerMTok: 3, OutputPerMTok: 15}, AcceptsSampling: sampling,
	}}, "m")
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"fake": fp}},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: timeout})
	return srv.Handler()
}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	return postAuth(h, body, "")
}

func postAuth(h http.Handler, body, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestSuccess(t *testing.T) {
	fp := &fakeProvider{resp: &provider.Response{Content: "hi", InputTokens: 10, OutputTokens: 5, FinishReason: "stop"}}
	h := testServer(t, fp, true, time.Second)
	rr := post(h, `{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	if rr.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body)
	}
	var resp openai.ChatCompletionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content.Text != "hi" || resp.Usage.TotalTokens != 15 {
		t.Fatalf("bad response: %+v", resp)
	}
}

func TestArrayContentAndImageRejected(t *testing.T) {
	fp := &fakeProvider{resp: &provider.Response{Content: "ok"}}
	h := testServer(t, fp, true, time.Second)

	// Array-of-parts text content must parse.
	rr := post(h, `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if rr.Code != 200 {
		t.Fatalf("array text content: want 200, got %d: %s", rr.Code, rr.Body)
	}
	// Image parts must be rejected explicitly, not silently dropped.
	rr = post(h, `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`)
	if rr.Code != 400 {
		t.Fatalf("image content: want 400, got %d", rr.Code)
	}
}

func TestUnknownModel(t *testing.T) {
	h := testServer(t, &fakeProvider{}, true, time.Second)
	rr := post(h, `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 400 {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestRejectTools(t *testing.T) {
	h := testServer(t, &fakeProvider{resp: &provider.Response{}}, true, time.Second)
	rr := post(h, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"}]}`)
	if rr.Code != 400 {
		t.Fatalf("want 400 for tools, got %d", rr.Code)
	}
}

func TestUpstream4xxPreserved(t *testing.T) {
	fp := &fakeProvider{err: &provider.Error{StatusCode: 400, Type: "invalid_request_error", Message: "bad"}}
	h := testServer(t, fp, true, time.Second)
	rr := post(h, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 400 {
		t.Fatalf("want upstream 400 preserved, got %d", rr.Code)
	}
}

func TestUpstream5xxBecomes502(t *testing.T) {
	fp := &fakeProvider{err: &provider.Error{StatusCode: 500, Type: "api_error", Message: "boom"}}
	h := testServer(t, fp, true, time.Second)
	rr := post(h, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rr.Code)
	}
}

func TestTimeoutBecomes504(t *testing.T) {
	fp := &fakeProvider{block: true}
	h := testServer(t, fp, true, 20*time.Millisecond)
	rr := post(h, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("want 504, got %d", rr.Code)
	}
}

func TestSamplingStrippedForRejectingModel(t *testing.T) {
	fp := &fakeProvider{resp: &provider.Response{Content: "ok"}}
	h := testServer(t, fp, false, time.Second) // AcceptsSampling=false
	rr := post(h, `{"model":"m","temperature":0.7,"top_p":0.5,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 200 {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if fp.gotReq.Temperature != nil || fp.gotReq.TopP != nil {
		t.Fatalf("sampling params should be stripped, got temp=%v topp=%v", fp.gotReq.Temperature, fp.gotReq.TopP)
	}
}

func TestBodyTooLarge(t *testing.T) {
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "up"}}, "m")
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"fake": &fakeProvider{resp: &provider.Response{}}}},
		Options{MaxRequestBytes: 32, RequestTimeout: time.Second})
	rr := post(srv.Handler(), `{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("x", 200)+`"}]}`)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", rr.Code)
	}
}

func authServer(t *testing.T, clients []controls.Client) http.Handler {
	t.Helper()
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "up-1"}}, "m")
	fp := &fakeProvider{resp: &provider.Response{Content: "ok", InputTokens: 1, OutputTokens: 1}}
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"fake": fp}, Guard: controls.New(true, clients, nil)},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	return srv.Handler()
}

func sha(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:])
}

func TestAuthRequiredAndAccepted(t *testing.T) {
	h := authServer(t, []controls.Client{{Name: "team-a", KeySHA256: sha("secret")}})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	if rr := post(h, body); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no key: want 401, got %d", rr.Code)
	}
	if rr := postAuth(h, body, "wrong"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad key: want 401, got %d", rr.Code)
	}
	if rr := postAuth(h, body, "secret"); rr.Code != 200 {
		t.Fatalf("good key: want 200, got %d: %s", rr.Code, rr.Body)
	}
}

func failoverServer(t *testing.T, primary, secondary provider.Provider) http.Handler {
	t.Helper()
	rtr := router.New([]router.ModelConfig{
		{Alias: "p", Provider: "a", Upstream: "ua", Price: router.Price{InputPerMTok: 5, OutputPerMTok: 25}, Fallbacks: []string{"s"}},
		{Alias: "s", Provider: "b", Upstream: "ub", Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}},
	}, "p")
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"a": primary, "b": secondary}},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	return srv.Handler()
}

func TestFailoverOnRetryableError(t *testing.T) {
	primary := &fakeProvider{err: &provider.Error{StatusCode: 500, Message: "boom"}}
	secondary := &fakeProvider{resp: &provider.Response{Content: "ok", InputTokens: 1, OutputTokens: 1}}
	h := failoverServer(t, primary, secondary)
	rr := post(h, `{"model":"p","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 200 {
		t.Fatalf("should have failed over to secondary: got %d %s", rr.Code, rr.Body)
	}
	if primary.calls != 1 || secondary.calls != 1 {
		t.Fatalf("expected primary+secondary each called once, got %d/%d", primary.calls, secondary.calls)
	}
}

func TestNoFailoverOnClientError(t *testing.T) {
	primary := &fakeProvider{err: &provider.Error{StatusCode: 400, Type: "invalid_request_error", Message: "bad"}}
	secondary := &fakeProvider{resp: &provider.Response{Content: "ok"}}
	h := failoverServer(t, primary, secondary)
	rr := post(h, `{"model":"p","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 400 {
		t.Fatalf("4xx is terminal: want 400, got %d", rr.Code)
	}
	if secondary.calls != 0 {
		t.Fatalf("must NOT fail over on a client error, secondary called %d times", secondary.calls)
	}
}

func TestRedactionScrubsBeforeDispatch(t *testing.T) {
	fp := &fakeProvider{resp: &provider.Response{Content: "ok"}}
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "u"}}, "m")
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"fake": fp}, Redactor: redact.New(true, nil)},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	rr := post(srv.Handler(), `{"model":"m","messages":[{"role":"user","content":"reach me at a@b.com"}]}`)
	if rr.Code != 200 {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	got := fp.gotReq.Messages[0].Content
	if strings.Contains(got, "a@b.com") || !strings.Contains(got, "[REDACTED_EMAIL]") {
		t.Fatalf("email should be redacted before dispatch, provider saw %q", got)
	}
}

func failoverServerH(t *testing.T, primary, secondary provider.Provider, tr *health.Tracker) http.Handler {
	t.Helper()
	rtr := router.New([]router.ModelConfig{
		{Alias: "p", Provider: "a", Upstream: "ua", Price: router.Price{InputPerMTok: 5, OutputPerMTok: 25}, Fallbacks: []string{"s"}},
		{Alias: "s", Provider: "b", Upstream: "ub", Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5}},
	}, "p")
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"a": primary, "b": secondary}, Health: tr},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	return srv.Handler()
}

func TestClientErrorDoesNotPoisonBreaker(t *testing.T) {
	tr := health.New(2, time.Minute, nil)
	primary := &fakeProvider{err: &provider.Error{StatusCode: 400, Type: "invalid_request_error", Message: "bad"}}
	secondary := &fakeProvider{resp: &provider.Response{Content: "ok"}}
	h := failoverServerH(t, primary, secondary, tr)
	for i := 0; i < 3; i++ {
		post(h, `{"model":"p","messages":[{"role":"user","content":"hi"}]}`)
	}
	if !tr.Healthy("a") {
		t.Fatal("client 4xx errors must not open the provider breaker")
	}
	if secondary.calls != 0 {
		t.Fatal("4xx is terminal; must not fail over")
	}
}

func Test429FailsOverWithoutOpeningBreaker(t *testing.T) {
	tr := health.New(2, time.Minute, nil)
	primary := &fakeProvider{err: &provider.Error{StatusCode: 429, Type: "rate_limit_error", RetryAfter: "5"}}
	secondary := &fakeProvider{resp: &provider.Response{Content: "ok", InputTokens: 1, OutputTokens: 1}}
	h := failoverServerH(t, primary, secondary, tr)
	for i := 0; i < 3; i++ {
		if rr := post(h, `{"model":"p","messages":[{"role":"user","content":"hi"}]}`); rr.Code != 200 {
			t.Fatalf("429 should fail over to secondary: got %d", rr.Code)
		}
	}
	if !tr.Healthy("a") {
		t.Fatal("429 is load, not unhealth — must not open the breaker")
	}
}

func TestProviderAuthMapsTo502NotClient401(t *testing.T) {
	primary := &fakeProvider{err: &provider.Error{StatusCode: 401, Type: "authentication_error", Message: "bad vendor key"}}
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "u"}}, "m")
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"fake": primary}},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	rr := post(srv.Handler(), `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("upstream 401 must become 502, not a client 401; got %d", rr.Code)
	}
}

func TestAllProvidersUnhealthyReturns503(t *testing.T) {
	tr := health.New(1, time.Hour, nil)
	tr.RecordFailure("fake") // threshold 1 → breaker open
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "u"}}, "m")
	fp := &fakeProvider{resp: &provider.Response{Content: "ok"}}
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"fake": fp}, Health: tr},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	rr := post(srv.Handler(), `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("all providers unhealthy → 503, got %d", rr.Code)
	}
	if fp.calls != 0 {
		t.Fatal("must not call an open-breaker provider")
	}
}

func TestServedProviderHeader(t *testing.T) {
	fp := &fakeProvider{resp: &provider.Response{Content: "ok"}}
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "up-x"}}, "m")
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"fake": fp}},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	rr := post(srv.Handler(), `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Header().Get("X-Heave-Provider") != "fake" || rr.Header().Get("X-Heave-Upstream") != "up-x" {
		t.Fatalf("served-provider headers missing: %v", rr.Header())
	}
}

func firewallServer(t *testing.T, limits firewall.Limits) http.Handler {
	return firewallServerFP(t, limits, &fakeProvider{resp: &provider.Response{Content: "ok", InputTokens: 1000, OutputTokens: 1000}})
}

func firewallServerFP(t *testing.T, limits firewall.Limits, fp *fakeProvider) http.Handler {
	t.Helper()
	rtr := router.New([]router.ModelConfig{{
		Alias: "m", Provider: "fake", Upstream: "u", Price: router.Price{InputPerMTok: 1, OutputPerMTok: 5},
	}}, "m")
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"fake": fp}, Firewall: firewall.New(true, limits, nil)},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	return srv.Handler()
}

func chatWithRun(h http.Handler, runID string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if runID != "" {
		req.Header.Set("X-Heave-Run-Id", runID)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestKillEndpointBlocksRun(t *testing.T) {
	h := firewallServer(t, firewall.Limits{})
	// Kill run "r1".
	kreq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/runs/r1/kill", nil)
	krr := httptest.NewRecorder()
	h.ServeHTTP(krr, kreq)
	if krr.Code != 200 {
		t.Fatalf("kill endpoint: want 200, got %d", krr.Code)
	}
	if rr := chatWithRun(h, "r1"); rr.Code != http.StatusForbidden {
		t.Fatalf("killed run must be 403, got %d", rr.Code)
	}
	if rr := chatWithRun(h, "r2"); rr.Code != 200 {
		t.Fatalf("other run must still work, got %d", rr.Code)
	}
}

// TestKillWorksWhenRequestSetsUserField is the F3 regression: firewall run scope
// must key off the authenticated client (empty when auth is off), NOT the
// client-controlled `user` field. Before the fix, a request carrying `"user"`
// reserved its run under that value while the kill endpoint targeted "", so the
// runaway was unkillable. Both must resolve to the same owner.
func TestKillWorksWhenRequestSetsUserField(t *testing.T) {
	h := firewallServer(t, firewall.Limits{})
	body := `{"model":"m","user":"spoofed-tenant","messages":[{"role":"user","content":"hi"}]}`
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("X-Heave-Run-Id", "ru")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := send(); rr.Code != 200 {
		t.Fatalf("run should work before kill, got %d", rr.Code)
	}
	kreq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/runs/ru/kill", nil)
	krr := httptest.NewRecorder()
	h.ServeHTTP(krr, kreq)
	if krr.Code != 200 {
		t.Fatalf("kill endpoint: want 200, got %d", krr.Code)
	}
	if rr := send(); rr.Code != http.StatusForbidden {
		t.Fatalf("killed run must be blocked even when request carries a user field, got %d", rr.Code)
	}
}

// TestRunIDCharsetIsValidatedOnBothPaths is the MF-2 regression: a run id must be
// a single safe token so every reservable run is addressable by the kill endpoint.
// An id the kill route (one path segment) can't name must be rejected on ingress,
// never silently accepted as an unkillable run.
func TestRunIDCharsetIsValidatedOnBothPaths(t *testing.T) {
	h := firewallServer(t, firewall.Limits{})
	// Reserve path: a slash-bearing run id (not a single path segment) is rejected.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Heave-Run-Id", "a/b/c")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("slash-bearing run id must be rejected on reserve, got %d", rr.Code)
	}
	// Kill path: same validation, so a malformed id gets a clear 400 (not a 200 for
	// a run that was never reservable).
	kreq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/runs/bad%20id/kill", nil)
	krr := httptest.NewRecorder()
	h.ServeHTTP(krr, kreq)
	if krr.Code != http.StatusBadRequest {
		t.Fatalf("malformed run id must be rejected on kill, got %d", krr.Code)
	}
	// A valid run id still works end to end.
	if rr := chatWithRun(h, "run.1_ok-2"); rr.Code != 200 {
		t.Fatalf("valid run id must be accepted, got %d", rr.Code)
	}
}

func TestLoopAutoKillViaHandler(t *testing.T) {
	h := firewallServer(t, firewall.Limits{LoopThreshold: 3})
	codes := []int{}
	for i := 0; i < 3; i++ {
		codes = append(codes, chatWithRun(h, "loopy").Code)
	}
	if codes[2] != http.StatusForbidden {
		t.Fatalf("3rd identical prompt on a run should be 403 (auto-killed), got %v", codes)
	}
}

func TestVelocityReturns429(t *testing.T) {
	// First request costs ~$0.006 (1000 in @ $1/Mtok + 1000 out @ $5/Mtok);
	// cap is $0.005/min, so the second request is over the window.
	h := firewallServer(t, firewall.Limits{MaxUSDPerMin: 0.005})
	if rr := chatWithRun(h, "run"); rr.Code != 200 {
		t.Fatalf("first request should pass, got %d", rr.Code)
	}
	rr := chatWithRun(h, "run")
	if rr.Code != http.StatusTooManyRequests || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("velocity cap should 429 with Retry-After, got %d ra=%q", rr.Code, rr.Header().Get("Retry-After"))
	}
}

func TestStreamingSuccess(t *testing.T) {
	fp := &fakeProvider{
		deltas: []string{"hello ", "world"},
		resp:   &provider.Response{InputTokens: 5, OutputTokens: 2, FinishReason: "stop"},
	}
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "up-x"}}, "m")
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"fake": fp}},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	rr := post(srv.Handler(), `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rr.Code != 200 {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("want SSE content-type, got %q", ct)
	}
	if rr.Header().Get("X-Heave-Provider") != "fake" {
		t.Fatalf("served-provider header missing on stream: %v", rr.Header())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`"role":"assistant"`,     // leading role chunk
		`"content":"hello "`,     // delta 1
		`"content":"world"`,      // delta 2
		`"finish_reason":"stop"`, // terminal chunk
		`"completion_tokens":2`,  // usage trailer values
		`"prompt_tokens":5`,
		"data: [DONE]",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream body missing %q:\n%s", want, body)
		}
	}
	// Content chunks must carry finish_reason:null (present, not omitted).
	if !strings.Contains(body, `"finish_reason":null`) {
		t.Fatalf("content chunks should emit finish_reason:null, got:\n%s", body)
	}
}

func streamRun(h http.Handler, runID string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Heave-Run-Id", runID)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestStreamingAbortChargesEstimate(t *testing.T) {
	// A stream that fails after emitting bytes must NOT be free: the reserved
	// estimate is charged (per-run scope), so a subsequent request on the same run
	// hits the velocity cap. If the abort released the hold to 0, the 2nd passes.
	fp := &fakeProvider{deltas: []string{"partial answer"}, midStreamErr: &provider.Error{StatusCode: 500, Message: "boom"}}
	h := firewallServerFP(t, firewall.Limits{MaxUSDPerMin: 0.03}, fp)

	rr := streamRun(h, "r")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "partial answer") || !strings.Contains(rr.Body.String(), "data: [DONE]") {
		t.Fatalf("aborted stream should still be 200 SSE with the partial + DONE: code=%d body=%q", rr.Code, rr.Body)
	}
	if rr2 := streamRun(h, "r"); rr2.Code != http.StatusTooManyRequests {
		t.Fatalf("aborted stream must charge its estimate (want 2nd request 429), got %d", rr2.Code)
	}
}

func TestStreamingUsageOmittedChargesEstimate(t *testing.T) {
	// Upstream success but zero usage (usage-omitting backend) must fail closed to
	// the estimate, not book zero — else it's a free bypass.
	fp := &fakeProvider{deltas: []string{"hi"}, resp: &provider.Response{FinishReason: "stop"}} // no tokens
	h := firewallServerFP(t, firewall.Limits{MaxUSDPerMin: 0.03}, fp)
	if rr := streamRun(h, "r"); rr.Code != 200 {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if rr2 := streamRun(h, "r"); rr2.Code != http.StatusTooManyRequests {
		t.Fatalf("zero-usage success must charge the estimate (want 2nd 429), got %d", rr2.Code)
	}
}

func TestStreamingFailsOverBeforeFirstByte(t *testing.T) {
	primary := &fakeProvider{err: &provider.Error{StatusCode: 500, Message: "boom"}}
	secondary := &fakeProvider{resp: &provider.Response{Content: "from-secondary", InputTokens: 1, OutputTokens: 1, FinishReason: "stop"}}
	h := failoverServer(t, primary, secondary)
	rr := post(h, `{"model":"p","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "from-secondary") {
		t.Fatalf("should have streamed from the secondary: code=%d body=%q", rr.Code, rr.Body)
	}
}

func TestStreamingProviderErrorBeforeByteIsJSONError(t *testing.T) {
	// A single-candidate stream that errors before any byte must return a normal
	// JSON error (not a half-open SSE), since no status was written yet.
	fp := &fakeProvider{err: &provider.Error{StatusCode: 400, Type: "invalid_request_error", Message: "bad"}}
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "u"}}, "m")
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"fake": fp}},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	rr := post(srv.Handler(), `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 400 {
		t.Fatalf("pre-byte error should be a 400 JSON error, got %d", rr.Code)
	}
}

func TestRateLimitReturns429(t *testing.T) {
	h := authServer(t, []controls.Client{{Name: "a", KeySHA256: sha("k"), RateLimitRPM: 1}})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	if rr := postAuth(h, body, "k"); rr.Code != 200 {
		t.Fatalf("first: want 200, got %d", rr.Code)
	}
	rr := postAuth(h, body, "k")
	if rr.Code != http.StatusTooManyRequests || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("second: want 429 with Retry-After, got %d ra=%q", rr.Code, rr.Header().Get("Retry-After"))
	}
}

func TestStatsAndDashboardEndpoints(t *testing.T) {
	h := testServer(t, &fakeProvider{resp: &provider.Response{Content: "ok", InputTokens: 5, OutputTokens: 5, FinishReason: "stop"}}, true, time.Second)
	// Drive one request so the ledger has attribution to report.
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Heave-Run-Id", "run-xyz")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("seed request failed: %d", rr.Code)
	}

	// /v1/stats reflects the request.
	sr := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/stats", nil)
	srr := httptest.NewRecorder()
	h.ServeHTTP(srr, sr)
	if srr.Code != 200 {
		t.Fatalf("/v1/stats want 200, got %d", srr.Code)
	}
	var stats struct {
		Total struct {
			Requests int64 `json:"requests"`
		} `json:"total"`
		Recent []struct {
			RunID string `json:"run_id"`
		} `json:"recent"`
	}
	if err := json.Unmarshal(srr.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.Total.Requests < 1 {
		t.Fatal("/v1/stats must report the request")
	}
	if len(stats.Recent) == 0 || stats.Recent[0].RunID != "run-xyz" {
		t.Fatalf("/v1/stats recent must carry the run attribution, got %+v", stats.Recent)
	}

	// /dashboard serves self-contained HTML.
	dr := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/dashboard", nil)
	drr := httptest.NewRecorder()
	h.ServeHTTP(drr, dr)
	if drr.Code != 200 || !strings.Contains(drr.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("/dashboard want 200 html, got %d %q", drr.Code, drr.Header().Get("Content-Type"))
	}
	if !strings.Contains(drr.Body.String(), "/v1/stats") {
		t.Fatal("dashboard must poll /v1/stats")
	}
}

func TestStatsRequiresAdminWhenAuthEnabled(t *testing.T) {
	h := authServer(t, []controls.Client{
		{Name: "ops", KeySHA256: sha("adminkey"), Admin: true},
		{Name: "tenant", KeySHA256: sha("tenantkey")},
	})
	get := func(bearer string) int {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/stats", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if code := get(""); code != http.StatusUnauthorized {
		t.Fatalf("no key must be 401, got %d", code)
	}
	if code := get("tenantkey"); code != http.StatusForbidden {
		t.Fatalf("a non-admin tenant must not read cross-tenant stats (403), got %d", code)
	}
	if code := get("adminkey"); code != 200 {
		t.Fatalf("admin key must read stats (200), got %d", code)
	}
	// The dashboard SHELL stays open (no data in it).
	dr := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/dashboard", nil)
	drr := httptest.NewRecorder()
	h.ServeHTTP(drr, dr)
	if drr.Code != 200 {
		t.Fatalf("dashboard shell must stay open, got %d", drr.Code)
	}
}

func TestErrorRecordCarriesRunAttribution(t *testing.T) {
	// Non-retryable error → an "error" ledger record on the same run, no failover.
	fp := &fakeProvider{err: &provider.Error{StatusCode: 400, Type: "invalid_request_error", Message: "bad"}}
	h := testServer(t, fp, true, time.Second)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Heave-Run-Id", "run-err")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == 200 {
		t.Fatal("expected the request to fail")
	}
	// /v1/stats (auth off → open) must attribute the failed request to its run.
	sr := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/stats", nil)
	srr := httptest.NewRecorder()
	h.ServeHTTP(srr, sr)
	if srr.Code != 200 {
		t.Fatalf("/v1/stats with auth off must be open, got %d", srr.Code)
	}
	var stats struct {
		Recent []struct {
			RunID  string `json:"run_id"`
			Status string `json:"status"`
		} `json:"recent"`
	}
	if err := json.Unmarshal(srr.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(stats.Recent) == 0 || stats.Recent[0].RunID != "run-err" || stats.Recent[0].Status != "error" {
		t.Fatalf("error record must carry run attribution, got %+v", stats.Recent)
	}
}
