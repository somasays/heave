# Security policy

heave is a security-adjacent control (it enforces spend/quota limits and holds
API keys and spend attribution), so we take reports seriously.

## Reporting a vulnerability
**Please do not open a public issue for a security vulnerability.**

- Preferred: open a [GitHub private security advisory](https://github.com/somasays/heave/security/advisories/new)
  (Security → Report a vulnerability).
- Or email **somasundaramide@gmail.com** with `heave security` in the subject.

Please include a description, affected version/commit, and a reproduction if you
have one. We'll acknowledge within a few days and keep you updated on the fix and
disclosure timeline.

## Scope — what's in
- Bypasses of the firewall's enforcement (velocity/kill/budget/concurrency),
  including cross-replica correctness under the documented Redis mode.
- Cross-tenant leakage (spend/attribution, run scoping, the observability
  endpoints).
- Secret handling (API keys / DB URLs must come from the environment, never config
  or logs — see Invariant #4).
- Auth bypass, injection, or resource-exhaustion in the request path.

## Known, documented limitations (not vulnerabilities)
These are disclosed in the README / `docs/INVARIANTS.md` and are by design:
- The input token **estimate is a heuristic** (chars/4), so a cap can be overshot
  by roughly one in-flight call's error.
- **Loop detection is exact-hash** — a per-turn nonce or growing context defeats
  it; the per-run `$` budget is the intended backstop.
- Enforcement is only meaningful with **auth enabled**; cross-replica velocity/
  concurrency require **Redis** (per-instance otherwise), and the shared store
  **fails open** (degrading to local enforcement) on a Redis outage.

If you think one of these is exploitable *beyond* what's documented, report it.
