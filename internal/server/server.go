// Package server wires the HTTP surface: the OpenAI-compatible chat endpoint,
// health, and metrics. It owns the request flow — translate wire -> neutral,
// route, dispatch to a provider, record spend, translate back — but contains no
// vendor-specific logic.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/somasays/heave/internal/controls"
	"github.com/somasays/heave/internal/health"
	"github.com/somasays/heave/internal/ledger"
	"github.com/somasays/heave/internal/openai"
	"github.com/somasays/heave/internal/provider"
	"github.com/somasays/heave/internal/redact"
	"github.com/somasays/heave/internal/router"
)

// Server holds the dependencies for the HTTP handlers.
type Server struct {
	router         *router.Router
	providers      map[string]provider.Provider
	ledger         *ledger.Ledger
	guard          *controls.Guard
	health         *health.Tracker
	redactor       *redact.Redactor
	log            *slog.Logger
	maxRequestBody int64
	requestTimeout time.Duration
}

// Deps bundles the collaborators the server orchestrates.
type Deps struct {
	Router    *router.Router
	Providers map[string]provider.Provider
	Ledger    *ledger.Ledger
	Guard     *controls.Guard
	Health    *health.Tracker
	Redactor  *redact.Redactor
	Log       *slog.Logger
}

// Options configures request hardening limits.
type Options struct {
	MaxRequestBytes int64
	RequestTimeout  time.Duration
}

// New builds a Server.
func New(d Deps, opts Options) *Server {
	if opts.MaxRequestBytes <= 0 {
		opts.MaxRequestBytes = 1 << 20
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 120 * time.Second
	}
	// Defend the pointer-receiver collaborators so a partially-filled Deps can't
	// panic mid-request (main always sets them; this guards misuse/tests).
	if d.Guard == nil {
		d.Guard = controls.New(false, nil, nil)
	}
	if d.Health == nil {
		d.Health = health.New(3, 30*time.Second, nil)
	}
	if d.Redactor == nil {
		d.Redactor = redact.New(false, nil)
	}
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{
		router:         d.Router,
		providers:      d.Providers,
		ledger:         d.Ledger,
		guard:          d.Guard,
		health:         d.Health,
		redactor:       d.Redactor,
		log:            d.Log,
		maxRequestBody: opts.MaxRequestBytes,
		requestTimeout: opts.RequestTimeout,
	}
}

