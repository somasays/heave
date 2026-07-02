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
	resp   *provider.Response
	err    error
	block  bool
	gotReq *provider.Request
	calls  int
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
