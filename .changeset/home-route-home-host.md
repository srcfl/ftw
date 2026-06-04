---
"forty-two-watts": minor
---

Owner remote access — single-home **`home.fortytwowatts.com`** cutover. The relay
gains `-home-host` / `-home-site` flags that forward a bare host (e.g.
`home.fortytwowatts.com`) verbatim to the single owner Pi, so the dashboard loads
at the clean root URL with working absolute asset paths (no `/me/<site_id>`
prefix). The Pi auth-gate is refined to keep static assets (CSS/JS/images) public
so the login page renders styled, while `/api/*` and the dashboard HTML shell stay
gated.
