// Package cachebench is a deterministic cost simulator that quantifies
// cache-aware routing vs naive per-turn routing on synthetic multi-turn traffic.
// It is the SPIKE proving the project's wedge (docs/INVARIANTS.md #3) before the
// org-grade build. Pure (stdlib only). See docs/BENCHMARK.md for the cost model,
// its fidelity corrections, and the honesty bounds.
package cachebench

import (
	"math/rand"
	"time"
)

// Provider prefix-cache economics, relative to a model's input rate:
//   - a warm cache READ bills at ~0.10x,
//   - a cache WRITE (new tokens on a caching turn) bills at ~1.25x.
//
// A model's cache is per-model and wall-clock-TTL'd: it survives intervening
// turns served by OTHER models, and a returning turn gets a PARTIAL hit for the
// leading prefix that model already holds. Modeling this (rather than "any
// switch = full cold") is the correction that keeps the benchmark honest.
const (
	cacheReadMult  = 0.10
	cacheWriteMult = 1.25
)

// Model is a routable model: prices in USD per million tokens, a quality score
// in [0,1] (higher = more capable), and the minimum prompt size the provider
// will cache (below it, no read/write discount applies).
type Model struct {
	Name           string
	InputPerMTok   float64
	OutputPerMTok  float64
	Quality        float64
	MinCacheTokens int
}

// Turn is one exchange in a conversation.
type Turn struct {
	UserTokens   int     // new input tokens this turn
	OutputTokens int     // assistant reply tokens
	Difficulty   float64 // [0,1); drives per-turn model choice
	GapSeconds   int     // idle time since the previous turn
}

// Conversation is a system prompt plus an ordered list of turns.
type Conversation struct {
	SystemTokens int
	Turns        []Turn
}

// Scenario is the workload plus the model pool (ordered cheap → capable) and the
// prefix-cache TTL.
type Scenario struct {
	Conversations []Conversation
	Models        []Model
	TTL           time.Duration
}

// Result is the outcome of simulating one router over a scenario.
type Result struct {
	Router      string
	CostUSD     float64
	Turns       int
	WarmTurns   int // turns that got a (partial or full) cache read
	RegretTurns int // turns served by a less-capable model than per-turn-ideal
	WarmRate    float64
	RegretRate  float64
}

// Comparison is a per-conversation naive-vs-cache-aware result, exposing the
// aggregate win AND the tail of conversations where cache-aware costs more.
type Comparison struct {
	Naive              Result
	CacheAware         Result
	TotalConversations int
	LossConversations  int     // conversations where cache-aware >= naive
	WorstLossUSD       float64 // largest single-conversation loss (cache - naive)
	SavingsPct         float64
}

// Router chooses a model index for a turn. warm is the index of the model whose
// cache is currently warm for this conversation, or -1 if none.
type Router interface {
	Name() string
	Choose(turnIdx int, difficulty float64, warm int, numModels int) int
}

// NaiveRouter scores every turn independently and picks the tier that fits the
// turn's difficulty — the article's "naive approach".
type NaiveRouter struct{}

// Name identifies the router.
func (NaiveRouter) Name() string { return "naive" }

// Choose picks a model tier from the turn's difficulty, ignoring cache warmth.
func (NaiveRouter) Choose(_ int, difficulty float64, _ int, numModels int) int {
	return byDifficulty(difficulty, numModels)
}

// CacheAwareRouter keeps a conversation on its warm model and only re-selects by
// difficulty once the cache has gone cold (warm == -1).
type CacheAwareRouter struct{}

// Name identifies the router.
func (CacheAwareRouter) Name() string { return "cache-aware" }

// Choose keeps the warm model if the cache is warm, else re-selects by difficulty.
func (CacheAwareRouter) Choose(_ int, difficulty float64, warm int, numModels int) int {
	if warm >= 0 {
		return warm
	}
	return byDifficulty(difficulty, numModels)
}

// byDifficulty maps a difficulty in [0,1) to a model index (0 = cheapest).
func byDifficulty(difficulty float64, numModels int) int {
	if numModels <= 1 {
		return 0
	}
	idx := int(difficulty * float64(numModels))
	if idx >= numModels {
		idx = numModels - 1
	}
	if idx < 0 {
		idx = 0
	}
	return idx
}

