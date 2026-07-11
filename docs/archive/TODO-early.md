# Archived early TODO

Archived on 2026-06-05. This file is kept only as historical context from
the early Rust/redb era. It is not the active roadmap and should not be used
for release planning. Use GitHub issues, release notes, and the current docs
instead.

Original heading:

# TODO — forty-two-watts 🐬

## Short term

- [ ] **Per-battery config in UI** — SoC min/max limits, max charge/discharge power, priority weight — editable from dashboard. Currently these are hardcoded constants in control.rs
- [ ] **Energy accumulation** — cumulative kWh counters (import/export/PV/charge/discharge) integrated from power over time, displayed as "today" and "all-time". Use the history data we already store
- [ ] **Chart range fix** — when selecting 1h/6h/24h/3d, `CHART_POINTS` constant (60) limits the chart. Make chart accept any N points from the history endpoint
- [ ] **Persist peak_limit and ev_charging across restarts** — currently only mode is restored from redb

## Medium term

- [ ] **More Lua drivers** — documented template in docs/lua-drivers.md. Test with additional inverters/batteries (SMA, Huawei, Pixii, etc.)
- [ ] **Systemd service** — auto-start on RPi boot, restart on crash, log to journald
- [ ] **MPC controller** — replace PI with Model Predictive Control (`clarabel` or `osqp` crate) for constraint-aware optimization that plans N steps ahead
- [ ] **Decouple measurement from control** — drivers push async telemetry with timestamps, control loop runs on fixed timer, Kalman provides best estimate at each tick
- [ ] **CI/CD** — GitHub Actions workflow: build static musl binaries for arm64+amd64 on tag push, auto-create release
- [ ] **Load display stability** — improve Kalman filter tuning for load calculation during battery transients

## Ideas / future

- [ ] **Price-aware charging** — integrate Nordpool tariff, charge during cheap hours, discharge during expensive hours
- [ ] **Weather-aware forecasting** — pull solar forecast, pre-charge battery before cloudy days
- [ ] **Multi-site** — one home-ems managing several physical sites via remote agents
- [ ] **Alerting** — push notifications on driver failure, critical SoC, etc.
- [ ] **WebSocket live updates** — replace 5s polling with push for instant updates
- [ ] **Docker Hub image** — publish multi-arch Docker image so people can just `docker run ghcr.io/frahlg/forty-two-watts`
- [ ] **Grafana exporter** — Prometheus endpoint for long-term metrics

## Done

- [x] PI controller (Kp=0.5, Ki=0.1) with anti-windup
- [x] 1D Kalman filter per DER signal (auto-adaptive)
- [x] Lua driver system (ferroamp.lua + sungrow.lua verified on hardware)
- [x] Anti-oscillation: slew rate 500W/cycle, 5s holdoff, 42W deadband
- [x] Fuse guard (16A shared breaker)
- [x] 6 dispatch modes: idle, self_consumption, peak_shaving, charge, priority, weighted
- [x] EV charging signal — batteries ignore EV load
- [x] Per-battery dispatch fix — synchronized proportional split (was drifting)
- [x] REST API: status, mode, target, peak_limit, ev_charging, drivers, history, health
- [x] Web dashboard: summary cards with fuse gauge, compact controls, chart with hover tooltip
- [x] Chart range selector (5m / 15m / 1h / 6h / 24h / 3d)
- [x] Telemetry history in redb (3-day retention, auto-prune, bucket downsampling)
- [x] Home Assistant MQTT autodiscovery (mode selector, grid target, peak limit, EV charging, all sensors)
- [x] HA mode selector fix (snake_case)
- [x] redb state persistence for crash recovery (mode restored on startup)
- [x] Static musl binaries (linux/arm64 + linux/amd64) via Docker
- [x] GitHub release v0.1.0 with downloadable binaries
- [x] Deployed to RPi (192.168.192.40) running autonomously
- [x] Douglas Adams theme throughout 🐬
