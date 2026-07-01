// Package provider defines the boundary between the gateway core and vendor
// APIs. Every model vendor is reached through a Provider adapter; no other
// package imports a vendor SDK or calls a vendor endpoint (docs/INVARIANTS.md,
// Invariant #2 — enforced by scripts/check_arch.sh).
package provider

import "context"

// Request is the provider-neutral completion request. The server builds it from
// the canonical OpenAI wire format after the router has resolved the upstream
// model; adapters translate it into their vendor's shape.
type Request struct {
	// Model is the upstream (vendor) model id to send, already resolved by the
	// router — not the client-facing alias.
	Model     string
	System    string
	Messages  []Message
	MaxTokens int
	// Temperature/TopP are nil when the client did not set them (or when the
	// server stripped them because the target model rejects sampling params),
	// so adapters omit them rather than send a vendor default.
	Temperature *float64
	TopP        *float64
}

// Message is one turn in the conversation. Role is "user" or "assistant";
// system content is lifted into Request.System.
type Message struct {
	Role    string
	Content string
}

// Response is the provider-neutral completion result. Cache token counts are
// carried explicitly: vendor "input tokens" exclude cache reads/writes, and the
// cache-aware routing wedge (Invariant #3) needs them both to price correctly
// and to observe cache warmth.
type Response struct {
	Content               string
	InputTokens           int
	OutputTokens          int
	CacheReadInputTokens  int
	CacheWriteInputTokens int
	FinishReason          string
}

// Error is a normalized upstream failure. StatusCode is the vendor HTTP status
// (0 for transport-level failures), so the server can preserve error provenance
// to the client (a 400 stays a 400, a 429 stays a 429) instead of laundering
// everything into 502.
type Error struct {
	StatusCode int
	Type       string
	Message    string
	// RetryAfter is the raw Retry-After header value when the vendor sent one.
	RetryAfter string
}

func (e *Error) Error() string { return e.Message }

// Provider is a vendor adapter. Implementations live in this package and are the
// only code permitted to speak a vendor's protocol.
type Provider interface {
	// Name is the stable identifier used in config and logs (e.g. "anthropic").
	Name() string
	// ChatCompletion sends one completion request and returns the normalized
	// result. Streaming is a later phase. Upstream HTTP failures are returned
	// as *Error.
	ChatCompletion(ctx context.Context, req *Request) (*Response, error)
}