type modelCache struct {
	prefixTokens int           // how much leading prefix this model holds cached
	lastSeen     time.Duration // wall-clock (cumulative) time of last use
}

// simulateConv runs one conversation and returns cost plus turn metrics. Each
// model keeps its own cached prefix with a wall-clock TTL, so switching away and
// back yields a partial hit rather than a full cold read.
func simulateConv(conv Conversation, sc Scenario, r Router) (cost float64, turns, warm, regret int) {
	n := len(sc.Models)
	caches := make(map[int]modelCache, n)
	var clock time.Duration
	prefix := conv.SystemTokens // system + all prior (user+assistant) tokens
	prevModel := -1

	for t, turn := range conv.Turns {
		clock += time.Duration(turn.GapSeconds) * time.Second

		// The router's "warm" signal: the previously-used model, if its cache is
		// still within TTL.
		warmIdx := -1
		if prevModel >= 0 {
			if c, ok := caches[prevModel]; ok && clock-c.lastSeen <= sc.TTL {
				warmIdx = prevModel
			}
		}
		choice := r.Choose(t, turn.Difficulty, warmIdx, n)
		m := sc.Models[choice]
		inRate := m.InputPerMTok / 1e6
		outRate := m.OutputPerMTok / 1e6
		prompt := prefix + turn.UserTokens

		var inputCost float64
		if prompt < m.MinCacheTokens {
			// Below the provider's minimum cacheable size: no discount, no store.
			inputCost = float64(prompt) * inRate
		} else {
			cached := 0
			if c, ok := caches[choice]; ok && clock-c.lastSeen <= sc.TTL {
				cached = c.prefixTokens
				if cached > prompt {
					cached = prompt
				}
			}
			newTokens := prompt - cached // written to cache at the write premium
			inputCost = float64(cached)*inRate*cacheReadMult + float64(newTokens)*inRate*cacheWriteMult
			// This model now holds the whole prompt + its reply as cached prefix.
			caches[choice] = modelCache{prefixTokens: prompt + turn.OutputTokens, lastSeen: clock}
			if cached > 0 {
				warm++
			}
		}
		cost += inputCost + float64(turn.OutputTokens)*outRate
		turns++
		if m.Quality < sc.Models[byDifficulty(turn.Difficulty, n)].Quality {
			regret++
		}

		prefix += turn.UserTokens + turn.OutputTokens
		prevModel = choice
	}
	return cost, turns, warm, regret
}

// Simulate runs a router over the whole scenario.
func Simulate(sc Scenario, r Router) Result {
	res := Result{Router: r.Name()}
	if len(sc.Models) == 0 {
		return res
	}
	for _, conv := range sc.Conversations {
		c, turns, warm, regret := simulateConv(conv, sc, r)
		res.CostUSD += c
		res.Turns += turns
		res.WarmTurns += warm
		res.RegretTurns += regret
	}
	if res.Turns > 0 {
		res.WarmRate = float64(res.WarmTurns) / float64(res.Turns)
		res.RegretRate = float64(res.RegretTurns) / float64(res.Turns)
	}
	return res
}

// Compare runs both routers and also tallies the per-conversation tail where
// cache-aware loses (stickiness can pin an expensive model over a cheap tail).
func Compare(sc Scenario) Comparison {
	cmp := Comparison{TotalConversations: len(sc.Conversations)}
	if len(sc.Models) == 0 {
		return cmp
	}
	naive, cache := NaiveRouter{}, CacheAwareRouter{}
	cmp.Naive = Result{Router: naive.Name()}
	cmp.CacheAware = Result{Router: cache.Name()}
	for _, conv := range sc.Conversations {
		nc, nt, nw, nr := simulateConv(conv, sc, naive)
		cc, ct, cw, cr := simulateConv(conv, sc, cache)
		cmp.Naive.CostUSD += nc
		cmp.Naive.Turns += nt
		cmp.Naive.WarmTurns += nw
		cmp.Naive.RegretTurns += nr
		cmp.CacheAware.CostUSD += cc
		cmp.CacheAware.Turns += ct
		cmp.CacheAware.WarmTurns += cw
		cmp.CacheAware.RegretTurns += cr
		if cc >= nc {
			cmp.LossConversations++
			if loss := cc - nc; loss > cmp.WorstLossUSD {
				cmp.WorstLossUSD = loss
			}
		}
	}
	finalize(&cmp.Naive)
	finalize(&cmp.CacheAware)
	cmp.SavingsPct = SavingsPct(cmp.Naive, cmp.CacheAware)
	return cmp
}

