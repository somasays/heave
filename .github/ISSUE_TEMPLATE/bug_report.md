---
name: Bug report
about: Something isn't working as documented
title: ''
labels: bug
assignees: ''
---

**What happened**
A clear description of the bug.

**Expected**
What you expected instead.

**Repro**
Steps / a minimal config + request. If it's about enforcement, include the
relevant `firewall:` config and whether `auth` and `redis_url` are set.

```yaml
# config (redact secrets)
```

**Environment**
- heave version / commit:
- Single node or multi-replica (Redis)? Postgres ledger on?
- OS / Go version:

**Logs / output**
```
# relevant logs, /metrics, or the HTTP status you got
```
