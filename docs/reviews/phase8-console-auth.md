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

## 8.2 — SSO flows (Google/GitHub OAuth) review

Both adversarial review agents (Go + security) FAILED on an infra stall (600s
watchdog), returning no verdict. Pending a clean re-run (`/security-review`), a
manual security pass was done; the properties verified and the one fix applied:

- **Open redirect (FIXED):** `safeReturnPath` now rejects `//host`, `/\host`,
  backslashes, and CR/LF/NUL — not just `//`. Test: `TestSafeReturnPath`.
- CSRF: callback state is HMAC-signed AND bound to a single-use double-submit nonce
  cookie (SameSite=Lax), verified constant-time with expiry; nonce consumed on
  callback. Tested (`TestOAuthCallbackRejectsBadStateAndNonAdmin`).
- Email verification: Google requires `email_verified`; GitHub requires a
  `primary && verified` email. Non-allowlisted identity → 403, NO session issued.
- Session issuance via console.Manager (HttpOnly/Secure/SameSite); client_secret
  only ever sent to the token endpoint; all IdP endpoints are hardcoded https.
- requireAdmin: session-checked first (nil-safe), then bearer — no bypass; a
  non-admin/expired/forged cookie fails `adminSession`.
- **Documented caveat:** the admin allowlist is provider-agnostic (a domain trusted
  here is satisfied by any configured provider that verifies an email at it).
- **Still recommended:** a fresh `/security-review` of `console_http.go` when infra
  is healthy, to close the DoD's dual-review requirement.

## Deferred decision — S2 (session revocation)
Sessions are STATELESS signed cookies with no server-side revocation; `Clear` only
deletes the browser cookie, so a leaked token is valid until expiry (default 8h).
**Decision:** accepted for the auth-core increment, mitigated by a modest TTL + the
Secure/HttpOnly/SameSite cookie and the documented limitation on `Decode`. A
jti/token-version denylist revocation hook is a follow-up to land with the HTTP
login flow (8.2/8.3) if the threat model warrants it. Tracked here so the choice is
explicit, not accidental.