func finalize(r *Result) {
	if r.Turns > 0 {
		r.WarmRate = float64(r.WarmTurns) / float64(r.Turns)
		r.RegretRate = float64(r.RegretTurns) / float64(r.Turns)
	}
}

// Params control synthetic scenario generation.
type Params struct {
	Conversations int
	MinTurns      int
	MaxTurns      int
	SystemTokens  int
	UserTokens    int
	OutputTokens  int
	TTL           time.Duration
	// ActiveGapSeconds is the typical idle gap during an active conversation;
	// IdleBreakChance is the per-turn probability of a gap exceeding the TTL.
	ActiveGapSeconds int
	IdleBreakChance  float64
	// DifficultyStickiness in [0,1] autocorrelates per-turn difficulty. Real
	// conversations are fairly sticky (a debugging thread stays hard); the
	// default reflects that — a lower value would flatter cache-aware by making
	// the naive router thrash more.
	DifficultyStickiness float64
}

// DefaultParams is a representative mixed workload with realistic (moderately
// high) difficulty stickiness.
func DefaultParams() Params {
	return Params{
		Conversations:        500,
		MinTurns:             3,
		MaxTurns:             15,
		SystemTokens:         1200,
		UserTokens:           300,
		OutputTokens:         500,
		TTL:                  5 * time.Minute,
		ActiveGapSeconds:     20,
		IdleBreakChance:      0.08,
		DifficultyStickiness: 0.8,
	}
}

// DefaultModels is a cheap→capable pool priced from the 2026 catalog, with each
// model's minimum cacheable prefix (Opus 4.8: 4096; Sonnet/Haiku-class: 2048).
func DefaultModels() []Model {
	return []Model{
		{Name: "haiku", InputPerMTok: 1, OutputPerMTok: 5, Quality: 0.6, MinCacheTokens: 2048},
		{Name: "sonnet", InputPerMTok: 3, OutputPerMTok: 15, Quality: 0.8, MinCacheTokens: 2048},
		{Name: "opus", InputPerMTok: 5, OutputPerMTok: 25, Quality: 1.0, MinCacheTokens: 4096},
	}
}

// Generate builds a deterministic scenario from a seed and params.
func Generate(seed int64, p Params, models []Model) Scenario {
	rng := rand.New(rand.NewSource(seed))
	sc := Scenario{Models: models, TTL: p.TTL}
	for i := 0; i < p.Conversations; i++ {
		turns := p.MinTurns
		if p.MaxTurns > p.MinTurns {
			turns += rng.Intn(p.MaxTurns - p.MinTurns + 1)
		}
		conv := Conversation{SystemTokens: jitter(rng, p.SystemTokens)}
		difficulty := rng.Float64()
		for t := 0; t < turns; t++ {
			difficulty = p.DifficultyStickiness*difficulty + (1-p.DifficultyStickiness)*rng.Float64()
			gap := jitter(rng, p.ActiveGapSeconds)
			if t > 0 && rng.Float64() < p.IdleBreakChance {
				gap = int(p.TTL.Seconds()) + 60 // exceed TTL: cache cold
			}
			conv.Turns = append(conv.Turns, Turn{
				UserTokens:   jitter(rng, p.UserTokens),
				OutputTokens: jitter(rng, p.OutputTokens),
				Difficulty:   difficulty,
				GapSeconds:   gap,
			})
		}
		sc.Conversations = append(sc.Conversations, conv)
	}
	return sc
}

// jitter returns base ±25% (never below 1).
func jitter(rng *rand.Rand, base int) int {
	if base <= 0 {
		return 0
	}
	delta := int(0.25 * float64(base))
	if delta < 1 {
		delta = 1
	}
	v := base - delta + rng.Intn(2*delta+1)
	if v < 1 {
		v = 1
	}
	return v
}

// SavingsPct is the percent cost reduction of cache-aware vs naive.
func SavingsPct(naive, cacheAware Result) float64 {
	if naive.CostUSD == 0 {
		return 0
	}
	return (naive.CostUSD - cacheAware.CostUSD) / naive.CostUSD * 100
}
