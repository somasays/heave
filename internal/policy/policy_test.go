package policy

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

// seed builds org=acme ▸ team=eng ▸ app=bot with a key on the app.
func seed(t *testing.T) *Store {
	t.Helper()
	s := New()
	must(t, s.CreateOrg("acme", "Acme", Limits{MaxUSDPerDay: 1000}))
	must(t, s.CreateTeam("eng", "Engineering", "acme", Limits{MaxUSDPerDay: 500, MaxUSDPerMin: 5}))
	must(t, s.CreateApp("bot", "Agent Bot", "eng", Limits{MaxUSDPerDay: 200, MaxUSDPerRun: 50}))
	must(t, s.IssueKey("keyhash-bot", App, "bot"))
	return s
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolveChainAndCaps(t *testing.T) {
	s := seed(t)
	ch, err := s.Resolve("keyhash-bot", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Scopes) != 4 {
		t.Fatalf("chain must be org▸team▸app▸run, got %d scopes: %+v", len(ch.Scopes), ch.Scopes)
	}
	want := []struct {
		name, key string
	}{
		{"org", "org:acme"},
		{"team", "team:eng"},
		{"app", "app:bot"},
		{"run", "run:app:bot\x00run-1"},
	}
	for i, w := range want {
		if ch.Scopes[i].Name != w.name || ch.Scopes[i].Key != w.key {
			t.Fatalf("scope %d: got %q/%q want %q/%q", i, ch.Scopes[i].Name, ch.Scopes[i].Key, w.name, w.key)
		}
	}
	// Each provisioned scope carries its OWN caps (independently enforced).
	if ch.Scopes[0].Limits.MaxUSDPerDay != 1000 || ch.Scopes[1].Limits.MaxUSDPerDay != 500 || ch.Scopes[2].Limits.MaxUSDPerDay != 200 {
		t.Fatalf("per-node day caps wrong: %+v", ch.Scopes)
	}
	// The run scope carries the tightest per-run cap in the chain (only app set it).
	if ch.Scopes[3].Limits.MaxUSDPerRun != 50 {
		t.Fatalf("run cap want 50, got %v", ch.Scopes[3].Limits.MaxUSDPerRun)
	}
	if ch.KilledBy != "" {
		t.Fatalf("nothing killed, got %q", ch.KilledBy)
	}
}

func TestEffectiveRunCapIsTightest(t *testing.T) {
	s := seed(t)
	// team sets a tighter per-run cap than the app.
	must(t, s.SetLimits(Team, "eng", Limits{MaxUSDPerDay: 500, MaxUSDPerRun: 30}))
	ch, _ := s.Resolve("keyhash-bot", "r")
	run := ch.Scopes[len(ch.Scopes)-1]
	if run.Limits.MaxUSDPerRun != 30 {
		t.Fatalf("run cap must be the tightest ancestor (30), got %v", run.Limits.MaxUSDPerRun)
	}
}

func TestNoRunIDNoRunScope(t *testing.T) {
	s := seed(t)
	ch, _ := s.Resolve("keyhash-bot", "")
	for _, sc := range ch.Scopes {
		if sc.Name == "run" {
			t.Fatal("no run id must not produce a run scope")
		}
	}
	if len(ch.Scopes) != 3 {
		t.Fatalf("want org▸team▸app, got %d", len(ch.Scopes))
	}
}

func TestKillPropagatesFromAnyNode(t *testing.T) {
	s := seed(t)
	must(t, s.Kill(Team, "eng")) // freeze the whole team
	ch, _ := s.Resolve("keyhash-bot", "r")
	if ch.KilledBy != "team:eng" {
		t.Fatalf("killing a team must deny its apps; KilledBy=%q", ch.KilledBy)
	}
	must(t, s.Unkill(Team, "eng"))
	ch, _ = s.Resolve("keyhash-bot", "r")
	if ch.KilledBy != "" {
		t.Fatalf("unkill must clear it, got %q", ch.KilledBy)
	}
}

func TestUnknownKey(t *testing.T) {
	s := seed(t)
	if _, err := s.Resolve("nope", "r"); err != ErrUnknownKey {
		t.Fatalf("want ErrUnknownKey, got %v", err)
	}
}

func TestManagementValidations(t *testing.T) {
	s := New()
	if err := s.CreateTeam("eng", "E", "missing-org", Limits{}); err != ErrBadParent {
		t.Fatalf("team under a missing org must fail, got %v", err)
	}
	must(t, s.CreateOrg("acme", "Acme", Limits{}))
	if err := s.CreateApp("bot", "B", "missing-team", Limits{}); err != ErrBadParent {
		t.Fatalf("app under a missing team must fail, got %v", err)
	}
	must(t, s.CreateTeam("eng", "E", "acme", Limits{}))
	if err := s.CreateTeam("eng", "E2", "acme", Limits{}); err != ErrExists {
		t.Fatalf("duplicate team must fail, got %v", err)
	}
	if err := s.IssueKey("k", App, "ghost"); err != ErrNotFound {
		t.Fatalf("issuing a key to a missing node must fail, got %v", err)
	}
	if err := s.SetLimits(App, "ghost", Limits{}); err != ErrNotFound {
		t.Fatalf("set-limits on a missing node must fail, got %v", err)
	}
}

func TestKeyRepointsToOneNode(t *testing.T) {
	s := seed(t)
	must(t, s.CreateApp("bot2", "Bot 2", "eng", Limits{}))
	must(t, s.IssueKey("keyhash-bot", App, "bot2")) // re-point the same key
	ch, _ := s.Resolve("keyhash-bot", "")
	if ch.Scopes[len(ch.Scopes)-1].Key != "app:bot2" {
		t.Fatalf("a re-issued key must resolve to the new node, got %+v", ch.Scopes)
	}
}

func TestOverAllocationSurfaced(t *testing.T) {
	s := New()
	must(t, s.CreateOrg("acme", "Acme", Limits{}))
	must(t, s.CreateTeam("eng", "E", "acme", Limits{MaxUSDPerDay: 1000}))
	// two apps allocate $1200/day under a $1000/day team (legal — parent binds).
	must(t, s.CreateApp("a", "A", "eng", Limits{MaxUSDPerDay: 700}))
	must(t, s.CreateApp("b", "B", "eng", Limits{MaxUSDPerDay: 500}))
	w := s.OverAllocations()
	if len(w) != 1 {
		t.Fatalf("expected one over-allocation warning, got %v", w)
	}
}

func TestKeyOnTeamChain(t *testing.T) {
	s := seed(t)
	// A key can belong to a team (not only an app): chain is org▸team, no app.
	must(t, s.IssueKey("keyhash-team", Team, "eng"))
	ch, err := s.Resolve("keyhash-team", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Scopes) != 3 { // org, team, run
		t.Fatalf("team key chain should be org▸team▸run, got %d: %+v", len(ch.Scopes), ch.Scopes)
	}
	if ch.Scopes[0].Key != "org:acme" || ch.Scopes[1].Key != "team:eng" {
		t.Fatalf("unexpected ancestry: %+v", ch.Scopes)
	}
	// The run is namespaced under the team leaf, not an app.
	if ch.Scopes[2].Key != "run:team:eng\x00r" {
		t.Fatalf("run must namespace under the team leaf, got %q", ch.Scopes[2].Key)
	}
}

func TestOrgAsTightestRunCap(t *testing.T) {
	s := New()
	// Only the org sets a per-run cap; it must flow down to the run scope.
	must(t, s.CreateOrg("acme", "Acme", Limits{MaxUSDPerRun: 5}))
	must(t, s.CreateTeam("eng", "E", "acme", Limits{}))
	must(t, s.CreateApp("bot", "B", "eng", Limits{}))
	must(t, s.IssueKey("k", App, "bot"))
	ch, _ := s.Resolve("k", "r")
	run := ch.Scopes[len(ch.Scopes)-1]
	if run.Limits.MaxUSDPerRun != 5 {
		t.Fatalf("org-set per-run cap must reach the run scope, got %v", run.Limits.MaxUSDPerRun)
	}
}

func TestNegativeCapsRejected(t *testing.T) {
	s := New()
	if err := s.CreateOrg("acme", "Acme", Limits{MaxUSDPerRun: -1}); err != ErrBadLimits {
		t.Fatalf("negative cap on create must fail, got %v", err)
	}
	must(t, s.CreateOrg("acme", "Acme", Limits{}))
	if err := s.SetLimits(Org, "acme", Limits{MaxUSDPerDay: -0.01}); err != ErrBadLimits {
		t.Fatalf("negative cap on set-limits must fail, got %v", err)
	}
	if err := s.CreateTeam("eng", "E", "acme", Limits{MaxTokensPerMin: -1}); err != ErrBadLimits {
		t.Fatalf("negative token cap must fail, got %v", err)
	}
}

func TestInvalidIDsRejected(t *testing.T) {
	s := New()
	for _, bad := range []string{"", "has:colon", "has\x00nul", "has space"} {
		if err := s.CreateOrg(bad, "X", Limits{}); err != ErrBadID {
			t.Fatalf("id %q must be rejected, got %v", bad, err)
		}
	}
	must(t, s.CreateOrg("acme", "Acme", Limits{}))
	if err := s.CreateTeam("bad:team", "T", "acme", Limits{}); err != ErrBadID {
		t.Fatalf("delimiter in team id must be rejected, got %v", err)
	}
	if err := s.IssueKey("", App, "acme"); err != ErrBadID {
		t.Fatalf("empty key hash must be rejected, got %v", err)
	}
}

func TestInvalidRunIDRejected(t *testing.T) {
	s := seed(t)
	// The NUL run-key separator must never appear in a run id.
	if _, err := s.Resolve("keyhash-bot", "run\x00forged"); err != ErrBadRunID {
		t.Fatalf("run id with the key separator must be rejected, got %v", err)
	}
	if _, err := s.Resolve("keyhash-bot", "run:with:colons"); err != ErrBadRunID {
		t.Fatalf("run id with a delimiter must be rejected, got %v", err)
	}
}

func TestBrokenChainFailsClosed(t *testing.T) {
	s := seed(t)
	// Simulate a durable-store integrity failure: the app's parent team vanishes.
	// Resolve must fail CLOSED (no chain missing the team/org budget), not return
	// a truncated chain that under-enforces.
	s.mu.Lock()
	delete(s.nodes, "team:eng")
	s.mu.Unlock()
	if _, err := s.Resolve("keyhash-bot", "r"); err != ErrBrokenChain {
		t.Fatalf("a missing ancestor must fail closed with ErrBrokenChain, got %v", err)
	}
}

func TestDanglingKeyFailsClosed(t *testing.T) {
	s := seed(t)
	// Simulate a durable-store integrity fault: the key still maps to a node ref,
	// but the node record is gone. This must fail CLOSED (ErrBrokenChain), NOT look
	// like an unknown/ungoverned key — otherwise a caller downgrades a previously
	// governed key to laxer enforcement.
	s.mu.Lock()
	delete(s.nodes, "app:bot")
	s.mu.Unlock()
	if _, err := s.Resolve("keyhash-bot", "r"); err != ErrBrokenChain {
		t.Fatalf("a key pointing at a missing node must fail closed, got %v", err)
	}
}

func TestCrossTenantRunKeysDistinct(t *testing.T) {
	s := seed(t)
	must(t, s.CreateApp("bot2", "Bot 2", "eng", Limits{}))
	must(t, s.IssueKey("keyhash-bot2", App, "bot2"))
	// The SAME run id under two different apps must not share a run scope key.
	a, _ := s.Resolve("keyhash-bot", "same-run")
	b, _ := s.Resolve("keyhash-bot2", "same-run")
	ka := a.Scopes[len(a.Scopes)-1].Key
	kb := b.Scopes[len(b.Scopes)-1].Key
	if ka == kb {
		t.Fatalf("run keys must differ across tenants, both were %q", ka)
	}
}

func TestOverAllocationNamesParent(t *testing.T) {
	s := New()
	must(t, s.CreateOrg("acme", "Acme", Limits{}))
	must(t, s.CreateTeam("eng", "E", "acme", Limits{MaxUSDPerDay: 1000}))
	must(t, s.CreateApp("a", "A", "eng", Limits{MaxUSDPerDay: 700}))
	must(t, s.CreateApp("b", "B", "eng", Limits{MaxUSDPerDay: 500}))
	w := s.OverAllocations()
	if len(w) != 1 || !strings.Contains(w[0], "team:eng") || !strings.Contains(w[0], "1200") {
		t.Fatalf("warning must name the binding parent and the over-allocation, got %v", w)
	}
}

func TestResolveConcurrentWithWrites(t *testing.T) {
	s := seed(t)
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			_, _ = s.Resolve("keyhash-bot", "r")
		}
	}()
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = s.CreateApp("app-"+strconv.Itoa(w)+"-"+strconv.Itoa(i), "x", "eng", Limits{MaxUSDPerRun: 10})
				_ = s.SetLimits(Team, "eng", Limits{MaxUSDPerDay: float64(i)})
			}
		}(w)
	}
	<-done
	wg.Wait()
}
