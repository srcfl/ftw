# Changelog

## [0.8.0](https://github.com/frahlg/forty-two-watts/compare/v0.7.0...v0.8.0) (2026-04-16)

### Features

* **drivers/zap:** aggregate PV from attached inverters ([fb8ca88](https://github.com/frahlg/forty-two-watts/commit/fb8ca8869bea4cac079f68fd9d66a96e7428aac3))
* **drivers:** add Sourceful Zap meter driver ([f1877cc](https://github.com/frahlg/forty-two-watts/commit/f1877cc5b6abdfc7634fbfb07ccdedc927342144))

### Bug Fixes

* key local-vs-cloud HTTP on connection_defaults.host ([5b30477](https://github.com/frahlg/forty-two-watts/commit/5b3047711d7410ef68dff75280a4f1f262a4a55b)), closes [#76](https://github.com/frahlg/forty-two-watts/issues/76)

## [0.7.0](https://github.com/frahlg/forty-two-watts/compare/v0.6.1...v0.7.0) (2026-04-16)

### Features

* **drivers:** align Solis + Deye control with Zap reference ([#74](https://github.com/frahlg/forty-two-watts/issues/74)) ([281f4df](https://github.com/frahlg/forty-two-watts/commit/281f4dfc8027acfedb9ac8ea7ad6fba290ee30c0))

## [0.6.1](https://github.com/frahlg/forty-two-watts/compare/v0.6.0...v0.6.1) (2026-04-16)

### Bug Fixes

* add HTTP capability support for catalog drivers + clarify grid tariff label ([#75](https://github.com/frahlg/forty-two-watts/issues/75)) ([d4cc95e](https://github.com/frahlg/forty-two-watts/commit/d4cc95e21df5853af82f0f11fd69d762a96f353e))

## [0.6.0](https://github.com/frahlg/forty-two-watts/compare/v0.5.2...v0.6.0) (2026-04-16)

### Features

* EV driver UI + lifecycle controls + creds visibility ([#73](https://github.com/frahlg/forty-two-watts/issues/73)) ([52a482a](https://github.com/frahlg/forty-two-watts/commit/52a482a81701ec0e9da2bdfa94e06ca03f5fa21b))

### Bug Fixes

* 3 P1 + 1 P2 from Codex + UI cleanup ([48e0d28](https://github.com/frahlg/forty-two-watts/commit/48e0d2865beac703805765ab238058565f1e91e7))

### UI

* move EV credentials to Devices tab, remove EV Charger tab ([7cd2d9f](https://github.com/frahlg/forty-two-watts/commit/7cd2d9f3af4a547cf9370c29614607b764d9e59f))

## [0.5.3](https://github.com/frahlg/forty-two-watts/compare/v0.5.2...v0.5.3) (2026-04-16)

### Bug Fixes

* 3 P1 + 1 P2 from Codex + UI cleanup ([48e0d28](https://github.com/frahlg/forty-two-watts/commit/48e0d2865beac703805765ab238058565f1e91e7))

### UI

* move EV credentials to Devices tab, remove EV Charger tab ([7cd2d9f](https://github.com/frahlg/forty-two-watts/commit/7cd2d9f3af4a547cf9370c29614607b764d9e59f))

## [0.5.2](https://github.com/frahlg/forty-two-watts/compare/v0.5.1...v0.5.2) (2026-04-16)

### Bug Fixes

* 4 wizard review bugs — path traversal, /setup route, scan API, skip validation ([#70](https://github.com/frahlg/forty-two-watts/issues/70)) ([f691015](https://github.com/frahlg/forty-two-watts/commit/f691015fe154f59e4ce24914674ea924184f556a))

## [0.5.1](https://github.com/frahlg/forty-two-watts/compare/v0.5.0...v0.5.1) (2026-04-16)

### Bug Fixes

* prevent driver paths from accumulating "../" on each config save ([790429f](https://github.com/frahlg/forty-two-watts/commit/790429f79b56281e5fe5875cc6c51e2d3e05572e))

## [0.5.0](https://github.com/frahlg/forty-two-watts/compare/v0.4.0...v0.5.0) (2026-04-16)

### Features

* add setup wizard frontend (web/setup.html + web/setup.js) ([#66](https://github.com/frahlg/forty-two-watts/issues/66)) ([bc1a285](https://github.com/frahlg/forty-two-watts/commit/bc1a2850e8f15c2d1d6d483be6ed627df7b76f5b))
* bootstrap mode + network scanner for onboarding wizard ([#67](https://github.com/frahlg/forty-two-watts/issues/67)) ([267cef4](https://github.com/frahlg/forty-two-watts/commit/267cef42481ee8515abe0ef26ebb5721650d414e))
* wizard dashboard trigger + driver catalog enrichment ([#68](https://github.com/frahlg/forty-two-watts/issues/68)) ([78c83cf](https://github.com/frahlg/forty-two-watts/commit/78c83cf207bf0664e17dabca6c988fdb6f0e5e81))

## [0.4.0](https://github.com/frahlg/forty-two-watts/compare/v0.3.0...v0.4.0) (2026-04-16)

### Features

* config/UI improvements — kWh display, secure EV password, planner tab ([#65](https://github.com/frahlg/forty-two-watts/issues/65)) ([35ab03d](https://github.com/frahlg/forty-two-watts/commit/35ab03d7b5f63ffcc471bf28e1409d761bf0f7d2))
* Easee Cloud driver + host.http_get/post for Lua drivers ([#56](https://github.com/frahlg/forty-two-watts/issues/56)) ([4cdc942](https://github.com/frahlg/forty-two-watts/commit/4cdc9421590385e8f00301925d590f6fb093ebaf))
* EV charger config + credential masking in API responses ([#58](https://github.com/frahlg/forty-two-watts/issues/58)) ([c22cb80](https://github.com/frahlg/forty-two-watts/commit/c22cb805af960bcafc353846f62e2406fc791e17))

### Bug Fixes

* 5 Go-side P1 bugs from Codex review ([#46](https://github.com/frahlg/forty-two-watts/issues/46)) ([0cd2885](https://github.com/frahlg/forty-two-watts/commit/0cd2885bdb79d6a4c3116bb4930ec785cea8f944))
* 5 Go-side P1 bugs from Codex review ([#47](https://github.com/frahlg/forty-two-watts/issues/47)) ([4f2eaf6](https://github.com/frahlg/forty-two-watts/commit/4f2eaf69f626caddf2bae456ac047301f9a36840))
* address P2 review comments across control, MPC, drivers, and UI ([#64](https://github.com/frahlg/forty-two-watts/issues/64)) ([fcafa88](https://github.com/frahlg/forty-two-watts/commit/fcafa88f12c714a1930342dd9f28ea07d18440c2))
* **ci:** disable @semantic-release/github PR annotation features ([4020d46](https://github.com/frahlg/forty-two-watts/commit/4020d4606e0f81924cca5d0e06f4ab743bf8f1d5)), closes [#32](https://github.com/frahlg/forty-two-watts/issues/32) [#33](https://github.com/frahlg/forty-two-watts/issues/33) [#34](https://github.com/frahlg/forty-two-watts/issues/34) [#35](https://github.com/frahlg/forty-two-watts/issues/35) [#36](https://github.com/frahlg/forty-two-watts/issues/36) [#39](https://github.com/frahlg/forty-two-watts/issues/39)
* **ci:** switch semantic-release to conventionalcommits preset ([7e0bb89](https://github.com/frahlg/forty-two-watts/commit/7e0bb895f7a8f8271033336899bed8639e772dc4))
* **ci:** upgrade GitHub Actions to Node.js 24 (drop deprecated Node 20) ([4005bd8](https://github.com/frahlg/forty-two-watts/commit/4005bd8b982c091bff4dcd428cebbe1a08447242))
* Lua driver Command() reading wrong field — Sungrow ignored targets ([9237156](https://github.com/frahlg/forty-two-watts/commit/923715691d55c9dc5c3058b72271d00a72d9c93a))
* populate EV Charger tab from driver config when ev_charger is empty ([5e6b116](https://github.com/frahlg/forty-two-watts/commit/5e6b11676bc972a2c983d39a345a3b5f8dbc77dc))
* remove dead evSlider event listeners that crash app.js ([8ae76c7](https://github.com/frahlg/forty-two-watts/commit/8ae76c710b4ca2d15eb71399211849c4ce03a4bb))
* replace wonky Catmull-Rom spline with simple linear forecast ([abea431](https://github.com/frahlg/forty-two-watts/commit/abea431d7895504116600384c6a92e9577675607))
* show '...' instead of stale v0.1.0 while JS loads version ([dc65065](https://github.com/frahlg/forty-two-watts/commit/dc65065784cad8c018f64338284b5f4b6441ac22))
* **solaredge_pv:** read SunSpec scale factors every poll, not cached ([#38](https://github.com/frahlg/forty-two-watts/issues/38)) ([26f8793](https://github.com/frahlg/forty-two-watts/commit/26f8793f22888dc11d29fd157b10b4340da34c8d))

### Drivers

* add Eastron SDM630 Lua driver ([#18](https://github.com/frahlg/forty-two-watts/issues/18)) ([d5ad806](https://github.com/frahlg/forty-two-watts/commit/d5ad8066377371eb63f320969d153ece50d1266a))
* add Ferroamp Modbus driver (alt transport to ferroamp.lua) ([#31](https://github.com/frahlg/forty-two-watts/issues/31)) ([03b802c](https://github.com/frahlg/forty-two-watts/commit/03b802cefcd1f4d2e07ad05f493ca5643585ed0c))
* fix 9 P1 bugs flagged by Codex review ([#44](https://github.com/frahlg/forty-two-watts/issues/44)) ([b20e485](https://github.com/frahlg/forty-two-watts/commit/b20e485f5fa0a5a20d3a4e83d49410528f81ea1e))
* port Deye SUN-SG hybrid inverter to 42W v2.1 Lua host ([#29](https://github.com/frahlg/forty-two-watts/issues/29)) ([df8fbc0](https://github.com/frahlg/forty-two-watts/commit/df8fbc006375dfc2a3abeb2bc8ec0f01f3e1d0e1))
* port Fronius GEN24 (SunSpec) to Lua ([#19](https://github.com/frahlg/forty-two-watts/issues/19)) ([c1fc875](https://github.com/frahlg/forty-two-watts/commit/c1fc87559b404aa0429ed8ca0a71539e634cb59d))
* port Fronius Smart Meter (SunSpec Modbus, read-only) ([#24](https://github.com/frahlg/forty-two-watts/issues/24)) ([575895c](https://github.com/frahlg/forty-two-watts/commit/575895c7469283bd139deb481e601068045f7519))
* port GoodWe hybrid inverter (ET-Plus / EH) to Lua v2.1 ([#28](https://github.com/frahlg/forty-two-watts/issues/28)) ([e43d2d9](https://github.com/frahlg/forty-two-watts/commit/e43d2d92ef1a7fd26c65b839944bc8d98fa4915a))
* port Growatt hybrid inverter driver (read-only) ([#20](https://github.com/frahlg/forty-two-watts/issues/20)) ([92524ac](https://github.com/frahlg/forty-two-watts/commit/92524acdd890507873a6d5f54b3b6d4335b8e610))
* port Huawei SUN2000 hybrid inverter ([#15](https://github.com/frahlg/forty-two-watts/issues/15)) ([09a8855](https://github.com/frahlg/forty-two-watts/commit/09a88558d0ae17c7e6bdd26387c663badb55e37b))
* port Kostal Plenticore / Piko IQ (Lua, read-only) ([#21](https://github.com/frahlg/forty-two-watts/issues/21)) ([bdeca96](https://github.com/frahlg/forty-two-watts/commit/bdeca96e6c3e05cfe968e20ceb298221f2be5c84))
* port Pixii PowerShaper battery driver to v2.1 Lua host ([#22](https://github.com/frahlg/forty-two-watts/issues/22)) ([70a96d1](https://github.com/frahlg/forty-two-watts/commit/70a96d1120b2aab2cb12ef49688fe3cb204789e3))
* port SMA hybrid inverter Lua driver ([#23](https://github.com/frahlg/forty-two-watts/issues/23)) ([dd34555](https://github.com/frahlg/forty-two-watts/commit/dd3455577c7a3adebad252f81d40b81d3b982350))
* port Sofar HYD-ES/HYD-EP from hugin to Lua v2.1 ([#26](https://github.com/frahlg/forty-two-watts/issues/26)) ([14f6131](https://github.com/frahlg/forty-two-watts/commit/14f6131952b033381a5501f76265714a2b985f1c))
* port SolarEdge SunSpec inverter + meter to Lua (read-only) ([#30](https://github.com/frahlg/forty-two-watts/issues/30)) ([1007e63](https://github.com/frahlg/forty-two-watts/commit/1007e63f9d1908f3210d9b80037e4a6e05e3fa78))
* port Solis hybrid inverter ([#27](https://github.com/frahlg/forty-two-watts/issues/27)) ([98b2a50](https://github.com/frahlg/forty-two-watts/commit/98b2a50ccf59c45130de951dd22db4fc17a67a1a))
* port Victron Energy GX Modbus driver ([#25](https://github.com/frahlg/forty-two-watts/issues/25)) ([ad71db2](https://github.com/frahlg/forty-two-watts/commit/ad71db269438e7aa6e11c632ba1db10897db81be))

### UI

* add status bar with driver health indicators ([b048d60](https://github.com/frahlg/forty-two-watts/commit/b048d60a57049385c498cc4e592ee049a3a05809))
* EV status card + Easee control commands ([#59](https://github.com/frahlg/forty-two-watts/issues/59)) ([b03749a](https://github.com/frahlg/forty-two-watts/commit/b03749ac9ae670447a201e65ed4a57e0db4e99d8))
* fix summary cards grid for 7 cards + raise side-by-side breakpoint ([6e19973](https://github.com/frahlg/forty-two-watts/commit/6e1997312df8ca5b889000d286d0b0782059b701))
* inline target on hover + driver card + collapsible model cards ([de88f43](https://github.com/frahlg/forty-two-watts/commit/de88f4326e5aa5587b623cde76371c0f410eff27))
* legend wrap + nice-tick y-axis + cleaner chart labels ([#33](https://github.com/frahlg/forty-two-watts/issues/33)) ([aeb1d1c](https://github.com/frahlg/forty-two-watts/commit/aeb1d1cb2ab6d69984cdcd424cb6c3da7d775407))
* remove manual EV charging slider ([063174c](https://github.com/frahlg/forty-two-watts/commit/063174cc259d46185da34bad827c16994a3c6e33))
* show mode band in plan chart + grid target on status card ([877e0bd](https://github.com/frahlg/forty-two-watts/commit/877e0bde83964ddb26ce4894ab0adc446fd7801b))
* smooth Catmull-Rom spline for forecast + 15min forecast zone ([dba51a5](https://github.com/frahlg/forty-two-watts/commit/dba51a54c26e6329a4eca850b81b4a22974efcfd))

### Control loop

* fold live DerEV readings into the EV clamp ([#36](https://github.com/frahlg/forty-two-watts/issues/36)) ([5d57d68](https://github.com/frahlg/forty-two-watts/commit/5d57d68c50e6a417b45695bd3ccf551e8566277a))
* slew-rate anchors on actual battery power, not stale command ([#41](https://github.com/frahlg/forty-two-watts/issues/41)) ([4f73f19](https://github.com/frahlg/forty-two-watts/commit/4f73f19abfb6e322a4934d9e9bb46b645afd1352))

### MPC planner

* fall back to forecast when learned PV twin collapses ([#39](https://github.com/frahlg/forty-two-watts/issues/39)) ([f3062ac](https://github.com/frahlg/forty-two-watts/commit/f3062acdd54206de8287b0a9af3862a13cb13105))
* log optimize params + ems_mode per action for plan chart ([9e8c14b](https://github.com/frahlg/forty-two-watts/commit/9e8c14bd388b869091c2315bd4a42def648bf987))
* value SoC at import−export spread in self-consumption modes ([#40](https://github.com/frahlg/forty-two-watts/issues/40)) ([a90d525](https://github.com/frahlg/forty-two-watts/commit/a90d5259209ca9fd8094927b060f62633dd3b5d0))

### Telemetry

* add DerEV type for EV charger readings ([#34](https://github.com/frahlg/forty-two-watts/issues/34)) ([65c9e2c](https://github.com/frahlg/forty-two-watts/commit/65c9e2c23b5f3eb7cb55fd952be7e724b2270e17))

### TSDB

* long-format SQLite (14d) + Parquet rolloff for older ([c53c964](https://github.com/frahlg/forty-two-watts/commit/c53c964e825c896fc0cf760a21ee7b0e29421d2f))

### Safety

* watchdog marks stale drivers offline + reverts to autonomous ([519196c](https://github.com/frahlg/forty-two-watts/commit/519196c01255db3947774bb8a267961b755d261e))

## v0.4.0-alpha (2026-04-16)

First public alpha. Running in production on real hardware but API and config format may still change. See the full changelog below or the [README](README.md) for what the system can do.

### Highlights

- **19 Lua drivers** — Sungrow, Solis, Huawei, Deye, SMA, Fronius, SolarEdge, Kostal, GoodWe, Growatt, Sofar, Victron, Ferroamp (MQTT + Modbus), Pixii, Eastron SDM630, Fronius Smart Meter, Easee Cloud
- **MPC planner** — 48h dynamic programming with three strategies (self-consumption, cheap charging, arbitrage)
- **EV charging** — Easee Cloud integration + OCPP 1.6J Central System
- **Digital twins** — self-learning PV, load, and price models
- **Pure Go + Lua** — single static binary, no Rust, no WASM, no CGo
- **Web dashboard** with real-time power flow, planner visualization, and full config UI
- **Home Assistant** MQTT autodiscovery

---

## Auto-generated changelog (internal)

## [2.3.0](https://github.com/frahlg/forty-two-watts/compare/v2.2.6...v2.3.0) (2026-04-16)

### Features

* config/UI improvements — kWh display, secure EV password, planner tab ([#65](https://github.com/frahlg/forty-two-watts/issues/65)) ([35ab03d](https://github.com/frahlg/forty-two-watts/commit/35ab03d7b5f63ffcc471bf28e1409d761bf0f7d2))

## [2.2.6](https://github.com/frahlg/forty-two-watts/compare/v2.2.5...v2.2.6) (2026-04-16)

### Bug Fixes

* populate EV Charger tab from driver config when ev_charger is empty ([5e6b116](https://github.com/frahlg/forty-two-watts/commit/5e6b11676bc972a2c983d39a345a3b5f8dbc77dc))

## [2.2.5](https://github.com/frahlg/forty-two-watts/compare/v2.2.4...v2.2.5) (2026-04-16)

### Bug Fixes

* address P2 review comments across control, MPC, drivers, and UI ([#64](https://github.com/frahlg/forty-two-watts/issues/64)) ([fcafa88](https://github.com/frahlg/forty-two-watts/commit/fcafa88f12c714a1930342dd9f28ea07d18440c2))

## [2.2.4](https://github.com/frahlg/forty-two-watts/compare/v2.2.3...v2.2.4) (2026-04-16)

### Bug Fixes

* replace wonky Catmull-Rom spline with simple linear forecast ([abea431](https://github.com/frahlg/forty-two-watts/commit/abea431d7895504116600384c6a92e9577675607))

### UI

* add status bar with driver health indicators ([b048d60](https://github.com/frahlg/forty-two-watts/commit/b048d60a57049385c498cc4e592ee049a3a05809))
* smooth Catmull-Rom spline for forecast + 15min forecast zone ([dba51a5](https://github.com/frahlg/forty-two-watts/commit/dba51a54c26e6329a4eca850b81b4a22974efcfd))

## [2.2.3](https://github.com/frahlg/forty-two-watts/compare/v2.2.2...v2.2.3) (2026-04-16)

### Bug Fixes

* remove dead evSlider event listeners that crash app.js ([8ae76c7](https://github.com/frahlg/forty-two-watts/commit/8ae76c710b4ca2d15eb71399211849c4ce03a4bb))

### UI

* fix summary cards grid for 7 cards + raise side-by-side breakpoint ([6e19973](https://github.com/frahlg/forty-two-watts/commit/6e1997312df8ca5b889000d286d0b0782059b701))

## [2.2.2](https://github.com/frahlg/forty-two-watts/compare/v2.2.1...v2.2.2) (2026-04-16)

### Bug Fixes

* show '...' instead of stale v0.1.0 while JS loads version ([dc65065](https://github.com/frahlg/forty-two-watts/commit/dc65065784cad8c018f64338284b5f4b6441ac22))

## [2.2.1](https://github.com/frahlg/forty-two-watts/compare/v2.2.0...v2.2.1) (2026-04-16)

### Bug Fixes

* **ci:** disable @semantic-release/github PR annotation features ([4020d46](https://github.com/frahlg/forty-two-watts/commit/4020d4606e0f81924cca5d0e06f4ab743bf8f1d5)), closes [#32](https://github.com/frahlg/forty-two-watts/issues/32) [#33](https://github.com/frahlg/forty-two-watts/issues/33) [#34](https://github.com/frahlg/forty-two-watts/issues/34) [#35](https://github.com/frahlg/forty-two-watts/issues/35) [#36](https://github.com/frahlg/forty-two-watts/issues/36) [#39](https://github.com/frahlg/forty-two-watts/issues/39)
* **ci:** switch semantic-release to conventionalcommits preset ([7e0bb89](https://github.com/frahlg/forty-two-watts/commit/7e0bb895f7a8f8271033336899bed8639e772dc4))
* **ci:** upgrade GitHub Actions to Node.js 24 (drop deprecated Node 20) ([4005bd8](https://github.com/frahlg/forty-two-watts/commit/4005bd8b982c091bff4dcd428cebbe1a08447242))

### UI

* remove manual EV charging slider ([063174c](https://github.com/frahlg/forty-two-watts/commit/063174cc259d46185da34bad827c16994a3c6e33))

# [2.2.0](https://github.com/frahlg/forty-two-watts/compare/v2.1.0...v2.2.0) (2026-04-16)


### Features

* EV charger config + credential masking in API responses ([#58](https://github.com/frahlg/forty-two-watts/issues/58)) ([c22cb80](https://github.com/frahlg/forty-two-watts/commit/c22cb805af960bcafc353846f62e2406fc791e17))

# [2.1.0](https://github.com/frahlg/forty-two-watts/compare/v2.0.1...v2.1.0) (2026-04-16)


### Features

* Easee Cloud driver + host.http_get/post for Lua drivers ([#56](https://github.com/frahlg/forty-two-watts/issues/56)) ([4cdc942](https://github.com/frahlg/forty-two-watts/commit/4cdc9421590385e8f00301925d590f6fb093ebaf))

## [2.0.1](https://github.com/frahlg/forty-two-watts/compare/v2.0.0...v2.0.1) (2026-04-16)


### Bug Fixes

* 5 Go-side P1 bugs from Codex review ([#46](https://github.com/frahlg/forty-two-watts/issues/46)) ([0cd2885](https://github.com/frahlg/forty-two-watts/commit/0cd2885bdb79d6a4c3116bb4930ec785cea8f944))
* 5 Go-side P1 bugs from Codex review ([#47](https://github.com/frahlg/forty-two-watts/issues/47)) ([4f2eaf6](https://github.com/frahlg/forty-two-watts/commit/4f2eaf69f626caddf2bae456ac047301f9a36840))
* **solaredge_pv:** read SunSpec scale factors every poll, not cached ([#38](https://github.com/frahlg/forty-two-watts/issues/38)) ([26f8793](https://github.com/frahlg/forty-two-watts/commit/26f8793f22888dc11d29fd157b10b4340da34c8d))
