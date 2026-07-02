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
	"github.com/somasays/heave/internal/ledger"
	"github.com/somasays/heave/internal/openai"
	"github.com/somasays/heave/internal/provider"
	"github.com/somasays/heave/internal/router"
)

type fakeProvider struct {
	resp   *provider.Response
	err    error
	block  bool
	gotReq *provider.Request
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) ChatCompletion(ctx context.Context, req *provider.Request) (*provider.Response, error) {
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
	led := ledger.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	guard := controls.New(false, nil, nil) // auth disabled for most tests
	srv := New(rtr, map[string]provider.Provider{"fake": fp}, led, guard, slog.New(slog.NewTextHandler(io.Discard, nil)),
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
	led := ledger.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	guard := controls.New(false, nil, nil)
	srv := New(rtr, map[string]provider.Provider{"fake": &fakeProvider{resp: &provider.Response{}}}, led, guard,
		slog.New(slog.NewTextHandler(io.Discard, nil)), Options{MaxRequestBytes: 32, RequestTimeout: time.Second})
	rr := post(srv.Handler(), `{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("x", 200)+`"}]}`)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", rr.Code)
	}
}

func authServer(t *testing.T, clients []controls.Client) http.Handler {
	t.Helper()
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "up-1"}}, "m")
	led := ledger.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	guard := controls.New(true, clients, nil)
	fp := &fakeProvider{resp: &provider.Response{Content: "ok", InputTokens: 1, OutputTokens: 1}}
	srv := New(rtr, map[string]provider.Provider{"fake": fp}, led, guard, slog.New(slog.NewTextHandler(io.Discard, nil)),
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
