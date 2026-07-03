package server

import _ "embed"

// dashboardHTML is the self-contained built-in dashboard (no external assets), a
// read-only operational view of the spend firewall served at GET /dashboard. It
// polls GET /v1/stats. Kept as a separate asset so it can evolve without touching
// handler code.
//
//go:embed dashboard.html
var dashboardHTML []byte
