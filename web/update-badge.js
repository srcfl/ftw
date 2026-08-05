// <ftw-update-badge> — self-contained Web Component that checks for a
// newer FTW image, renders a notification dot in the header,
// and drives the update/restart flow end-to-end (pull → recreate →
// auto-reload). Everything lives in shadow DOM so dashboard styles are
// untouched.
//
// Placement: one <ftw-update-badge></ftw-update-badge> inside the header.
// The element exposes a public open() method so the #version span (which
// lives outside shadow DOM) can also trigger the modal.

(function () {
  "use strict";

  function apiFetch(path, opts) {
    return fetch(path, opts);
  }

  function driverFileKey(path) {
    return String(path || "").replace(/\\/g, "/").split("/").pop().toLowerCase();
  }

  // Header status marks. Inline SVG rather than font glyphs: at 16px the
  // three announcements have to be separable by silhouette alone, because
  // colour is not reliable for every operator and the marks sit in the
  // same slot. A glyph gives you weight and hue; a path gives you shape.
  const ICON_UPDATE ='<svg class="icon" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M8 1.7v6.2"/><path d="M5.3 5.5 8 8.4l2.7-2.9"/><rect x="1.8" y="10.5" width="12.4" height="3.8" rx="1.2"/><path d="M4.4 12.4h.01"/></svg>';
  const ICON_WARNING = '<svg class="icon" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M8 2.3 14.9 13.7H1.1z"/><path d="M8 6.4v3"/><path d="M8 11.6h.01"/></svg>';
  const ICON_DOT = '<svg class="icon" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" focusable="false"><circle cx="8" cy="8" r="4.2" fill="currentColor"/></svg>';

  // Upstream version checks don't change often; 3 h is plenty of
  // headroom to surface a new release on a normal workday without
  // hammering /api/version/check (which can hit GitHub each tick if
  // the local cache is stale).
  const CHECK_INTERVAL_MS = 3 * 60 * 60 * 1000; // /api/version/check cadence
  const STATUS_INTERVAL_MS = 2000;               // during updates
  const UPDATE_SOFT_TIMEOUT_MS = 180 * 1000;     // after this we stop auto-reloading
  const SNAPSHOT_SOFT_TIMEOUT_MS = 15 * 60 * 1000; // large state.db snapshots can be slow

  class FtwUpdateBadge extends HTMLElement {
    constructor() {
      super();
      this._shadow = this.attachShadow({ mode: "open" });
      this._info = null;              // last /api/version/check payload
      this._phase = "idle";           // idle | dialog | updating
      this._sidecarState = null;      // last /api/version/update/status
      this._updateStartedAt = 0;
      this._updateOriginalVersion = null;
      this._expectedRun = null;
      this._checkTimer = null;
      this._statusTimer = null;
      this._elapsedTimer = null;
      this._disabled = false;         // set true on 503 (feature gated off)
      this._snapshots = null;         // last /api/version/snapshots payload (#150)
      this._deletingSnapshot = null;  // id being deleted right now (#150)
      this._creatingSnapshot = false;
      this._backups = null;
      this._creatingBackup = false;
      this._deletingBackup = "";
      this._verifyingBackup = "";
      this._components = null;
      this._componentHistory = null;
      this._driverCatalog = null;
      this._driverVersions = {};
      this._componentAction = "";
      this._connected = true;         // header liveness light; see setConnected()
      this._render();
    }

    connectedCallback() {
      this._resumeUpdateStatus();
      this._refresh(false);
      this._refreshComponents(false);
      this._refreshDriverCatalog();
      this._checkTimer = setInterval(() => {
        this._refresh(false);
        this._refreshComponents(false);
        this._refreshDriverCatalog();
      }, CHECK_INTERVAL_MS);
    }

    _resumeUpdateStatus() {
      apiFetch("/api/version/update/status", { cache: "no-store" })
        .then((r) => (r.ok ? r.json() : null))
        .then((st) => {
          if (!st || !isUpdateInFlight(st.state) || this._phase === "updating") return;
          const started = st.started_at ? Date.parse(st.started_at) : 0;
          this._phase = "updating";
          this._sidecarState = st;
          this._updateStartedAt = started > 0 ? started : Date.now();
          this._expectedRun = {
            action: st.action || "update",
            target: st.target || "",
            snapshot: st.snapshot || "",
            component: st.component || "",
          };
          this._render();
          this._startElapsedTicker();
          this._startStatusPolling();
        })
        .catch(() => { /* the service may still be starting */ });
    }

    disconnectedCallback() {
      clearInterval(this._checkTimer);
      clearInterval(this._statusTimer);
      clearInterval(this._elapsedTimer);
    }

    // Public: called by app.js on every poll tick. The header's liveness
    // light used to be a separate #conn-status span; folding it in here
    // means one slot decides what the corner says instead of two elements
    // hiding each other with CSS. app.js runs before this deferred script,
    // so the first tick or two may land before the element upgrades and
    // find no method — harmless, since polling re-asserts the state within
    // one cycle and the constructor defaults to connected, exactly what
    // the old markup shipped as.
    setConnected(ok) {
      const next = !!ok;
      if (next === this._connected) return; // avoid re-rendering the modal mid-interaction
      this._connected = next;
      this._render();
    }

    // Public: called by the header #version click handler in index.html so
    // the operator can open the modal without aiming at the tiny dot. No-op
    // when the backend has told us the feature is gated off.
    open() {
      if (this._disabled) return;
      this._phase = "dialog";
      this._render();
      this._refresh(false); // surface the freshest info when opened
      this._refreshSnapshots(); // pull the list for the Snapshots accordion
      this._refreshBackups();
      this._refreshComponents(false);
      this._refreshComponentHistory();
      this._refreshDriverCatalog();
    }

    // Fetch the snapshot list so the operator sees the retained set and
    // can delete entries without SSH. Tolerates 503 (feature off) and
    // 404s silently — the UI simply hides the section.
    _refreshSnapshots() {
      if (this._disabled) return;
      apiFetch("/api/version/snapshots")
        .then((r) => (r.ok ? r.json() : null))
        .then((body) => {
          if (!body) return;
          this._snapshots = body;
          this._render();
        })
        .catch(() => { /* silent */ });
    }

    _deleteSnapshot(id) {
      if (!id) return;
      // Guard against rapid double-clicks while the request is pending.
      if (this._deletingSnapshot) return;
      this._deletingSnapshot = id;
      apiFetch("/api/version/snapshots/" + encodeURIComponent(id), { method: "DELETE" })
        .finally(() => {
          this._deletingSnapshot = null;
          this._refreshSnapshots();
        });
    }

    _createSnapshot() {
      if (this._creatingSnapshot) return;
      this._creatingSnapshot = true;
      this._render();
      apiFetch("/api/version/snapshots", { method: "POST" })
        .then(async (resp) => {
          const body = await resp.json().catch(() => ({}));
          if (!resp.ok) throw new Error(body.error || "failed to create rollback point");
          return body;
        })
        .then(() => this._refreshSnapshots())
        .catch((err) => window.alert("Rollback point failed: " + err.message))
        .finally(() => {
          this._creatingSnapshot = false;
          this._render();
        });
    }

    _refreshBackups() {
      if (this._disabled) return;
      apiFetch("/api/backups")
        .then((r) => (r.ok ? r.json() : null))
        .then((body) => {
          if (!body) return;
          this._backups = body;
          this._render();
        })
        .catch(() => { /* full backup may not be enabled on native installs */ });
    }

    _createBackup() {
      if (this._creatingBackup) return;
      this._creatingBackup = true;
      this._render();
      apiFetch("/api/backups", { method: "POST" })
        .then(async (resp) => {
          const body = await resp.json().catch(() => ({}));
          if (!resp.ok) throw new Error(body.error || "failed to create full backup");
          return body;
        })
        .then(() => this._refreshBackups())
        .catch((err) => window.alert("Full backup failed: " + err.message))
        .finally(() => {
          this._creatingBackup = false;
          this._render();
        });
    }

    _verifyBackup(id) {
      if (!id || this._verifyingBackup) return;
      this._verifyingBackup = id;
      this._render();
      apiFetch("/api/backups/" + encodeURIComponent(id) + "/verify", { method: "POST" })
        .then(async (resp) => {
          const body = await resp.json().catch(() => ({}));
          if (!resp.ok) throw new Error(body.error || "verification failed");
        })
        .then(() => this._refreshBackups())
        .catch((err) => window.alert("Backup verification failed: " + err.message))
        .finally(() => {
          this._verifyingBackup = "";
          this._render();
        });
    }

    _deleteBackup(id) {
      if (!id || this._deletingBackup) return;
      this._deletingBackup = id;
      this._render();
      apiFetch("/api/backups/" + encodeURIComponent(id), { method: "DELETE" })
        .then(async (resp) => {
          const body = await resp.json().catch(() => ({}));
          if (!resp.ok) throw new Error(body.error || "delete failed");
        })
        .then(() => this._refreshBackups())
        .catch((err) => window.alert("Backup delete failed: " + err.message))
        .finally(() => {
          this._deletingBackup = "";
          this._render();
        });
    }

    _refreshComponents(force) {
      if (this._disabled) return;
      apiFetch("/api/components" + (force ? "?force=1" : ""))
        .then((r) => (r.ok ? r.json() : null))
        .then((body) => {
          if (!body) return;
          this._components = body;
          this._render();
        })
        .catch(() => { /* diagnostics stay optional */ });
    }

    _refreshComponentHistory() {
      apiFetch("/api/components/history?limit=20")
        .then((r) => (r.ok ? r.json() : null))
        .then((body) => {
          if (!body) return;
          this._componentHistory = body;
          this._render();
        })
        .catch(() => { /* old backends do not expose history */ });
    }

    _refreshDriverCatalog() {
      Promise.all([
        apiFetch("/api/drivers/catalog").then((r) => (r.ok ? r.json() : null)),
        apiFetch("/api/config").then((r) => (r.ok ? r.json() : null)),
        apiFetch("/api/device_repository/catalog?channel=beta").then((r) => (r.ok ? r.json() : { entries: [] })),
      ])
        .then(([catalog, config, betaCatalog]) => {
          if (!catalog || !config) return;
          const configured = new Set((Array.isArray(config.drivers) ? config.drivers : [])
            .map((driver) => driverFileKey(driver && driver.lua))
            .filter(Boolean));
          const betaByID = new Map((betaCatalog && Array.isArray(betaCatalog.entries) ? betaCatalog.entries : [])
            .map((candidate) => [candidate && candidate.driver && candidate.driver.id, candidate]));
          // Every configured driver is part of the inventory, whether or not
          // it has an update waiting — the dialog answers "what am I running?"
          // before it answers "what can I change?". Locally edited drivers are
          // listed too, but carry no actions: nothing signed to move them to.
          const entries = (Array.isArray(catalog.entries) ? catalog.entries : [])
            .filter((entry) => configured.has(driverFileKey(entry && (entry.path || entry.filename))))
            .map((entry) => {
              const beta = betaByID.get(entry.id) || null;
              const current = entry.installed_version || entry.version || "";
              const betaDriver = beta && beta.driver;
              const managed = entry.source !== "local";
              const stableAvailable = !!(managed && entry.update_available && entry.repository_id && entry.upstream_version);
              const betaAvailable = !!(managed && betaDriver && betaDriver.version && betaDriver.version !== current);
              return {
                ...entry,
                beta_candidate: beta,
                managed,
                stable_available: stableAvailable,
                beta_available: betaAvailable,
                pending_update: stableAvailable || betaAvailable,
              };
            });
          this._driverCatalog = { entries };
          this._render();
        })
        .catch(() => { /* config or repository discovery may be unavailable */ });
    }

    _loadDriverVersions(id) {
      if (!id) return;
      apiFetch("/api/device_repository/drivers/" + encodeURIComponent(id) + "/versions")
        .then(async (resp) => {
          const body = await resp.json().catch(() => ({}));
          if (!resp.ok) throw new Error(body.error || "failed to load driver history");
          this._driverVersions[id] = body;
          this._render();
        })
        .catch((err) => window.alert("Driver history failed: " + err.message));
    }

    _changeDriverVersion(id, repositoryID, version, sha256, installed, channel) {
      if (!id || !version || this._componentAction) return;
      this._componentAction = "driver:" + id;
      this._render();
      const url = "/api/device_repository/drivers/" + encodeURIComponent(id) + (installed ? "/activate" : "/install");
      const body = installed
        ? { version, sha256 }
        : { repository_id: repositoryID, version, ...(channel ? { channel } : {}) };
      this._postJSON(url, body)
        .then((resp) => {
          if (!resp.ok) throw new Error((resp.body && resp.body.error) || "driver update failed");
          delete this._driverVersions[id];
          this._refreshDriverCatalog();
          this._refreshComponents(false);
          this._refreshComponentHistory();
        })
        .catch((err) => window.alert("Driver update failed: " + err.message))
        .finally(() => {
          this._componentAction = "";
          this._render();
        });
    }

    // _beginRollback kicks off a rollback-to-snapshot. Reuses the same
    // "updating" modal skin as _beginUpdate — the sidecar emits state
    // transitions (restoring → restarting → done) that feed straight
    // into the existing _tickStatus → _render path. See #152.
    _beginRollback(snapshotID) {
      this._phase = "updating";
      this._updateStartedAt = Date.now();
      this._updateOriginalVersion = this._info ? this._info.current : null;
      this._expectedRun = { action: "rollback", target: "", snapshot: snapshotID };
      this._sidecarState = { state: "starting", action: "rollback", snapshot: snapshotID };
      this._render();
      this._startElapsedTicker();
      this._startStatusPolling();

      this._postJSON("/api/version/rollback", { snapshot_id: snapshotID })
        .then((resp) => {
          if (!resp.ok) {
            this._sidecarState = { state: "failed", action: "rollback", message: (resp.body && resp.body.error) || "failed to start" };
            this._stopUpdateTimers();
            this._render();
            return;
          }
        })
        .catch((e) => {
          this._sidecarState = { state: "failed", action: "rollback", message: String(e) };
          this._stopUpdateTimers();
          this._render();
        });
    }

    // Permanently shut the element down: stop polling, clear shadow DOM, hide
    // from layout, and fire an event so the #version bridge can drop its
    // cursor/pointer affordance. Called when the backend returns 503, which
    // means the feature is gated off (FTW_SELFUPDATE_ENABLED unset) — not a
    // transient error, so we don't ever retry.
    _disable() {
      if (this._disabled) return;
      this._disabled = true;
      clearInterval(this._checkTimer);
      clearInterval(this._statusTimer);
      clearInterval(this._elapsedTimer);
      this._shadow.innerHTML = "";
      this.hidden = true;
      this.dispatchEvent(new CustomEvent("ftw-selfupdate-disabled", { bubbles: true }));
    }

    // ---- data ----
    _refresh(force) {
      if (this._disabled) return;
      const url = force ? "/api/version/check?force=1" : "/api/version/check";
      apiFetch(url)
        .then((r) => {
          // 503 = feature disabled by the backend. Stop polling and get out
          // of the way entirely — this is deployment config, not a bug.
          if (r.status === 503) {
            this._disable();
            return null;
          }
          return r.json()
            .then((body) => ({ ok: r.ok, body }))
            .catch(() => ({ ok: r.ok, body: null }));
        })
        .then((result) => {
          if (!result) return; // disabled, nothing to render
          // The handler returns the full Info schema on both success and the
          // force=1 error path, so we render either way. When ok=false,
          // body.err carries the reason and the UI shows "Last check failed".
          if (result.body && typeof result.body === "object") {
            this._info = result.body;
            this._render();
          }
        })
        .catch(() => { /* silent — periodic noise is not useful */ });
    }

    _postJSON(url, body) {
      return apiFetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: body ? JSON.stringify(body) : undefined,
      }).then((r) => r.json().then((j) => ({ ok: r.ok, body: j })));
    }

    // ---- actions ----
    _skip() {
      if (!this._info || !this._info.latest) return;
      this._postJSON("/api/version/skip", { version: this._info.latest }).then(() => {
        this._phase = "idle";
        this._refresh(false);
      });
    }

    _unskipAndCheck() {
      // "Check for updates" also clears skip so a hidden version resurfaces
      // without waiting for something newer. Matches user intent: if you're
      // asking, you want to see it.
      this._postJSON("/api/version/unskip", null).finally(() => this._refresh(true));
    }

    _setChannel(channel) {
      if (!channel || (this._info && this._info.channel === channel)) return;
      this._postJSON("/api/version/channel", { channel })
        .then((resp) => {
          if (!resp.ok) {
            this._info = Object.assign({}, this._info || {}, {
              err: (resp.body && resp.body.error) || "failed to change update channel",
            });
            this._render();
            return;
          }
          this._info = resp.body;
          this._render();
          this._refresh(true);
        })
        .catch((e) => {
          this._info = Object.assign({}, this._info || {}, { err: String(e) });
          this._render();
        });
    }

    _setOptimizerChannel(channel) {
      const updates = this._components && this._components.optimizer && this._components.optimizer.updates;
      if (!channel || (updates && updates.channel === channel)) return;
      this._postJSON("/api/components/optimizer/channel", { channel })
        .then((resp) => {
          if (!resp.ok) throw new Error((resp.body && resp.body.error) || "failed to change optimizer channel");
          this._refreshComponents(true);
        })
        .catch((err) => window.alert("Optimizer channel failed: " + err.message));
    }

    _beginOptimizerUpdate(rollback) {
      const optimizer = this._components && this._components.optimizer;
      const updates = optimizer && optimizer.updates;
      const action = rollback ? "component_rollback" : "update";
      const target = rollback ? "" : ((updates && updates.latest) || "");
      this._phase = "updating";
      this._updateStartedAt = Date.now();
      this._updateOriginalVersion = updates ? updates.current : null;
      this._expectedRun = { action, target, snapshot: "", component: "optimizer" };
      this._sidecarState = { state: "starting", action, component: "optimizer", target };
      this._render();
      this._startElapsedTicker();
      this._startStatusPolling();
      const url = rollback ? "/api/components/optimizer/rollback" : "/api/components/optimizer/update";
      const body = rollback ? null : { target };
      this._postJSON(url, body)
        .then((resp) => {
          if (!resp.ok) {
            this._sidecarState = { state: "failed", action, component: "optimizer", message: (resp.body && resp.body.error) || "failed to start" };
            this._stopUpdateTimers();
            this._render();
          }
        })
        .catch((e) => {
          this._sidecarState = { state: "failed", action, component: "optimizer", message: String(e) };
          this._stopUpdateTimers();
          this._render();
        });
    }

    _beginUpdate(action) {
      this._phase = "updating";
      this._updateStartedAt = Date.now();
      this._updateOriginalVersion = this._info ? this._info.current : null;
      this._expectedRun = {
        action,
        target: action === "update" && this._info ? (this._info.latest || "") : "",
        snapshot: "",
      };
      this._sidecarState = { state: "starting", action };
      this._render();
      this._startElapsedTicker();
      this._startStatusPolling();

      const url = action === "restart" ? "/api/version/restart" : "/api/version/update";
      this._postJSON(url, null)
        .then((resp) => {
          if (!resp.ok) {
            this._sidecarState = { state: "failed", action, message: (resp.body && resp.body.error) || "failed to start" };
            this._stopUpdateTimers();
            this._render();
            return;
          }
          if (action === "update") {
            const skipped = resp.body && resp.body.snapshot_skipped;
            const skipReason = resp.body && resp.body.snapshot_skip_reason;
            this._sidecarState = {
              state: skipped ? "pulling" : "snapshotting",
              action,
              target: this._expectedRun.target,
              message: skipped
                ? (skipReason === "database schema unchanged"
                  ? "Database schema unchanged; full history backup not needed"
                  : "Backup snapshot skipped for this update")
                : "Creating backup snapshot",
            };
            this._render();
          }
        })
        .catch((e) => {
          this._sidecarState = { state: "failed", action, message: String(e) };
          this._stopUpdateTimers();
          this._render();
        });
    }

    _startStatusPolling() {
      clearInterval(this._statusTimer);
      this._statusTimer = setInterval(() => this._tickStatus(), STATUS_INTERVAL_MS);
      this._tickStatus();
    }

    _startElapsedTicker() {
      clearInterval(this._elapsedTimer);
      this._elapsedTimer = setInterval(() => {
        if (this._phase !== "updating") {
          clearInterval(this._elapsedTimer);
          return;
        }
        this._markSoftTimeout();
        this._render();
      }, 1000);
    }

    _stopUpdateTimers() {
      clearInterval(this._statusTimer);
      clearInterval(this._elapsedTimer);
    }

    _tickStatus() {
      // 1) Poll sidecar state.json.
      apiFetch("/api/version/update/status")
        .then((r) => (r.ok ? r.json() : null))
        .then((st) => {
          if (st && this._statusMatchesCurrentRun(st)) {
            const keepTimeout = this._sidecarState && this._sidecarState.timedOut &&
              this._sidecarState.state === st.state &&
              (this._sidecarState.phase_started_at || "") === (st.phase_started_at || "");
            this._sidecarState = keepTimeout ? Object.assign({}, st, { timedOut: true }) : st;
            this._render();
            if (st.state === "done") {
              this._attemptReload();
            }
          }
        })
        .catch(() => {
          // Main container is likely mid-restart; expected — keep polling.
        });

      // 2) If we've been updating too long with no progress, give the user
      // a manual reload escape hatch instead of spinning forever.
      if (this._markSoftTimeout()) {
        this._render();
      }
    }

    _statusMatchesCurrentRun(st) {
      if (!st || !this._expectedRun) return false;
      if (st.action && st.action !== this._expectedRun.action) return false;
      if (this._expectedRun.target && st.target && st.target !== this._expectedRun.target) return false;
      if (this._expectedRun.snapshot && st.snapshot && st.snapshot !== this._expectedRun.snapshot) return false;
      if (this._expectedRun.component && st.component && st.component !== this._expectedRun.component) return false;

      // Polling starts before POST /update returns, so the status file can
      // still contain an old "done" from the previous update. Never let that
      // stale terminal state auto-reload the page for the new run.
      const startedMs = st.started_at ? Date.parse(st.started_at) : 0;
      if (startedMs && startedMs < this._updateStartedAt - 5000) return false;
      if (!startedMs && (st.state === "done" || st.state === "failed" || st.state === "idle")) return false;
      return true;
    }

    _markSoftTimeout() {
      const timeoutMs = this._sidecarState && this._sidecarState.state === "snapshotting"
        ? SNAPSHOT_SOFT_TIMEOUT_MS
        : UPDATE_SOFT_TIMEOUT_MS;
      const phaseStarted = this._sidecarState && this._sidecarState.phase_started_at
        ? Date.parse(this._sidecarState.phase_started_at)
        : 0;
      const timeoutStartedAt = phaseStarted > 0 ? phaseStarted : this._updateStartedAt;
      if (Date.now() - timeoutStartedAt <= timeoutMs) return false;
      if (!this._sidecarState || this._sidecarState.state === "done" || this._sidecarState.timedOut) return false;
      this._sidecarState = Object.assign({}, this._sidecarState, { timedOut: true });
      return true;
    }

    _attemptReload() {
      // Give the new container a moment to open its listener, then
      // hard-reload. Bypass cache so a new app.js version is picked up.
      clearInterval(this._statusTimer);
      clearInterval(this._elapsedTimer);
      setTimeout(() => {
        // location.reload(true) is deprecated; a cache-busting query is a
        // reliable cross-browser alternative that forces a fresh index.html.
        const u = new URL(window.location.href);
        u.searchParams.set("_u", Date.now().toString());
        window.location.replace(u.toString());
      }, 800);
    }

    // ---- render ----

    // _driverEntries is the full configured inventory; _pendingUpdates counts
    // only what an operator could act on right now. The badge, the summary
    // line and the footer all read the same count so they can't disagree.
    _driverEntries() {
      return this._driverCatalog && Array.isArray(this._driverCatalog.entries)
        ? this._driverCatalog.entries
        : [];
    }

    _pendingUpdates() {
      const info = this._info || {};
      const optimizerUpdates = this._components && this._components.optimizer && this._components.optimizer.updates;
      const core = !!(info.update_available && !info.skipped);
      const optimizer = !!(optimizerUpdates && optimizerUpdates.update_available);
      const drivers = this._driverEntries().filter((entry) => entry.pending_update).length;
      return { core, optimizer, drivers, total: (core ? 1 : 0) + (optimizer ? 1 : 0) + drivers };
    }

    _render() {
      const info = this._info || {};
      const optimizer = this._components && this._components.optimizer;
      const pending = this._pendingUpdates();
      const showDot = pending.total > 0 && this._phase !== "updating";
      const showOptimizerWarning = !!(optimizer && optimizer.configured && (optimizer.degraded === true || optimizer.healthy === false)) && this._phase !== "updating";
      const showBadge = showDot || showOptimizerWarning;
      const activeSolver = optimizer && optimizer.active_solver;
      const optimizerFallbackActive = !!(activeSolver && (activeSolver.fallback || activeSolver.engine === "go-dp"));
      const optimizerReason = optimizer && (optimizer.fallback_reason || optimizer.health_error || optimizer.error);
      const warningTitle = (optimizerFallbackActive ? "Planner fallback active" : "Optimizer unavailable") + (optimizerReason ? ": " + optimizerReason : "");
      const updateTitle = pending.core && info.latest
        ? `Core update available: ${info.latest}`
        : pending.total === 1 ? "1 component update available" : `${pending.total} component updates available`;

      // One header slot, three announcements. Connection loss outranks
      // the other two and shows alone: the update and optimizer payloads
      // behind them are whatever we last managed to fetch, so flagging a
      // possibly-stale update while the site is unreachable is worse than
      // saying nothing. Otherwise an update and a degraded optimizer are
      // unrelated facts and both get their own mark — before this the
      // warning suppressed the update entirely (#693).
      const marks = [];
      if (!this._connected) {
        marks.push(`<span part="badge" class="mark offline" role="img" title="Connection lost" aria-label="Connection lost">${ICON_DOT}</span>`);
      } else {
        if (showDot) {
          marks.push(`<button part="badge" class="mark update" title="${escapeHTML(updateTitle)}" aria-label="${escapeHTML(updateTitle)}">${ICON_UPDATE}</button>`);
        }
        if (showOptimizerWarning) {
          marks.push(`<button part="badge" class="mark warning" title="${escapeHTML(warningTitle)}" aria-label="${escapeHTML(warningTitle)}">${ICON_WARNING}</button>`);
        }
        if (!showBadge) {
          marks.push(`<span part="badge" class="mark ok" role="img" title="Connected, nothing pending" aria-label="Connected, nothing pending">${ICON_DOT}</span>`);
        }
      }

      this._shadow.innerHTML = `
        <style>${this._styles()}</style>
        <span class="marks">${marks.join("")}</span>
        ${this._phase !== "idle" ? this._modalHTML() : ""}
      `;

      this._shadow.querySelectorAll("button.mark").forEach((btn) => {
        btn.addEventListener("click", () => this.open());
      });

      const modal = this._shadow.querySelector(".modal");
      if (modal) this._wireModal(modal);
    }

    _modalHTML() {
      const info = this._info || {};
      if (this._phase === "updating") return this._updatingModalHTML();

      const hasUpdate = !!info.update_available;
      const pending = this._pendingUpdates();
      // The summary covers the whole inventory, not just Core: an operator
      // with a waiting driver update should not read "up to date".
      const subtitle = pending.total === 0
        ? "Everything is up to date."
        : pending.total === 1
        ? "1 update available."
        : `${pending.total} updates available.`;

      // A version check that ran days ago answers a different question than
      // one that ran a minute ago, so always say when it last ran.
      const checked = info.checked_at ? Date.parse(info.checked_at) : 0;
      const checkedLine = checked > 0
        ? `Checked ${new Date(checked).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })} · rechecks every 3 h`
        : "Not checked yet.";

      // Restart is the one footer action that interrupts dispatch, so it
      // never competes with the primary button for weight.
      const actions = hasUpdate
        ? `
            <button class="btn btn-ghost" data-action="skip">Skip this version</button>
            <button class="btn btn-ghost" data-action="restart">Restart</button>
            <button class="btn btn-primary" data-action="update">Update Core to ${escapeHTML(info.latest || "")}</button>
          `
        : `
            <button class="btn btn-ghost" data-action="restart">Restart</button>
            <button class="btn" data-action="check">Check for updates</button>
          `;

      const notesHref = safeHref(info.release_notes_url);
      const notesLink = hasUpdate && notesHref
        ? `<a class="notes-link" href="${escapeHTML(notesHref)}" target="_blank" rel="noopener">Open on GitHub ↗</a>`
        : "";
      // Render the release body inline so the operator can read what's
      // about to be applied without opening a tab. Markdown is a small
      // subset (headings, lists, code, strong, safe links) — anything
      // else stays as plain escaped text. See renderReleaseBody.
      const bodyHTML = hasUpdate && info.release_body
        ? `<details class="changelog" open>
             <summary>What's in ${escapeHTML(info.latest || "this release")}</summary>
             <div class="changelog-body">${renderReleaseBody(info.release_body)}</div>
             ${notesLink ? `<p class="changelog-link">${notesLink}</p>` : ""}
           </details>`
        : (hasUpdate && notesLink ? `<p class="changelog-link">${notesLink}</p>` : "");

      // Updates always create a local rollback point when snapshots are
      // configured. Full, portable backups are managed separately below.
      const snapshotHint = hasUpdate
        ? `<div class="snapshot-hint">
             <p>🛟 A local rollback point with a consistent database and config is saved before each Core update.</p>
           </div>`
        : "";

      const skipped = info.skipped_version
        ? `<p class="dim skipped-note">Skipped ${escapeHTML(info.skipped_version)}. "Check for updates" brings it back.</p>`
        : "";

      return `
        <div class="backdrop" data-action="close"></div>
        <div class="modal" role="dialog" aria-modal="true" aria-labelledby="ftw-upd-title">
          <header>
            <h3 id="ftw-upd-title">Updates</h3>
            <button class="x" data-action="close" aria-label="Close">×</button>
          </header>
          <div class="body">
            <div class="status-line">
              <p class="subtitle">${escapeHTML(subtitle)}</p>
              <p class="checked-at">${escapeHTML(checkedLine)}</p>
            </div>
            ${skipped}
            ${bodyHTML}
            ${snapshotHint}
            ${this._componentsSectionHTML()}
            ${this._channelSectionHTML()}
            ${this._storageSectionHTML()}
            ${info.err ? `<p class="err">Last check failed: ${escapeHTML(info.err)}</p>` : ""}
          </div>
          <footer>${actions}</footer>
        </div>
      `;
    }

    // _channelSectionHTML puts both channel controls side by side. They used
    // to sit in different parts of the dialog, which left the operator no way
    // to see that Core and Optimizer can track different channels.
    _channelSectionHTML() {
      const info = this._info || {};
      const channels = Array.isArray(info.channels) && info.channels.length
        ? info.channels
        : ["stable", "beta"];
      const selectedChannel = info.channel || "stable";
      const channelButtons = channels.map((channel) => `
        <button class="channel-option${selectedChannel === channel ? " active" : ""}"
                data-action="set-channel" data-channel="${escapeHTML(channel)}"
                aria-pressed="${selectedChannel === channel ? "true" : "false"}">
          ${escapeHTML(channel)}
        </button>`).join("");
      const channelNote = selectedChannel === "beta"
        ? "Beta receives prereleases and promoted stable releases."
        : "Stable receives production releases only.";

      const optimizerUpdates = (this._components && this._components.optimizer && this._components.optimizer.updates) || {};
      const optimizerConfigured = !!(this._components && this._components.optimizer && this._components.optimizer.configured);
      const optimizerChannels = Array.isArray(optimizerUpdates.channels) && optimizerUpdates.channels.length
        ? optimizerUpdates.channels
        : ["stable", "beta"];
      const optimizerChannel = optimizerUpdates.channel || "stable";
      const optimizerButtons = optimizerConfigured
        ? optimizerChannels.map((channel) => `
            <button class="channel-option${optimizerChannel === channel ? " active" : ""}"
                    data-action="set-optimizer-channel" data-channel="${escapeHTML(channel)}"
                    aria-pressed="${optimizerChannel === channel ? "true" : "false"}">
              ${escapeHTML(channel)}
            </button>`).join("")
        : "";
      const optimizerRow = optimizerConfigured
        ? `<div class="channel-row">
             <span class="channel-label">Optimizer</span>
             <div class="channel-options" role="group" aria-label="Optimizer update channel">${optimizerButtons}</div>
           </div>
           ${optimizerChannel !== selectedChannel
             ? `<p class="channel-note">Optimizer tracks ${escapeHTML(optimizerChannel)} while Core tracks ${escapeHTML(selectedChannel)}.</p>`
             : ""}`
        : "";

      // Only Core and the optimizer subscribe to a channel. A driver is
      // pinned to an exact version, and "stable"/"beta" only says where that
      // artifact came from — so these buttons must not appear to govern it.
      return `<details class="snapshots channels">
        <summary>Update channel · Core ${escapeHTML(selectedChannel)}${optimizerConfigured && optimizerChannel !== selectedChannel ? ` · Optimizer ${escapeHTML(optimizerChannel)}` : ""}</summary>
        <div class="channel-body">
          <div class="channel-row">
            <span class="channel-label">Core</span>
            <div class="channel-options" role="group" aria-label="Update channel">${channelButtons}</div>
          </div>
          <p class="channel-note">${escapeHTML(channelNote)}</p>
          ${optimizerRow}
          <p class="channel-note">Drivers follow no channel. Each one is pinned to a version you pick per driver above, from either stream.</p>
        </div>
      </details>`;
    }

    // _storageSectionHTML folds the two backup lists into one collapsed
    // section whose summary carries the counts, so an empty backup list
    // costs a phrase instead of a whole row.
    _storageSectionHTML() {
      const snapshots = this._snapshots;
      const backups = this._backups;
      const snapshotList = snapshots && snapshots.enabled && Array.isArray(snapshots.snapshots) ? snapshots.snapshots : null;
      const backupList = backups && backups.enabled && Array.isArray(backups.backups) ? backups.backups : null;
      if (!snapshotList && !backupList) return "";
      const parts = [];
      if (snapshotList) parts.push(`${snapshotList.length} rollback point${snapshotList.length === 1 ? "" : "s"}`);
      if (backupList) parts.push(`${backupList.length} full backup${backupList.length === 1 ? "" : "s"}`);
      return `<details class="snapshots storage">
        <summary>Backups · ${escapeHTML(parts.join(" · "))}</summary>
        ${this._snapshotsSectionHTML()}
        ${this._backupsSectionHTML()}
      </details>`;
    }

    _snapshotsSectionHTML() {
      const payload = this._snapshots;
      if (!payload || !payload.enabled) return "";
      const snaps = Array.isArray(payload.snapshots) ? payload.snapshots : [];
      const table = snaps.length
        ? `<table class="snapshots-table">
             <thead>
               <tr><th>Created</th><th>From → To</th><th>Size</th><th></th></tr>
             </thead>
             <tbody>${snaps.map((s) => this._snapshotRowHTML(s)).join("")}</tbody>
           </table>`
        : "";
      return `<div class="storage-block">
                <h4 class="storage-title">Local rollback points (${snaps.length})</h4>
                <div class="snapshots-intro">
                  <p class="dim">Stored on this device. They protect software changes, not SD-card failure.</p>
                  <button class="btn btn-small" data-action="create-snapshot" ${this._creatingSnapshot ? "disabled" : ""}>${this._creatingSnapshot ? "Creating…" : "Create rollback point"}</button>
                </div>
                ${table}
              </div>`;
    }

    _snapshotRowHTML(s) {
      const when = s.created_at ? new Date(s.created_at).toLocaleString() : "?";
      const range = s.action === "manual"
        ? (s.from_version || "current") + " checkpoint"
        : (s.from_version || "?") + " → " + (s.to_version || "?");
      const sizeMB = s.size_bytes ? (s.size_bytes / (1024 * 1024)).toFixed(1) + " MB" : "?";
      const deleting = this._deletingSnapshot === s.id;
      const restorable = s.restorable === true;
      // Rollback target for a *pre-rollback* safety snapshot takes the
      // operator forward again — the 'from' version is what was running
      // when we captured it. For a routine pre-update snapshot the 'from'
      // version is what was running before that update — rolling back to
      // it reverts that update. Either way the operation is the same:
      // restore the files from this snapshot.
      const deleteBtn = deleting
        ? `<span class="dim">deleting…</span>`
        : `<button class="btn btn-ghost btn-small" data-action="delete-snapshot" data-id="${escapeHTML(s.id)}" title="Delete this backup">Delete</button>`;
      const rollbackBtn = deleting
        ? ""
        : restorable
        ? `<button class="btn btn-small" data-action="rollback-snapshot" data-id="${escapeHTML(s.id)}" data-from="${escapeHTML(s.from_version || "")}" title="Restore this complete backup (service will restart)">Roll back</button>`
        : `<span class="dim" title="Older backups omitted history and are blocked to prevent data loss">legacy backup — restore disabled</span>`;
      return `<tr>
                <td class="nowrap">${escapeHTML(when)}</td>
                <td class="mono">${escapeHTML(range)}</td>
                <td class="nowrap">${escapeHTML(sizeMB)}</td>
                <td class="snapshot-actions">${rollbackBtn}${deleteBtn}</td>
              </tr>`;
    }

    _backupsSectionHTML() {
      const payload = this._backups;
      if (!payload || !payload.enabled) return "";
      const backups = Array.isArray(payload.backups) ? payload.backups : [];
      const location = payload.on_device
        ? "Backups are currently staged on this device. Download them to another device or configure an external backup path; local files do not survive SD-card failure."
        : "Backups are written to the configured external backup target.";
      const rows = backups.map((b) => this._backupRowHTML(b)).join("");
      return `<div class="storage-block full-backups">
                <h4 class="storage-title">Full backups (${backups.length})</h4>
                <div class="snapshots-intro backup-intro">
                  <p class="dim">${escapeHTML(location)}</p>
                  <button class="btn btn-small" data-action="create-backup" ${this._creatingBackup ? "disabled" : ""}>${this._creatingBackup ? "Creating and verifying…" : "Create full backup"}</button>
                </div>
                ${backups.length ? `<table class="snapshots-table">
                  <thead><tr><th>Created</th><th>Size</th><th>Status</th><th></th></tr></thead>
                  <tbody>${rows}</tbody>
                </table>` : `<p class="dim backups-empty">No full backups yet.</p>`}
              </div>`;
    }

    _backupRowHTML(b) {
      const when = b.created_at ? new Date(b.created_at).toLocaleString() : "?";
      const sizeMB = b.size_bytes ? (b.size_bytes / (1024 * 1024)).toFixed(1) + " MB" : "?";
      const verified = b.verified === true;
      const id = escapeHTML(b.id || "");
      const verifyLabel = this._verifyingBackup === b.id ? "Verifying…" : "Verify";
      const deleteLabel = this._deletingBackup === b.id ? "Deleting…" : "Delete";
      const download = safeHref(b.download_url || "");
      return `<tr>
                <td class="nowrap">${escapeHTML(when)}</td>
                <td class="nowrap">${escapeHTML(sizeMB)}</td>
                <td>${verified ? "✓ verified" : "not verified"}${b.on_device ? " · on device" : " · external"}</td>
                <td class="snapshot-actions">
                  ${download ? `<a class="btn btn-small backup-download" href="${escapeHTML(download)}" download>Download</a>` : ""}
                  <button class="btn btn-ghost btn-small" data-action="verify-backup" data-id="${id}" ${this._verifyingBackup ? "disabled" : ""}>${verifyLabel}</button>
                  <button class="btn btn-ghost btn-small" data-action="delete-backup" data-id="${id}" ${this._deletingBackup ? "disabled" : ""}>${deleteLabel}</button>
                </td>
              </tr>`;
    }

    _componentsSectionHTML() {
      const payload = this._components;
      if (!payload) return "";
      const optimizer = payload.optimizer || {};
      const optimizerUpdates = optimizer.updates || {};
      const optimizerRuntime = optimizer.runtime || {};
      const sharedUpdateStatus = payload.updates && payload.updates.status;
      const previousImages = (sharedUpdateStatus && sharedUpdateStatus.previous_images) || {};
      const optimizerCurrent = optimizerUpdates.current || optimizerRuntime.version || "";
      // Only claim a pending version when it actually differs. The old row
      // printed "v1.3.2 → v1.3.2" next to the words "up to date".
      const optimizerTarget = optimizerUpdates.latest && optimizerUpdates.latest !== optimizerCurrent
        ? optimizerUpdates.latest
        : "";
      const optimizerAction = optimizerUpdates.update_available
        ? `<button class="btn btn-small" data-action="optimizer-update">Update to ${escapeHTML(optimizerUpdates.latest || "")}</button>`
        : "";
      // Rolling back stays available whenever a previous image exists — that
      // is exactly the state you are in right after an update goes wrong. It
      // sits in the action column so it no longer competes with the status
      // text for the eye.
      const optimizerRollback = previousImages.optimizer
        ? `<button class="btn btn-ghost btn-small" data-action="optimizer-rollback" title="Restore the previous optimizer image">Roll back</button>`
        : "";
      const activeSolver = optimizer.active_solver || {};
      const optimizerFallbackActive = activeSolver.fallback || activeSolver.engine === "go-dp";
      const optimizerReason = optimizer.fallback_reason || optimizer.health_error || optimizer.error || "";
      const optimizerWarning = optimizer.degraded === true || optimizer.healthy === false
        ? `<p class="component-warning" role="alert"><strong>${optimizerFallbackActive ? "Planner fallback active." : "Optimizer unavailable."}</strong>${optimizerFallbackActive ? " Core is using the built-in Go planner." : " The current plan stays active until the next replan."}${optimizerReason ? " " + escapeHTML(optimizerReason) : ""}</p>`
        : "";

      const entries = this._driverEntries();
      const driverRows = entries.map((entry) => {
        const current = entry.installed_version || entry.version || "unknown";
        const busy = this._componentAction === "driver:" + entry.id;
        const action = entry.stable_available
          ? `<button class="btn btn-small" data-action="driver-change" data-id="${escapeHTML(entry.id || "")}" data-repository="${escapeHTML(entry.repository_id)}" data-version="${escapeHTML(entry.upstream_version)}" data-installed="false" ${this._componentAction ? "disabled" : ""}>${busy ? "Updating…" : "Stable " + escapeHTML(entry.upstream_version)}</button>`
          : "";
        const beta = entry.beta_candidate || {};
        const betaDriver = beta.driver || {};
        const betaAction = entry.beta_available
          ? `<button class="btn btn-ghost btn-small" data-action="driver-change" data-id="${escapeHTML(entry.id || "")}" data-repository="${escapeHTML(beta.repository_id || "")}" data-version="${escapeHTML(betaDriver.version)}" data-channel="beta" data-installed="false" ${this._componentAction ? "disabled" : ""}>${busy ? "Updating…" : "Beta " + escapeHTML(betaDriver.version)}</button>`
          : "";
        const history = entry.repository_id
          ? `<button class="btn btn-ghost btn-small" data-action="driver-versions" data-id="${escapeHTML(entry.id || "")}">History</button>`
          : "";
        // A locally edited driver has no signed counterpart to move to, so it
        // reports what it is instead of offering an action it cannot perform.
        const target = entry.stable_available ? entry.upstream_version : (betaDriver.version || "");
        const status = !entry.managed
          ? `<span class="dim" title="Edited on this device; no signed version to switch to">local copy</span>`
          : entry.pending_update
          ? `<span class="status-pending">${escapeHTML(target)} available</span>`
          : `<span class="dim">up to date</span>`;
        return `<tr>
          <th scope="row">${escapeHTML(entry.name || entry.id || "driver")}</th>
          <td class="dim mono">${escapeHTML(current)}</td>
          <td class="component-status">${status}</td>
          <td class="component-actions">${action}${betaAction}${history}</td>
        </tr>${this._driverVersionsHTML(entry.id)}`;
      }).join("");

      const history = this._componentHistory && Array.isArray(this._componentHistory.events)
        ? this._componentHistory.events.slice(0, 8) : [];
      const historyRows = history.map((event) => {
        const when = event.started_at_ms ? new Date(event.started_at_ms).toLocaleString() : "?";
        const range = (event.from_version || "?") + " → " + (event.to_version || "?");
        const result = event.message
          ? `${event.outcome || "?"} — ${event.message}`
          : (event.outcome || "?");
        return `<tr><td>${escapeHTML(when)}</td><td>${escapeHTML(event.kind + (event.kind === "driver" ? ":" + event.component_id : ""))}</td><td>${escapeHTML(range)}</td><td>${escapeHTML(result)}</td></tr>`;
      }).join("");

      const info = this._info || {};
      const coreStatus = info.update_available
        ? `<span class="status-pending">${escapeHTML(info.latest || "update")} available</span>`
        : `<span class="dim">up to date</span>`;
      const optimizerStatus = !optimizer.configured
        ? `<span class="dim">not configured</span>`
        : optimizerUpdates.update_available
        ? `<span class="status-pending">${escapeHTML(optimizerTarget || "update")} available</span>`
        : `<span class="dim">up to date</span>`;

      // One table listing every component, whether or not it has work waiting.
      // Rows only ever change their status and action cells, so the operator
      // reads the running inventory off a shape that holds still — and the
      // columns line up, which four independent grids could not manage.
      return `<div class="inventory">
        ${optimizerWarning}
        <table class="inventory-table">
          <thead>
            <tr><th scope="col">Component</th><th scope="col">Version</th><th scope="col">Status</th><th scope="col"><span class="sr-only">Actions</span></th></tr>
          </thead>
          <tbody>
            <tr>
              <th scope="row">Core</th>
              <td class="dim mono">${escapeHTML(payload.core && payload.core.version || info.current || "?")}</td>
              <td class="component-status">${coreStatus}</td>
              <td class="component-actions"></td>
            </tr>
            <tr class="optimizer-row">
              <th scope="row">Optimizer</th>
              <td class="dim mono">${escapeHTML(optimizerCurrent || "not running")}</td>
              <td class="component-status">${optimizerStatus}</td>
              <td class="component-actions">${optimizerAction}${optimizerRollback}</td>
            </tr>
            ${driverRows}
          </tbody>
        </table>
        ${historyRows ? `<details class="component-history"><summary>Update history</summary><table class="snapshots-table"><thead><tr><th>When</th><th>Component</th><th>Version</th><th>Result</th></tr></thead><tbody>${historyRows}</tbody></table></details>` : ""}
      </div>`;
    }

    _driverVersionsHTML(id) {
      const payload = id && this._driverVersions[id];
      if (!payload) return "";
      const versions = Array.isArray(payload.available) ? payload.available : [];
      if (!versions.length) return `<tr class="driver-history-row"><td colspan="4" class="dim">No signed or retained versions found.</td></tr>`;
      const rows = versions.map((candidate) => {
        const driver = candidate.driver || {};
        const installed = candidate.installed || null;
        const active = installed && installed.active;
        const label = active ? "active" : (installed ? "Activate" : "Install");
        const button = active ? `<span class="dim">active</span>` : `<button class="btn btn-ghost btn-small" data-action="driver-change" data-id="${escapeHTML(id)}" data-repository="${escapeHTML(candidate.repository_id || "")}" data-version="${escapeHTML(driver.version || "")}" data-sha="${escapeHTML(driver.sha256 || "")}" data-installed="${installed ? "true" : "false"}" ${this._componentAction ? "disabled" : ""}>${label}</button>`;
        return `<span class="driver-version"><span class="mono">${escapeHTML(driver.version || "?")}</span>${button}</span>`;
      }).join("");
      return `<tr class="driver-history-row"><td colspan="4"><div class="driver-history">${rows}</div></td></tr>`;
    }

    _updatingModalHTML() {
      const st = this._sidecarState || { state: "starting" };
      const action = st.action || "update";
      const elapsed = Math.round((Date.now() - this._updateStartedAt) / 1000);
      const label = actionLabel(st.state, action);
      const spinner = st.state === "failed" ? "" : `<span class="spinner"></span>`;
      const timedOut = !!st.timedOut;
      const failed = st.state === "failed";
      const progress = operationProgress(st, action);
      const phaseStarted = st.phase_started_at ? Date.parse(st.phase_started_at) : 0;
      const phaseElapsed = Math.max(0, Math.round((Date.now() - (phaseStarted > 0 ? phaseStarted : this._updateStartedAt)) / 1000));
      const byteProgress = st.progress_unit === "bytes" && st.progress_total > 0
        ? `<p class="dim">${escapeHTML(formatBytes(st.progress_current || 0))} / ${escapeHTML(formatBytes(st.progress_total))}</p>`
        : "";
      const progressHTML = failed ? "" : `
        <div class="update-progress" role="progressbar" aria-valuemin="0" aria-valuemax="${progress.total}" aria-valuenow="${progress.step}">
          <span style="width:${progress.percent}%"></span>
        </div>
        <p class="update-step">Step ${progress.step} of ${progress.total} · ${escapeHTML(label)}</p>
        <p class="dim">This step: ${escapeHTML(formatElapsed(phaseElapsed))}</p>
        ${byteProgress}`;

      const body = failed
        ? `<p class="err">${escapeHTML(st.message || "Update failed")}</p>
           <p>The main service may still be running — reload the page to check.</p>`
        : timedOut
        ? `<p>Still working after ${elapsed}s. The main container may have been slow to restart.</p>
           <p>You can reload manually if the UI keeps the overlay stuck.</p>`
        : `${progressHTML}
           ${this._operationDetailHTML(st)}
           <p class="dim">Total: ${escapeHTML(formatElapsed(elapsed))}. The page will reload automatically.</p>`;

      const footer = failed || timedOut
        ? `<button class="btn btn-primary" data-action="reload">Reload page</button>
           <button class="btn btn-ghost" data-action="close">Dismiss</button>`
        : `<span class="dim">Don't close this tab.</span>`;

      let title;
      switch (action) {
        case "restart":  title = "Restarting service"; break;
        case "rollback": title = "Rolling back"; break;
        case "component_rollback": title = "Rolling back optimizer"; break;
        default:         title = st.component === "optimizer" ? "Updating optimizer" : "Updating service";
      }

      return `
        <div class="backdrop"></div>
        <div class="modal" role="dialog" aria-modal="true" aria-live="polite">
          <header>
            <h3>${title}</h3>
          </header>
          <div class="body center">
            ${spinner}
            ${body}
          </div>
          <footer>${footer}</footer>
        </div>
      `;
    }

    _operationDetailHTML(st) {
      const msg = st && st.message ? `<p class="dim">${escapeHTML(st.message)}</p>` : "";
      if (!st) return msg;
      switch (st.state) {
        case "snapshotting":
          return msg + `<p class="dim">Creating a local rollback snapshot before touching the running service. Large history databases can take several minutes.</p>`;
        case "pulling":
          return msg + `<p class="dim">Downloading the pinned release image from GHCR.</p>`;
        case "restarting":
          return msg + `<p class="dim">Recreating the service. Short polling errors are expected while the container swaps.</p>`;
        case "checking":
          return msg + `<p class="dim">Core has started. Waiting for its API and health checks before marking the update complete.</p>`;
        case "restoring":
          return msg + `<p class="dim">Restoring files from the selected backup snapshot.</p>`;
        default:
          return msg;
      }
    }

    _wireModal(modal) {
      // Delegate: one listener on the shadow root, dispatch by data-action.
      this._shadow.querySelectorAll("[data-action]").forEach((el) => {
        el.addEventListener("click", (e) => {
          const action = e.currentTarget.dataset.action;
          switch (action) {
            case "close":
              this._phase = "idle";
              this._stopUpdateTimers();
              this._render();
              break;
            case "update":
              this._beginUpdate("update");
              break;
            case "restart":
              // Restarting drops control for as long as the service takes to
              // come back, so it asks first even though it changes no versions.
              if (window.confirm("Restart the service? Dispatch stops until Core is back and healthy.")) {
                this._beginUpdate("restart");
              }
              break;
            case "skip":
              this._skip();
              break;
            case "check":
              this._unskipAndCheck();
              break;
            case "set-channel":
              this._setChannel(e.currentTarget.dataset.channel);
              break;
            case "set-optimizer-channel":
              this._setOptimizerChannel(e.currentTarget.dataset.channel);
              break;
            case "optimizer-update":
              this._beginOptimizerUpdate(false);
              break;
            case "optimizer-rollback":
              if (window.confirm("Roll back only the optimizer to its previous healthy image? Core and drivers stay unchanged.")) {
                this._beginOptimizerUpdate(true);
              }
              break;
            case "driver-versions":
              this._loadDriverVersions(e.currentTarget.dataset.id);
              break;
            case "driver-change": {
              const dataset = e.currentTarget.dataset;
              const installed = dataset.installed === "true";
              const verb = installed ? "activate" : "install";
              const channel = dataset.channel || "stable";
              if (window.confirm(`${verb} ${channel} driver ${dataset.id} ${dataset.version}? Only affected driver instances restart and must return fresh telemetry.`)) {
                this._changeDriverVersion(dataset.id, dataset.repository, dataset.version, dataset.sha || "", installed, dataset.channel || "");
              }
              break;
            }
            case "reload":
              this._attemptReload();
              break;
            case "create-snapshot":
              this._createSnapshot();
              break;
            case "create-backup":
              this._createBackup();
              break;
            case "verify-backup":
              this._verifyBackup(e.currentTarget.dataset.id);
              break;
            case "delete-backup": {
              const id = e.currentTarget.dataset.id;
              if (id && window.confirm(`Delete full backup ${id}? This can't be undone.`)) {
                this._deleteBackup(id);
              }
              break;
            }
            case "delete-snapshot": {
              const id = e.currentTarget.dataset.id;
              // Simple confirm — this is a destructive operation but a
              // recoverable one (the retention/prune logic will regenerate
              // on future updates). Don't over-engineer the dialog.
              if (id && window.confirm(`Delete snapshot ${id}? This can't be undone.`)) {
                this._deleteSnapshot(id);
                this._render(); // reflect the "deleting…" state immediately
              }
              break;
            }
            case "rollback-snapshot": {
              const id = e.currentTarget.dataset.id;
              const from = e.currentTarget.dataset.from || "that point";
              // Sharper warning for rollback — it stops the service,
              // swaps live state, and restarts. Much more visible
              // consequence than a Delete.
              const msg =
                `Roll back to ${id}?\n\n` +
                `This will stop the service, restore state.db + config.yaml ` +
                `from the snapshot (state as of "${from}"), and restart. ` +
                `Any data written since the snapshot will be lost.\n\n` +
                `A pre-rollback backup of the current state is saved ` +
                `automatically so you can roll forward again.`;
              if (id && window.confirm(msg)) {
                this._beginRollback(id);
              }
              break;
            }
          }
        });
      });
    }

    _styles() {
      return `
        :host { all: initial; font-family: inherit; }
        .hidden { display: none !important; }
        /* The header's status slot. Marks sit side by side when an update
           and a degraded optimizer are pending at once. */
        .marks { display: inline-flex; align-items: center; gap: 0.1rem; }
        .mark {
          appearance: none;
          background: transparent;
          border: none;
          padding: 0 0.15rem;
          display: inline-flex;
          align-items: center;
          justify-content: center;
          line-height: 0;
        }
        .mark .icon { display: block; width: 16px; height: 16px; }
        button.mark { cursor: pointer; }

        /* An update waiting is an affordance — "something is downloadable,
           open me" — so it fades to ask for attention. Blue keeps it clear
           of every alarm colour in the palette; nothing is wrong. */
        .mark.update { color: var(--cyan, #38bdf8); animation: fade 1.8s ease-in-out infinite; }

        /* A degraded optimizer is a state, not an affordance: the planner
           has fallen back to the Go solver, which often needs no action.
           The triangle carries that on silhouette alone, so the two marks
           stay apart for an operator who cannot separate them by colour
           — the failure that got the old shared amber dot misread as an
           update icon in the field (#690, #693). */
        .mark.warning { color: var(--amber, #f59e0b); animation: fade 1.8s ease-in-out infinite; }

        /* Nothing pending: the quiet "all good" light. Static, because a
           steady state should not compete with the two that want reading. */
        .mark.ok { color: var(--green-e, #22c55e); }

        /* Connection lost outranks and replaces the others — see _render. */
        .mark.offline { color: var(--red-e, #ef4444); }

        @keyframes fade {
          0%, 100% { opacity: 1; }
          50%      { opacity: 0.3; }
        }
        /* Fading is the only cue here that is pure motion; shape and colour
           already carry the meaning without it. */
        @media (prefers-reduced-motion: reduce) {
          .mark { animation: none; }
        }
        .backdrop {
          position: fixed; inset: 0;
          background: rgba(0,0,0,0.65);
          z-index: 1000;
        }
        .modal {
          position: fixed;
          top: 50%; left: 50%;
          transform: translate(-50%, -50%);
          width: min(94vw, 720px);
          /* Cap height + scroll so shorter viewports can't push the
             header (close ×) or the footer (Update / Restart / Skip)
             off-screen. Without this the modal clipped above the
             viewport and the operator saw only the middle "Release
             notes" block with no actionable buttons — reported on a
             laptop-height browser running the dashboard. */
          max-height: 85vh;
          overflow-y: auto;
          background: var(--ink-raised, #161616);
          color: var(--fg, #e8e8e8);
          border: 1px solid var(--line, #2a2a2a);
          border-radius: var(--radius-sm, 8px);
          z-index: 1001;
          display: flex; flex-direction: column;
          font-family: var(--sans, system-ui, -apple-system, sans-serif);
          font-size: 0.9rem;
        }
        .modal header {
          display: flex; align-items: center; justify-content: space-between;
          padding: 0.9rem 1rem;
          border-bottom: 1px solid var(--line, #2a2a2a);
        }
        .modal h3 { margin: 0; font-size: 1rem; }
        .modal .x {
          appearance: none; background: transparent;
          color: var(--fg-dim, #a0a0a0);
          border: none; cursor: pointer;
          font-size: 1.25rem; line-height: 1;
        }
        .modal .body { padding: 1rem; }
        .modal .body.center { text-align: center; padding: 1.4rem 1rem; }
        .status-line {
          display: flex;
          align-items: baseline;
          justify-content: space-between;
          gap: 0.75rem;
          flex-wrap: wrap;
          margin-bottom: 0.75rem;
        }
        .subtitle { margin: 0; color: var(--fg, #e8e8e8); font-size: 0.95rem; }
        .checked-at { margin: 0; color: var(--fg-dim, #a0a0a0); font-size: 0.75rem; white-space: nowrap; }
        .skipped-note { margin: 0 0 0.75rem; }
        .channel-body { padding: 0.1rem 0.75rem 0.75rem; }
        .channel-row {
          display: grid;
          grid-template-columns: minmax(0, 1fr) minmax(0, 1.4fr);
          align-items: center;
          gap: 0.6rem;
          margin-bottom: 0.4rem;
        }
        .channel-label {
          color: var(--fg-dim, #a0a0a0);
          font-size: 0.78rem;
        }
        .channel-options {
          display: grid;
          grid-auto-flow: column;
          grid-auto-columns: minmax(0, 1fr);
          border: 1px solid var(--line, #2a2a2a);
          border-radius: var(--radius-xs, 4px);
          overflow: hidden;
        }
        .channel-option {
          appearance: none;
          min-width: 0;
          padding: 0.4rem 0.3rem;
          border: 0;
          border-right: 1px solid var(--line, #2a2a2a);
          background: transparent;
          color: var(--fg-dim, #a0a0a0);
          cursor: pointer;
          font: inherit;
          font-size: 0.8rem;
          text-transform: capitalize;
        }
        .channel-option:last-child { border-right: 0; }
        .channel-option:hover { background: color-mix(in srgb, var(--fg) 4%, transparent); }
        .channel-option.active {
          background: var(--accent-e, #f59e0b);
          color: var(--on-accent, #0a0a0a);
          font-weight: 600;
        }
        .channel-note {
          margin: 0.35rem 0 0;
          color: var(--fg-dim, #a0a0a0);
          font-size: 0.75rem;
          line-height: 1.35;
        }
        .changelog {
          margin-top: 0.75rem;
          border: 1px solid var(--line, #2a2a2a);
          border-radius: var(--radius-xs, 4px);
          background: color-mix(in srgb, var(--fg) 2%, transparent);
        }
        .changelog > summary {
          padding: 0.5rem 0.75rem;
          cursor: pointer;
          font-weight: 600;
          font-size: 0.85rem;
          color: var(--fg-dim, #a0a0a0);
          list-style: none;
        }
        .changelog > summary::-webkit-details-marker { display: none; }
        .changelog > summary::before {
          content: "▸";
          display: inline-block;
          margin-right: 0.4rem;
          transition: transform 0.15s;
        }
        .changelog[open] > summary::before { transform: rotate(90deg); }
        .changelog-body {
          padding: 0.25rem 0.9rem 0.5rem;
          max-height: 40vh;
          overflow-y: auto;
          font-size: 0.85rem;
          line-height: 1.45;
        }
        .changelog-body h4 {
          margin: 0.75rem 0 0.3rem;
          font-size: 0.9rem;
          color: var(--fg, #e8e8e8);
        }
        .changelog-body h5 {
          margin: 0.6rem 0 0.25rem;
          font-size: 0.8rem;
          color: var(--fg-dim, #a0a0a0);
          text-transform: uppercase;
          letter-spacing: 0.03em;
        }
        .changelog-body ul {
          margin: 0.25rem 0 0.25rem;
          padding-left: 1.1rem;
        }
        .changelog-body li { margin-bottom: 0.2rem; }
        .changelog-body p { margin: 0.35rem 0; }
        .changelog-body code {
          background: color-mix(in srgb, var(--fg) 8%, transparent);
          padding: 0.05rem 0.25rem;
          border-radius: 3px;
          font-size: 0.82rem;
        }
        .changelog-body a {
          color: var(--accent-e, #f59e0b);
          text-decoration: none;
        }
        .changelog-body a:hover { text-decoration: underline; }
        .changelog-link {
          margin: 0.4rem 0.9rem 0.6rem;
          font-size: 0.8rem;
        }
        .notes-link {
          color: var(--accent-e, #f59e0b);
          text-decoration: none;
        }
        .notes-link:hover { text-decoration: underline; }
        .snapshot-hint {
          margin-top: 0.75rem;
          padding: 0.5rem 0.7rem;
          border: 1px solid var(--line, #2a2a2a);
          border-radius: var(--radius-xs, 4px);
          background: color-mix(in srgb, var(--fg) 6%, transparent);
          color: var(--fg-dim, #a0a0a0);
          font-size: 0.78rem;
          line-height: 1.4;
        }
        .snapshot-hint p { margin: 0; }
        .snapshots {
          margin-top: 0.75rem;
          border: 1px solid var(--line, #2a2a2a);
          border-radius: var(--radius-xs, 4px);
          background: color-mix(in srgb, var(--fg) 2%, transparent);
        }
        .snapshots > summary {
          padding: 0.5rem 0.75rem;
          cursor: pointer;
          font-weight: 600;
          font-size: 0.85rem;
          color: var(--fg-dim, #a0a0a0);
          list-style: none;
        }
        .snapshots > summary::-webkit-details-marker { display: none; }
        .snapshots > summary::before {
          content: "▸";
          display: inline-block;
          margin-right: 0.4rem;
          transition: transform 0.15s;
        }
        .snapshots[open] > summary::before { transform: rotate(90deg); }
        .storage-block { padding: 0 0 0.4rem; }
        .storage-block + .storage-block { border-top: 1px solid var(--line, #2a2a2a); padding-top: 0.6rem; }
        .storage-title {
          margin: 0.3rem 0.75rem 0.1rem;
          font-size: 0.78rem;
          font-weight: 600;
          color: var(--fg, #e8e8e8);
        }
        .snapshots-intro {
          display: flex;
          align-items: center;
          justify-content: space-between;
          gap: 0.75rem;
          padding: 0.1rem 0.75rem 0.6rem;
        }
        .snapshots-intro p { margin: 0; }
        .backup-intro p { max-width: 70%; }
        .backups-empty { margin: 0.25rem 0.75rem 0.7rem; }
        .backup-download { text-decoration: none; }
        .inventory { margin-top: 0.25rem; }
        .sr-only {
          position: absolute; width: 1px; height: 1px;
          padding: 0; margin: -1px; overflow: hidden;
          clip-path: inset(50%); white-space: nowrap;
        }
        .component-warning { margin: 0 0 0.65rem; padding: 0.55rem 0.65rem; border: 1px solid var(--accent-e, #f59e0b); border-radius: var(--radius-xs, 4px); background: rgba(245,158,11,0.12); color: var(--fg, #e8e8e8); font-size: 0.78rem; line-height: 1.4; overflow-wrap: anywhere; }
        .inventory-table {
          width: 100%;
          border-collapse: collapse;
          border: 1px solid var(--line, #2a2a2a);
          border-radius: var(--radius-xs, 4px);
          font-size: 0.8rem;
        }
        .inventory-table th,
        .inventory-table td {
          padding: 0.45rem 0.65rem;
          text-align: left;
          vertical-align: middle;
          border-top: 1px solid var(--line, #2a2a2a);
        }
        .inventory-table thead th {
          border-top: 0;
          background: color-mix(in srgb, var(--fg) 3%, transparent);
          color: var(--fg-dim, #a0a0a0);
          font-size: 0.68rem;
          font-weight: 600;
          text-transform: uppercase;
          letter-spacing: 0.04em;
        }
        .inventory-table tbody th {
          font-size: 0.82rem;
          font-weight: 600;
          color: var(--fg, #e8e8e8);
        }
        /* A version string reads as one token or not at all, so it never
           wraps; the action column shrinks to its buttons and the component
           name absorbs whatever width is left. */
        .inventory-table td.mono,
        .inventory-table td.component-status { white-space: nowrap; }
        .status-pending { color: var(--accent-e, #f59e0b); font-weight: 600; }
        .component-actions { width: 1%; white-space: nowrap; text-align: right; }
        .component-actions > * { margin-left: 0.3rem; }
        .component-actions .btn { white-space: nowrap; }
        .driver-history-row > td { padding-top: 0; }
        .driver-history { display: flex; gap: 0.35rem; flex-wrap: wrap; }
        .driver-version { display: inline-flex; align-items: center; gap: 0.25rem; padding: 0.2rem 0.3rem; border: 1px solid var(--line, #2a2a2a); border-radius: 4px; }
        .component-history { margin: 0.5rem 0 0; }
        .component-history > summary { cursor: pointer; color: var(--fg-dim, #a0a0a0); font-size: 0.75rem; }
        .snapshots-table {
          width: 100%;
          border-collapse: collapse;
          font-size: 0.78rem;
          color: var(--fg-dim, #a0a0a0);
        }
        .snapshots-table th,
        .snapshots-table td {
          padding: 0.3rem 0.75rem;
          text-align: left;
          border-top: 1px solid var(--line, #2a2a2a);
        }
        .snapshots-table th {
          font-weight: 600;
          border-top: none;
          color: var(--fg, #e8e8e8);
        }
        .snapshots-table .nowrap { white-space: nowrap; }
        .snapshots-table .mono { font-family: var(--mono, ui-monospace, monospace); }
        .snapshot-actions {
          display: flex;
          gap: 0.3rem;
          justify-content: flex-end;
          flex-wrap: wrap;
        }
        .btn-small {
          padding: 0.2rem 0.55rem;
          font-size: 0.75rem;
        }
        .err {
          margin-top: 0.75rem;
          color: var(--red-e, #f87171); font-size: 0.85rem;
        }
        .dim { color: var(--fg-dim, #a0a0a0); font-size: 0.8rem; }
        .modal footer {
          display: flex; gap: 0.5rem; justify-content: flex-end;
          padding: 0.75rem 1rem;
          border-top: 1px solid var(--line, #2a2a2a);
          flex-wrap: wrap;
          /* Stick to the bottom of the modal while body scrolls so
             the primary action (Update / Restart) remains visible
             however long the release-notes body grows. */
          position: sticky;
          bottom: 0;
          background: var(--ink-raised, #161616);
        }
        .btn {
          appearance: none;
          padding: 0.4rem 0.9rem;
          border: 1px solid var(--line, #2a2a2a);
          background: transparent;
          color: var(--fg, #e8e8e8);
          border-radius: var(--radius-xs, 4px);
          cursor: pointer;
          font-size: 0.85rem;
          font-family: inherit;
        }
        .btn:hover { background: color-mix(in srgb, var(--fg) 4%, transparent); }
        .btn-primary {
          background: var(--accent-e, #f59e0b);
          border-color: var(--accent-e, #f59e0b);
          /* the shared design system: on-accent text is near-black (#0a0a0a), never
             white — keeps the amber legible without halation in dark
             mode and stays correct when the theme flips to light. */
          color: var(--on-accent, #0a0a0a);
          font-weight: 600;
        }
        .btn-primary:hover { opacity: 0.9; background: var(--accent-e, #f59e0b); }
        .btn-ghost { color: var(--fg-dim, #a0a0a0); border-color: transparent; }
        .spinner {
          display: inline-block;
          width: 20px; height: 20px;
          border: 2px solid var(--line, #2a2a2a);
          border-top-color: var(--accent-e, #f59e0b);
          border-radius: 50%;
          animation: spin 0.9s linear infinite;
          margin-bottom: 0.6rem;
        }
        .update-progress {
          width: min(360px, 82vw);
          height: 8px;
          margin: 0.2rem auto 0.7rem;
          overflow: hidden;
          border: 1px solid var(--line, #2a2a2a);
          border-radius: 999px;
          background: color-mix(in srgb, var(--fg) 4%, transparent);
        }
        .update-progress > span {
          display: block;
          height: 100%;
          min-width: 4px;
          border-radius: inherit;
          background: var(--accent-e, #f59e0b);
          transition: width 0.25s ease;
        }
        .update-step { margin: 0.25rem 0; font-weight: 600; }
        .body.center .dim { margin: 0.25rem 0; }
        @keyframes spin { to { transform: rotate(360deg); } }
        /* Four columns don't fit a phone. Drop the header row and let each
           row stack into its own block, with the component name leading. */
        @media (max-width: 560px) {
          .inventory-table thead { display: none; }
          .inventory-table,
          .inventory-table tbody,
          .inventory-table tr,
          .inventory-table th,
          .inventory-table td { display: block; width: auto; }
          .inventory-table tr { border-top: 1px solid var(--line, #2a2a2a); padding: 0.3rem 0; }
          .inventory-table tbody tr:first-child { border-top: 0; }
          .inventory-table th,
          .inventory-table td { border-top: 0; padding: 0.15rem 0.65rem; }
          /* Stacked rows are no longer columns to align, so the action cell
             stops holding itself to one line — otherwise its buttons set the
             modal's width and the whole dialog scrolls sideways. */
          .component-actions {
            width: auto;
            white-space: normal;
            text-align: left;
            padding-top: 0.3rem;
          }
          .component-actions > * { margin: 0 0.3rem 0.3rem 0; }
          .channel-row { grid-template-columns: minmax(0, 1fr); gap: 0.25rem; }
        }
      `;
    }
  }

  function actionLabel(state, action) {
    switch (state) {
      case "snapshotting": return "Creating backup";
      case "pulling":    return "Pulling new image";
      case "restoring":  return "Restoring snapshot";
      case "restarting":
        if (action === "restart")  return "Restarting service";
        if (action === "rollback") return "Restarting on restored state";
        return "Applying update";
      case "checking":   return "Checking service health";
      case "done":       return "Reloading";
      case "failed":     return "Failed";
      default:
        if (action === "restart")  return "Restarting";
        if (action === "rollback") return "Starting rollback";
        return "Starting update";
    }
  }

  function isUpdateInFlight(state) {
    return ["starting", "snapshotting", "pulling", "restarting", "checking", "restoring"].includes(state);
  }

  function operationProgress(st, action) {
    let total = Number(st && st.total_steps) || 0;
    let step = Number(st && st.step) || 0;
    if (!total) {
      const hasBackup = action === "update" && (!st || !st.component || st.component === "core");
      total = hasBackup ? 4 : 3;
      const offset = hasBackup ? 1 : 0;
      switch (st && st.state) {
        case "snapshotting": step = 1; break;
        case "pulling": step = 1 + offset; break;
        case "restarting": step = 2 + offset; break;
        case "checking":
        case "done": step = 3 + offset; break;
        default: step = 1;
      }
    }
    step = Math.max(0, Math.min(step, total));
    return { step, total, percent: total > 0 ? Math.round((step / total) * 100) : 0 };
  }

  function formatElapsed(seconds) {
    const value = Math.max(0, Number(seconds) || 0);
    if (value < 60) return `${value}s`;
    const minutes = Math.floor(value / 60);
    const rest = value % 60;
    return `${minutes}m ${rest}s`;
  }

  function formatBytes(bytes) {
    let value = Math.max(0, Number(bytes) || 0);
    const units = ["B", "KB", "MB", "GB"];
    let unit = 0;
    while (value >= 1024 && unit < units.length - 1) {
      value /= 1024;
      unit++;
    }
    const digits = unit > 0 && value < 10 ? 1 : 0;
    return `${value.toFixed(digits)} ${units[unit]}`;
  }

  // safeHref rejects anything that isn't http:/https:. The release-notes URL
  // comes from the GitHub Releases API, but we belt-and-brace here: an
  // attacker who somehow lands a javascript:/data: URL into the payload
  // shouldn't get code execution via the anchor href.
  function safeHref(u) {
    if (!u) return "";
    try {
      const p = new URL(String(u), window.location.href);
      if (p.protocol === "http:" || p.protocol === "https:") return p.toString();
    } catch (_) { /* fall through */ }
    return "";
  }

  function escapeHTML(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  // renderReleaseBody turns GitHub-flavored markdown (as emitted by
  // semantic-release: headings, bullet lists, links, `code`, **bold**)
  // into a safe HTML subset. Strategy: escape everything first, then
  // rewrite a short whitelist of markdown tokens. Untrusted content —
  // link URLs — is routed through safeHref so a `javascript:` href
  // can't sneak in.
  //
  // What we handle (enough for conventional-commits changelogs):
  //   ##, ###           → h4, h5
  //   - x / * x         → unordered list (adjacent bullets grouped)
  //   **bold**          → <strong>
  //   `code`            → <code>
  //   [text](url)       → <a href=...>   (url filtered)
  //   blank line        → paragraph break
  //
  // What we deliberately drop: images, tables, raw HTML, setext
  // headings, nested lists, numbered lists. They're rare in release
  // notes and the operator still has the "Open on GitHub ↗" link for
  // the full formatted version.
  function renderReleaseBody(md) {
    const escaped = escapeHTML(String(md || "").trim());
    const lines = escaped.split(/\r?\n/);
    const out = [];
    let inList = false;
    const flushList = () => {
      if (inList) {
        out.push("</ul>");
        inList = false;
      }
    };
    for (let raw of lines) {
      const line = raw.replace(/\s+$/, "");
      if (!line) {
        flushList();
        continue;
      }
      // Bullet: "- text" or "* text" (leading spaces tolerated for
      // semantic-release which indents scope details).
      const bullet = line.match(/^\s*[*-]\s+(.*)$/);
      if (bullet) {
        if (!inList) {
          out.push("<ul>");
          inList = true;
        }
        out.push("<li>" + renderInline(bullet[1]) + "</li>");
        continue;
      }
      flushList();
      // Headings
      const h3 = line.match(/^###\s+(.*)$/);
      if (h3) { out.push("<h5>" + renderInline(h3[1]) + "</h5>"); continue; }
      const h2 = line.match(/^##\s+(.*)$/);
      if (h2) { out.push("<h4>" + renderInline(h2[1]) + "</h4>"); continue; }
      // Paragraph fallback.
      out.push("<p>" + renderInline(line) + "</p>");
    }
    flushList();
    return out.join("");
  }

  // renderInline handles **bold**, `code`, and [text](url) on an
  // already-HTML-escaped line. Order matters: code first so backticks
  // can't eat a `**bold**` marker that happened to be inside code.
  function renderInline(s) {
    // Inline code: backticks are already literal in the escaped text.
    s = s.replace(/`([^`]+)`/g, (_m, code) => "<code>" + code + "</code>");
    // Bold: **text**
    s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
    // Links: [text](url). The URL has been HTML-escaped already (amp →
    // &amp;), so decode just the &amp; inside the href before running
    // safeHref — otherwise a legitimate query-string URL gets rejected.
    s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_m, text, url) => {
      const clean = String(url).replace(/&amp;/g, "&");
      const safe = safeHref(clean);
      if (!safe) return text; // drop the link, keep the visible text
      return '<a href="' + escapeHTML(safe) + '" target="_blank" rel="noopener">' + text + "</a>";
    });
    return s;
  }

  customElements.define("ftw-update-badge", FtwUpdateBadge);
})();
