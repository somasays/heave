package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxRespBytes bounds how much of an upstream response we buffer, so a
// misbehaving or hostile upstream cannot OOM the gateway.
const maxRespBytes = 16 << 20 // 16 MiB

// OpenAICompat adapts the gateway to any OpenAI-compatible Chat Completions
// endpoint (OpenAI itself, OpenRouter, Together, local runtimes, ...). Because
// the gateway's own wire format is OpenAI-shaped, this adapter is a thin,
// authenticated relay. It is the only place outside this package permitted to
// call these vendors directly.
type OpenAICompat struct {
	name    string
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewOpenAICompat builds an OpenAI-compatible adapter. baseURL should include
// the version prefix, e.g. https://api.openai.com/v1. The request-level deadline
// is owned by the caller's context (the server imposes it); the client timeout
// here is only a generous transport backstop.
func NewOpenAICompat(name, apiKey, baseURL string) *OpenAICompat {
	return &OpenAICompat{
		name:    name,
		apiKey:  apiKey,
		baseURL: baseURL,
		http:    &http.Client{},
	}
}

// Name returns the adapter's stable identifier.
func (o *OpenAICompat) Name() string { return o.name }

type ocMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ocRequest struct {
	Model         string           `json:"model"`
	Messages      []ocMessage      `json:"messages"`
	MaxTokens     int              `json:"max_tokens,omitempty"`
	Temperature   *float64         `json:"temperature,omitempty"`
	TopP          *float64         `json:"top_p,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	StreamOptions *ocStreamOptions `json:"stream_options,omitempty"`
}

type ocStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ocChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	// Error is a mid-stream error object some backends emit instead of choices.
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type ocResponse struct {
	Choices []struct {
		Message      ocMessage `json:"message"`
		FinishReason string    `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type ocErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// ChatCompletion relays the request upstream and normalizes the response.
func (o *OpenAICompat) ChatCompletion(ctx context.Context, req *Request) (*Response, error) {
	msgs := make([]ocMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, ocMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, ocMessage(m))
	}

	body, err := json.Marshal(ocRequest{
		Model:       req.Model,
		Messages:    msgs,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	})
	if err != nil {
		return nil, &Error{Type: "api_error", Message: o.name + ": marshal: " + err.Error()}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Type: "api_error", Message: o.name + ": new request: " + err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.http.Do(httpReq)
	if err != nil {
		// Transport-level (includes context deadline/cancel): StatusCode 0 so
		// the server can classify timeouts as 504.
		return nil, &Error{StatusCode: 0, Type: "api_error", Message: o.name + ": " + err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, &Error{StatusCode: 0, Type: "api_error", Message: o.name + ": read body: " + err.Error()}
	}

	if resp.StatusCode >= 400 {
		return nil, o.httpError(resp, data)
	}

	var parsed ocResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, &Error{StatusCode: 0, Type: "api_error", Message: o.name + ": decode: " + err.Error()}
	}
	if len(parsed.Choices) == 0 {
		return nil, &Error{StatusCode: 502, Type: "api_error", Message: o.name + ": upstream returned no choices"}
	}

	out := &Response{
		Content:      parsed.Choices[0].Message.Content,
		FinishReason: parsed.Choices[0].FinishReason,
	}
	// Some OpenAI-compatible backends omit usage; cost is then unknown rather
	// than free. Phase 3 will flag/estimate this; for now tokens stay zero.
	if parsed.Usage != nil {
		out.InputTokens = parsed.Usage.PromptTokens
		out.OutputTokens = parsed.Usage.CompletionTokens
	}
	return out, nil
}

// ChatCompletionStream relays a streaming request and parses the upstream SSE,
// invoking onDelta per content delta and returning the final usage.
func (o *OpenAICompat) ChatCompletionStream(ctx context.Context, req *Request, onDelta StreamFunc) (*Response, error) {
	msgs := make([]ocMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, ocMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, ocMessage(m))
	}
	body, err := json.Marshal(ocRequest{
		Model: req.Model, Messages: msgs, MaxTokens: req.MaxTokens,
		Temperature: req.Temperature, TopP: req.TopP,
		Stream: true, StreamOptions: &ocStreamOptions{IncludeUsage: true},
	})
	if err != nil {
		return nil, &Error{Type: "api_error", Message: o.name + ": marshal: " + err.Error()}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Type: "api_error", Message: o.name + ": new request: " + err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.http.Do(httpReq)
	if err != nil {
		return nil, &Error{StatusCode: 0, Type: "api_error", Message: o.name + ": " + err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		// Error before any delta — safe to fail over.
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
		return nil, o.httpError(resp, data)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var content strings.Builder
	out := &Response{FinishReason: "stop"}
	terminated := false // saw [DONE] or a terminal finish_reason
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			terminated = true
			break
		}
		var chunk ocChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Error != nil {
			return nil, &Error{StatusCode: 502, Type: chunk.Error.Type, Message: o.name + ": upstream stream error: " + chunk.Error.Message}
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				content.WriteString(ch.Delta.Content)
				if err := onDelta(ch.Delta.Content); err != nil {
					return nil, err
				}
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				out.FinishReason = *ch.FinishReason
				terminated = true
			}
		}
		if chunk.Usage != nil {
			out.InputTokens = chunk.Usage.PromptTokens
			out.OutputTokens = chunk.Usage.CompletionTokens
		}
	}
	if err := sc.Err(); err != nil {
		return nil, &Error{StatusCode: 0, Type: "api_error", Message: o.name + ": stream: " + err.Error()}
	}
	if !terminated {
		// Clean EOF without [DONE]/finish_reason = a truncated stream; don't
		// report partial content as a successful completion.
		return nil, &Error{StatusCode: 502, Type: "api_error", Message: o.name + ": stream truncated (no completion marker)"}
	}
	out.Content = content.String()
	return out, nil
}

// httpError builds a normalized error from an upstream >=400 response body.
func (o *OpenAICompat) httpError(resp *http.Response, data []byte) *Error {
	perr := &Error{StatusCode: resp.StatusCode, Type: statusToType(resp.StatusCode), RetryAfter: resp.Header.Get("Retry-After")}
	var env ocErrorEnvelope
	if json.Unmarshal(data, &env) == nil && env.Error.Message != "" {
		perr.Message = fmt.Sprintf("%s: upstream %d: %s", o.name, resp.StatusCode, env.Error.Message)
		if env.Error.Type != "" {
			perr.Type = env.Error.Type
		}
	} else {
		perr.Message = fmt.Sprintf("%s: upstream %d: %s", o.name, resp.StatusCode, truncate(data, 512))
	}
	return perr
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
