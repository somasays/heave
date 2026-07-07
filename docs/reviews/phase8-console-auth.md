# Phase 8.1 — console auth core (`internal/console`) review record

Two adversarial reviews of the stdlib-only auth layer (session cookies, PBKDF2
local accounts, SSO admin allowlist, OAuth state/CSRF). Both **PASS-WITH-NITS**,
no must-fixes — the core crypto was verified free of auth bypasses (empty-digest,
alg-confusion, domain-suffix, and enumeration/timing probes all came back safe).

## Go review — PASS-WITH-NITS → folded
- SF1 domain-allowlist normalization ran `TrimPrefix("@")` before `TrimSpace`, so a
  padded config domain silently keyed wrong (fail-closed). Fixed: trim → lower →
  strip. Test: `TestDomainAllowlistNormalizesWhitespace`.
- SF2 missing positive-admin assertion. Added: `NewSSOSession` admin path asserted.
- Nits: `NewSSOSession` subject now trimmed to match `AdminForEmail`; ignored
  `json.Marshal` error documented as safe-for-fixed-shape.

## Security review — PASS-WITH-NITS → folded
- S1 (Secure-by-default): the cookie `Secure` flag defaulted to false. Fixed —
  cookies are Secure by DEFAULT; `Options.AllowInsecure` is an explicit dev-only
  opt-out.
- S3 (work-factor floor): `VerifyPassword` accepted any `iter>=1` / any digest len.
  Fixed — floors `minPBKDF2Iter=100_000`, `minDKLen=16` (floors, not equalities).
  Test: `TestVerifyPasswordRejectsWeakWorkFactor`.
- Caller-responsibility items documented in-code: open-redirect via the state
  `redirect` (SignState warns — caller must pass a sanitized same-origin path);
  state replay within TTL (double-submit nonce is the caller's to clear).

## Deferred decision — S2 (session revocation)
Sessions are STATELESS signed cookies with no server-side revocation; `Clear` only
deletes the browser cookie, so a leaked token is valid until expiry (default 8h).
**Decision:** accepted for the auth-core increment, mitigated by a modest TTL + the
Secure/HttpOnly/SameSite cookie and the documented limitation on `Decode`. A
jti/token-version denylist revocation hook is a follow-up to land with the HTTP
login flow (8.2/8.3) if the threat model warrants it. Tracked here so the choice is
explicit, not accidental.
