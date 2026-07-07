package server

// Tests for the /v1/guard decision API (ADR 0007): reserve/settle/release enforce
// the SAME budgets as the inline path, hand back a signed stateless reservation,
// and are idempotent + tamper-evident. Estimates are caller-supplied here, so the
// hold/deny arithmetic is deterministic.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/somasays/heave/internal/controls"
	"github.com/somasays/heave/internal/enforcer"
	"github.com/somasays/heave/internal/firewall"
	"github.com/somasays/heave/internal/ledger"
	"github.com/somasays/heave/internal/policy"
	"github.com/somasays/heave/internal/provider"
	"github.com/somasays/heave/internal/redisstore"
	"github.com/somasays/heave/internal/router"
)

const tenantSHA = "tenanthash" // the tenant key the PEP asserts scope for

// newGuardEnv builds a gateway with the OOB decision API on. It uses the SUPPORTED
// config — a firewall backed by a shared store (miniredis) + a redis-backed
// reconcile dedup — because the guard API requires both (cross-replica idempotency
// + orphaned-hold reaping). org acme($1/min)▸team eng▸app bot; a tenant key → app.
func newGuardEnv(t *testing.T) (http.Handler, *policy.Store) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	shared := redisstore.NewClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), time.Hour)

	clients := []controls.Client{
		{Name: "pep", KeySHA256: sha("adminkey"), Admin: true},
		{Name: "user", KeySHA256: sha("userkey")}, // non-admin, to test gating
	}
	store := policy.New()
	must(t, store.CreateOrg("acme", "Acme", policy.Limits{MaxUSDPerMin: 1.0}))
	must(t, store.CreateTeam("eng", "E", "acme", policy.Limits{}))
	must(t, store.CreateApp("bot", "Bot", "eng", policy.Limits{}))
	must(t, store.IssueKey(tenantSHA, policy.App, "bot"))
	fw := firewall.New(true, firewall.Limits{}, nil).WithScopeStore(shared)
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "up"}}, "m")
	fp := &fakeProvider{resp: &provider.Response{Content: "ok", InputTokens: 1, OutputTokens: 1}}
	srv := newTestServer(t, Deps{
		Router: rtr, Providers: map[string]provider.Provider{"fake": fp},
		Guard: controls.New(true, clients, nil), Ledger: ledger.New(discardLog()), Firewall: fw,
		Policy: store, Resolver: enforcer.NewResolver(store),
		GuardSecret: bytes.Repeat([]byte("s"), 32), GuardDedup: shared,
	}, Options{MaxRequestBytes: 1 << 20, RequestTimeout: 2 * time.Second})
	return srv.Handler(), store
}

func reserve(t *testing.T, h http.Handler, keySHA, runID string, estUSD float64) (guardReserveResp, int) {
	t.Helper()
	body := fmt.Sprintf(`{"key_sha256":%q,"run_id":%q,"estimate":{"usd":%v,"tokens":0}}`, keySHA, runID, estUSD)
	rr := adminReq(t, h, http.MethodPost, "/v1/guard/reserve", "adminkey", body)
	var resp guardReserveResp
	if rr.Code == http.StatusOK {
		_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	}
	return resp, rr.Code
}

func settle(t *testing.T, h http.Handler, rid string, actualUSD float64) map[string]any {
	t.Helper()
	body := fmt.Sprintf(`{"reservation_id":%q,"actual":{"usd":%v,"tokens":0}}`, rid, actualUSD)
	rr := adminReq(t, h, http.MethodPost, "/v1/guard/settle", "adminkey", body)
	var m map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	m["_code"] = rr.Code
	return m
}

func TestGuardReserveHoldsAndDeniesAtBindingNode(t *testing.T) {
	h, _ := newGuardEnv(t)
	// Reserve $0.90 against the $1/min org cap → admitted and HELD.
	r1, code := reserve(t, h, tenantSHA, "run-1", 0.90)
	if code != 200 || !r1.Admitted || r1.ReservationID == "" {
		t.Fatalf("first reserve must be admitted with a token, got code=%d %+v", code, r1)
	}
	// A second reserve of $0.20 exceeds the org cap → denied, naming the org.
	r2, _ := reserve(t, h, tenantSHA, "run-2", 0.20)
	if r2.Admitted {
		t.Fatal("second reserve must be denied by the held org budget")
	}
	// Velocity denials name the scope LEVEL ("org"); node kills name the full ref
	// ("team:eng"). Both are actionable; the difference is the firewall's error shape.
	if r2.HTTPStatus != http.StatusTooManyRequests || r2.Reason != "velocity" || r2.BindingNode != "org" {
		t.Fatalf("deny verdict wrong: %+v", r2)
	}
	// Settle the first reservation down to a real $0.10 → the org window frees up.
	if m := settle(t, h, r1.ReservationID, 0.10); m["applied"] != true {
		t.Fatalf("settle must apply, got %+v", m)
	}
	if r3, _ := reserve(t, h, tenantSHA, "run-3", 0.85); !r3.Admitted { // 0.10 + 0.85 < 1.0
		t.Fatal("after settle, a fitting reserve must be admitted")
	}
}

