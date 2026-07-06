// Package policy is heave's org-wide spend hierarchy and budget-resolution
// protocol (docs/adr/0006): org ▸ team ▸ app ▸ run, with a budget settable at any
// node and composed under the "umbrella" model — each node caps the AGGREGATE
// spend at and under it, and a request is admitted only if it fits under EVERY
// node in its chain (enforced by the atomic multi-scope reserve). A key maps to
// exactly one node; the run is supplied per request.
//
// This package owns the MODEL and the RESOLUTION (mapping a request to the
// ordered set of scopes + caps it must satisfy). Wiring the resolved chain into
// the reserve engine and durable (Postgres) persistence are separate increments;
// the in-memory store here sits behind the same shape a durable store will take.
package policy

import (
	"errors"
	"sort"
	"strconv"
	"sync"
)

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// NodeType is a level in the provisioned hierarchy.
type NodeType string

// The provisioned node types, from the root down.
const (
	Org  NodeType = "org"
	Team NodeType = "team"
	App  NodeType = "app"
)

func (t NodeType) parent() NodeType {
	switch t {
	case Team:
		return Org
	case App:
		return Team
	}
	return ""
}

// Limits is a node's cap set. 0 = unconstrained on that dimension. Velocity is a
// rolling window; Day/Month are calendar budgets; MaxUSDPerRun applies to EACH run
// under the node (distributed to run scope as the tightest ancestor value).
type Limits struct {
	MaxUSDPerMin    float64
	MaxTokensPerMin int
	MaxConcurrent   int
	MaxUSDPerDay    float64
	MaxUSDPerMonth  float64
	MaxUSDPerRun    float64
}

// Node is a provisioned entity (org/team/app).
type Node struct {
	Type     NodeType
	ID       string
	Name     string
	ParentID string // "" for an org
	Limits   Limits
	Killed   bool
}

// refFor is the single source of truth for a node's key in the store map AND its
// reserve-store scope key. Keeping one builder prevents the two from drifting.
func refFor(t NodeType, id string) string { return string(t) + ":" + id }

func (n Node) scopeKey() string { return refFor(n.Type, n.ID) }

// validID bounds an id to a safe token so it can't inject the ":" or NUL
// delimiters used to build scope keys, or grow the reserve keyspace unboundedly.
// The same rule validates a run id (both flow into scope keys).
func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// valid reports whether every cap is non-negative (ADR 0006 §8). A negative cap
// would silently deny every request on that scope.
func (l Limits) valid() bool {
	return l.MaxUSDPerMin >= 0 && l.MaxTokensPerMin >= 0 && l.MaxConcurrent >= 0 &&
		l.MaxUSDPerDay >= 0 && l.MaxUSDPerMonth >= 0 && l.MaxUSDPerRun >= 0
}

// Scope is one enforceable level of a request's chain: a stable reserve-store key
// + its caps. The enforcement layer reserves across all scopes, all-or-nothing.
type Scope struct {
	Name   string // "org" | "team" | "app" | "run"
	Key    string // stable reserve-store key, e.g. "team:acme-eng"
	Limits Limits
}

// Chain is the ordered (org→run) set of scopes a request must satisfy. KilledBy
// names the first killed node in the chain (a per-node circuit breaker), or "".
type Chain struct {
	Scopes   []Scope
	KilledBy string
}

var (
	// ErrUnknownKey means the bearer maps to no provisioned node.
	ErrUnknownKey = errors.New("policy: unknown api key")
	// ErrExists means a node with that type+id already exists.
	ErrExists = errors.New("policy: node already exists")
	// ErrNotFound means the referenced node does not exist.
	ErrNotFound = errors.New("policy: node not found")
	// ErrBadParent means the parent is missing or the wrong type.
	ErrBadParent = errors.New("policy: parent not found or wrong type")
	// ErrBadID means a node id is empty, too long, or contains a character that
	// could inject a scope-key delimiter.
	ErrBadID = errors.New("policy: invalid node id")
	// ErrBadLimits means a cap was negative (ADR 0006 §8: caps ≥ 0).
	ErrBadLimits = errors.New("policy: limits must be non-negative")
	// ErrBadRunID means the run id is empty, too long, or contains a character
	// that could inject the run scope-key delimiter.
	ErrBadRunID = errors.New("policy: invalid run id")
	// ErrBrokenChain means a node's ancestry does not reach an org root — a
	// data-integrity failure. Resolve fails CLOSED rather than enforce a chain
	// missing an ancestor's budget.
	ErrBrokenChain = errors.New("policy: broken node chain")
)

