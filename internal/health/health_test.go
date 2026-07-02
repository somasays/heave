package health

import (
	"sync"
	"testing"
	"time"
)

func TestConcurrentRecordsRaceClean(t *testing.T) {
	tr := New(3, time.Minute, nil)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.RecordFailure("p")
			_ = tr.Healthy("p")
			tr.RecordSuccess("p")
		}()
	}
	wg.Wait()
}

func TestOpensAfterThresholdAndRecovers(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	tr := New(3, 30*time.Second, clock)

	if !tr.Healthy("p") {
		t.Fatal("fresh provider should be healthy")
	}
	tr.RecordFailure("p")
	tr.RecordFailure("p")
	if !tr.Healthy("p") {
		t.Fatal("2 failures < threshold: still healthy")
	}
	tr.RecordFailure("p") // 3rd → open
	if tr.Healthy("p") {
		t.Fatal("should be open after threshold failures")
	}
	// Still open before cooldown elapses.
	now = now.Add(29 * time.Second)
	if tr.Healthy("p") {
		t.Fatal("should still be open before cooldown")
	}
	// Healthy again after cooldown (probe allowed).
	now = now.Add(2 * time.Second)
	if !tr.Healthy("p") {
		t.Fatal("should be healthy after cooldown")
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tr := New(2, time.Minute, func() time.Time { return now })
	tr.RecordFailure("p")
	tr.RecordSuccess("p") // reset
	tr.RecordFailure("p") // count is 1 again, not 2
	if !tr.Healthy("p") {
		t.Fatal("success should have reset the failure count")
	}
}
