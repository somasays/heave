package cachebench

import (
	"testing"
	"time"
)

func TestCacheAwareBeatsNaiveOnDefaultWorkload(t *testing.T) {
	sc := Generate(42, DefaultParams(), DefaultModels())
	cmp := Compare(sc)

	t.Logf("naive:       $%.2f  warm=%.0f%%  regret=%.0f%%", cmp.Naive.CostUSD, cmp.Naive.WarmRate*100, cmp.Naive.RegretRate*100)
	t.Logf("cache-aware: $%.2f  warm=%.0f%%  regret=%.0f%%", cmp.CacheAware.CostUSD, cmp.CacheAware.WarmRate*100, cmp.CacheAware.RegretRate*100)
	t.Logf("savings:     %.1f%%   loss on %d/%d convs (worst $%.4f)",
		cmp.SavingsPct, cmp.LossConversations, cmp.TotalConversations, cmp.WorstLossUSD)

	if cmp.CacheAware.CostUSD >= cmp.Naive.CostUSD {
		t.Fatalf("cache-aware should be cheaper in aggregate: naive=$%.2f cache=$%.2f", cmp.Naive.CostUSD, cmp.CacheAware.CostUSD)
	}
	if cmp.CacheAware.WarmRate <= cmp.Naive.WarmRate {
		t.Fatalf("cache-aware should warm more turns: naive=%.2f cache=%.2f", cmp.Naive.WarmRate, cmp.CacheAware.WarmRate)
	}
	// Honesty: naive picks per-turn-ideal so its regret is 0 by construction;
	// cache-aware trades real quality (regret > 0) for the savings.
	if cmp.Naive.RegretRate != 0 {
		t.Fatalf("naive is per-turn-ideal, regret must be 0, got %.2f", cmp.Naive.RegretRate)
	}
	if cmp.CacheAware.RegretRate <= 0 {
		t.Fatalf("cache-aware must show a real quality trade-off (regret > 0), got %.2f", cmp.CacheAware.RegretRate)
	}
	// Honesty: the aggregate win must hide a real tail where cache-aware loses.
	if cmp.LossConversations == 0 {
		t.Fatal("expected a non-empty tail of conversations where cache-aware costs more")
	}
}

func TestSingleTurnHasNoCacheBenefit(t *testing.T) {
	p := DefaultParams()
	p.MinTurns, p.MaxTurns = 1, 1
	sc := Generate(7, p, DefaultModels())
	if r := Simulate(sc, CacheAwareRouter{}); r.WarmTurns != 0 {
		t.Fatalf("single-turn convs cannot warm the cache, got %d", r.WarmTurns)
	}
}

func TestPartialHitOnSwitchBackIsWarm(t *testing.T) {
	// A→B→A within TTL: the return to A must be a (partial) warm hit, not a full
	// cold read — the fidelity correction from the cost-model review.
	sc := Scenario{
		Models: []Model{{Name: "a", InputPerMTok: 1, OutputPerMTok: 5, Quality: 1}}, // single model unused for switch; build manual 2-model
		TTL:    5 * time.Minute,
	}
	sc.Models = []Model{
		{Name: "a", InputPerMTok: 5, OutputPerMTok: 25, Quality: 1.0},
		{Name: "b", InputPerMTok: 1, OutputPerMTok: 5, Quality: 0.5},
	}
	// difficulties that make naive pick a(t0), b(t1), a(t2)
	sc.Conversations = []Conversation{{
		SystemTokens: 3000,
		Turns: []Turn{
			{UserTokens: 200, OutputTokens: 200, Difficulty: 0.99},
			{UserTokens: 200, OutputTokens: 200, Difficulty: 0.0, GapSeconds: 10},
			{UserTokens: 200, OutputTokens: 200, Difficulty: 0.99, GapSeconds: 10},
		},
	}}
	r := Simulate(sc, NaiveRouter{})
	if r.WarmTurns == 0 {
		t.Fatal("switching back to a model within TTL must yield a partial warm hit")
	}
}

func TestBelowMinCacheFloorNoDiscount(t *testing.T) {
	sc := Scenario{
		Models: []Model{{Name: "opus", InputPerMTok: 5, OutputPerMTok: 25, Quality: 1, MinCacheTokens: 4096}},
		TTL:    5 * time.Minute,
		Conversations: []Conversation{{
			SystemTokens: 500, // well below the 4096 floor
			Turns: []Turn{
				{UserTokens: 100, OutputTokens: 100, Difficulty: 0.9},
				{UserTokens: 100, OutputTokens: 100, Difficulty: 0.9, GapSeconds: 10},
			},
		}},
	}
	if r := Simulate(sc, CacheAwareRouter{}); r.WarmTurns != 0 {
		t.Fatalf("prompts below the min cacheable size must not warm, got %d", r.WarmTurns)
	}
}

func TestEmptyModelsNoPanic(t *testing.T) {
	sc := Scenario{Conversations: []Conversation{{SystemTokens: 100, Turns: []Turn{{UserTokens: 10, OutputTokens: 10}}}}}
	if r := Simulate(sc, NaiveRouter{}); r.Turns != 0 || r.CostUSD != 0 {
		t.Fatalf("empty model pool must be a safe no-op, got %+v", r)
	}
}

func TestDeterministic(t *testing.T) {
	a := Compare(Generate(1, DefaultParams(), DefaultModels()))
	b := Compare(Generate(1, DefaultParams(), DefaultModels()))
	if a.Naive.CostUSD != b.Naive.CostUSD || a.CacheAware.CostUSD != b.CacheAware.CostUSD {
		t.Fatalf("same seed must reproduce: %+v vs %+v", a, b)
	}
}
