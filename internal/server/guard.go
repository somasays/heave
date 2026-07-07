package server

// The /v1/guard/* DECISION API (ADR 0007): reserve/settle/release exposed as a
// pure decision (scope + estimate, never the payload) so an external PEP —
// LiteLLM, Envoy, a library — enforces heave's budgets OOB without heave sitting
// in the data path. It reuses the SAME resolver + firewall path as the inline
// handler (one enforcement engine), and hands the caller a SIGNED, STATELESS
// reservation token carrying the reconcile state, so settle/release need no
// server-side per-reservation registry and work across replicas in shared mode.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/somasays/heave/internal/firewall"
)

// GuardDedup makes settle/release idempotent across replicas: ClaimReconcile
// atomically claims a reservation nonce fleet-wide and reports whether THIS call
// claimed it (true) or it was already reconciled (false). Backed by the shared
// store (Redis SET NX). Required for the decision API — a stateless reservation
// token can be settled on any replica, so a per-instance dedup would let a
// cross-replica replay double-apply.
type GuardDedup interface {
	ClaimReconcile(nonce string, ttlSec int) (claimed bool, err error)
}

// maxGuardEstUSD / maxGuardEstTokens bound a caller-asserted estimate so a bad PEP
// can't drive a scope's counter to a poisoning near-infinity.
const (
	maxGuardEstUSD    = 1_000_000.0
	maxGuardEstTokens = 1_000_000_000
)

// reservationClaim is the signed payload of a reservation_id: the firewall
// reconcile state + a nonce (idempotency) + an expiry (the lease).
type reservationClaim struct {
	R     firewall.Reservation `json:"r"`
	Nonce string               `json:"n"`
	Exp   int64                `json:"exp"`
}

// reservationTTL is the lease: long enough to outlast the request, short enough
// that an orphaned reserve self-heals promptly (via the shared store's hold-TTL).
func (s *Server) reservationTTL() time.Duration { return s.requestTimeout + 60*time.Second }

// claimReconcile is true if THIS call newly claimed the nonce (apply), false if it
// was already reconciled (no-op). On a dedup-store error it FAILS OPEN (applies +
// logs) so a transient Redis blip doesn't strand reconciles — consistent with the
// shared store's fail-open stance; the rare cost is a possible double-apply during
// an outage, bounded by the reservation's own estimate.
func (s *Server) claimReconcile(nonce string) bool {
	claimed, err := s.guardDedup.ClaimReconcile(nonce, int(s.reservationTTL().Seconds()))
	if err != nil {
		s.log.Error("guard reconcile dedup unavailable; applying best-effort", "err", err)
		return true
	}
	return claimed
}

func (s *Server) signReservation(r firewall.Reservation) (string, error) {
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return "", err
	}
	claim := reservationClaim{R: r, Nonce: hex.EncodeToString(nb[:]), Exp: time.Now().Add(s.reservationTTL()).Unix()}
	payload, err := json.Marshal(claim)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.guardSecret)
	mac.Write(payload)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// verifyReservation checks the HMAC (constant time) and returns the claim. valid=false
// on a forged/edited/garbage token; expired is reported separately (a late reconcile
// on an expired lease is a no-op, not an error).
func (s *Server) verifyReservation(token string) (claim reservationClaim, expired, valid bool) {
	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return claim, false, false
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(token[:dot])
	if err != nil {
		return claim, false, false
	}
	sig, err := enc.DecodeString(token[dot+1:])
	if err != nil {
		return claim, false, false
	}
	mac := hmac.New(sha256.New, s.guardSecret)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return claim, false, false
	}
	if err := json.Unmarshal(payload, &claim); err != nil {
		return claim, false, false
	}
	return claim, time.Now().Unix() >= claim.Exp, true
}