// Store is the in-memory org model + resolver. Safe for concurrent use; Resolve
// is on the request hot path and takes only a read lock.
type Store struct {
	mu    sync.RWMutex
	nodes map[string]*Node  // scopeKey() -> node
	keys  map[string]string // key sha256 -> node scopeKey()
}

// New builds an empty Store.
func New() *Store {
	return &Store{nodes: map[string]*Node{}, keys: map[string]string{}}
}

// CreateOrg provisions a top-level org.
func (s *Store) CreateOrg(id, name string, l Limits) error {
	if !validID(id) {
		return ErrBadID
	}
	if !l.valid() {
		return ErrBadLimits
	}
	return s.put(Node{Type: Org, ID: id, Name: name, Limits: l})
}

// CreateTeam provisions a team under an existing org.
func (s *Store) CreateTeam(id, name, orgID string, l Limits) error {
	if !validID(id) {
		return ErrBadID
	}
	if !l.valid() {
		return ErrBadLimits
	}
	return s.putChild(Node{Type: Team, ID: id, Name: name, ParentID: orgID, Limits: l}, Org, orgID)
}

// CreateApp provisions an app under an existing team.
func (s *Store) CreateApp(id, name, teamID string, l Limits) error {
	if !validID(id) {
		return ErrBadID
	}
	if !l.valid() {
		return ErrBadLimits
	}
	return s.putChild(Node{Type: App, ID: id, Name: name, ParentID: teamID, Limits: l}, Team, teamID)
}

func (s *Store) put(n Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[n.scopeKey()]; ok {
		return ErrExists
	}
	cp := n
	s.nodes[n.scopeKey()] = &cp
	return nil
}

func (s *Store) putChild(n Node, parentType NodeType, parentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[refFor(parentType, parentID)]; !ok {
		return ErrBadParent
	}
	if _, ok := s.nodes[n.scopeKey()]; ok {
		return ErrExists
	}
	cp := n
	s.nodes[n.scopeKey()] = &cp
	return nil
}

