# Durable Postgres ledger (Phase 5 increment 2) — security review

**Scope:** `internal/pgledger`, config/main wiring, secret handling.
**Initial verdict:** FAIL (secret in config + unenforced TLS). **Post-fix:** both
must-fixes resolved.

## Verified clean
- **No SQL injection.** `schema` is a compile-time const; the only identifier is
  `pgx.Identifier{"spend"}` (safely quoted) + a static column list; `user`/`run_id`
  flow only as CopyFrom binary VALUES, never identifiers, never concatenated.
- **No credential leak on failure.** No path logs the DSN; connect errors are
  surfaced generically (main returns "could not connect to the configured
  database", not the wrapped pgx error). The drop counter carries no secret.
- **Best-effort durability honestly documented** (package doc, Sink contract,
  config example) — operators aren't led to assume exactly-once.

## Must-fix → resolution
| # | Finding | Resolution |
|---|---------|-----------|
| 1 | DB password embedded in the config file (`Ledger.DatabaseURL`) violates Invariant #4 (secrets never in config files). | Replaced with `database_url_env` — a NAME pointing at an env var, resolved via `os.Getenv` in main (mirrors `api_key_env`). Config example updated; the secret never touches the file. |
| 2 | pgx defaults to `sslmode=prefer` → silent plaintext fallback for a store holding client attribution + costs. | main WARNs at startup when the DSN lacks `sslmode=require`/`verify-*`; config example documents setting `sslmode=require` for remote databases. (Not force-required, so localhost dev still works.) |

## Nits → resolution
- **NUL in `user` poisons a whole batch** (Postgres TEXT rejects 0x00 → CopyFrom
  fails → up to 256 dropped; 256:1 amplification) → `cleanText` strips NUL from
  string columns at the persistence boundary (no-op for the common case).
- **Unbounded durable growth + PII retention** (client `user` can be PII, stored
  indefinitely, one row/request) → documented as an operator responsibility
  (retention/partitioning) in the config example.
