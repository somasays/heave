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
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/somasays/gateway/internal/ledger"
	"github.com/somasays/gateway/internal/openai"
	"github.com/somasays/gateway/internal/provider"
	"github.com/somasays/gateway/internal/router"
)

// Server holds the dependencies for the HTTP handlers.
type Server struct {
	router         *router.Router
	providers      map[string]provider.Provider
	ledger         *ledger.Ledger
	log            *slog.Logger
	maxRequestBody int64
	requestTimeout time.Duration
}

// Options configures request hardening limits.
type Options struct {
	MaxRequestBytes int64
	RequestTimeout  time.Duration
}

// New builds a Server.
func New(r *router.Router, providers map[string]provider.Provider, l *ledger.Ledger, log *slog.Logger, opts Options) *Server {
	if opts.MaxRequestBytes <= 0 {
		opts.MaxRequestBytes = 1 << 20
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 120 * time.Second
	}
	return &Server{
		router:         r,
		providers:      providers,
		ledger:         l,
		log:            log,
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

	decision, err := s.router.Route(req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	prov, ok := s.providers[decision.Provider]
	if !ok {
		writeError(w, http.StatusInternalServerError, "api_error", "provider not configured: "+decision.Provider)
		return
	}

	preq, err := toProviderRequest(&req, decision)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()

	presp, err := prov.ChatCompletion(ctx, preq)
	if err != nil {
		s.ledger.Record(ledger.Record{
			RequestID: requestID, Alias: decision.Alias, Provider: decision.Provider,
			Upstream: decision.Upstream, User: req.User, LatencyMS: time.Since(start).Milliseconds(),
			Status: "error",
		})
		status, typ, msg, retryAfter := classifyError(ctx, err)
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		writeError(w, status, typ, msg)
		return
	}

	cost := ledger.Cost(presp.InputTokens, presp.OutputTokens, presp.CacheReadInputTokens, presp.CacheWriteInputTokens,
		decision.Price.InputPerMTok, decision.Price.OutputPerMTok)
	s.ledger.Record(ledger.Record{
		RequestID: requestID, Alias: decision.Alias, Provider: decision.Provider, Upstream: decision.Upstream,
		User: req.User, InputTokens: presp.InputTokens, OutputTokens: presp.OutputTokens,
		CacheReadTokens: presp.CacheReadInputTokens, CacheWriteTokens: presp.CacheWriteInputTokens,
		CostUSD: cost, LatencyMS: time.Since(start).Milliseconds(), Status: "ok",
	})

	writeJSON(w, http.StatusOK, toOpenAIResponse(requestID, decision.Alias, presp))
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
func toProviderRequest(req *openai.ChatCompletionRequest, decision router.Decision) (*provider.Request, error) {
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
	return pr, nil
}

// classifyError maps a provider failure to an HTTP status that preserves
// provenance: upstream 4xx stays 4xx, timeouts become 504, everything else 502.
func classifyError(ctx context.Context, err error) (status int, typ, msg, retryAfter string) {
	var pe *provider.Error
	if errors.As(err, &pe) {
		if pe.StatusCode >= 400 && pe.StatusCode < 500 {
			return pe.StatusCode, pe.Type, pe.Message, pe.RetryAfter
		}
		msg = pe.Message
	} else {
		msg = err.Error()
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return http.StatusGatewayTimeout, "timeout_error", "upstream request timed out", ""
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
