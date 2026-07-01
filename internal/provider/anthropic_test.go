package provider

import "testing"

func TestToAnthropicMessagesCoalesces(t *testing.T) {
	msgs, err := toAnthropicMessages([]Message{
		{Role: "user", Content: "a"},
		{Role: "user", Content: "b"},      // consecutive user -> merged
		{Role: "assistant", Content: ""},  // empty -> dropped
		{Role: "assistant", Content: "c"}, // single assistant
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 coalesced messages, got %d", len(msgs))
	}
}

func TestToAnthropicMessagesEmptyErrors(t *testing.T) {
	if _, err := toAnthropicMessages([]Message{{Role: "user", Content: ""}}); err == nil {
		t.Fatal("expected error for all-empty messages")
	}
}

func TestToAnthropicMessagesFirstMustBeUser(t *testing.T) {
	if _, err := toAnthropicMessages([]Message{{Role: "assistant", Content: "hi"}}); err == nil {
		t.Fatal("expected error when first message is assistant")
	}
}

func TestStatusToType(t *testing.T) {
	if statusToType(429) != "rate_limit_error" || statusToType(400) != "invalid_request_error" {
		t.Fatal("wrong error type mapping")
	}
}