func TestGuardKilledNodeDenied(t *testing.T) {
	h, store := newGuardEnv(t)
	must(t, store.Kill(policy.Team, "eng"))
	r, _ := reserve(t, h, tenantSHA, "run-1", 0.01)
	if r.Admitted || r.HTTPStatus != http.StatusForbidden || r.Reason != "killed" || r.BindingNode != "team:eng" {
		t.Fatalf("a killed node must deny with 403/killed naming the node, got %+v", r)
	}
}

func TestGuardSettleIsIdempotent(t *testing.T) {
	h, _ := newGuardEnv(t)
	r, _ := reserve(t, h, tenantSHA, "run-1", 0.5)
	if m := settle(t, h, r.ReservationID, 0.4); m["applied"] != true {
		t.Fatalf("first settle applies, got %+v", m)
	}
	// A retried settle must NOT double-apply.
	if m := settle(t, h, r.ReservationID, 0.4); m["applied"] != false || m["ok"] != true {
		t.Fatalf("second settle must be a no-op (applied:false), got %+v", m)
	}
}

func TestGuardSettleBlocksSubsequentRelease(t *testing.T) {
	// The nonce is shared across settle+release, so a reservation can be reconciled
	// exactly ONCE — settling it must make a later release a no-op (and vice versa).
	h, _ := newGuardEnv(t)
	r, _ := reserve(t, h, tenantSHA, "run-1", 0.5)
	if m := settle(t, h, r.ReservationID, 0.4); m["applied"] != true {
		t.Fatalf("settle must apply, got %+v", m)
	}
	body := fmt.Sprintf(`{"reservation_id":%q}`, r.ReservationID)
	rr := adminReq(t, h, http.MethodPost, "/v1/guard/release", "adminkey", body)
	var m map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	if m["applied"] != false {
		t.Fatalf("release after settle must be a no-op (applied:false), got %+v", m)
	}
}

func TestGuardNegativeActualClampedAndBadInputRejected(t *testing.T) {
	h, _ := newGuardEnv(t)
	// A negative actual must not deflate OTHER holds sharing the window slot. Hold
	// two reserves (A=0.5, B=0.4 → org window 0.9), then settle A with a huge negative
	// actual. Clamped to 0, it removes only A's own 0.5 (→ 0.4). WITHOUT the clamp the
	// delta would floor the shared slot at 0, wiping B's 0.4 too.
	rA, _ := reserve(t, h, tenantSHA, "run-a", 0.5)
	if _, code := reserve(t, h, tenantSHA, "run-b", 0.4); code != 200 {
		t.Fatalf("B reserve setup failed: %d", code)
	}
	if m := settle(t, h, rA.ReservationID, -1000); m["applied"] != true {
		t.Fatalf("settle must apply, got %+v", m)
	}
	rC, _ := reserve(t, h, tenantSHA, "run-c", 0.7) // 0.4 (B) + 0.7 > 1.0 → must deny
	if rC.Admitted {
		t.Fatal("a negative actual must be clamped so it can't wipe another hold's spend")
	}
	// Input validation: empty key_sha256 and out-of-range estimate are 400s.
	bad := adminReq(t, h, http.MethodPost, "/v1/guard/reserve", "adminkey",
		`{"key_sha256":"","run_id":"r","estimate":{"usd":0.1,"tokens":0}}`)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("empty key_sha256 must be 400, got %d", bad.Code)
	}
	huge := adminReq(t, h, http.MethodPost, "/v1/guard/reserve", "adminkey",
		fmt.Sprintf(`{"key_sha256":%q,"run_id":"r","estimate":{"usd":9e9,"tokens":0}}`, tenantSHA))
	if huge.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range estimate must be 400, got %d", huge.Code)
	}
}

