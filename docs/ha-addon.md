# Home Assistant add-on

This documents the first-party Home Assistant OS / Supervised add-on that
ships in [`deploy/homeassistant/`](../deploy/homeassistant/), why the
community add-on stopped tracking releases, and what is left to automate.
For MQTT-bridge integration (running FTW anywhere and surfacing it as HA
entities) see [`ha-integration.md`](ha-integration.md) — that is a separate
concern from running the app *as* an add-on.

## Findings: the project was restructured under Sourceful

The community add-on
([`erikarenhill/ha-addon-forty-two-watts`](https://github.com/erikarenhill/ha-addon-forty-two-watts))
was a four-line wrapper:

```dockerfile
FROM ghcr.io/frahlg/forty-two-watts:v1.3.0
USER root
WORKDIR /data
CMD ["-config", "/data/config.yaml", "-web", "/app/web", "-drivers", "/app/drivers"]
```

with `version: "1.3.0"`. Checking it against the current upstream
(`srcfl/ftw` master + the published images) turned up:

1. **The image was renamed.** The canonical image is now
   **`ghcr.io/srcfl/ftw`**, plus **`ghcr.io/srcfl/ftw-updater`** and
   **`ghcr.io/srcfl/ftw-optimizer`**. The old
   `ghcr.io/frahlg/forty-two-watts` path is kept alive only as a
   **byte-identical legacy mirror** — `srcfl/ftw:latest` and the mirror's
   `latest` resolve to the *same manifest digest* (verified against GHCR).
   That mirror is why the community add-on still *builds*.

2. **The runtime binary was renamed** to **`/app/ftw`**
   (`ENTRYPOINT ["/app/ftw"]`). This is *not* a break for the wrapper: it
   overrides `CMD` with **flags only** and inherits `ENTRYPOINT`, so the name
   is irrelevant. The `-config/-web/-drivers/-user-drivers` flag interface is
   unchanged.

3. **The MPC optimizer runs as a separate `ftw-optimizer` sidecar** (a
   CVXPY/Python subsystem in `optimizer/`, `Dockerfile.optimizer`, its own
   compose service). See the packaging-limitations section — this is the one
   restructure a single-container add-on genuinely cannot mirror.

4. **The image is `/app/data`-centric and non-root** (`WORKDIR /app/data`,
   `VOLUME /app/data`, `USER 100:101`, a `HEALTHCHECK` on `/api/health`, a
   `CMD` hardcoding `/app/data` + `-user-drivers /app/data/drivers`). The app
   resolves relative state paths (`state.db`, `cold/`) against the CWD, so
   that WORKDIR is load-bearing. A thin external wrapper has to override USER
   + WORKDIR + the full CMD and drifts on every one of these changes — the
   community CMD predates `-user-drivers`, so operator hot-reload drivers were
   silently not loaded.

### So — does the community add-on "work"?

**It still runs, but it is stale and lossy.** Pinned to `:v1.3.0` it serves an
old build. If bumped to a current tag on the still-live mirror (or, better,
the canonical `srcfl/ftw`), the wrapper *does* start — the binary rename is
invisible and the flags are unchanged — but with two regressions from the
restructure: the missing `-user-drivers` overlay (fixed here), and **no
optimizer sidecar** (structural; see below).

The root cause is a namespace rename shielded by a mirror plus feature drift a
hand-maintained external wrapper can't keep up with — not a hard runtime
break, and *not* a version-ordering problem: the current release line is
`1.4.0` (`package.json`, latest GitHub release, and the `docker-compose.yml`
image tag all agree), so `1.3.0 < 1.4.0` and Supervisor would happily offer an
update to a rebuilt add-on. (GHCR also carries unrelated edge/beta `0.x` tags
and a legacy-mirror `v2.x` line; ignore those and track the release line.)

## The fix: a versioned, in-tree add-on

`deploy/homeassistant/` is a complete HA add-on repository (a
`repository.yaml` plus the `ftw/` add-on) living beside the code, so each
release can bump the pinned image tag (`build.yaml`) and the add-on `version:`
(`config.yaml`) in the same commit.

Design choices:

- **Target the canonical `ghcr.io/srcfl/ftw`** (pinned `v1.4.0`), not the
  legacy mirror.
- **Rebase wrapper, not a plain `image:`.** HA add-ons cannot override CMD on
  a prebuilt `image:`; the wrapper repoints WORKDIR/USER/CMD at `/data` in one
  place. Supervisor builds it locally on install; no add-on-side CI.
- **Restore `-user-drivers /data/drivers`** so hot-reload drivers persist
  across add-on updates.
- **Host networking, no Ingress** (Modbus TCP / LAN MQTT / mDNS; app isn't
  `X-Ingress-Path` aware).
- **In-app self-update stays off** — Supervisor owns the add-on lifecycle.

### Verification performed

- Confirmed the mirror identity (`srcfl/ftw:latest` == mirror `latest`, same
  manifest digest) and that `ghcr.io/srcfl/ftw:v1.4.0` exists for the pinned
  arches, directly against the GHCR registry.
- Confirmed the current ENTRYPOINT / CMD / USER / WORKDIR / HEALTHCHECK from
  `master`'s `Dockerfile`, and that the flag interface the wrapper depends on
  is unchanged.
- **Not** yet run: a live Supervisor build + boot. No Docker daemon is
  available in the authoring environment, and the image blob CDN
  (`pkg-containers.githubusercontent.com`) is blocked by the outbound proxy,
  so a container smoke-test is a remaining manual step (below).

## Storage & sizing

Add-on **image size** is not a Supervisor-enforced limit and is not the
concern here: the core Go image is tens of MB. The only image-size risk is
bundling the Python `ftw-optimizer` into the same container (a HA add-on is
single-container) — Python + numeric deps add hundreds of MB. That argues for
graceful-degrade-without-sidecar rather than bundling.

The real operational risk is **unbounded `/data` growth**. FTW is a
time-series system: the long-format TSDB rolls off to zstd Parquet cold
storage and, per `docs/tsdb.md`, *"a year of 50 metrics is typically a few
GB"*, plus `state.db`. In the add-on model that all lands in the add-on's
`/data` on the **shared HA OS data partition** (Core + every add-on + media +
backups). On an HA Green / Pi SD card that is a real squeeze, and it compounds
through **backups**: HA includes an add-on's `/data` in full backups, and
add-on data is the classic cause of runaway backup size (community reports of
add-ons inflating a backup to 10–15 GB and filling `/backup`).

Mitigations to bake into the add-on:

- Don't bundle the optimizer; degrade gracefully.
- Default `state.cold_dir` onto `/share` (or a mounted data disk) so history
  isn't on the OS partition; expose `RecentRetention` for tuning.
- Document a minimum-storage recommendation (SSD / large eMMC, not a small SD
  card) and advise excluding FTW data from routine backups.

Refs: [HA — free up storage](https://www.home-assistant.io/more-info/free-space/),
[HA developer — OS partitioning](https://developers.home-assistant.io/docs/operating-system/partition/),
[community — full backup size very large](https://community.home-assistant.io/t/full-backup-size-very-large/713768).

## Packaging limitations to resolve before release

- [ ] **Optimizer sidecar.** Upstream runs the MPC optimizer as a separate
      `ftw-optimizer` container. A HA add-on is single-container. Decide:
      (a) confirm the app degrades gracefully with no sidecar (built-in / Go
      fallback), or (b) bundle the optimizer process in the same image via an
      s6 / supervisor init. Biggest open item.
- [ ] **Storage defaults.** Point `state.cold_dir` off the OS partition (e.g.
      `/share`), expose retention, and document a min-storage recommendation —
      see Storage & sizing above.
- [ ] **Distribution.** A HA add-on repository must have `repository.yaml` at
      its git root, so Supervisor can't add the monorepo root URL directly.
      Mirror `deploy/homeassistant/` to a dedicated repo (e.g. `srcfl/ha-addons`)
      from CI on release; until then, copy it onto the HA host under `/addons`
      (Supervised) or `/root/addons` for local testing.
- [ ] **Release automation.** CI step to bump `build.yaml` + `config.yaml`
      versions and mirror the subtree on each release.
- [ ] **Options schema.** Map HA `options.json` onto the app's YAML config for
      common knobs (site name, currency, MQTT) so basic setup doesn't require
      the web wizard.
- [ ] **Icon/logo** (`icon.png`, `logo.png`).
- [ ] **Smoke test.** `docker build --build-arg
      BUILD_FROM=ghcr.io/srcfl/ftw:v1.4.0 deploy/homeassistant/ftw`, install
      via Supervisor, run the setup wizard, confirm `/data` persistence and the
      `/data/drivers` overlay.
- [ ] Decide whether to archive/redirect the community
      `erikarenhill/ha-addon-forty-two-watts` repo.
