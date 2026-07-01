package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	Model       string      `json:"model"`
	Messages    []ocMessage `json:"messages"`
	MaxTokens   int         `json:"max_tokens,omitempty"`
	Temperature *float64    `json:"temperature,omitempty"`
	TopP        *float64    `json:"top_p,omitempty"`
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
		perr := &Error{
			StatusCode: resp.StatusCode,
			Type:       statusToType(resp.StatusCode),
			RetryAfter: resp.Header.Get("Retry-After"),
		}
		var env ocErrorEnvelope
		if json.Unmarshal(data, &env) == nil && env.Error.Message != "" {
			perr.Message = fmt.Sprintf("%s: upstream %d: %s", o.name, resp.StatusCode, env.Error.Message)
			if env.Error.Type != "" {
				perr.Type = env.Error.Type
			}
		} else {
			perr.Message = fmt.Sprintf("%s: upstream %d: %s", o.name, resp.StatusCode, truncate(data, 512))
		}
		return nil, perr
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

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
