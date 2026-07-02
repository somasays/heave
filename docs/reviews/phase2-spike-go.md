# Phase 2 spike — Go expert review (adversarial)

**Scope:** `internal/cache`, `internal/cachebench`, `cmd/cachebench`.
**Verdict:** pass-with-follow-ups. `-race`/`vet` clean; no crashers on the
exercised paths.

| # | Finding | Sev | Resolution |
|---|---------|-----|-----------|
| 1 | The "honesty" regret assertion was vacuous: naive regret is structurally 0 (idealIndex == naive choice), so `cache.RegretRate < naive.RegretRate` reduced to `< 0` and could never fail | MED | Fixed — test now asserts `naive.RegretRate == 0` AND `cache.RegretRate > 0`, plus a non-empty loss tail |
| 2 | Empty `Models` slice → index-out-of-range panic in `Simulate`/`byDifficulty` | LOW-MED | Fixed — `Simulate`/`Compare` guard `len(Models)==0`; `byDifficulty` guards `<=1`; `TestEmptyModelsNoPanic` |
| 3 | `ConversationKey` NUL-separator framing non-injective if a payload contains NUL | LOW | See note — kept NUL framing (UTF-8 chat text never contains NUL); a length-prefixed framing is the follow-up if binary payloads are ever keyed |
| 4 | Test coverage gaps (TTL boundary, regret>0, per-conv cost) | LOW | Added partial-hit, min-floor, empty-models, and loss-tail tests; determinism test now compares both routers |
| 5 | Float FMA contraction could flake a golden cost assertion cross-platform | LOW/latent | No action — all assertions are relative, none pin an exact cost |

**Confirmed correct by the reviewer:** `cache.Store` mutex discipline and TTL
semantics; prefix accumulation (system charged once, current turn is new input,
prior turns are the cached prefix); first turn always cold; `jitter`/`Intn`
bounds; division guards. Follow-up (#3 length-prefix framing) tracked.
