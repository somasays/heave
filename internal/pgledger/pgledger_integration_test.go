//go:build integration

// Integration test for the real Postgres path. NOT part of the hermetic `make
// check` gate: it needs a live database and is opt-in via the `integration` build
// tag + HEAVE_TEST_DATABASE_URL. Run against the compose `state` profile, e.g.:
//
//	docker compose --profile state up -d postgres
//	HEAVE_TEST_DATABASE_URL=postgres://gateway:gateway@localhost:5432/gateway \
//	  go test -tags integration ./internal/pgledger/
package pgledger

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/somasays/heave/internal/ledger"
)

func TestIntegrationDurablePersist(t *testing.T) {
	url := os.Getenv("HEAVE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("HEAVE_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s, err := New(ctx, url) // connects + ensures schema
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.pool.Exec(ctx, "TRUNCATE spend"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	const n = 5
	for i := 0; i < n; i++ {
		s.Write(ledger.Record{
			RequestID: "int", Alias: "m", Provider: "p", User: "team", RunID: "r1",
			InputTokens: 10, OutputTokens: 20, CostUSD: 0.02, LatencyMS: 12, Status: "ok",
		})
	}
	if err := s.Close(); err != nil { // flushes the buffer, then closes the pool
		t.Fatalf("Close: %v", err)
	}

	// Reconnect (Close closed the pool) and verify the rows landed.
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("verify connect: %v", err)
	}
	defer pool.Close()
	var got int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM spend WHERE run_id = $1", "r1").Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != n {
		t.Fatalf("durable ledger persisted %d rows, want %d", got, n)
	}
	// Verify COLUMN MAPPING (a transposition bug would pass a count-only check):
	// assert the persisted values equal what was written.
	var (
		client, status string
		inTok, outTok  int
		cost           float64
	)
	if err := pool.QueryRow(ctx,
		"SELECT client, input_tokens, output_tokens, cost_usd, status FROM spend WHERE run_id=$1 LIMIT 1", "r1",
	).Scan(&client, &inTok, &outTok, &cost, &status); err != nil {
		t.Fatalf("column read: %v", err)
	}
	if client != "team" || inTok != 10 || outTok != 20 || cost != 0.02 || status != "ok" {
		t.Fatalf("column mapping wrong: client=%q in=%d out=%d cost=%v status=%q", client, inTok, outTok, cost, status)
	}
	if s.Dropped() != 0 {
		t.Fatalf("no records should have dropped, got %d", s.Dropped())
	}
}