// SetLimits updates a node's caps (a budget can be set at any level).
func (s *Store) SetLimits(t NodeType, id string, l Limits) error {
	if !l.valid() {
		return ErrBadLimits
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[refFor(t, id)]
	if !ok {
		return ErrNotFound
	}
	n.Limits = l
	return nil
}

// Kill trips a per-node circuit breaker; any request whose chain includes this
// node is denied until Unkill.
func (s *Store) Kill(t NodeType, id string) error { return s.setKilled(t, id, true) }

// Unkill clears a node's circuit breaker.
func (s *Store) Unkill(t NodeType, id string) error { return s.setKilled(t, id, false) }

func (s *Store) setKilled(t NodeType, id string, v bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[refFor(t, id)]
	if !ok {
		return ErrNotFound
	}
	n.Killed = v
	return nil
}

// IssueKey maps a bearer's SHA-256 to a node (usually an app). A key belongs to
// exactly one node; re-issuing re-points it.
func (s *Store) IssueKey(keySHA256 string, t NodeType, id string) error {
	if keySHA256 == "" {
		return ErrBadID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[refFor(t, id)]; !ok {
		return ErrNotFound
	}
	s.keys[keySHA256] = refFor(t, id)
	return nil
}

// Resolve maps a request (bearer key + optional run id) to the ordered chain of
// scopes it must satisfy — the org▸team▸app ancestry of the key's node, each with
// its caps, plus a run scope carrying the tightest per-run cap in the chain.
//
// Preconditions & semantics the enforcement layer relies on:
//   - runID must be a safe token (validated here); "" omits the run scope, so
//     run-level caps do NOT apply — ancestor caps still do (ADR 0006 §9).
//   - The run scope key is namespaced under the key's own leaf node, so a run id
//     can never collide with or forge another tenant's scope key. Rotating run
//     ids yields a fresh per-run budget each time; ancestor caps still bind.
//   - If the lineage does not reach an org root (a missing ancestor), Resolve
//     fails CLOSED with ErrBrokenChain rather than enforce a chain with a budget
//     silently dropped.
func (s *Store) Resolve(keySHA256, runID string) (Chain, error) {
	if runID != "" && !validID(runID) {
		return Chain{}, ErrBadRunID
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ref, ok := s.keys[keySHA256]
	if !ok {
		return Chain{}, ErrUnknownKey
	}
	leaf, ok := s.nodes[ref]
	if !ok {
		// The key maps to a node that no longer exists — a data-integrity failure,
		// NOT an ungoverned key. Fail CLOSED (ErrBrokenChain) so a caller cannot
		// mistake it for "not provisioned" and downgrade to laxer enforcement.
		// Unreachable via the current API (no delete); guards the durable store.
		return Chain{}, ErrBrokenChain
	}

	// Walk to the org root, collecting ancestors leaf..org. A node claiming a
	// parent whose record is absent is a data-integrity failure: fail closed
	// rather than return a chain missing that ancestor's budget.
	var lineage []*Node
	for n := leaf; ; {
		lineage = append(lineage, n)
		if n.ParentID == "" {
			break
		}
		parent := s.nodes[refFor(n.Type.parent(), n.ParentID)]
		if parent == nil {
			return Chain{}, ErrBrokenChain
		}
		n = parent
	}
	// The lineage must terminate at an org root; anything else is malformed.
	if lineage[len(lineage)-1].Type != Org {
		return Chain{}, ErrBrokenChain
	}

	// Emit scopes top-down (org first) so the binding-node report reads naturally.
	ch := Chain{}
	var runCap float64
	for i := len(lineage) - 1; i >= 0; i-- {
		n := lineage[i]
		if n.Killed && ch.KilledBy == "" {
			ch.KilledBy = n.scopeKey()
		}
		ch.Scopes = append(ch.Scopes, Scope{Name: string(n.Type), Key: n.scopeKey(), Limits: n.Limits})
		runCap = tightest(runCap, n.Limits.MaxUSDPerRun)
	}

	if runID != "" {
		// Namespace the run under its app (leaf) so run ids can't collide or be
		// spoofed across tenants.
		ch.Scopes = append(ch.Scopes, Scope{
			Name:   "run",
			Key:    "run:" + leaf.scopeKey() + "\x00" + runID,
			Limits: Limits{MaxUSDPerRun: runCap},
		})
	}
	return ch, nil
}

// tightest returns the smaller of two caps, treating 0 as "unconstrained".
func tightest(a, b float64) float64 {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	if b < a {
		return b
	}
	return a
}

// NodeView is a read-only snapshot of a provisioned node for the management API
// (the internal *Node is never exposed). Fields mirror Node minus the pointer.
type NodeView struct {
	Type     NodeType
	ID       string
	Name     string
	ParentID string
	Limits   Limits
	Killed   bool
}

// Snapshot returns every provisioned node, ordered root-first (org▸team▸app) then
// by id, so the management API renders a stable tree. Takes only a read lock.
func (s *Store) Snapshot() []NodeView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]NodeView, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, NodeView{
			Type: n.Type, ID: n.ID, Name: n.Name,
			ParentID: n.ParentID, Limits: n.Limits, Killed: n.Killed,
		})
	}
	rank := map[NodeType]int{Org: 0, Team: 1, App: 2}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return rank[out[i].Type] < rank[out[j].Type]
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// OverAllocations returns human-readable warnings where a parent's children
// allocate more than the parent's own cap on a dimension. Over-allocation is
// LEGAL under the umbrella model (the parent binds); this only surfaces it for the
// management UI. Currently checks the daily budget.
func (s *Store) OverAllocations() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sum := map[string]float64{} // parent ref -> Σ children MaxUSDPerDay
	for _, n := range s.nodes {
		if n.ParentID != "" && n.Limits.MaxUSDPerDay > 0 {
			sum[refFor(n.Type.parent(), n.ParentID)] += n.Limits.MaxUSDPerDay
		}
	}
	var out []string
	for ref, child := range sum {
		p := s.nodes[ref]
		if p != nil && p.Limits.MaxUSDPerDay > 0 && child > p.Limits.MaxUSDPerDay {
			out = append(out, ref+": children allocate $"+ftoa(child)+"/day vs the $"+ftoa(p.Limits.MaxUSDPerDay)+"/day cap (parent binds)")
		}
	}
	return out
}
