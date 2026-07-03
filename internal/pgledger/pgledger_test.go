package pgledger

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/somasays/heave/internal/ledger"
)

// recorder is a flush target that captures batches.
type recorder struct {
	mu  sync.Mutex
	got []ledger.Record
}

func (r *recorder) flush(b []ledger.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, b...)
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func TestBatchesAndFlushesOnClose(t *testing.T) {
	rec := &recorder{}
	s := newStore(rec.flush, 100)
	go s.loop(10, time.Hour) // big interval: only a full batch or Close flushes
	s.Write(ledger.Record{RequestID: "a"})
	s.Write(ledger.Record{RequestID: "b"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 2 {
		t.Fatalf("Close must flush the buffered remainder, got %d", rec.count())
	}
}

func TestFlushesOnFullBatch(t *testing.T) {
	flushed := make(chan int, 8)
	s := newStore(func(b []ledger.Record) error { flushed <- len(b); return nil }, 100)
	go s.loop(3, time.Hour)
	for i := 0; i < 3; i++ {
		s.Write(ledger.Record{})
	}
	select {
	case n := <-flushed:
		if n != 3 {
			t.Fatalf("full batch must flush 3, got %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a full batch must flush without waiting for the timer")
	}
	_ = s.Close()
}

func TestDropsWhenBufferFull(t *testing.T) {
	entered := make(chan struct{}, 8)
	block := make(chan struct{})
	s := newStore(func(b []ledger.Record) error { entered <- struct{}{}; <-block; return nil }, 2)
	go s.loop(1, time.Hour)

	s.Write(ledger.Record{RequestID: "a"}) // consumed → flush blocks
	<-entered                              // loop is now parked in flush; buffer empty
	s.Write(ledger.Record{RequestID: "b"}) // buffer 1/2
	s.Write(ledger.Record{RequestID: "c"}) // buffer 2/2 (full)
	s.Write(ledger.Record{RequestID: "d"}) // dropped
	s.Write(ledger.Record{RequestID: "e"}) // dropped
	if s.Dropped() != 2 {
		t.Fatalf("exactly the 2 writes past the full buffer must drop, got %d", s.Dropped())
	}
	close(block)
	_ = s.Close()
}

func TestTickerFlushesPartialBatch(t *testing.T) {
	flushed := make(chan int, 4)
	s := newStore(func(b []ledger.Record) error { flushed <- len(b); return nil }, 100)
	go s.loop(100, 20*time.Millisecond) // batch too big to fill: only the ticker flushes
	s.Write(ledger.Record{})
	select {
	case n := <-flushed:
		if n != 1 {
			t.Fatalf("ticker must flush the partial batch, got %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the ticker must flush a partial batch without a full batch or Close")
	}
	_ = s.Close()
}

func TestFailedFlushIsCounted(t *testing.T) {
	s := newStore(func(b []ledger.Record) error { return errors.New("db down") }, 100)
	go s.loop(2, time.Hour)
	s.Write(ledger.Record{})
	s.Write(ledger.Record{}) // full batch → flush → error
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.Dropped() != 2 {
		t.Fatalf("a failed flush must count the lost records, got %d", s.Dropped())
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s := newStore((&recorder{}).flush, 10)
	go s.loop(5, time.Hour)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A second Close must not panic on a re-closed channel.
	_ = s.Close()
}

func TestWriteAfterCloseIsDroppedNotPanic(t *testing.T) {
	s := newStore((&recorder{}).flush, 10)
	go s.loop(5, time.Hour)
	_ = s.Close()
	s.Write(ledger.Record{}) // must NOT panic (send on closed channel) — dropped
	if s.Dropped() == 0 {
		t.Fatal("a write after close must be counted as dropped")
	}
}

func TestConcurrentWriteAndClose(t *testing.T) {
	s := newStore((&recorder{}).flush, 100)
	go s.loop(10, time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				s.Write(ledger.Record{}) // writers race Close; must never panic
			}
		}()
	}
	time.Sleep(time.Millisecond)
	_ = s.Close()
	wg.Wait()
}
