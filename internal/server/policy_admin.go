package server

// The org control-plane MANAGEMENT API (ADR 0005/0006): admin-gated CRUD over the
// policy hierarchy (org▸team▸app), the budgets on any node, key→node mappings, and
// per-node kill. Mounted only when a policy store is configured (see Handler).
// This is the provisioning/management surface; enforcement of the resolved chain
// is a separate path (internal/enforcer + firewall.EnterChain).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/somasays/heave/internal/policy"
)

// limitsBody is the snake_case wire form of a node's caps. 0/absent = unconstrained
// on that dimension.
type limitsBody struct {
	MaxUSDPerMin    float64 `json:"max_usd_per_min"`
	MaxTokensPerMin int     `json:"max_tokens_per_min"`
	MaxConcurrent   int     `json:"max_concurrent"`
	MaxUSDPerDay    float64 `json:"max_usd_per_day"`
	MaxUSDPerMonth  float64 `json:"max_usd_per_month"`
	MaxUSDPerRun    float64 `json:"max_usd_per_run"`
}

func (l limitsBody) toPolicy() policy.Limits {
	return policy.Limits{
		MaxUSDPerMin: l.MaxUSDPerMin, MaxTokensPerMin: l.MaxTokensPerMin,
		MaxConcurrent: l.MaxConcurrent, MaxUSDPerDay: l.MaxUSDPerDay,
		MaxUSDPerMonth: l.MaxUSDPerMonth, MaxUSDPerRun: l.MaxUSDPerRun,
	}
}

func limitsFromPolicy(l policy.Limits) limitsBody {
	return limitsBody{
		MaxUSDPerMin: l.MaxUSDPerMin, MaxTokensPerMin: l.MaxTokensPerMin,
		MaxConcurrent: l.MaxConcurrent, MaxUSDPerDay: l.MaxUSDPerDay,
		MaxUSDPerMonth: l.MaxUSDPerMonth, MaxUSDPerRun: l.MaxUSDPerRun,
	}
}

// nodeJSON is the wire form of a snapshot node.
type nodeJSON struct {
	Type     string     `json:"type"`
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	ParentID string     `json:"parent_id,omitempty"`
	Limits   limitsBody `json:"limits"`
	Killed   bool       `json:"killed"`
}

// decodeAdmin bounds and strictly decodes an admin request body. Unknown fields are
// rejected so a typo'd budget field fails loudly rather than silently no-op.
func (s *Server) decodeAdmin(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxRequestBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// policyErr maps a policy management error to the right HTTP status.
func policyErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policy.ErrExists):
		writeError(w, http.StatusConflict, "invalid_request_error", err.Error())
	case errors.Is(err, policy.ErrNotFound):
		writeError(w, http.StatusNotFound, "invalid_request_error", err.Error())
	case errors.Is(err, policy.ErrBadID), errors.Is(err, policy.ErrBadLimits), errors.Is(err, policy.ErrBadParent):
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "api_error", "policy update failed")
	}
}

func parseNodeType(s string) (policy.NodeType, bool) {
	switch s {
	case "org":
		return policy.Org, true
	case "team":
		return policy.Team, true
	case "app":
		return policy.App, true
	}
	return "", false
}

// handlePolicyList returns the whole provisioned tree plus any over-allocation
// warnings (informational — over-allocation is legal under the umbrella model).
func (s *Server) handlePolicyList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	snap := s.policy.Snapshot()
	nodes := make([]nodeJSON, len(snap))
	for i, n := range snap {
		nodes[i] = nodeJSON{
			Type: string(n.Type), ID: n.ID, Name: n.Name, ParentID: n.ParentID,
			Limits: limitsFromPolicy(n.Limits), Killed: n.Killed,
		}
	}
	over := s.policy.OverAllocations()
	if over == nil {
		over = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "over_allocations": over})
}

func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		ID     string     `json:"id"`
		Name   string     `json:"name"`
		Limits limitsBody `json:"limits"`
	}
	if !s.decodeAdmin(w, r, &body) {
		return
	}
	if err := s.policy.CreateOrg(body.ID, body.Name, body.Limits.toPolicy()); err != nil {
		policyErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, nodeJSON{Type: "org", ID: body.ID, Name: body.Name, Limits: body.Limits})
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		ID     string     `json:"id"`
		Name   string     `json:"name"`
		OrgID  string     `json:"org_id"`
		Limits limitsBody `json:"limits"`
	}
	if !s.decodeAdmin(w, r, &body) {
		return
	}
	if err := s.policy.CreateTeam(body.ID, body.Name, body.OrgID, body.Limits.toPolicy()); err != nil {
		policyErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, nodeJSON{Type: "team", ID: body.ID, Name: body.Name, ParentID: body.OrgID, Limits: body.Limits})
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		ID     string     `json:"id"`
		Name   string     `json:"name"`
		TeamID string     `json:"team_id"`
		Limits limitsBody `json:"limits"`
	}
	if !s.decodeAdmin(w, r, &body) {
		return
	}
	if err := s.policy.CreateApp(body.ID, body.Name, body.TeamID, body.Limits.toPolicy()); err != nil {
		policyErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, nodeJSON{Type: "app", ID: body.ID, Name: body.Name, ParentID: body.TeamID, Limits: body.Limits})
}

func (s *Server) handleSetLimits(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	t, ok := parseNodeType(r.PathValue("type"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unknown node type (want org|team|app)")
		return
	}
	var body limitsBody
	if !s.decodeAdmin(w, r, &body) {
		return
	}
	if err := s.policy.SetLimits(t, r.PathValue("id"), body.toPolicy()); err != nil {
		policyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleNodeKill(w http.ResponseWriter, r *http.Request) { s.setNodeKilled(w, r, true) }
func (s *Server) handleNodeUnkill(w http.ResponseWriter, r *http.Request) {
	s.setNodeKilled(w, r, false)
}

func (s *Server) setNodeKilled(w http.ResponseWriter, r *http.Request, kill bool) {
	if !s.requireAdmin(w, r) {
		return
	}
	t, ok := parseNodeType(r.PathValue("type"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unknown node type (want org|team|app)")
		return
	}
	id := r.PathValue("id")
	var err error
	if kill {
		err = s.policy.Kill(t, id)
	} else {
		err = s.policy.Unkill(t, id)
	}
	if err != nil {
		policyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "killed": kill})
}

// handleIssueKey maps a bearer to a node. The caller may send the raw bearer
// (hashed server-side, never stored) or a precomputed hex SHA-256.
func (s *Server) handleIssueKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		Key       string `json:"key"`
		KeySHA256 string `json:"key_sha256"`
		NodeType  string `json:"node_type"`
		NodeID    string `json:"node_id"`
	}
	if !s.decodeAdmin(w, r, &body) {
		return
	}
	sha := strings.ToLower(strings.TrimSpace(body.KeySHA256))
	if sha == "" && body.Key != "" {
		h := sha256.Sum256([]byte(body.Key))
		sha = hex.EncodeToString(h[:])
	}
	if sha == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "one of key or key_sha256 is required")
		return
	}
	t, ok := parseNodeType(body.NodeType)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unknown node_type (want org|team|app)")
		return
	}
	if err := s.policy.IssueKey(sha, t, body.NodeID); err != nil {
		policyErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "key_sha256": sha})
}