func TestGuardTamperedTokenRejected(t *testing.T) {
	h, _ := newGuardEnv(t)
	r, _ := reserve(t, h, tenantSHA, "run-1", 0.5)
	// Flip the last byte of the token → HMAC fails → 400.
	bad := r.ReservationID[:len(r.ReservationID)-1] + "X"
	body := fmt.Sprintf(`{"reservation_id":%q,"actual":{"usd":0.1,"tokens":0}}`, bad)
	rr := adminReq(t, h, http.MethodPost, "/v1/guard/settle", "adminkey", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("a tampered reservation_id must be 400, got %d", rr.Code)
	}
}

func TestGuardRequiresAdmin(t *testing.T) {
	h, _ := newGuardEnv(t)
	body := `{"key_sha256":"` + tenantSHA + `","run_id":"r","estimate":{"usd":0.1,"tokens":0}}`
	if rr := adminReq(t, h, http.MethodPost, "/v1/guard/reserve", "userkey", body); rr.Code != http.StatusForbidden {
		t.Fatalf("a non-admin caller must be 403, got %d", rr.Code)
	}
	if rr := adminReq(t, h, http.MethodPost, "/v1/guard/reserve", "", body); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no key must be 401, got %d", rr.Code)
	}
}

func TestGuardReserveFailsOpenWhenRedisDown(t *testing.T) {
	// N1: with the shared store down, a reserve DEGRADES to a local hold. It must
	// fail OPEN (admit — never block traffic on an infra outage) and, per the fix,
	// hand back a no-op reservation rather than strand an unreapable local hold.
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	shared := redisstore.NewClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), time.Hour)
	clients := []controls.Client{{Name: "pep", KeySHA256: sha("adminkey"), Admin: true}}
	store := policy.New()
	must(t, store.CreateOrg("acme", "Acme", policy.Limits{MaxUSDPerMin: 1.0, MaxConcurrent: 2}))
	must(t, store.CreateTeam("eng", "E", "acme", policy.Limits{}))
	must(t, store.IssueKey(tenantSHA, policy.Team, "eng"))
	fw := firewall.New(true, firewall.Limits{}, nil).WithScopeStore(shared)
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "up"}}, "m")
	fp := &fakeProvider{resp: &provider.Response{Content: "ok"}}
	srv := newTestServer(t, Deps{
		Router: rtr, Providers: map[string]provider.Provider{"fake": fp},
		Guard: controls.New(true, clients, nil), Firewall: fw,
		Policy: store, Resolver: enforcer.NewResolver(store),
		GuardSecret: bytes.Repeat([]byte("s"), 32), GuardDedup: shared,
	}, Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	h := srv.Handler()
	mr.Close() // the outage

	r, code := reserve(t, h, tenantSHA, "run-1", 0.5)
	if code != 200 || !r.Admitted || r.ReservationID == "" {
		t.Fatalf("a reserve during a redis outage must fail OPEN (admit), got code=%d %+v", code, r)
	}
}

func TestGuardMountedOnlyWithAllPreconditions(t *testing.T) {
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "up"}}, "m")
	fp := &fakeProvider{resp: &provider.Response{Content: "ok"}}
	secret := bytes.Repeat([]byte("s"), 32)
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	shared := redisstore.NewClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), time.Hour)

	cases := []struct {
		name   string
		secret []byte
		dedup  GuardDedup
		fw     *firewall.Firewall
	}{
		{"no secret", nil, shared, firewall.New(true, firewall.Limits{}, nil).WithScopeStore(shared)},
		{"short secret", []byte("tooshort"), shared, firewall.New(true, firewall.Limits{}, nil).WithScopeStore(shared)},
		{"no dedup", secret, nil, firewall.New(true, firewall.Limits{}, nil).WithScopeStore(shared)},
		{"no shared firewall", secret, shared, firewall.New(true, firewall.Limits{}, nil)}, // local ⇒ off
	}
	for _, c := range cases {
		srv := newTestServer(t, Deps{
			Router: rtr, Providers: map[string]provider.Provider{"fake": fp}, Firewall: c.fw,
			Guard: controls.New(false, nil, nil), GuardSecret: c.secret, GuardDedup: c.dedup,
		}, Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
		rr := adminReq(t, srv.Handler(), http.MethodPost, "/v1/guard/reserve", "", `{}`)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s: guard must be unmounted (404), got %d", c.name, rr.Code)
		}
	}
}
