package api

import (
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
)

// The router owns eligibility. These hints only describe queries; they never
// decide which Read routes a connected assistant can reach.
var agentAPIHints = []protocol.AgentAPIRoute{
	{Path: "/api/status", Description: "Current measurements, control state and freshness"},
	{Path: "/api/health", Description: "Box health"},
	{Path: "/api/system/info", Description: "Runtime and host diagnostics, including host details"},
	{Path: "/api/storage/inventory", Description: "Storage use and retention"},
	{Path: "/api/components", Description: "Component versions and state"},
	{Path: "/api/components/history", Description: "Component changes"},
	{Path: "/api/drivers", Description: "Driver diagnostics by configured name"},
	{Path: "/api/data-sources", Description: "Available data sources and freshness"},
	{Path: "/api/battery_models", Description: "Learned battery model and efficiency"},
	{Path: "/api/loadmodel", Description: "Load forecast model and learning"},
	{Path: "/api/thermal/twins", Description: "Learned thermal twins of room sensors; shadow only, nothing controls from them"},
	{Path: "/api/pvmodel", Description: "Solar forecast model and learning"},
	{Path: "/api/forecast", Description: "Load and solar forecasts", Query: map[string]string{"range": "Time ahead, such as 24h"}},
	{Path: "/api/prices", Description: "Stored import and export prices", Query: map[string]string{"range": "Time ahead, such as 24h", "since_ms": "Unix milliseconds", "until_ms": "Unix milliseconds"}},
	{Path: "/api/planner/prefs", Description: "Planner preferences"},
	{Path: "/api/mpc/plan", Description: "Full current plan and per-asset slots"},
	{Path: "/api/mpc/diagnose", Description: "Current planner inputs, constraints and result"},
	{Path: "/api/mpc/diagnose/history", Description: "Recorded planning decisions", Query: map[string]string{"since": "Unix milliseconds", "until": "Unix milliseconds", "limit": "Maximum snapshots, up to 5000"}},
	{Path: "/api/mpc/diagnose/at", Description: "The planning snapshot active at an incident", Query: map[string]string{"ts": "Required incident time, Unix milliseconds"}},
	{Path: "/api/history", Description: "Recorded site measurements", Query: map[string]string{"range": "Time back, such as 1h", "points": "Maximum rows"}},
	{Path: "/api/energy/daily", Description: "Daily measured energy and costs", Query: map[string]string{"days": "Number of days"}},
	{Path: "/api/energy/assets", Description: "Energy-ledger assets by configured name"},
	{Path: "/api/energy/history", Description: "Energy-ledger entries in Wh, including flow, source, quality, provenance and sample count", Query: map[string]string{"scope": "system or asset", "asset_id": "Asset id from /api/energy/assets", "since": "Unix milliseconds", "until": "Unix milliseconds", "bucket": "Bucket duration, at least 5m", "limit": "Maximum buckets, up to 5000"}},
	{Path: "/api/savings/daily", Description: "Daily cost comparison and coverage", Query: map[string]string{"days": "Number of days"}},
	{Path: "/api/series/catalog", Description: "Stored drivers, metric names and units"},
	{Path: "/api/series", Description: "One driver's measured series with min, max, sample count, resolution and bounds", Query: map[string]string{"driver": "Required driver name from /api/series/catalog", "metric": "Required metric name or comma-separated names from the catalogue", "range": "Time back, such as 6h", "since": "Unix milliseconds", "until": "Unix milliseconds", "points": "Maximum points per metric"}},
}

// These GET handlers return diagnostic data despite their Local tier for
// phone passthrough. OAuth GETs start or complete a credential exchange and
// are not reads. Configure and Actuate handlers keep their command path.
var agentLocalReads = map[string]bool{
	"/api/config":              true,
	"/api/setup/draft":         true,
	"/api/drivers/{id}/source": true,
	"/api/drivers/{name}/logs": true,
	"/api/logs":                true,
	"/api/support/dump":        true,
	"/api/support/report":      true,
	"/api/ocpp/chargers":       true,
	"/api/backups/{id}":        true,
	"/api/agents":              true,
	"/api/agent-invites":       true,
	"/api/agents/audit":        true,
}

func agentReadMark(pattern string, mark routeMark) bool {
	fields := strings.Fields(pattern)
	return len(fields) == 2 && fields[0] == http.MethodGet && !mark.static &&
		(mark.tier == apiauth.TierRead || mark.tier == apiauth.TierLocal && agentLocalReads[fields[1]])
}

func (s *Server) agentReadAllowed(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	_, pattern := s.mux.Handler(r)
	mark, ok := s.marks[pattern]
	return ok && agentReadMark(pattern, mark)
}

func (s *Server) agentReadCatalog() []protocol.AgentAPIRoute {
	routes := make([]protocol.AgentAPIRoute, 0, len(s.marks))
	for pattern, mark := range s.marks {
		if !agentReadMark(pattern, mark) {
			continue
		}
		path := strings.Fields(pattern)[1]
		route := protocol.AgentAPIRoute{Path: path, Scope: protocol.ScopeDiagRun, Description: "Read " + path}
		for _, hint := range agentAPIHints {
			if hint.Path == path {
				route.Description, route.Query = hint.Description, hint.Query
				break
			}
		}
		if query, ok := agentAPIQueryHints[path]; ok {
			route.Query = query
		}
		routes = append(routes, route)
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Path < routes[j].Path })
	return routes
}