// Handler returns the configured HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	reqs, toks, cost := s.ledger.Totals()
	writeJSON(w, http.StatusOK, map[string]any{
		"requests": reqs,
		"tokens":   toks,
		"cost_usd": cost,
	})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := newRequestID()

	// Cap the body so an unauthenticated client cannot OOM the process.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxRequestBody)

	// Authenticate + rate limit BEFORE any parsing or vendor call (Invariant
	// #7). client is nil when auth is disabled. Budget is enforced after the
	// cost is known (Reserve, below).
	client, err := s.guard.Admit(bearerToken(r))
	if err != nil {
		s.denied(w, client, err)
		return
	}

	var req openai.ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}

	// Reject — never silently drop — capabilities Phase 0 does not implement.
	if msg, ok := unsupported(&req); !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}

	// Pre-flight redaction: scrub PII/secrets from request content before it can
	// reach any vendor. Counts (never values) are logged.
	if s.redactor.Enabled() {
		if counts := redactRequest(s.redactor, &req); len(counts) > 0 {
			s.log.Info("redacted request content", "request_id", requestID, "counts", counts)
		}
	}

	candidates, err := s.router.Candidates(req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	primary := candidates[0]

	// Reserve against the MAX estimated cost across the candidate chain (a
	// pricier fallback must not let a client slip past its budget), BEFORE
	// dispatch, so concurrent requests cannot all pass and overshoot the cap.
	reservation, err := s.guard.Reserve(client, maxEstimateUSD(&req, candidates))
	if err != nil {
		s.denied(w, client, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()

	// Failover: try candidates in order, skipping unhealthy providers, stopping
	// on success or a terminal (non-retryable) error. Each failed attempt is
	// recorded so vendor-side spend is never invisible.
	var (
		presp     *provider.Response
		used      router.Decision
		lastErr   error
		served    bool
		attempted bool
	)
	for _, d := range candidates {
		prov, ok := s.providers[d.Provider]
		if !ok || !s.health.Healthy(d.Provider) {
			continue
		}
		attempted = true
		resp, err := prov.ChatCompletion(ctx, toProviderRequest(&req, d))
		if err == nil {
			s.health.RecordSuccess(d.Provider)
			presp, used, served = resp, d, true
			break
		}
		lastErr = err
		s.ledger.Record(ledger.Record{
			RequestID: requestID, Alias: d.Alias, Provider: d.Provider, Upstream: d.Upstream,
			User: userName(client, &req), LatencyMS: time.Since(start).Milliseconds(), Status: "error",
		})
		if ctx.Err() != nil || !retryable(err) {
			break // client cancel / deadline, or terminal error — do not blame provider health
		}
		if !isRateLimited(err) {
			// 429 is load, not unhealth; counting it would evacuate all traffic
			// to fallbacks and cascade. Only real infra faults open the breaker.
			s.health.RecordFailure(d.Provider)
		}
	}

	if !served {
		s.guard.Settle(reservation, 0) // release the hold; no successful billable spend
		if !attempted {
			writeError(w, http.StatusServiceUnavailable, "api_error", "no healthy provider available for this model")
			return
		}
		status, typ, msg, retryAfter := classifyError(ctx, lastErr)
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		writeError(w, status, typ, msg)
		return
	}

	cost := ledger.Cost(presp.InputTokens, presp.OutputTokens, presp.CacheReadInputTokens, presp.CacheWriteInputTokens,
		used.Price.InputPerMTok, used.Price.OutputPerMTok)
	s.guard.Settle(reservation, cost)
	s.ledger.Record(ledger.Record{
		RequestID: requestID, Alias: used.Alias, Provider: used.Provider, Upstream: used.Upstream,
		User: userName(client, &req), InputTokens: presp.InputTokens, OutputTokens: presp.OutputTokens,
		CacheReadTokens: presp.CacheReadInputTokens, CacheWriteTokens: presp.CacheWriteInputTokens,
		CostUSD: cost, LatencyMS: time.Since(start).Milliseconds(), Status: "ok",
	})

	// Surface the provider/model that actually served the request, so a
	// cross-provider failover is never invisible to the caller (data-residency).
	w.Header().Set("X-Heave-Provider", used.Provider)
	w.Header().Set("X-Heave-Upstream", used.Upstream)
	// Respond with the alias the client requested (primary); the header above
	// carries the real upstream when it differs.
	writeJSON(w, http.StatusOK, toOpenAIResponse(requestID, primary.Alias, presp))
}

// retryable reports whether a provider error warrants trying the next candidate.
// Transport failures/timeouts (status 0), 408, 429, and 5xx are retryable. So
// are upstream 401/403 — client auth was already handled by Guard.Admit, so a
// 401/403 here is a gateway-side credential/permission fault with our vendor
// key, which a different provider may not share. Other 4xx (400/404/409/422)
// are terminal — the request would fail identically everywhere.
func retryable(err error) bool {
	var pe *provider.Error
	if errors.As(err, &pe) {
		switch {
		case pe.StatusCode == 0, pe.StatusCode >= 500:
			return true
		case pe.StatusCode == http.StatusRequestTimeout,
			pe.StatusCode == http.StatusTooManyRequests,
			pe.StatusCode == http.StatusUnauthorized,
			pe.StatusCode == http.StatusForbidden:
			return true
		default:
			return false
		}
	}
	return true
}

// isRateLimited reports whether err is an upstream 429.
func isRateLimited(err error) bool {
	var pe *provider.Error
	return errors.As(err, &pe) && pe.StatusCode == http.StatusTooManyRequests
}

// redactRequest scrubs each message's content (and the caller-supplied `user`
// identifier) in place and returns aggregate replacement counts by rule name
// (safe to log — never the values).
func redactRequest(r *redact.Redactor, req *openai.ChatCompletionRequest) map[string]int {
	var total map[string]int
	add := func(counts map[string]int) {
		for k, v := range counts {
			if total == nil {
				total = map[string]int{}
			}
			total[k] += v
		}
	}
	for i := range req.Messages {
		scrubbed, counts := r.Redact(req.Messages[i].Content.Text)
		req.Messages[i].Content.Text = scrubbed
		add(counts)
	}
	// The `user` field often carries an end-user email/id and lands in the
	// ledger, so scrub it too when redaction is on.
	if scrubbed, counts := r.Redact(req.User); len(counts) > 0 {
		req.User = scrubbed
		add(counts)
	}
	return total
}

// estimateCostUSD is an upper-bound cost for budget reservation: estimated input
// tokens (rough chars/4) plus the resolved max output tokens, at the model's
// price. Reconciled to the real cost by Settle after the response.
func estimateCostUSD(preq *provider.Request, decision router.Decision) float64 {
	chars := len(preq.System)
	for _, m := range preq.Messages {
		chars += len(m.Content)
	}
	estInput := chars / 4
	maxOut := preq.MaxTokens
	if maxOut <= 0 {
		maxOut = 4096
	}
	return ledger.Cost(estInput, maxOut, 0, 0, decision.Price.InputPerMTok, decision.Price.OutputPerMTok)
}

// maxEstimateUSD is the largest per-candidate cost estimate across the failover
// chain, so the budget reservation covers whichever candidate ends up serving.
func maxEstimateUSD(req *openai.ChatCompletionRequest, candidates []router.Decision) float64 {
	max := 0.0
	for _, d := range candidates {
		if e := estimateCostUSD(toProviderRequest(req, d), d); e > max {
			max = e
		}
	}
	return max
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header,
// trimming surrounding whitespace (copy-paste / header-file newlines are a
// common source of otherwise-silent 401s).
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// userName attributes spend to the authenticated client, falling back to the
// request's own `user` field when auth is disabled.
func userName(client *controls.Client, req *openai.ChatCompletionRequest) string {
	if client != nil {
		return client.Name
	}
	return req.User
}

// denied maps a controls rejection to the right HTTP status and logs it, so
// throttled/over-budget callers are observable (rejections never reach the
// ledger, since no spend occurred). client may be nil (auth failure).
func (s *Server) denied(w http.ResponseWriter, client *controls.Client, err error) {
	who := "unknown"
	if client != nil {
		who = client.Name
	}
	var rle *controls.RateLimitError
	var be *controls.BudgetError
	switch {
	case errors.Is(err, controls.ErrUnauthorized):
		s.log.Warn("request denied", "reason", "unauthorized", "client", who)
		writeError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid API key")
	case errors.As(err, &rle):
		s.log.Warn("request denied", "reason", "rate_limited", "client", who)
		if rle.RetryAfterSec > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(rle.RetryAfterSec))
		}
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "rate limit exceeded")
	case errors.As(err, &be):
		s.log.Warn("request denied", "reason", "budget_exceeded", "client", who)
		if be.RetryAfterSec > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(be.RetryAfterSec))
		}
		writeError(w, http.StatusTooManyRequests, "insufficient_quota", "monthly budget exceeded")
	default:
		s.log.Warn("request denied", "reason", "authorization_failed", "client", who)
		writeError(w, http.StatusInternalServerError, "api_error", "authorization failed")
	}
}

