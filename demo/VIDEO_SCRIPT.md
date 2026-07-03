# heave — 90-second launch video script

**Goal:** one idea lands — *a runaway agent, stopped before the vendor is billed.*
No talking head needed; a screen recording of the terminal + two captions does it.
Record the terminal at ~100×30, large font. Music: low, tense → resolve.

---

### 0:00–0:10 — The hook (caption over a black title card)

> **Caption:** "An AI agent can burn five figures before you wake up."
> **Caption (fade):** "Monthly budgets don't stop it. Failover after a 429 doesn't
> stop it."
> **Title:** `heave — a spend & quota firewall for AI agents`

Voiceover (optional, one line): *"Agents don't spend money like apps do. When they
go wrong, they go wrong fast."*

### 0:10–0:22 — Set the scene (terminal)

Type and run:

```
$ export ANTHROPIC_API_KEY=sk-ant-...
$ ./demo/runaway.sh
```

> **Caption:** "Firewall ON — kill a run after 3 identical calls, or $0.01."

Show the two startup lines (`building…`, `starting heave…`). Keep it snappy.

### 0:22–0:48 — The money shot (terminal, let it breathe)

The runaway loop prints, line by line. Slow the recording slightly here:

```
  a runaway agent starts looping the same request on run 'agent-run-7':
  call 1 → 200 OK         (reached the vendor, billed)
  call 2 → 200 OK         (reached the vendor, billed)
  call 3 → 403 RUN KILLED (refused PRE-vendor — $0)
  call 4 → 403 RUN KILLED (refused PRE-vendor — $0)
  ...
  call 8 → 403 RUN KILLED (refused PRE-vendor — $0)
```

> **Caption (on call 3):** "Killed. Every call after this costs $0 — it never
> reaches the vendor."

Let calls 3–8 scroll. The red `403 RUN KILLED` repeating is the visual.

### 0:48–1:02 — Prove it (terminal)

```
  /v1/stats: billed 2 vendor calls, $0.00008 — then the firewall stopped it
             live kills recorded: 1
```

> **Caption:** "Without heave, all 8 calls bill. With heave, the loss is bounded
> by *your control* — not by how long the runaway ran."

Cut to a 2-second flash of the counterfactual table (OFF: 8 calls · ON: 2, then $0).

### 1:02–1:20 — What it is (fast montage of captions over the /dashboard)

> **Caption:** "Hard, real-time, PRE-vendor."
> **Caption:** "Velocity caps · per-run kill switch · per-run $ budget ·
> provider-quota brokering."
> **Caption:** "Holds across a replica fleet. Every request attributed by run."

Show the built-in `/dashboard` for 3 seconds (top spend by run, live kills).

### 1:20–1:30 — The close (title card)

> **Title:** `heave`
> **Caption:** "Self-hostable. OpenAI-compatible. One Go binary. Apache-2.0."
> **Caption:** "github.com/somasays/heave"
> **Caption (small):** "Built to validate *Engineering for AI Agents*."

---

## Production notes

- **The single most important frame** is call 3 flipping to `403 RUN KILLED`. If
  you cut anything, protect that beat.
- Record with `asciinema` (crisp, small) for an embeddable player, and export a
  short **GIF of 0:22–0:48** for the README/tweet — the GIF alone should sell it.
- Keep captions high-contrast, sans-serif, bottom third. No jargon in captions.
- Total ≤ 90s. The 15-second GIF (loop kill only) is the version that travels on
  social.
