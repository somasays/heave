// Package openai defines the gateway's canonical wire format: the subset of the
// OpenAI Chat Completions API that clients speak to the gateway. This is the
// ONE ingress format (see docs/INVARIANTS.md, Invariant #1). Provider-specific
// shapes never appear here — adapters translate to/from this format.
package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ChatCompletionRequest is the request body for POST /v1/chat/completions.
//
// Capability fields the gateway does not yet honor (Tools, Functions, N, ...)
// are decoded as raw JSON so the server can REJECT requests that use them with
// a clear error, rather than silently dropping them (a wrong-behavior trap).
type ChatCompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	N           *int      `json:"n,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	// User is an opaque caller identifier used for spend attribution.
	User string `json:"user,omitempty"`

	// Unsupported-in-Phase-0 capabilities, captured only to detect presence.
	Tools          json.RawMessage `json:"tools,omitempty"`
	Functions      json.RawMessage `json:"functions,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
}

// Message is a single chat message. Content accepts either a plain string or the
// OpenAI content-parts array (used for vision and emitted by many client SDKs
// even for text); see MessageContent.
type Message struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
	// Name is accepted but not yet forwarded (multi-speaker / tool names).
	Name string `json:"name,omitempty"`
}

// MessageContent holds message text, flattened from whichever wire form the
// client sent: a string, null, or an array of typed parts. Non-text parts
// (images) are flagged so the server can reject them explicitly.
type MessageContent struct {
	Text     string
	HasImage bool
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// UnmarshalJSON accepts a JSON string, null, or an array of content parts.
func (c *MessageContent) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		c.Text = ""
		return nil
	}
	switch b[0] {
	case '"':
		return json.Unmarshal(b, &c.Text)
	case '[':
		var parts []contentPart
		if err := json.Unmarshal(b, &parts); err != nil {
			return fmt.Errorf("content parts: %w", err)
		}
		var buf bytes.Buffer
		for _, p := range parts {
			switch p.Type {
			case "text", "input_text", "":
				buf.WriteString(p.Text)
			default:
				c.HasImage = true // image_url, input_image, etc.
			}
		}
		c.Text = buf.String()
		return nil
	default:
		return fmt.Errorf("content must be a string or an array of parts")
	}
}

// MarshalJSON emits content as a plain string (the gateway's responses are
// single-text), keeping the response shape OpenAI clients expect.
func (c MessageContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.Text)
}

// ChatCompletionResponse is the response body, matching OpenAI's shape so
// existing OpenAI clients work unmodified.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is one completion candidate.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage reports token counts for a completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionChunk is one SSE event in a streaming response
// (`object: chat.completion.chunk`), matching OpenAI's streaming shape.
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	// Usage is populated only on the final chunk (stream_options.include_usage).
	Usage *Usage `json:"usage,omitempty"`
}

// ChunkChoice is one streamed choice carrying an incremental delta. FinishReason
// is a pointer so it serializes as JSON null on content chunks (what OpenAI
// sends) and as the reason string only on the terminal chunk.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Delta is the incremental content of a streamed chunk.
type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// ErrorResponse is the OpenAI-compatible error envelope.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries a single error's detail.
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}
