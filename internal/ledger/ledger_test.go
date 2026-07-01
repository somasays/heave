package ledger

import (
	"math"
	"testing"
)

func TestCost(t *testing.T) {
	// 1M input @ $3, 0.5M output @ $15 => 3 + 7.5 = 10.5
	got := Cost(1_000_000, 500_000, 0, 0, 3.0, 15.0)
	if math.Abs(got-10.5) > 1e-9 {
		t.Fatalf("expected 10.5, got %v", got)
	}
}

func TestCostWithCacheTokens(t *testing.T) {
	// input 3 + output 7.5 + cache read 1M*0.1*3 = 0.3 + cache write 1M*1.25*3 = 3.75
	got := Cost(1_000_000, 500_000, 1_000_000, 1_000_000, 3.0, 15.0)
	want := 10.5 + 0.3 + 3.75
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestCostZero(t *testing.T) {
	if got := Cost(0, 0, 0, 0, 3, 15); got != 0 {
		t.Fatalf("expected 0, got %v", got)
	}
}
