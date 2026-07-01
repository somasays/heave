# Go Style & Conventions

House style for this repo. Most of it is enforced mechanically by
`.golangci.yml` (run inside `make check`); the rest is review-enforced. When a
rule here can be a linter rule, it must be — prose is the fallback, not the
mechanism (article lever #12). The enforcement column in `docs/INVARIANTS.md`
says which is which.

## Formatting
- `gofmt` is law; the pipeline rejects unformatted code. No exceptions, no
  hand-tuned alignment that `gofmt` would undo.
- Group imports: stdlib, then third-party, then this module. `gofmt`/`goimports`
  handles the ordering.

## Naming
- Packages: short, lower-case, no underscores, no `util`/`common`/`helpers`
  grab-bags. A package name is a prefix for its exports (`ledger.Record`, not
  `ledger.LedgerRecord`).
- Exported identifiers carry a doc comment starting with the identifier name.
- Receivers: short and consistent per type (`s *Server`, `a *Anthropic`).
- Acronyms keep case: `ID`, `URL`, `HTTP`, `API` (`requestID`, `baseURL`).

## Errors
- Return errors; do not `panic` in library code. `panic` is allowed only for
  truly-unreachable invariants, never for control flow or bad input.
- Wrap with context using `%w`: `fmt.Errorf("read config: %w", err)`. The wrap
  text is lower-case, no trailing punctuation, and adds *what failed*, not a
  restatement of the callee.
- Inspect errors with `errors.Is` / `errors.As`, never string matching.
- Upstream/vendor failures cross the provider boundary as `*provider.Error` so
  callers can preserve HTTP status provenance (Invariant #7).
- Check every returned error. Deliberate ignores are explicit: `_ = f()` with a
  reason, only where failure is genuinely irrelevant.

## Context
- Any function that does I/O or can block takes `ctx context.Context` as its
  first parameter. Never store a context in a struct.
- Propagate the caller's context to downstream calls; do not create a
  `context.Background()` mid-request. The single request deadline is imposed once
  in the server and flows down.

## Logging & output
- Structured logging only, via `log/slog`. No `fmt.Print*`, no `log.Print*` in
  library code (enforced by `forbidigo`). Handlers and adapters receive a
  `*slog.Logger`; they do not reach for a package-global.
- Log key/value pairs, not interpolated sentences, so logs stay queryable
  (article lever #9). Never log secrets, API keys, or full prompt bodies.

## State & construction
- No package-level mutable state (enforced by `gochecknoglobals`). Constants and
  small pure lookup tables are fine. Dependencies are passed in via constructors
  (`New(...)`), not reached through globals.
- Only `cmd/gateway` wires concrete implementations together (reads config,
  constructs providers/router/ledger/server). Library packages accept
  interfaces/values; they do not self-wire.

## HTTP
- Every outbound HTTP request is built with `http.NewRequestWithContext`
  (enforced by `noctx`) and its response body is always closed and bounded with
  `io.LimitReader` (enforced by `bodyclose`).
- Inbound bodies are size-capped before decode; handlers never trust
  client-supplied sizes.

## Concurrency
- Guard shared mutable state with a mutex or a channel; the race detector runs in
  the test gate (`go test -race`) and must stay clean.
- A goroutine has a clear owner and a clear exit (context cancellation or a
  closed channel). No fire-and-forget goroutines without a lifecycle.

## Tests
- Tests are hermetic: no network, no real API keys, no wall-clock sleeps beyond
  small deterministic timeouts. Use `httptest` for HTTP boundaries and fakes for
  the `Provider` interface.
- Table-driven where it reduces duplication; one clear assertion focus per test.
- The request path (handlers, translation, error mapping, config validation) must
  stay covered — a regression there should fail CI, not production.

## Comments
- Every package has a package doc comment explaining its responsibility and any
  invariant it upholds (with the `docs/INVARIANTS.md` number where relevant).
- Comment *why*, not *what*. Load-bearing comments explain a constraint a future
  reader would otherwise violate (e.g. "Anthropic rejects empty text blocks").