var agentAPIQueryHints = map[string]map[string]string{
	"/api/components/history":        {"id": "Component id", "kind": "Component kind", "limit": "Maximum changes"},
	"/api/data-sources":              {"lat": "Latitude", "lon": "Longitude"},
	"/api/device_repository/catalog": {"channel": "Driver channel"},
	"/api/notifications/history":     {"limit": "Maximum notifications"},
	"/api/logs":                      {"limit": "Maximum log entries, up to 5000"},
	"/api/drivers/{name}/logs":       {"limit": "Maximum log entries, up to 2000"},
	"/api/ev/status":                 {"driver": "Configured driver name"},
	"/api/research/load/dump":        {"days": "Days of original observations, 1 to 365; tar.gz export"},
	"/api/agents/audit":              {"before": "Audit sequence before which to page", "n": "Maximum audit rows"},
	"/api/series":                    {"driver": "Configured driver name", "metric": "Metric or comma-separated metrics from /api/series/catalog", "range": "Time back, such as 6h", "since": "Unix milliseconds", "until": "Unix milliseconds", "points": "Maximum points per metric; 0 returns stored rows", "format": "json (default) or csv"},
	"/api/energy/history.csv":        {"scope": "system or asset", "asset_id": "Asset id from /api/energy/assets", "since": "Unix milliseconds", "until": "Unix milliseconds", "bucket": "Bucket duration, at least 5m", "limit": "Maximum buckets, up to 5000"},
	"/api/energy/battery-return":     {"year": "Calendar year in Swedish time, 2018 to now; default last year"},
	"/api/energy/battery-return.csv": {"year": "Calendar year in Swedish time, 2018 to now; default last year"},
}

// This context is requested for technical debugging, not for routine status,
// history or control. Source links name a recorded build or a declared
// release; a default-branch fallback says explicitly that it may differ.
func (s *Server) agentDebugContext() map[string]any {
	ref, kind, revision, modified := "master", "default branch; may differ from the running build", "", false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value
			}
			if setting.Key == "vcs.modified" {
				modified = setting.Value == "true"
			}
		}
	}
	if revision != "" {
		ref, kind = revision, "recorded build revision"
	}
	if regexp.MustCompile(`^v?0\.[0-9]+\.[0-9]+(?:-beta\.[0-9]+)?$`).MatchString(s.deps.Version) {
		ref, kind = "v"+strings.TrimPrefix(s.deps.Version, "v"), "declared release tag"
	}
	tree := "https://github.com/srcfl/ftw/tree/" + url.PathEscape(ref)
	out := map[string]any{
		"core": map[string]any{"version": s.deps.Version, "revision": revision, "modified": modified, "source_ref_kind": kind,
			"repository": "https://github.com/srcfl/ftw", "source": tree,
			"api": tree + "/go/internal/api", "energy_history": tree + "/go/internal/state", "planner": tree + "/go/internal/mpc",
			"driver_host": tree + "/go/internal/drivers", "bundled_driver_pin": tree + "/drivers/BUNDLED_SOURCE.json", "issues": "https://github.com/srcfl/ftw/issues"},
		"related_sources": []map[string]string{
			{"part": "Editable Lua drivers", "repository": "https://github.com/srcfl/device-drivers", "running_source": "Read /api/drivers/catalog, /api/drivers/{id}/source and /api/device_repository/drivers/{id}/versions for installed code and hashes"},
			{"part": "FTW Cloud and MCP", "repository": "https://github.com/srcfl/ftw-cloud", "access": "Private repository since 10 November 2026; FTW Cloud serves no source"},
			{"part": "Web client", "repository": "https://github.com/srcfl/ftw-webapp", "access": "Private repository since 10 November 2026", "earlier_source": "https://github.com/srcfl/ftw-webapp-source-archive"},
			{"part": "Native apps", "repository": "https://github.com/srcfl/ftw-app", "access": "Private repository since 10 November 2026", "earlier_source": "https://github.com/srcfl/ftw-app-source-archive"},
			{"part": "Client protocol", "repository": "https://github.com/srcfl/ftw", "source": "https://github.com/srcfl/ftw/blob/master/docs/protocol.md", "contracts": "https://github.com/srcfl/ftw/tree/master/contract"},
			{"part": "Compiled Energyplan solver", "repository": "https://github.com/srcfl/energyplan", "access": "Private repository; Core carries binaries and validates their plans"},
		},
		"workflow": []string{
			"Record the symptom, expected behavior, incident time and timezone. Separate measured facts from guesses.",
			"Read /api/health, /api/support/report and /api/components for versions, clock, freshness and failures.",
			"Use /api/energy/history for Wh and provenance, /api/series for original measurements, and /api/mpc/diagnose/history then /api/mpc/diagnose/at for the plan active at the incident.",
			"Inspect configuration, driver details, driver source and logs as needed. Compare with source for the running version; default-branch code may differ.",
			"For charger faults, read charger_connections below and use /api/config, /api/drivers and /api/loadpoints to compare loadpoints[].driver_name with drivers[].name. If OCPP is enabled, also check /api/ocpp/chargers. A configured name does not prove online equipment or successful control.",
			"Basic config_get and drivers_status hide technical names and cannot prove these bindings. Use these full diagnostic API reads. If a read is refused, record the path, refusal and missing grant or version; do not infer a driver fault from unavailable or redacted evidence.",
			"For a bug report, include the Core version and source reference, equipment and driver versions, reproduction steps, expected and observed results, incident time, exact API queries and relevant logs or snapshots. Mark data gaps and whether evidence came from real equipment, a simulator or a test.",
			"Use the existing control and goal tools when an action is needed. Reading a diagnostic view does not issue a command. Prepare a bug report for the user; do not publish it without their request.",
		},
	}
	if s.deps.Cfg != nil {
		if s.deps.CfgMu != nil {
			s.deps.CfgMu.RLock()
			defer s.deps.CfgMu.RUnlock()
		}
		out["charger_connections"] = s.deps.Cfg.LoadpointConnections()
	}
	return out
}
