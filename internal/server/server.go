// Package server wires the HTTP surface: the OpenAI-compatible chat endpoint,
// health, and metrics. It owns the request flow — translate wire -> neutral,
// route, dispatch to a provider, record spend, translate back — but contains no
// vendor-specific logic.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/somasays/heave/internal/firewall"
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
	firewall       *firewall.Firewall
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
	Firewall  *firewall.Firewall
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
	if d.Firewall == nil {
		d.Firewall = firewall.New(false, firewall.Limits{}, nil)
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
		firewall:       d.Firewall,
		log:            d.Log,
		maxRequestBody: opts.MaxRequestBytes,
		requestTimeout: opts.RequestTimeout,
	}
}

// Handler returns the configured HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("POST /v1/runs/{run_id}/kill", s.handleKillRun)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	return mux
}

// handleKillRun hard-stops an agent run: every subsequent request on it is
// rejected. Requires a valid API key when auth is enabled.
func (s *Server) handleKillRun(w http.ResponseWriter, r *http.Request) {
	client, err := s.guard.Admit(bearerToken(r))
	if err != nil {
		s.denied(w, nil, err)
		return
	}
	runID := r.PathValue("run_id")
	if runID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "run_id is required")
		return
	}
	// A caller can only kill its own runs (scoped by the authenticated key).
	ownerKey := ""
	if client != nil {
		ownerKey = client.Name
	}
	s.firewall.Kill(ownerKey, runID)
	s.log.Warn("run killed", "run_id", runID, "owner", ownerKey)
	writeJSON(w, http.StatusOK, map[string]any{"killed": runID})
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

	estUSD, estTokens := maxEstimate(&req, candidates)

	// Reserve against the MAX estimated cost across the candidate chain (a
	// pricier fallback must not let a client slip past its budget), BEFORE
	// dispatch, so concurrent requests cannot all pass and overshoot the cap.
	reservation, err := s.guard.Reserve(client, estUSD)
	if err != nil {
		s.denied(w, client, err)
		return
	}

	// Firewall (Invariant #9): pre-vendor velocity / concurrency / kill / loop
	// enforcement, scoped to the client key and the agent run. The estimate is
	// reserved (held in the window) and reconciled by Settle on success.
	fwKey := userName(client, &req)
	runID := strings.TrimSpace(r.Header.Get("X-Heave-Run-Id"))
	ticket, ferr := s.firewall.Enter(fwKey, runID, promptHash(&req), estUSD, estTokens)
	if ferr != nil {
		s.guard.Settle(reservation, 0)
		s.firewallDenied(w, fwKey, runID, ferr)
		return
	}
	defer ticket.Release()

	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()

	acct := accounting{
		client: client, reservation: reservation, ticket: ticket,
		requestID: requestID, start: start, primaryAlias: primary.Alias,
		estUSD: estUSD, estTokens: estTokens,
	}
	if req.Stream {
		s.serveStreaming(ctx, w, &req, candidates, acct)
		return
	}
	s.serveUnary(ctx, w, &req, candidates, acct)
}

// accounting carries the per-request settle/record state shared by the unary and
// streaming dispatch paths.
type accounting struct {
	client       *controls.Client
	reservation  *controls.Reservation
	ticket       *firewall.Ticket
	requestID    string
	start        time.Time
	primaryAlias string
	estUSD       float64
	estTokens    int
}

