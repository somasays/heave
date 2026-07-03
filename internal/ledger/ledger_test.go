package ledger

import (
	"io"
	"log/slog"
	"math"
	"strconv"
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

func discardLedger() *Ledger {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRecordAggregatesByUserAndRun(t *testing.T) {
	l := discardLedger()
	l.Record(Record{User: "team-a", RunID: "r1", OutputTokens: 100, CostUSD: 1.0, Status: "ok"})
	l.Record(Record{User: "team-a", RunID: "r1", OutputTokens: 50, CostUSD: 0.5, Status: "ok"})
	l.Record(Record{User: "team-b", OutputTokens: 10, CostUSD: 0.1, Status: "ok"}) // no run id

	snap := l.Snapshot(10)
	if snap.Total.Requests != 3 || math.Abs(snap.Total.CostUSD-1.6) > 1e-9 {
		t.Fatalf("total wrong: %+v", snap.Total)
	}
	// Top user is team-a (1.5) then team-b (0.1).
	if len(snap.TopUsers) != 2 || snap.TopUsers[0].Name != "team-a" || snap.TopUsers[0].Requests != 2 {
		t.Fatalf("top users wrong: %+v", snap.TopUsers)
	}
	// Only run-tagged traffic appears in by-run (team-b's untagged request is excluded).
	if len(snap.TopRuns) != 1 || snap.TopRuns[0].Name != "r1" || math.Abs(snap.TopRuns[0].CostUSD-1.5) > 1e-9 {
		t.Fatalf("top runs wrong: %+v", snap.TopRuns)
	}
}

func TestSnapshotTopNSortedByCost(t *testing.T) {
	l := discardLedger()
	l.Record(Record{User: "low", CostUSD: 1})
	l.Record(Record{User: "high", CostUSD: 9})
	l.Record(Record{User: "mid", CostUSD: 5})
	top := l.Snapshot(2).TopUsers
	if len(top) != 2 || top[0].Name != "high" || top[1].Name != "mid" {
		t.Fatalf("top-2 by cost wrong: %+v", top)
	}
}

func TestRecentRingNewestFirstAndBounded(t *testing.T) {
	l := discardLedger()
	for i := 0; i < recentRing+50; i++ {
		l.Record(Record{RequestID: string(rune('A')), User: "u", Status: "ok", CostUSD: float64(i)})
	}
	rec := l.Snapshot(10).Recent
	if len(rec) != recentRing {
		t.Fatalf("recent must be bounded to %d, got %d", recentRing, len(rec))
	}
	// Newest first: the last recorded cost was recentRing+49.
	if rec[0].CostUSD != float64(recentRing+49) {
		t.Fatalf("recent must be newest-first, got head cost %v", rec[0].CostUSD)
	}
}

func TestOverflowBucketBoundsMapAndReconciles(t *testing.T) {
	l := discardLedger()
	n := maxTracked + 2
	for i := 0; i < n; i++ {
		l.Record(Record{User: "u" + strconv.Itoa(i), CostUSD: 1})
	}
	l.mu.Lock()
	tracked := len(l.byUser)
	l.mu.Unlock()
	if tracked > maxTracked+1 { // +1 for the overflow bucket
		t.Fatalf("by-user map must stay bounded, got %d entries", tracked)
	}
	if _, ok := l.byUser[overflowKey]; !ok {
		t.Fatal("overflow beyond the cap must fold into the overflow bucket")
	}
	// The grand total is always exact regardless of per-dimension capping —
	// requests, tokens, AND cost.
	tot := l.Snapshot(0).Total
	if tot.Requests != int64(n) {
		t.Fatalf("total requests must count every record, got %d", tot.Requests)
	}
	if math.Abs(tot.CostUSD-float64(n)) > 1e-9 {
		t.Fatalf("total cost must reconcile despite capping, got %v want %d", tot.CostUSD, n)
	}
}

func TestRecentPartialFillNewestFirst(t *testing.T) {
	l := discardLedger()
	for i := 0; i < 3; i++ { // fewer than recentRing → partial-fill branch
		l.Record(Record{User: "u", CostUSD: float64(i)})
	}
	rec := l.Snapshot(10).Recent
	if len(rec) != 3 || rec[0].CostUSD != 2 || rec[2].CostUSD != 0 {
		t.Fatalf("partial ring must be newest-first, got %+v", rec)
	}
}

// TestSnapshotConcurrentWithRecord runs Record and Snapshot concurrently; it must
// be race-free (run under -race) — both operate under the ledger lock.
func TestSnapshotConcurrentWithRecord(t *testing.T) {
	l := discardLedger()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			l.Record(Record{User: "u" + strconv.Itoa(i%50), RunID: "r" + strconv.Itoa(i%20), CostUSD: 0.001, OutputTokens: 1})
		}
		close(done)
	}()
	for {
		select {
		case <-done:
			return
		default:
			_ = l.Snapshot(10) // read while writes are in flight
		}
	}
}
