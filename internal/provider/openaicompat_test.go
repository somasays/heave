package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAICompatSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("missing auth header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	}))
	defer srv.Close()

	a := NewOpenAICompat("openai", "k", srv.URL+"/v1")
	resp, err := a.ChatCompletion(context.Background(), &Request{Model: "gpt", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hi" || resp.InputTokens != 7 || resp.OutputTokens != 3 {
		t.Fatalf("bad resp: %+v", resp)
	}
}

func TestOpenAICompatUpstreamErrorPreservesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error"}}`))
	}))
	defer srv.Close()

	a := NewOpenAICompat("openai", "k", srv.URL+"/v1")
	_, err := a.ChatCompletion(context.Background(), &Request{Model: "gpt", Messages: []Message{{Role: "user", Content: "hi"}}})
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("want *Error, got %T", err)
	}
	if pe.StatusCode != 429 || pe.Type != "rate_limit_error" || pe.RetryAfter != "5" {
		t.Fatalf("provenance lost: %+v", pe)
	}
}

func TestOpenAICompatMissingUsageIsZeroNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	a := NewOpenAICompat("local", "k", srv.URL+"/v1")
	resp, err := a.ChatCompletion(context.Background(), &Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.InputTokens != 0 || resp.Content != "x" {
		t.Fatalf("bad resp: %+v", resp)
	}
}