type guardReserveResp struct {
	Admitted      bool   `json:"admitted"`
	ReservationID string `json:"reservation_id,omitempty"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	Reason        string `json:"reason,omitempty"`
	BindingNode   string `json:"binding_node,omitempty"`
	RetryAfterSec int    `json:"retry_after_sec,omitempty"`
}

// handleGuardReserve runs the pre-vendor check-and-hold for an externally-asserted
// scope and returns a signed reservation on admit, or the deny verdict (never a
// non-200: the PEP reads `admitted` + `http_status` and rejects its own caller).
func (s *Server) handleGuardReserve(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) { // the PEP is trusted infra; gate like the management API
		return
	}
	var body struct {
		KeySHA256 string `json:"key_sha256"`
		RunID     string `json:"run_id"`
		Estimate  struct {
			USD    float64 `json:"usd"`
			Tokens int     `json:"tokens"`
		} `json:"estimate"`
	}
	if !s.decodeAdmin(w, r, &body) {
		return
	}
	if body.RunID != "" && !validRunID(body.RunID) {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"run_id must be 1-128 chars of [A-Za-z0-9._-]")
		return
	}
	keySHA := strings.ToLower(strings.TrimSpace(body.KeySHA256))
	if keySHA == "" {
		// A PEP must assert WHOSE budget this is; an empty key would collide distinct
		// callers on one empty-owner scope.
		writeError(w, http.StatusBadRequest, "invalid_request_error", "key_sha256 is required")
		return
	}
	if body.Estimate.USD < 0 || body.Estimate.USD > maxGuardEstUSD ||
		body.Estimate.Tokens < 0 || body.Estimate.Tokens > maxGuardEstTokens {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "estimate out of range")
		return
	}

	scopes, killedBy, governed, rerr := s.resolveChain(keySHA, body.RunID)
	if rerr != nil {
		s.log.Error("guard reserve: policy resolution failed", "err", rerr)
		writeError(w, http.StatusInternalServerError, "api_error", "policy resolution failed")
		return
	}
	if governed && killedBy != "" {
		writeJSON(w, http.StatusOK, guardReserveResp{
			Admitted: false, HTTPStatus: http.StatusForbidden, Reason: "killed", BindingNode: killedBy})
		return
	}

	var ticket *firewall.Ticket
	var ferr error
	if governed {
		ticket, ferr = s.firewall.EnterChain(scopes, "", body.Estimate.USD, body.Estimate.Tokens)
	} else {
		// Ungoverned key ⇒ flat enforcement, namespaced by the asserted key sha
		// (promptHash is empty: loop detection needs the prompt and is inline-only).
		ticket, ferr = s.firewall.Enter(keySHA, body.RunID, "", body.Estimate.USD, body.Estimate.Tokens)
	}
	if ferr != nil {
		st, reason, node, retry := guardDeny(ferr)
		writeJSON(w, http.StatusOK, guardReserveResp{
			Admitted: false, HTTPStatus: st, Reason: reason, BindingNode: node, RetryAfterSec: retry})
		return
	}
	// Admitted: the hold PERSISTS (no inline Release) until settle/release or the
	// lease self-heals. Capture its reconcile state into a signed token.
	resv := ticket.Reservation()
	if !resv.Shared && len(resv.ScopeKeys) > 0 {
		// The shared reserve DEGRADED to a LOCAL hold (Redis unreachable at reserve
		// time). A local hold has no TTL reaper, so persisting it past this call would
		// leak concurrency/scope (the M2 defect, re-manifested under an outage). Fail
		// OPEN like the inline path: release the hold NOW (while we still hold the
		// ticket) and hand back a no-op reservation, so the PEP's settle/release still
		// succeed but no hold is stranded.
		ticket.Release()
		resv = firewall.Reservation{}
	}
	tokenID, err := s.signReservation(resv)
	if err != nil {
		ticket.Release() // idempotent; couldn't hand back a handle → don't leak the hold
		writeError(w, http.StatusInternalServerError, "api_error", "could not issue reservation")
		return
	}
	writeJSON(w, http.StatusOK, guardReserveResp{Admitted: true, ReservationID: tokenID})
}

// handleGuardSettle reconciles a reservation to actual usage and frees its hold.
func (s *Server) handleGuardSettle(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		ReservationID string `json:"reservation_id"`
		Actual        struct {
			USD    float64 `json:"usd"`
			Tokens int     `json:"tokens"`
		} `json:"actual"`
	}
	if !s.decodeAdmin(w, r, &body) {
		return
	}
	claim, expired, valid := s.verifyReservation(body.ReservationID)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid reservation_id")
		return
	}
	// Expired lease already self-healed (window drained / hold reaped); a late settle
	// must NOT re-apply. Idempotency: only the first reconcile for a nonce applies.
	if expired || !s.claimReconcile(claim.Nonce) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": false})
		return
	}
	// Clamp actual to >=0: a negative actual would drive a shared counter's current
	// slot below the reservation's own contribution, freeing other callers' spend.
	actualUSD := body.Actual.USD
	if actualUSD < 0 {
		actualUSD = 0
	}
	actualTokens := body.Actual.Tokens
	if actualTokens < 0 {
		actualTokens = 0
	}
	s.firewall.SettleReservation(claim.R, actualUSD, actualTokens)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": true})
}

// handleGuardRelease unwinds a reservation whose call never billed.
func (s *Server) handleGuardRelease(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		ReservationID string `json:"reservation_id"`
	}
	if !s.decodeAdmin(w, r, &body) {
		return
	}
	claim, expired, valid := s.verifyReservation(body.ReservationID)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid reservation_id")
		return
	}
	if expired || !s.claimReconcile(claim.Nonce) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": false})
		return
	}
	s.firewall.ReleaseReservation(claim.R)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": true})
}

// guardDeny maps a firewall rejection to the decision-API verdict fields.
func guardDeny(err error) (httpStatus int, reason, bindingNode string, retryAfterSec int) {
	var ve *firewall.VelocityError
	var ce *firewall.ConcurrencyError
	switch {
	case errors.Is(err, firewall.ErrKilled):
		return http.StatusForbidden, "killed", "", 0
	case errors.As(err, &ve):
		return http.StatusTooManyRequests, "velocity", ve.Scope, ve.RetryAfterSec
	case errors.As(err, &ce):
		return http.StatusTooManyRequests, "concurrency", ce.Scope, 0
	default:
		return http.StatusTooManyRequests, "blocked", "", 0
	}
}
