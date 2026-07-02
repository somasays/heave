// Command cachebench runs the cache-aware-routing cost simulation and prints a
// naive-vs-cache-aware comparison. It is the SPIKE harness for the project's
// wedge — a deterministic lab result, not a live benchmark. See docs/BENCHMARK.md.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/somasays/heave/internal/cachebench"
)

func main() {
	seed := flag.Int64("seed", 42, "random seed for scenario generation")
	convs := flag.Int("conversations", 0, "number of conversations (0 = default)")
	flag.Parse()

	p := cachebench.DefaultParams()
	if *convs > 0 {
		p.Conversations = *convs
	}
	sc := cachebench.Generate(*seed, p, cachebench.DefaultModels())
	cmp := cachebench.Compare(sc)

	fmt.Printf("cache-aware routing benchmark (seed=%d, conversations=%d, stickiness=%.2f)\n\n",
		*seed, p.Conversations, p.DifficultyStickiness)
	fmt.Printf("%-14s %12s %10s %10s\n", "router", "cost (USD)", "warm %", "regret %")
	fmt.Printf("%-14s %12s %10s %10s\n", "------", "----------", "------", "--------")
	row := func(r cachebench.Result) {
		fmt.Printf("%-14s %12.2f %9.0f%% %9.0f%%\n", r.Router, r.CostUSD, r.WarmRate*100, r.RegretRate*100)
	}
	row(cmp.Naive)
	row(cmp.CacheAware)
	fmt.Printf("\ncache-aware routing is %.1f%% cheaper on this workload.\n", cmp.SavingsPct)
	fmt.Printf("trade-offs: cache-aware serves %.0f%% of turns on a less-capable model, and\n", cmp.CacheAware.RegretRate*100)
	fmt.Printf("            costs MORE than naive on %d of %d conversations (worst case +$%.4f).\n",
		cmp.LossConversations, cmp.TotalConversations, cmp.WorstLossUSD)

	if cmp.CacheAware.CostUSD >= cmp.Naive.CostUSD {
		fmt.Fprintln(os.Stderr, "WARNING: cache-aware was not cheaper on this workload")
		os.Exit(1)
	}
}
