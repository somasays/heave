package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/somasays/heave/internal/controls"
	"github.com/somasays/heave/internal/policy"
	"github.com/somasays/heave/internal/provider"
	"github.com/somasays/heave/internal/router"
)

// policyServer builds a server with the control plane on. authOn=true seeds an
// admin key ("adminkey") and a non-admin tenant ("tenantkey").
func policyServer(t *testing.T, authOn bool) http.Handler {
	t.Helper()
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "up-1"}}, "m")
	fp := &fakeProvider{resp: &provider.Response{Content: "ok", InputTokens: 1, OutputTokens: 1}}
	guard := controls.New(false, nil, nil)
	if authOn {
		guard = controls.New(true, []controls.Client{
			{Name: "ops", KeySHA256: sha("adminkey"), Admin: true},
			{Name: "tenant", KeySHA256: sha("tenantkey")},
		}, nil)
	}
	srv := newTestServer(t, Deps{
		Router: rtr, Providers: map[string]provider.Provider{"fake": fp},
		Guard: guard, Policy: policy.New(),
	}, Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	return srv.Handler()
}

func adminReq(t *testing.T, h http.Handler, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestPolicyRoutesNotMountedWhenControlPlaneOff(t *testing.T) {
	// No Policy in Deps ⇒ the management routes must not exist (404), not 401/500.
	rtr := router.New([]router.ModelConfig{{Alias: "m", Provider: "fake", Upstream: "up-1"}}, "m")
	fp := &fakeProvider{resp: &provider.Response{Content: "ok", InputTokens: 1, OutputTokens: 1}}
	srv := newTestServer(t, Deps{Router: rtr, Providers: map[string]provider.Provider{"fake": fp}, Guard: controls.New(false, nil, nil)},
		Options{MaxRequestBytes: 1 << 20, RequestTimeout: time.Second})
	rr := adminReq(t, srv.Handler(), http.MethodGet, "/v1/policy", "", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("control plane off: /v1/policy must be unmounted (404), got %d", rr.Code)
	}
}

func TestPolicyManagementFullFlow(t *testing.T) {
	h := policyServer(t, false) // auth off ⇒ admin gate open
	// Provision org ▸ team ▸ app with budgets at each level.
	if rr := adminReq(t, h, http.MethodPost, "/v1/policy/orgs", "",
		`{"id":"acme","name":"Acme","limits":{"max_usd_per_day":1000}}`); rr.Code != http.StatusCreated {
		t.Fatalf("create org: want 201, got %d (%s)", rr.Code, rr.Body)
	}
	if rr := adminReq(t, h, http.MethodPost, "/v1/policy/teams", "",
		`{"id":"eng","name":"Engineering","org_id":"acme","limits":{"max_usd_per_min":5}}`); rr.Code != http.StatusCreated {
		t.Fatalf("create team: want 201, got %d (%s)", rr.Code, rr.Body)
	}
	if rr := adminReq(t, h, http.MethodPost, "/v1/policy/apps", "",
		`{"id":"bot","name":"Bot","team_id":"eng","limits":{"max_usd_per_run":0.5}}`); rr.Code != http.StatusCreated {
		t.Fatalf("create app: want 201, got %d (%s)", rr.Code, rr.Body)
	}
	// Update a budget.
	if rr := adminReq(t, h, http.MethodPut, "/v1/policy/nodes/app/bot/limits", "",
		`{"max_usd_per_run":0.25}`); rr.Code != http.StatusOK {
		t.Fatalf("set limits: want 200, got %d (%s)", rr.Code, rr.Body)
	}
	// Map a key to the app (raw bearer, hashed server-side).
	if rr := adminReq(t, h, http.MethodPost, "/v1/policy/keys", "",
		`{"key":"sk-agent-1","node_type":"app","node_id":"bot"}`); rr.Code != http.StatusCreated {
		t.Fatalf("issue key: want 201, got %d (%s)", rr.Code, rr.Body)
	}
	// Kill the team (circuit breaker).
	if rr := adminReq(t, h, http.MethodPost, "/v1/policy/nodes/team/eng/kill", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("kill team: want 200, got %d (%s)", rr.Code, rr.Body)
	}
	// List reflects the tree, the updated cap, and the kill.
	rr := adminReq(t, h, http.MethodGet, "/v1/policy", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", rr.Code)
	}
	var got struct {
		Nodes []struct {
			Type, ID string
			Killed   bool
			Limits   struct {
				MaxUSDPerRun float64 `json:"max_usd_per_run"`
			}
		} `json:"nodes"`
		OverAllocations []string `json:"over_allocations"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(got.Nodes) != 3 {
		t.Fatalf("want org▸team▸app, got %d nodes", len(got.Nodes))
	}
	if got.Nodes[0].Type != "org" || got.Nodes[2].Type != "app" {
		t.Fatalf("nodes must be root-first, got %+v", got.Nodes)
	}
	if got.Nodes[2].Limits.MaxUSDPerRun != 0.25 {
		t.Fatalf("app cap update not reflected: %+v", got.Nodes[2])
	}
	if !got.Nodes[1].Killed { // team eng
		t.Fatalf("team kill not reflected: %+v", got.Nodes[1])
	}
}

func TestPolicyManagementRequiresAdmin(t *testing.T) {
	h := policyServer(t, true)
	body := `{"id":"acme","name":"Acme","limits":{}}`
	if rr := adminReq(t, h, http.MethodPost, "/v1/policy/orgs", "", body); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no key must be 401, got %d", rr.Code)
	}
	if rr := adminReq(t, h, http.MethodPost, "/v1/policy/orgs", "tenantkey", body); rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin must be 403, got %d", rr.Code)
	}
	if rr := adminReq(t, h, http.MethodPost, "/v1/policy/orgs", "adminkey", body); rr.Code != http.StatusCreated {
		t.Fatalf("admin must succeed (201), got %d (%s)", rr.Code, rr.Body)
	}
}

func TestPolicyManagementErrorMapping(t *testing.T) {
	h := policyServer(t, false)
	mk := func(method, path, body string) int {
		return adminReq(t, h, method, path, "", body).Code
	}
	// Seed an org+team for the parent/dup/notfound cases.
	mk(http.MethodPost, "/v1/policy/orgs", `{"id":"acme","name":"Acme","limits":{}}`)

	if c := mk(http.MethodPost, "/v1/policy/orgs", `{"id":"acme","name":"dup","limits":{}}`); c != http.StatusConflict {
		t.Fatalf("duplicate org must be 409, got %d", c)
	}
	if c := mk(http.MethodPost, "/v1/policy/teams", `{"id":"eng","name":"E","org_id":"ghost","limits":{}}`); c != http.StatusBadRequest {
		t.Fatalf("team under a missing org must be 400, got %d", c)
	}
	if c := mk(http.MethodPost, "/v1/policy/orgs", `{"id":"bad:id","name":"X","limits":{}}`); c != http.StatusBadRequest {
		t.Fatalf("a delimiter in an id must be 400, got %d", c)
	}
	if c := mk(http.MethodPost, "/v1/policy/orgs", `{"id":"neg","name":"X","limits":{"max_usd_per_run":-1}}`); c != http.StatusBadRequest {
		t.Fatalf("a negative cap must be 400, got %d", c)
	}
	if c := mk(http.MethodPut, "/v1/policy/nodes/app/ghost/limits", `{"max_usd_per_run":1}`); c != http.StatusNotFound {
		t.Fatalf("set-limits on a missing node must be 404, got %d", c)
	}
	if c := mk(http.MethodPost, "/v1/policy/nodes/dragon/x/kill", ""); c != http.StatusBadRequest {
		t.Fatalf("an unknown node type must be 400, got %d", c)
	}
	// Strict decoding: an unknown/typo'd budget field must fail loudly, not no-op.
	if c := mk(http.MethodPost, "/v1/policy/orgs", `{"id":"typo","name":"X","limits":{"max_usd_per_minute":5}}`); c != http.StatusBadRequest {
		t.Fatalf("an unknown body field must be 400, got %d", c)
	}
}