// runCandidates tries candidates in order, skipping unhealthy providers and
// recording each failed attempt. attempt performs one dispatch (unary or
// streaming); canFailover reports whether it is still safe to try the next
// candidate (false once a stream has written bytes). It stops on success or a
// terminal/no-longer-failover-able error.
func (s *Server) runCandidates(
	ctx context.Context, req *openai.ChatCompletionRequest, candidates []router.Decision, acct accounting,
	attempt func(prov provider.Provider, d router.Decision) (*provider.Response, error),
	canFailover func() bool,
) (presp *provider.Response, used router.Decision, served, attempted bool, lastErr error) {
	for _, d := range candidates {
		prov, ok := s.providers[d.Provider]
		if !ok || !s.health.Healthy(d.Provider) {
			continue
		}
		attempted = true
		resp, err := attempt(prov, d)
		if err == nil {
			s.health.RecordSuccess(d.Provider)
			return resp, d, true, true, nil
		}
		lastErr = err
		s.ledger.Record(ledger.Record{
			RequestID: acct.requestID, Alias: d.Alias, Provider: d.Provider, Upstream: d.Upstream,
			User: userName(acct.client, req), LatencyMS: time.Since(acct.start).Milliseconds(), Status: "error",
		})
		if ctx.Err() != nil || !retryable(err) || !canFailover() {
			break
		}
		if !isRateLimited(err) {
			s.health.RecordFailure(d.Provider)
		}
	}
	return nil, router.Decision{}, false, attempted, lastErr
}

// recordSuccess settles the budget/firewall reservations and logs the billable
// record. If the upstream returned no usage (some OpenAI-compatible backends omit
// it on streams), it FAILS CLOSED to the reserved estimate rather than booking
// zero — otherwise a usage-omitting backend would make requests free and evade
// the velocity/budget caps.
func (s *Server) recordSuccess(req *openai.ChatCompletionRequest, acct accounting, used router.Decision, presp *provider.Response) {
	inTok, outTok := presp.InputTokens, presp.OutputTokens
	cacheR, cacheW := presp.CacheReadInputTokens, presp.CacheWriteInputTokens
	status := "ok"
	cost := ledger.Cost(inTok, outTok, cacheR, cacheW, used.Price.InputPerMTok, used.Price.OutputPerMTok)
	if inTok == 0 && outTok == 0 && cacheR == 0 && cacheW == 0 {
		cost = acct.estUSD
		inTok, outTok = acct.estTokens, 0
		status = "ok_estimated"
		s.log.Warn("upstream returned no usage; charging reserved estimate",
			"request_id", acct.requestID, "provider", used.Provider)
	}
	s.guard.Settle(acct.reservation, cost)
	acct.ticket.Settle(cost, inTok+outTok)
	s.ledger.Record(ledger.Record{
		RequestID: acct.requestID, Alias: used.Alias, Provider: used.Provider, Upstream: used.Upstream,
		User: userName(acct.client, req), InputTokens: inTok, OutputTokens: outTok,
		CacheReadTokens: cacheR, CacheWriteTokens: cacheW,
		CostUSD: cost, LatencyMS: time.Since(acct.start).Milliseconds(), Status: status,
	})
}

