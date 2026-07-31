// Settings → Remote: local-only Home Link setup and passkey enrollment.
(function () {
  "use strict";

  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function statusText(status) {
    if (!status.identity_ready) return "Gateway identity is not ready on this host.";
    if (!status.enabled) return "Remote access is off. Enable it below, save, then restart Core.";
    if (status.runtime && status.runtime.connected) return "Connected to the Home Link relay.";
    if (status.runtime && status.runtime.last_error) return "Trying to reconnect to the Home Link relay.";
    return "Starting the Home Link relay connection.";
  }

  function actionsState(status) {
    return {
      showAdmin: !!status.identity_ready,
      showSetup: !!status.identity_ready && !!status.enabled,
    };
  }

  function validPairingLabel(label) {
    return typeof label === "string" && label.length > 0 &&
      label.trim() === label &&
      new TextEncoder().encode(label).length <= 80 &&
      !/[\u0000-\u001f\u007f-\u009f\u2028\u2029\u200e\u200f\u202a-\u202e\u2066-\u2069\ufeff]/u.test(label);
  }

  S.tabs["home-link"] = {
    render: function (ctx) {
      if (!ctx.config.home_link) ctx.config.home_link = { enabled: false };
      return '<div id="home-link-status" class="ha-status-indicator">checking…</div>' +
        '<fieldset><legend>Remote access</legend>' +
        '<label><input type="checkbox" data-checkbox-path="home_link.enabled"' +
        (ctx.config.home_link.enabled ? " checked" : "") +
        '> Enable Home Link</label>' +
        '<p class="hint">FTW connects out to the relay. The relay cannot read your data. Every remote read needs your local passkey.</p>' +
        '<div id="home-link-actions" class="hidden">' +
        '<div id="home-link-setup" class="hidden">' +
        '<label>Passkey label</label>' +
        '<div class="field-row"><input id="home-link-label" type="text" maxlength="80" value="My device">' +
        '<button id="home-link-enroll" type="button" class="btn-send">Open passkey setup</button></div>' +
        '<label>Remote link</label>' +
        '<div class="field-row"><input id="home-link-invite" type="text" readonly>' +
        '<button id="home-link-copy" type="button" class="btn-ghost">Copy</button></div>' +
        '</div>' +
        '<p class="hint">Passkeys remain available to revoke when Remote is off.</p>' +
        '<div id="home-link-credentials"></div>' +
        '</div></fieldset>';
    },
    after: function () {
      var statusEl = document.getElementById("home-link-status");
      var actionsEl = document.getElementById("home-link-actions");
      var setupEl = document.getElementById("home-link-setup");
      var inviteEl = document.getElementById("home-link-invite");
      var credentialsEl = document.getElementById("home-link-credentials");
      var enrollBtn = document.getElementById("home-link-enroll");
      var copyBtn = document.getElementById("home-link-copy");
      var latestStatus = null;

      function request(path, options) {
        return fetch(path, options).then(function (response) {
          return response.json().catch(function () { return {}; }).then(function (body) {
            if (!response.ok) throw new Error(body.error || ("HTTP " + response.status));
            return body;
          });
        });
      }

      function renderCredentials(items) {
        credentialsEl.innerHTML = "";
        if (!items || !items.length) {
          credentialsEl.textContent = "No passkeys enrolled yet.";
          return;
        }
        items.forEach(function (item) {
          var row = document.createElement("div");
          row.className = "field-row";
          var label = document.createElement("span");
          label.textContent = item.label || "Passkey";
          var revoke = document.createElement("button");
          revoke.type = "button";
          revoke.className = "btn-ghost";
          revoke.textContent = "Revoke";
          revoke.addEventListener("click", function () {
            revoke.disabled = true;
            request("/api/home-link/passkeys/revoke", {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({ credential_id: item.id }),
            }).then(refresh).catch(function () {
              revoke.disabled = false;
              statusEl.textContent = "Could not revoke the passkey.";
            });
          });
          row.appendChild(label);
          row.appendChild(revoke);
          credentialsEl.appendChild(row);
        });
      }

      function refresh() {
        return request("/api/home-link/status").then(function (status) {
          latestStatus = status;
          statusEl.textContent = statusText(status);
          statusEl.className = "ha-status-indicator " +
            (status.runtime && status.runtime.connected ? "ha-ok" :
              status.enabled ? "ha-warn" : "ha-off");
          var view = actionsState(status);
          actionsEl.classList.toggle("hidden", !view.showAdmin);
          setupEl.classList.toggle("hidden", !view.showSetup);
          inviteEl.value = status.invite_url || "";
          renderCredentials(status.credentials || []);
        }).catch(function () {
          statusEl.className = "ha-status-indicator ha-warn";
          statusEl.textContent = "Remote status is unavailable.";
        });
      }

      enrollBtn.addEventListener("click", function () {
        var label = document.getElementById("home-link-label").value;
        if (!validPairingLabel(label)) {
          statusEl.textContent = "Use a label from 1 to 80 bytes without control characters.";
          return;
        }
        var setupTab = window.open("about:blank", "_blank");
        if (!setupTab) {
          statusEl.textContent = "Allow popups for this local FTW page, then try again.";
          return;
        }
        setupTab.opener = null;
        enrollBtn.disabled = true;
        request("/api/home-link/pairing", { method: "POST" })
          .then(function (created) {
            var setup = new URL(latestStatus.invite_url);
            var fragment = new URLSearchParams();
            fragment.set("pairing_id", created.id);
            fragment.set("pairing_secret", created.secret);
            fragment.set("label", label);
            setup.hash = fragment.toString();
            setupTab.location.replace(setup.toString());
            statusEl.textContent = "Finish passkey setup in the new secure tab.";
          })
          .catch(function () {
            setupTab.close();
            statusEl.textContent = "Could not start passkey setup.";
          })
          .finally(function () { enrollBtn.disabled = false; });
      });

      copyBtn.addEventListener("click", function () {
        if (!latestStatus || !latestStatus.invite_url) return;
        navigator.clipboard.writeText(latestStatus.invite_url).then(function () {
          copyBtn.textContent = "Copied";
          setTimeout(function () { copyBtn.textContent = "Copy"; }, 1200);
        });
      });

      refresh();
      if (window._homeLinkStatusTimer) clearInterval(window._homeLinkStatusTimer);
      window._homeLinkStatusTimer = setInterval(refresh, 5000);
    },
    _pure: {
      actionsState: actionsState,
      statusText: statusText,
      validPairingLabel: validPairingLabel,
    },
  };
})();
