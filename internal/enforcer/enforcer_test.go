package enforcer

import (
	"testing"

	"github.com/somasays/heave/internal/policy"
)

// seed builds org=acme ▸ team=eng ▸ app=bot with a key on the app.
func seed(t *testing.T) *policy.Store {
	t.Helper()
	s := policy.New()
	must(t, s.CreateOrg("acme", "Acme", policy.Limits{MaxUSDPerMin: 10, MaxUSDPerDay: 1000}))
	must(t, s.CreateTeam("eng", "Engineering", "acme", policy.Limits{MaxConcurrent: 4}))
	must(t, s.CreateApp("bot", "Bot", "eng", policy.Limits{MaxUSDPerRun: 0.5}))
	must(t, s.IssueKey("keyhash-bot", policy.App, "bot"))
	return s
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolveTranslatesChain(t *testing.T) {
	r := NewResolver(seed(t))
	scopes, killedBy, governed, err := r.Resolve("keyhash-bot", "run-1")
	if err != nil || !governed {
		t.Fatalf("a provisioned key must resolve, got governed=%v err=%v", governed, err)
	}
	if killedBy != "" {
		t.Fatalf("nothing killed, got %q", killedBy)
	}
	if len(scopes) != 4 {
		t.Fatalf("want org▸team▸app▸run, got %d: %+v", len(scopes), scopes)
	}
	// Keys and the per-node caps the firewall enforces must carry over.
	if scopes[0].Name != "org" || scopes[0].Key != "org:acme" || scopes[0].Limits.MaxUSDPerMin != 10 {
		t.Fatalf("org scope mistranslated: %+v", scopes[0])
	}
	if scopes[1].Limits.MaxConcurrent != 4 {
		t.Fatalf("team concurrency cap lost: %+v", scopes[1])
	}
	// The run scope carries the tightest per-run cap (only the app set it).
	run := scopes[len(scopes)-1]
	if run.Name != "run" || run.Key != "run:app:bot\x00keyhash-bot\x00run-1" || run.Limits.MaxUSDPerRun != 0.5 {
		t.Fatalf("run scope mistranslated: %+v", run)
	}
}

func TestDayBudgetNotCarriedToFirewall(t *testing.T) {
	// The org has a $1000/day budget; the firewall does not (yet) enforce calendar
	// budgets, so it must NOT leak in as some other cap.
	r := NewResolver(seed(t))
	scopes, _, _, _ := r.Resolve("keyhash-bot", "")
	org := scopes[0]
	if org.Limits.MaxUSDPerMin != 10 {
		t.Fatalf("velocity cap should carry, got %+v", org)
	}
	// firewall.Limits has no Day/Month field, so there is nothing to assert beyond
	// "it compiled and only the enforced fields are set" — the velocity cap above.
}

func TestUnknownKeyIsNotGovernedNotAnError(t *testing.T) {
	r := NewResolver(seed(t))
	scopes, _, governed, err := r.Resolve("no-such-key", "run-1")
	if governed || err != nil || scopes != nil {
		t.Fatalf("an unknown key must fall through (governed=false, err=nil), got governed=%v err=%v", governed, err)
	}
}

func TestBadRunIDFailsClosedAsError(t *testing.T) {
	r := NewResolver(seed(t))
	// A run id carrying the reserve-key separator must surface as an ERROR (deny),
	// never as a silent fall-through to flat enforcement.
	_, _, governed, err := r.Resolve("keyhash-bot", "bad\x00run")
	if governed {
		t.Fatal("a bad run id must not be treated as governed")
	}
	if err == nil {
		t.Fatal("a resolution failure must be surfaced as an error (fail closed), not err=nil")
	}
}

func TestKilledNodeSurfacesKilledBy(t *testing.T) {
	s := seed(t)
	must(t, s.Kill(policy.Team, "eng"))
	r := NewResolver(s)
	_, killedBy, governed, err := r.Resolve("keyhash-bot", "run-1")
	if err != nil || !governed {
		t.Fatalf("a killed node still resolves (the caller denies on killedBy), got governed=%v err=%v", governed, err)
	}
	if killedBy != "team:eng" {
		t.Fatalf("killing the team must surface as killedBy, got %q", killedBy)
	}
}
