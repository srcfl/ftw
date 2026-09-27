// <ftw-update-badge> — self-contained Web Component that checks for a
// newer FTW release, renders the header status marks, and shows the running
// version. The owner updates a native install with the ftw command (ADR
// 0007, decision 12); this dialog follows a run started there until Core is
// back, then reloads. Everything lives in shadow DOM so dashboard styles
// are untouched.
//
// Placement: one <ftw-update-badge></ftw-update-badge> inside the header.
// The element exposes a public open() method so the #version span (which
// lives outside shadow DOM) can also trigger the modal.

(function () {
  "use strict";

  function apiFetch(path, opts) {
    return fetch(path, opts);
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
  // A transient GitHub 5xx is stored as last_error. Do not wait the full
  // 3 h poll to try again — three bounded force checks, then stop.
  const ERROR_RETRY_DELAYS_MS = [30 * 1000, 90 * 1000, 180 * 1000];
  const STATUS_INTERVAL_MS = 2000;               // during updates
  const UPDATE_SOFT_TIMEOUT_MS = 180 * 1000;     // after this we stop auto-reloading

  class FtwUpdateBadge extends HTMLElement {
    constructor() {
      super();
      this._shadow = this.attachShadow({ mode: "open" });
      this._dialogHost = null;
      this._dialogRoot = null;
      this._info = null;              // last /api/version/check payload
      this._lastRun = null;           // last finished update or rollback
      this._phase = "idle";           // idle | dialog | updating
      this._runStatus = null;         // last /api/version/update/status
      this._updateStartedAt = 0;
      this._expectedRun = null;
      this._checkTimer = null;
      this._errorRetryTimer = null;
      this._errorRetries = 0;
      this._statusTimer = null;
      this._elapsedTimer = null;
      this._disabled = false;         // set true on 503 (feature gated off)
      this._components = null;        // last /api/components payload
      this._connected = true;         // header liveness light; see setConnected()
      this._bootHealth = null;
      this._bootConnected = true;
      this._migrationHTML = "";
      this._resumeGeneration = 0;
      this._render();
    }

    connectedCallback() {
      this._resumeUpdateStatus();
      this._refresh(false);
      this._refreshComponents(false);
      this._checkTimer = setInterval(() => {
        this._refresh(false);
        this._refreshComponents(false);
      }, CHECK_INTERVAL_MS);
    }

    _resumeUpdateStatus() {
      const generation = this._resumeGeneration;
      return apiFetch("/api/version/update/status", { cache: "no-store" })
        .then((r) => (r.ok ? r.json() : null))
        .then((st) => {
          if (!this.isConnected || generation !== this._resumeGeneration) return;
          if (st && (st.state === "done" || st.state === "failed")) this._lastRun = st;
          if (!st || !isUpdateInFlight(st.state) || this._phase === "updating") return;
          const started = st.started_at ? Date.parse(st.started_at) : 0;
          this._phase = "updating";
          this._runStatus = st;
          this._updateStartedAt = started > 0 ? started : Date.now();
          this._expectedRun = {
            action: st.action || "update",
            target: st.target || "",
            component: st.component || "",
          };
          this._render();
          this._startElapsedTicker();
          this._startStatusPolling();
        })
        .catch(() => { /* the service may still be starting */ });
    }

    disconnectedCallback() {
      this._resumeGeneration += 1;
      this._removeDialog();
      clearInterval(this._checkTimer);
      clearTimeout(this._errorRetryTimer);
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

    setBootHealth(health) {
      const wasStarting = !!this._bootHealth;
      this._bootConnected = !!health;
      if (health && health.status !== "starting") {
        this._bootHealth = null;
        this._migrationHTML = "";
        if (wasStarting) {
          if (!this._expectedRun && this._phase === "updating") {
            this._phase = "dialog";
            this._stopUpdateTimers();
          }
          this._refresh(false);
          this._render();
        }
        return;
      }
      if (health) this._bootHealth = health;
      if (!this._bootHealth) return;
      const snapshot = this._bootHealth;
      const connected = this._bootConnected;
      import("/history-migration.js").then(({ migrationHTML }) => {
        if (this._bootHealth !== snapshot || this._bootConnected !== connected) return;
        this._migrationHTML = migrationHTML(snapshot.migration, { boot: true, connected });
        this._render();
      }).catch(() => {});
      this._render();
    }

    // Public: called by the header #version click handler in index.html so
    // the operator can open the modal without aiming at the tiny dot. No-op
    // when the backend has told us the feature is gated off.
    open() {
      if (this._disabled) return;
      if (this._phase === "updating" || this._bootHealth) {
        this._phase = "updating";
        this._render();
        this._startStatusPolling();
        return;
      }
      this._phase = "dialog";
      this._render();
      // Shows the last run's result, or follows one started by ftw.
      this._resumeUpdateStatus().finally(() => this._render());
      this._refresh(false); // surface the freshest info when opened
      this._refreshComponents(false);
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

    // Permanently shut the element down: stop polling, clear shadow DOM, hide
    // from layout, and fire an event so the #version bridge can drop its
    // cursor/pointer affordance. Only an explicit feature-disabled response
    // does this; the same 503 status also occurs during normal startup.
    _disable() {
      if (this._disabled) return;
      this._disabled = true;
      clearInterval(this._checkTimer);
      clearInterval(this._statusTimer);
      clearInterval(this._elapsedTimer);
      this._shadow.innerHTML = "";
      this._removeDialog();
      this.hidden = true;
      this.dispatchEvent(new CustomEvent("ftw-selfupdate-disabled", { bubbles: true }));
    }

    // ---- data ----
    _refresh(force) {
      if (this._disabled) return;
      const url = force ? "/api/version/check?force=1" : "/api/version/check";
      apiFetch(url)
        .then(async (r) => {
          const body = await r.json().catch(() => null);
          if (r.status === 503 && body?.error === "starting") {
            this.setBootHealth({ status: "starting", migration: body.migration });
            return null;
          }
          if (r.status === 503 && body?.error === "self-update disabled") {
            this._disable();
            return null;
          }
          return { ok: r.ok, body };
        })
        .then((result) => {
          if (!result) return; // disabled, nothing to render
          // The handler returns the full Info schema on both success and the
          // force=1 error path, so we render either way. When ok=false,
          // body.err carries the reason and the UI shows "Last check failed".
          if (result.body && typeof result.body === "object") {
            this._info = result.body;
            this._render();
            this._scheduleErrorRetry();
          }
        })
        .catch(() => { /* silent — periodic noise is not useful */ });
    }

    _scheduleErrorRetry() {
      const err = this._info && this._info.err;
      if (!err) {
        this._errorRetries = 0;
        clearTimeout(this._errorRetryTimer);
        return;
      }
      if (this._errorRetries >= ERROR_RETRY_DELAYS_MS.length) return;
      clearTimeout(this._errorRetryTimer);
      const delay = ERROR_RETRY_DELAYS_MS[this._errorRetries];
      this._errorRetryTimer = setTimeout(() => {
        this._errorRetries += 1;
        this._refresh(true);
      }, delay);
    }

    _postJSON(url, body) {
      return apiFetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: body ? JSON.stringify(body) : undefined,
      }).then((r) => r.json().then((j) => ({ ok: r.ok, body: j })));
    }

    // ---- actions ----
    _unskipAndCheck() {
      // "Check for updates" also clears skip so a hidden version resurfaces
      // without waiting for something newer. Matches user intent: if you're
      // asking, you want to see it.
      this._postJSON("/api/version/unskip", null).finally(() => this._refresh(true));
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
      // 1) Poll the saved update status.
      apiFetch("/api/version/update/status")
        .then((r) => (r.ok ? r.json() : null))
        .then((st) => {
          if (this._phase !== "updating" || !this.isConnected) return;
          if (st && this._statusMatchesCurrentRun(st)) {
            const keepTimeout = this._runStatus && this._runStatus.timedOut &&
              this._runStatus.state === st.state &&
              (this._runStatus.phase_started_at || "") === (st.phase_started_at || "");
            this._runStatus = keepTimeout ? Object.assign({}, st, { timedOut: true }) : st;
            this._render();
            if (st.state === "done") {
              this._attemptReload();
            }
          }
        })
        .catch(() => {
          // Core is likely mid-restart; expected — keep polling.
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
      if (this._expectedRun.component && st.component && st.component !== this._expectedRun.component) return false;

      // The status file can still contain an old "done" from the previous
      // update. Never let that stale terminal state auto-reload the page
      // for the new run.
      const startedMs = st.started_at ? Date.parse(st.started_at) : 0;
      if (startedMs && startedMs < this._updateStartedAt - 5000) return false;
      if (!startedMs && (st.state === "done" || st.state === "failed" || st.state === "idle")) return false;
      return true;
    }

    _markSoftTimeout() {
      if (this._bootHealth) return false;
      const phaseStarted = this._runStatus && this._runStatus.phase_started_at
        ? Date.parse(this._runStatus.phase_started_at)
        : 0;
      const timeoutStartedAt = phaseStarted > 0 ? phaseStarted : this._updateStartedAt;
      if (Date.now() - timeoutStartedAt <= UPDATE_SOFT_TIMEOUT_MS) return false;
      if (!this._runStatus || this._runStatus.state === "done" || this._runStatus.timedOut) return false;
      this._runStatus = Object.assign({}, this._runStatus, { timedOut: true });
      return true;
    }

    _attemptReload() {
      // Give the new Core a moment to open its listener, then
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

    // Driver versions are chosen per device under Settings › Devices, the
    // one place they are shown and changed, so only Core counts here.
    _pendingUpdates() {
      const info = this._info || {};
      const core = !!(info.update_available && !info.skipped);
      return { core, total: core ? 1 : 0 };
    }

    _render() {
      // A 503 from /api/version/check permanently disables this component.
      // Component requests start alongside that check, so their
      // responses can arrive later. Never let one of those responses rebuild
      // controls that open() will refuse to use.
      if (this._disabled) {
        this._shadow.innerHTML = "";
        this._removeDialog();
        this.hidden = true;
        return;
      }
      const info = this._info || {};
      const optimizer = this._components && this._components.optimizer;
      const pending = this._pendingUpdates();
      const showDot = pending.total > 0 && this._phase !== "updating";
      const showOptimizerWarning = !!(optimizer && optimizer.configured && (optimizer.degraded === true || optimizer.healthy === false)) && this._phase !== "updating";
      const showBadge = showDot || showOptimizerWarning;
      const activeSolver = optimizer && optimizer.active_solver;
      // Core producing the plan is the default, so the engine name says
      // nothing about health; only the fallback flag means the operator asked
      // for the external planner and did not get it.
      const optimizerFallbackActive = !!(activeSolver && activeSolver.fallback);
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
      `;

      this._shadow.querySelectorAll("button.mark").forEach((btn) => {
        btn.addEventListener("click", () => this.open());
      });

      this._renderDialog();
    }

    _removeDialog() {
      if (this._dialogHost) this._dialogHost.remove();
      this._dialogHost = null;
      this._dialogRoot = null;
    }

    _renderDialog() {
      if (!this.isConnected || this._phase === "idle") {
        this._removeDialog();
        return;
      }
      const previous = this._dialogRoot && this._dialogRoot.querySelector(".modal");
      const scrollTop = previous && this._phase === "dialog" ? previous.scrollTop : 0;
      const focusedAction = this._dialogRoot?.activeElement?.dataset?.action;

      if (!this._dialogHost) {
        // The mobile menu hides the badge's ancestors. Keep the dialog at page level.
        this._dialogHost = document.createElement("div");
        this._dialogHost.className = "ftw-update-dialog";
        this._dialogRoot = this._dialogHost.attachShadow({ mode: "open" });
        document.body.appendChild(this._dialogHost);
      }
      this._dialogRoot.innerHTML = `<style>${this._styles()}</style>${this._modalHTML()}`;
      this._wireModal(this._dialogRoot);
      const modal = this._dialogRoot.querySelector(".modal");
      if (modal) modal.scrollTop = scrollTop;
      if (focusedAction) {
        const button = [...this._dialogRoot.querySelectorAll("[data-action]")].find(el => el.dataset.action === focusedAction);
        if (button && !button.disabled) button.focus({ preventScroll:true });
      }
    }

    _modalHTML() {
      const info = this._info || {};
      if (this._phase === "updating") return this._updatingModalHTML();
      if (!this._info) return this._versionLoadingHTML();
      return this._versionHTML(info);
    }

    // The owner runs updates on the machine, by hand or from their own
    // automation (ADR 0007, decision 12). This panel reports; it has no
    // update, rollback, channel or backup controls.
    _versionHTML(info) {
      const channel = info.channel ? ` on the ${escapeHTML(info.channel)} channel` : "";
      const checked = info.checked_at ? Date.parse(info.checked_at) : 0;
      const checkedLine = checked > 0
        ? `Checked ${new Date(checked).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`
        : "Not checked yet.";
      const notesHref = safeHref(info.release_notes_url);
      const notes = notesHref
        ? ` <a class="notes-link" href="${escapeHTML(notesHref)}" target="_blank" rel="noopener">What's new ↗</a>`
        : "";
      let release;
      if (info.update_available && info.last_failed && info.last_failed === info.latest) {
        release = `<p><strong>${escapeHTML(info.latest)}</strong> is published.${notes}</p>
          <p class="err">It failed on this machine, so FTW waits for a newer release. <code>ftw update --retry</code> tries it again.</p>`;
      } else if (info.update_available && info.full_backup_required) {
        release = `<p><strong>${escapeHTML(info.latest || "A newer release")}</strong> is published.${notes}</p>
          <p class="err">It changes stored data, which a native update cannot take yet. ${escapeHTML(info.current || "This release")} stays installed.</p>`;
      } else if (info.update_available) {
        release = `<p><strong>${escapeHTML(info.latest || "A newer release")}</strong> is published.${notes}</p>
          <p>On the machine that runs FTW, install it with:</p>
          <pre class="cmd">ftw update</pre>`;
      } else {
        release = `<p class="dim">Nothing newer is published${channel}.</p>`;
      }
      const run = this._lastRun || {};
      const action = run.action === "rollback" ? "rollback" : "update";
      let lastRun = "";
      if (run.state === "done") {
        lastRun = `<p class="dim">Last ${action}: ${escapeHTML(run.message || "done")}</p>`;
      } else if (run.state === "failed") {
        lastRun = `<p class="err">Last ${action} failed: ${escapeHTML(run.message || "no reason given")}</p>`;
      }
      return `
        <div class="backdrop" data-action="close"></div>
        <div class="modal" role="dialog" aria-modal="true" aria-labelledby="ftw-upd-title">
          <header>
            <h3 id="ftw-upd-title">Version</h3>
            <button class="x" data-action="close" aria-label="Close">×</button>
          </header>
          <div class="body">
            <p><strong>${escapeHTML(info.current || "unknown")}</strong> is running${channel}.</p>
            ${release}
            ${lastRun}
            <p class="dim">On that machine, <code>ftw status</code> shows health and the last update, and <code>ftw help</code> lists the other steps.</p>
            <p class="checked-at">${escapeHTML(checkedLine)}</p>
            ${info.err ? `<p class="err">Last check failed: ${escapeHTML(info.err)}</p>` : ""}
          </div>
          <footer>
            <button class="btn" data-action="check">Check again</button>
          </footer>
        </div>`;
    }

    // Shown until the first version check answers.
    _versionLoadingHTML() {
      return `
        <div class="backdrop" data-action="close"></div>
        <div class="modal" role="dialog" aria-modal="true" aria-labelledby="ftw-upd-title">
          <header>
            <h3 id="ftw-upd-title">Version</h3>
            <button class="x" data-action="close" aria-label="Close">×</button>
          </header>
          <div class="body">
            <p class="dim" role="status">Reading the running version…</p>
          </div>
        </div>`;
    }

    _updatingModalHTML() {
      if (this._bootHealth) {
        return `<div class="backdrop"></div><div class="modal" role="dialog" aria-modal="true" aria-labelledby="boot-title">
          <header><h3 id="boot-title">Starting FTW</h3></header><div class="body">
          ${this._migrationHTML || '<p>Core is preparing to start. Control has not started yet.</p><p>Keep the box powered. This page checks progress automatically.</p>'}
          <p class="dim">${this._bootConnected ? "The box is responding." : "Cannot reach the box. The last report may be out of date."}</p>
          </div><footer><button class="btn btn-primary" data-action="reload">Reload status</button>
          <span class="dim">Reloading this page does not restart the box.</span></footer></div>`;
      }
      const st = this._runStatus || { state: "starting" };
      const action = st.action || "update";
      const elapsed = Math.round((Date.now() - this._updateStartedAt) / 1000);
      const label = actionLabel(st.state, action);
      const spinner = st.state === "failed" ? "" : `<span class="spinner"></span>`;
      const timedOut = !!st.timedOut;
      const failed = st.state === "failed";
      const progress = operationProgress(st);
      const phaseStarted = st.phase_started_at ? Date.parse(st.phase_started_at) : 0;
      const phaseElapsed = Math.max(0, Math.round((Date.now() - (phaseStarted > 0 ? phaseStarted : this._updateStartedAt)) / 1000));
      const bytesNow = Number(st.progress_current) || 0;
      const bytesTotal = Number(st.progress_total) || 0;
      if (st.progress_unit === "bytes" && bytesNow !== this._measuredBytes) {
        this._measuredBytes = bytesNow;
        this._measuredAt = Date.now();
      }
      const byteProgress = st.progress_unit === "bytes" && (bytesNow > 0 || bytesTotal > 0)
        ? (bytesTotal > 0
          ? `<p class="dim">${escapeHTML(formatBytes(bytesNow))} / ${escapeHTML(formatBytes(bytesTotal))}</p>`
          : `<p class="dim">${escapeHTML(formatBytes(bytesNow))} written, total unknown</p>`)
        : "";
      const quietFor = this._measuredAt ? Date.now() - this._measuredAt : 0;
      const stalled = st.progress_unit === "bytes" && bytesNow > 0 && quietFor > 20000
        ? `<p class="dim">No new measured progress for ${escapeHTML(formatElapsed(Math.round(quietFor / 1000)))}. The clock above is only how long this step has been open.</p>`
        : "";
      const progressHTML = failed ? "" : `
        <div class="update-progress" role="progressbar" aria-valuemin="0" aria-valuemax="${progress.total}" aria-valuenow="${progress.step}">
          <span style="width:${progress.percent}%"></span>
        </div>
        <p class="update-step">Step ${progress.step} of ${progress.total} · ${escapeHTML(label)}</p>
        <p class="dim">This step: ${escapeHTML(formatElapsed(phaseElapsed))}</p>
        ${byteProgress}
        ${stalled}`;

      const body = failed
        ? `<p class="err">${escapeHTML(st.message || "Update failed")}</p>
           <p>The main service may still be running — reload the page to check.</p>`
        : timedOut
        ? `<p>Still working after ${elapsed}s. Core may have been slow to restart.</p>
           <p>You can reload manually if the UI keeps the overlay stuck.</p>`
        : `${progressHTML}
           ${this._operationDetailHTML(st)}
           <p class="dim">Total: ${escapeHTML(formatElapsed(elapsed))}. The page will reload automatically.</p>`;

      const footer = failed || timedOut
        ? `<button class="btn btn-primary" data-action="reload">Reload page</button>
           <button class="btn btn-ghost" data-action="close">Dismiss</button>`
        : `<span class="dim">Keep the box powered. You can reopen this page to check progress.</span>`;

      let title;
      switch (action) {
        case "restart":  title = "Restarting service"; break;
        case "rollback": title = "Rolling back"; break;
        default:         title = "Updating service";
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
        case "pulling":
          return msg + `<p class="dim">Downloading and checking the pinned release package.</p>`;
        case "restarting":
          return msg + `<p class="dim">Starting the chosen Core binary. Brief connection errors are expected.</p>`;
        case "checking":
          return msg + `<p class="dim">Core has started. Waiting for its API and health checks before marking the update complete.</p>`;
        default:
          return msg;
      }
    }

    _wireModal(root) {
      // Bind the dialog controls and its sibling backdrop.
      root.querySelectorAll("[data-action]").forEach((el) => {
        el.addEventListener("click", (e) => {
          const action = e.currentTarget.dataset.action;
          switch (action) {
            case "close":
              this._resumeGeneration += 1;
              this._phase = "idle";
              this._stopUpdateTimers();
              this._render();
              break;
            case "check":
              this._unskipAndCheck();
              break;
            case "reload":
              this._attemptReload();
              break;
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
             header (close ×) or the footer off-screen. */
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
        .checked-at { margin: 0; color: var(--fg-dim, #a0a0a0); font-size: 0.75rem; white-space: nowrap; }
        .notes-link {
          color: var(--accent-e, #f59e0b);
          text-decoration: none;
        }
        .notes-link:hover { text-decoration: underline; }
        .err {
          margin-top: 0.75rem;
          color: var(--red-e, #f87171); font-size: 0.85rem;
        }
        .dim { color: var(--fg-dim, #a0a0a0); font-size: 0.8rem; }
        .cmd, code { font-family: var(--mono, ui-monospace, monospace); }
        .cmd {
          margin: 0.25rem 0 0.75rem;
          padding: 0.5rem 0.7rem;
          border: 1px solid var(--line, #2a2a2a);
          border-radius: var(--radius-xs, 4px);
          color: var(--fg, #e5e5e5);
          user-select: all;
        }
        .modal footer {
          display: flex; gap: 0.5rem; justify-content: flex-end;
          padding: 0.75rem 1rem;
          border-top: 1px solid var(--line, #2a2a2a);
          flex-wrap: wrap;
          /* Stick to the bottom of the modal while the body scrolls. */
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
      `;
    }
  }

  function actionLabel(state, action) {
    switch (state) {
      case "pulling":    return "Downloading release package";
      case "restarting": return action === "rollback" ? "Starting previous Core" : "Starting Core";
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
    return ["starting", "pulling", "checking", "restarting"].includes(state);
  }

  // Core names the step and step count of every run; the fallback follows a
  // native update: download, check, start.
  function operationProgress(st) {
    let total = Number(st && st.total_steps) || 0;
    let step = Number(st && st.step) || 0;
    if (!total) {
      total = 3;
      switch (st && st.state) {
        case "checking": step = 2; break;
        case "restarting":
        case "done": step = 3; break;
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

  customElements.define("ftw-update-badge", FtwUpdateBadge);
})();
