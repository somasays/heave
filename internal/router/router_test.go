package router

import "testing"

func testRouter() *Router {
	return New([]ModelConfig{
		{Alias: "fast", Provider: "anthropic", Upstream: "claude-haiku-4-5", Price: Price{1, 5}},
		{Alias: "smart", Provider: "anthropic", Upstream: "claude-opus-4-8", Price: Price{5, 25}},
	}, "fast")
}

func TestRouteKnownAlias(t *testing.T) {
	d, err := testRouter().Route("smart")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Upstream != "claude-opus-4-8" || d.Provider != "anthropic" {
		t.Fatalf("wrong decision: %+v", d)
	}
}

func TestRouteEmptyUsesDefault(t *testing.T) {
	d, err := testRouter().Route("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Alias != "fast" {
		t.Fatalf("expected default 'fast', got %q", d.Alias)
	}
}

func TestRouteUnknownAliasErrors(t *testing.T) {
	if _, err := testRouter().Route("nope"); err == nil {
		t.Fatal("expected error for unknown alias")
	}
}