func (s *Server) serveUnary(ctx context.Context, w http.ResponseWriter, req *openai.ChatCompletionRequest, candidates []router.Decision, acct accounting) {
	presp, used, served, attempted, lastErr := s.runCandidates(ctx, req, candidates, acct,
		func(prov provider.Provider, d router.Decision) (*provider.Response, error) {
			return prov.ChatCompletion(ctx, toProviderRequest(req, d))
		},
		func() bool { return true })

	if !served {
		s.guard.Settle(acct.reservation, 0)
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
	s.recordSuccess(req, acct, used, presp)
	w.Header().Set("X-Heave-Provider", used.Provider)
	w.Header().Set("X-Heave-Upstream", used.Upstream)
	writeJSON(w, http.StatusOK, toOpenAIResponse(acct.requestID, acct.primaryAlias, presp))
}

func (s *Server) serveStreaming(ctx context.Context, w http.ResponseWriter, req *openai.ChatCompletionRequest, candidates []router.Decision, acct accounting) {
	fl, ok := w.(http.Flusher)
	if !ok {
		s.guard.Settle(acct.reservation, 0)
		writeError(w, http.StatusInternalServerError, "api_error", "server writer does not support streaming")
		return
	}
	sw := &sseWriter{w: w, fl: fl, id: acct.requestID, model: acct.primaryAlias}

	presp, used, served, attempted, lastErr := s.runCandidates(ctx, req, candidates, acct,
		func(prov provider.Provider, d router.Decision) (*provider.Response, error) {
			sw.setCandidate(d) // written as X-Heave-* headers on the first delta
			return prov.ChatCompletionStream(ctx, toProviderRequest(req, d), sw.writeDelta)
		},
		func() bool { return !sw.wroteAny }) // can only fail over before the first byte

	if !served {
		if sw.wroteAny {
			// Bytes were already streamed (upstream failed mid-stream, or the
			// client disconnected). The vendor billed us for what it generated, so
			// FAIL CLOSED: charge the reserved estimate rather than releasing it —
			// otherwise streaming + early disconnect would be a free firewall bypass.
			s.guard.Settle(acct.reservation, acct.estUSD)
			acct.ticket.Settle(acct.estUSD, acct.estTokens)
			s.ledger.Record(ledger.Record{
				RequestID: acct.requestID, Alias: acct.primaryAlias,
				User: userName(acct.client, req), InputTokens: acct.estTokens,
				CostUSD: acct.estUSD, LatencyMS: time.Since(acct.start).Milliseconds(), Status: "aborted",
			})
			sw.finishError(lastErr)
			return
		}
		s.guard.Settle(acct.reservation, 0)
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
	s.recordSuccess(req, acct, used, presp)
	sw.finish(presp)
}

// promptHash is a stable hash of the request's system + message content, used by
// the firewall's loop detection (a run resending the same prompt is a runaway).
func promptHash(req *openai.ChatCompletionRequest) string {
	h := sha256.New()
	for _, m := range req.Messages {
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(m.Content.Text))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// firewallDenied maps a firewall rejection to an HTTP status and logs it.
func (s *Server) firewallDenied(w http.ResponseWriter, key, runID string, err error) {
	var ve *firewall.VelocityError
	var ce *firewall.ConcurrencyError
	switch {
	case errors.Is(err, firewall.ErrKilled):
		s.log.Warn("request denied", "reason", "run_killed", "key", key, "run_id", runID)
		writeError(w, http.StatusForbidden, "run_killed", "this run has been killed")
	case errors.As(err, &ve):
		s.log.Warn("request denied", "reason", "velocity_exceeded", "scope", ve.Scope, "key", key, "run_id", runID)
		if ve.RetryAfterSec > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(ve.RetryAfterSec))
		}
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "spend/token velocity limit exceeded")
	case errors.As(err, &ce):
		s.log.Warn("request denied", "reason", "concurrency_exceeded", "scope", ce.Scope, "key", key, "run_id", runID)
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "concurrency limit exceeded")
	default:
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "request blocked by firewall")
	}
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

// estimate is an upper-bound cost (USD) and token count for a request on one
// decision: estimated input tokens (rough chars/4) plus the resolved max output
// tokens, at the model's price. Reconciled to the real values after the response.
func estimate(preq *provider.Request, decision router.Decision) (usd float64, tokens int) {
	chars := len(preq.System)
	for _, m := range preq.Messages {
		chars += len(m.Content)
	}
	estInput := chars / 4
	maxOut := preq.MaxTokens
	if maxOut <= 0 {
		maxOut = 4096
	}
	return ledger.Cost(estInput, maxOut, 0, 0, decision.Price.InputPerMTok, decision.Price.OutputPerMTok), estInput + maxOut
}