// unsupported reports a clear message (and false) when the request uses a
// capability Phase 0 does not implement, so the gateway never pretends.
func unsupported(req *openai.ChatCompletionRequest) (string, bool) {
	if req.Stream {
		return "streaming is not yet supported", false
	}
	if len(req.Tools) > 0 || len(req.Functions) > 0 {
		return "tool/function calling is not yet supported", false
	}
	if len(req.ResponseFormat) > 0 {
		return "response_format is not yet supported", false
	}
	if req.N != nil && *req.N > 1 {
		return "n>1 is not supported", false
	}
	for _, m := range req.Messages {
		if m.Content.HasImage {
			return "image content is not yet supported (text only)", false
		}
	}
	return "", true
}

// toProviderRequest lifts system/developer messages out, maps the rest to
// neutral form, applies the per-model max-token default, and strips sampling
// params for models that reject them.
func toProviderRequest(req *openai.ChatCompletionRequest, decision router.Decision) *provider.Request {
	var system string
	msgs := make([]provider.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			if system != "" {
				system += "\n\n"
			}
			system += m.Content.Text
			continue
		}
		msgs = append(msgs, provider.Message{Role: m.Role, Content: m.Content.Text})
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = decision.MaxOutputTokens // may be 0 -> adapter default
	}

	pr := &provider.Request{
		Model:       decision.Upstream,
		System:      system,
		Messages:    msgs,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}
	if !decision.AcceptsSampling {
		pr.Temperature = nil
		pr.TopP = nil
	}
	return pr
}

// classifyError maps a provider failure to an HTTP status that preserves
// provenance without lying to the client: a client-fixable upstream 4xx stays
// 4xx; an upstream 429 stays 429 (with Retry-After); an upstream auth/permission
// failure (401/403) is OUR credential problem, not the client's, so it becomes a
// 502 rather than a misleading 401; timeouts become 504; everything else 502.
func classifyError(ctx context.Context, err error) (status int, typ, msg, retryAfter string) {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return http.StatusGatewayTimeout, "timeout_error", "upstream request timed out", ""
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		switch {
		case pe.StatusCode == http.StatusUnauthorized || pe.StatusCode == http.StatusForbidden:
			// Do not surface a vendor credential fault as a client auth error.
			return http.StatusBadGateway, "api_error", "upstream provider rejected the gateway's credentials", ""
		case pe.StatusCode == http.StatusTooManyRequests:
			return http.StatusTooManyRequests, pe.Type, pe.Message, pe.RetryAfter
		case pe.StatusCode >= 400 && pe.StatusCode < 500:
			return pe.StatusCode, pe.Type, pe.Message, pe.RetryAfter
		}
		msg = pe.Message
	} else {
		msg = err.Error()
	}
	return http.StatusBadGateway, "api_error", msg, ""
}

func toOpenAIResponse(id, alias string, presp *provider.Response) openai.ChatCompletionResponse {
	return openai.ChatCompletionResponse{
		ID:      "chatcmpl-" + id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   alias,
		Choices: []openai.Choice{{
			Index:        0,
			Message:      openai.Message{Role: "assistant", Content: openai.MessageContent{Text: presp.Content}},
			FinishReason: presp.FinishReason,
		}},
		Usage: openai.Usage{
			PromptTokens:     presp.InputTokens,
			CompletionTokens: presp.OutputTokens,
			TotalTokens:      presp.InputTokens + presp.OutputTokens,
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, openai.ErrorResponse{Error: openai.ErrorBody{Message: msg, Type: typ}})
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
