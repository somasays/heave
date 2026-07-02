package cache

import (
	"testing"
	"time"
)

func TestWarmWithinTTLThenCold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := New(5*time.Minute, func() time.Time { return now })

	if _, ok := s.Warm("c1"); ok {
		t.Fatal("unknown conversation must be cold")
	}
	s.Touch("c1", "opus")
	if m, ok := s.Warm("c1"); !ok || m != "opus" {
		t.Fatalf("should be warm on opus, got %q %v", m, ok)
	}
	now = now.Add(4 * time.Minute) // still within TTL
	if _, ok := s.Warm("c1"); !ok {
		t.Fatal("should still be warm before TTL")
	}
	now = now.Add(2 * time.Minute) // now 6m > 5m TTL
	if _, ok := s.Warm("c1"); ok {
		t.Fatal("should be cold after TTL lapses")
	}
}

func TestTouchRefreshesWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := New(5*time.Minute, func() time.Time { return now })
	s.Touch("c", "haiku")
	now = now.Add(4 * time.Minute)
	s.Touch("c", "haiku") // refresh
	now = now.Add(4 * time.Minute)
	if _, ok := s.Warm("c"); !ok {
		t.Fatal("touch should have refreshed the warmth window")
	}
}

func TestConversationKeyStableAndDistinct(t *testing.T) {
	a := ConversationKey("sys", []string{"hello", "hi there"})
	b := ConversationKey("sys", []string{"hello", "hi there"})
	if a != b {
		t.Fatal("same prefix must yield the same key")
	}
	c := ConversationKey("sys", []string{"hello", "different"})
	if a == c {
		t.Fatal("different prefixes must yield different keys")
	}
}
