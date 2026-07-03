# demo

Watch heave's spend firewall kill a runaway agent, pre-vendor.

```bash
export ANTHROPIC_API_KEY=sk-ant-...   # a Claude key; the demo uses cheap haiku calls
./demo/runaway.sh
```

It builds heave, starts it with [`config.yaml`](config.yaml) (firewall ON,
`loop_threshold=3`, `max_usd_per_run=$0.01`), then loops the same request on one
run. Calls 1–2 reach the vendor; the run is auto-killed and calls 3–8 are refused
with `403` **before** they reach the vendor — so they cost `$0`.

- [`config.yaml`](config.yaml) — the demo gateway config (throwaway demo key).
- [`runaway.sh`](runaway.sh) — the runaway agent + the reveal.
- [`VIDEO_SCRIPT.md`](VIDEO_SCRIPT.md) — 90-second launch video script.

The demo bearer is `heave-demo-key` (a throwaway — never use it in production).
