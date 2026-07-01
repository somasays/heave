package provider

import (
	"context"
	"errors"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Anthropic adapts the gateway to the Anthropic Messages API via the official
// Go SDK. Per the project's Claude-API guidance, vendor calls use the official
// SDK rather than hand-rolled HTTP.
type Anthropic struct {
	name   string
	client anthropic.Client
}

// defaultMaxTokens is used only when neither the client nor the model config
// supplies a limit; the server normally passes a per-model default.
const defaultMaxTokens = 4096

// NewAnthropic builds an Anthropic adapter. apiKey is read from config (which
// sources it from the environment) — keys never appear in code (Invariant #4).
func NewAnthropic(name, apiKey, baseURL string) *Anthropic {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &Anthropic{name: name, client: anthropic.NewClient(opts...)}
}

// Name returns the adapter's stable identifier.
func (a *Anthropic) Name() string { return a.name }

// ChatCompletion translates the neutral request into an Anthropic Messages
// request and normalizes the response back. The gateway is a passthrough: it
// does not inject thinking or sampling defaults the client did not ask for, but
// it DOES normalize message structure to satisfy Anthropic's constraints
// (non-empty, alternating roles, no empty text blocks).
func (a *Anthropic) ChatCompletion(ctx context.Context, req *Request) (*Response, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	msgs, err := toAnthropicMessages(req.Messages)
	if err != nil {
		return nil, &Error{StatusCode: 400, Type: "invalid_request_error", Message: err.Error()}
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(maxTokens),
		Messages:  msgs,
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	if req.Temperature != nil {
		params.Temperature = anthropic.Float(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = anthropic.Float(*req.TopP)
	}

	resp, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return nil, mapAnthropicError(err)
	}

	var text string
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			text += tb.Text
		}
	}

	return &Response{
		Content:               text,
		InputTokens:           int(resp.Usage.InputTokens),
		OutputTokens:          int(resp.Usage.OutputTokens),
		CacheReadInputTokens:  int(resp.Usage.CacheReadInputTokens),
		CacheWriteInputTokens: int(resp.Usage.CacheCreationInputTokens),
		FinishReason:          mapAnthropicStop(string(resp.StopReason)),
	}, nil
}

// toAnthropicMessages coalesces consecutive same-role turns, drops empty
// content, and requires the first turn to be a user turn — Anthropic rejects
// requests that violate any of these.
func toAnthropicMessages(in []Message) ([]anthropic.MessageParam, error) {
	// Merge consecutive same-role messages and skip empty content.
	type turn struct {
		role string
		text string
	}
	var turns []turn
	for _, m := range in {
		if m.Content == "" {
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		if len(turns) > 0 && turns[len(turns)-1].role == role {
			turns[len(turns)-1].text += "\n\n" + m.Content
			continue
		}
		turns = append(turns, turn{role: role, text: m.Content})
	}

	if len(turns) == 0 {
		return nil, errors.New("at least one non-empty user or assistant message is required")
	}
	if turns[0].role != "user" {
		return nil, errors.New("the first message must be a user message")
	}

	out := make([]anthropic.MessageParam, 0, len(turns))
	for _, t := range turns {
		if t.role == "assistant" {
			out = append(out, anthropic.NewAssistantMessage(anthropic.NewTextBlock(t.text)))
		} else {
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(t.text)))
		}
	}
	return out, nil
}

// mapAnthropicError converts an SDK error into a normalized provider.Error,
// preserving the upstream HTTP status so the server can pass provenance through.
func mapAnthropicError(err error) error {
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		return &Error{
			StatusCode: apierr.StatusCode,
			Type:       statusToType(apierr.StatusCode),
			Message:    apierr.Error(),
		}
	}
	// Transport-level failure (timeout, connection reset, ...): StatusCode 0.
	return &Error{StatusCode: 0, Type: "api_error", Message: err.Error()}
}

// statusToType maps an HTTP status to the OpenAI-style error type vocabulary.
func statusToType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// mapAnthropicStop converts an Anthropic stop reason to the OpenAI vocabulary.
func mapAnthropicStop(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}