// maxEstimate is the largest per-candidate USD and token estimate across the
// failover chain, so reservations cover whichever candidate ends up serving.
func maxEstimate(req *openai.ChatCompletionRequest, candidates []router.Decision) (usd float64, tokens int) {
	for _, d := range candidates {
		u, t := estimate(toProviderRequest(req, d), d)
		if u > usd {
			usd = u
		}
		if t > tokens {
			tokens = t
		}
	}
	return usd, tokens
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

// sseWriter emits an OpenAI-compatible chat.completion.chunk SSE stream. It
// defers writing the 200 status + headers until the first delta, so a provider
// that fails before any byte can still be failed over / returned as a JSON error.
type sseWriter struct {
	w        http.ResponseWriter
	fl       http.Flusher
	id       string
	model    string
	provider string
	upstream string
	wroteAny bool
	created  int64
}

func (sw *sseWriter) setCandidate(d router.Decision) {
	sw.provider, sw.upstream = d.Provider, d.Upstream
}

func (sw *sseWriter) start() error {
	h := sw.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Heave-Provider", sw.provider)
	h.Set("X-Heave-Upstream", sw.upstream)
	sw.w.WriteHeader(http.StatusOK)
	sw.created = time.Now().Unix()
	sw.wroteAny = true
	return sw.emit(openai.ChatCompletionChunk{
		ID: sw.chunkID(), Object: "chat.completion.chunk", Created: sw.created, Model: sw.model,
		Choices: []openai.ChunkChoice{{Delta: openai.Delta{Role: "assistant"}}},
	})
}

// writeDelta is the provider.StreamFunc; returning an error aborts the upstream
// stream (e.g. the client disconnected and the write failed).
func (sw *sseWriter) writeDelta(delta string) error {
	if !sw.wroteAny {
		if err := sw.start(); err != nil {
			return err
		}
	}
	return sw.emit(openai.ChatCompletionChunk{
		ID: sw.chunkID(), Object: "chat.completion.chunk", Created: sw.created, Model: sw.model,
		Choices: []openai.ChunkChoice{{Delta: openai.Delta{Content: delta}}},
	})
}

func (sw *sseWriter) chunkID() string { return "chatcmpl-" + sw.id }

func (sw *sseWriter) emit(chunk openai.ChatCompletionChunk) error {
	b, _ := json.Marshal(chunk)
	line := append([]byte("data: "), b...)
	line = append(line, '\n', '\n')
	_, err := sw.w.Write(line)
	sw.fl.Flush()
	return err
}

// finish emits the terminal finish_reason chunk, then a separate usage-only
// chunk (choices:[]) matching OpenAI's include_usage shape, then [DONE].
func (sw *sseWriter) finish(presp *provider.Response) {
	if !sw.wroteAny {
		_ = sw.start()
	}
	reason := presp.FinishReason
	_ = sw.emit(openai.ChatCompletionChunk{
		ID: sw.chunkID(), Object: "chat.completion.chunk", Created: sw.created, Model: sw.model,
		Choices: []openai.ChunkChoice{{Delta: openai.Delta{}, FinishReason: &reason}},
	})
	_ = sw.emit(openai.ChatCompletionChunk{
		ID: sw.chunkID(), Object: "chat.completion.chunk", Created: sw.created, Model: sw.model,
		Choices: []openai.ChunkChoice{},
		Usage: &openai.Usage{
			PromptTokens: presp.InputTokens, CompletionTokens: presp.OutputTokens,
			TotalTokens: presp.InputTokens + presp.OutputTokens,
		},
	})
	sw.done()
}

// finishError ends an already-started stream with an error object then [DONE].
// The status is already 200, so this is the only in-band way to signal a fault.
// It does NOT leak the raw upstream message: only a normalized type + status.
func (sw *sseWriter) finishError(err error) {
	body := openai.ErrorBody{Message: "upstream request failed", Type: "api_error"}
	var pe *provider.Error
	if errors.As(err, &pe) {
		if pe.Type != "" {
			body.Type = pe.Type
		}
		if pe.StatusCode >= 400 {
			body.Code = strconv.Itoa(pe.StatusCode)
		}
	}
	b, _ := json.Marshal(openai.ErrorResponse{Error: body})
	line := append([]byte("data: "), b...)
	line = append(line, '\n', '\n')
	_, _ = sw.w.Write(line)
	sw.done()
}

func (sw *sseWriter) done() {
	_, _ = sw.w.Write([]byte("data: [DONE]\n\n"))
	sw.fl.Flush()
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
